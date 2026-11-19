package roost

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

var now = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func testApp(t *testing.T, sources ...Source) *App {
	t.Helper()
	cfg := Defaults()
	cfg.DataDir = t.TempDir()
	cfg.ArchiveDir = t.TempDir()
	cfg.Sources = sources
	cfg.Notify = false
	rec, err := openRecord(t.Context(), filepath.Join(cfg.DataDir, "roost.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rec.close() })
	return &App{Config: cfg, Log: slog.New(slog.DiscardHandler), Now: func() time.Time { return now }, rec: rec}
}

func writeAged(t *testing.T, path, body string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := now.Add(-age)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

func TestScanSortsFinishedFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "projects")
	m := testApp(t, Source{Kind: "claude", Dir: dir})
	m.Config.ExcludeProjects = []string{"Secret-Project"}
	writeAged(t, filepath.Join(dir, "-work-app", "old.jsonl"), "{}\n", 48*time.Hour)
	writeAged(t, filepath.Join(dir, "-work-app", "older", "tool-results", "x.txt"), "out", 72*time.Hour)
	writeAged(t, filepath.Join(dir, "-work-app", "live.jsonl"), "{}\n", time.Hour)
	writeAged(t, filepath.Join(dir, "-work-secret-project", "hidden.jsonl"), "{}\n", 48*time.Hour)
	found, err := m.scan(t.Context(), emptyKnown(), true)
	if err != nil {
		t.Fatal(err)
	}
	if found.Files != 4 || len(found.Pending) != 2 || found.Pending[0].Entry != "claude/projects/-work-app/older/tool-results/x.txt" {
		t.Fatalf("found = %+v", found)
	}
	if len(found.Indexable) != 1 || found.Indexable[0].Entry != "claude/projects/-work-app/old.jsonl" {
		t.Fatalf("indexable = %+v", found.Indexable)
	}
}

func TestScanFollowsChangesAndMoves(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	archived := filepath.Join(root, "archived_sessions")
	m := testApp(t, Source{Kind: "codex", Dir: sessions}, Source{Kind: "codex", Dir: archived})
	writeAged(t, filepath.Join(sessions, "a.jsonl"), "one\n", 48*time.Hour)
	writeAged(t, filepath.Join(sessions, "b.jsonl"), "two\n", 48*time.Hour)
	first, err := m.scan(t.Context(), emptyKnown(), true)
	if err != nil {
		t.Fatal(err)
	}
	known := emptyKnown()
	for _, f := range first.Pending {
		c := Copy{Size: f.Size, Modified: f.Modified, SHA256: f.SHA256, Bundle: "2026/11/x", Archived: now}
		known.Entries[f.Entry], known.Hashes[hashKey("codex", f.SHA256)] = c, c
	}
	// a is resumed and changes; b moves to archived_sessions unchanged.
	writeAged(t, filepath.Join(sessions, "a.jsonl"), "one\nmore\n", 30*time.Hour)
	if err := os.Rename(filepath.Join(sessions, "b.jsonl"), filepath.Join(archived, "b.jsonl")); err != nil {
		if err := os.MkdirAll(archived, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(sessions, "b.jsonl"), filepath.Join(archived, "b.jsonl")); err != nil {
			t.Fatal(err)
		}
	}
	second, err := m.scan(t.Context(), known, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Pending) != 1 || second.Pending[0].Entry != "codex/sessions/a.jsonl" {
		t.Fatalf("pending = %+v", second.Pending)
	}
	if len(second.Same) != 1 || second.Same[0].Entry != "codex/archived_sessions/b.jsonl" || second.Same[0].Bundle != "2026/11/x" {
		t.Fatalf("same = %+v", second.Same)
	}
}

func TestAnExcludeMatchesFoldersAndDirectoriesAlike(t *testing.T) {
	m := testApp(t)
	for _, want := range []string{"/work/secret.project", "work-secret-project", "Secret Project"} {
		m.Config.ExcludeProjects = []string{want}
		if !m.excluded("-Users-me-work-secret-project") || !m.excluded("/Users/me/work/secret.project") {
			t.Errorf("%q missed", want)
		}
		if m.excluded("-Users-me-work-other") {
			t.Errorf("%q matched too much", want)
		}
	}
}

func TestMissingSourceIsNoted(t *testing.T) {
	m := testApp(t, Source{Kind: "codex", Dir: filepath.Join(t.TempDir(), "gone")})
	found, err := m.scan(t.Context(), emptyKnown(), true)
	if err != nil || found.Missing != 1 {
		t.Fatalf("found = %+v, err = %v", found, err)
	}
}

func TestDue(t *testing.T) {
	small := []File{{Size: 10}}
	if due(now, now.Add(-time.Hour), nil, 100) {
		t.Error("nothing waiting is not due")
	}
	if due(now, now.Add(-time.Hour), small, 100) {
		t.Error("a small batch waits for the day's bundle")
	}
	if !due(now, now.Add(-time.Hour), []File{{Size: 100}}, 100) {
		t.Error("a full bundle goes at once")
	}
	if !due(now, now.Add(-bundleEvery), small, 100) || !due(now, time.Time{}, small, 100) {
		t.Error("an aged or first bundle is due")
	}
}

func TestNextBatch(t *testing.T) {
	batch, rest := nextBatch([]File{{Size: 5}, {Size: 5}, {Size: 5}}, 10)
	if len(batch) != 2 || len(rest) != 1 {
		t.Fatalf("batch %d rest %d", len(batch), len(rest))
	}
	if batch, rest = nextBatch([]File{{Size: 50}, {Size: 1}}, 10); len(batch) != 1 || len(rest) != 1 {
		t.Fatalf("an oversized file goes alone: batch %d rest %d", len(batch), len(rest))
	}
}

func bundleOf(t *testing.T, dir string, names ...string) (string, Manifest) {
	t.Helper()
	var files []File
	for _, name := range names {
		path := filepath.Join(dir, "src", name)
		writeAged(t, path, "body of "+name+"\n", 48*time.Hour)
		files = append(files, File{Entry: "claude/projects/p/" + name, Path: path})
	}
	work := filepath.Join(dir, "bundle.tar.zst")
	m, err := writeBundle(t.Context(), work, files)
	if err != nil {
		t.Fatal(err)
	}
	return work, m
}

func TestBundleRoundTrip(t *testing.T) {
	dir := t.TempDir()
	work, m := bundleOf(t, dir, "a.jsonl", "b.jsonl")
	if len(m.Entries) != 2 || m.Bytes == 0 {
		t.Fatalf("manifest = %+v", m)
	}
	if err := verifyBundle(t.Context(), work, m); err != nil {
		t.Fatal(err)
	}
	m.Entries[1].SHA256 = "00"
	if err := verifyBundle(t.Context(), work, m); err == nil {
		t.Fatal("a wrong hash passed the check")
	}
	m.Entries = m.Entries[:1]
	if err := verifyBundle(t.Context(), work, m); err == nil {
		t.Fatal("an extra entry passed the check")
	}
}

func TestBundleSkipsAFileGoneSinceTheScan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.jsonl")
	writeAged(t, path, "x\n", 48*time.Hour)
	m, err := writeBundle(t.Context(), filepath.Join(dir, "b.tar.zst"), []File{{Entry: "a", Path: path}, {Entry: "gone", Path: filepath.Join(dir, "gone")}})
	if err != nil || len(m.Entries) != 1 {
		t.Fatalf("manifest = %+v, err = %v", m, err)
	}
}

func TestPublishOnlyAdds(t *testing.T) {
	dir := t.TempDir()
	root := t.TempDir()
	work, m := bundleOf(t, dir, "a.jsonl")
	for i, want := range []string{"2026/11/2026-11-03-01", "2026/11/2026-11-03-02"} {
		if _, err := publish(t.Context(), work, root, now, &m); err != nil {
			t.Fatal(i, err)
		}
		if m.Bundle != want {
			t.Fatalf("bundle = %s, want %s", m.Bundle, want)
		}
	}
	found, err := manifests(root, nil)
	if err != nil || len(found) != 2 || found[0].SHA256 == "" {
		t.Fatalf("manifests = %+v, err = %v", found, err)
	}
	if found, err = manifests(root, map[string]bool{"2026/11/2026-11-03-01": true}); err != nil || len(found) != 1 {
		t.Fatalf("skip: %+v, %v", found, err)
	}
	stale, _ := filepath.Glob(filepath.Join(root, "2026", "11", "*.partial"))
	if len(stale) != 0 {
		t.Fatalf("left behind: %v", stale)
	}
}

func TestPublishNeverCreatesTheArchiveFolder(t *testing.T) {
	dir := t.TempDir()
	work, m := bundleOf(t, dir, "a.jsonl")
	root := filepath.Join(dir, "not-mounted")
	if _, err := publish(t.Context(), work, root, now, &m); err == nil {
		t.Fatal("published to a missing folder")
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created the folder: %v", err)
	}
}

func TestPublishResumesAfterACrashedCopy(t *testing.T) {
	dir := t.TempDir()
	root := t.TempDir()
	work, m := bundleOf(t, dir, "a.jsonl")
	writeAged(t, filepath.Join(root, "2026", "11", "2026-11-03-01.tar.zst.partial"), "half a bundle", time.Hour)
	if _, err := publish(t.Context(), work, root, now, &m); err != nil {
		t.Fatal(err)
	}
	if m.Bundle != "2026/11/2026-11-03-01" {
		t.Fatalf("bundle = %s", m.Bundle)
	}
	if err := verifyBundle(t.Context(), filepath.Join(root, "2026", "11", "2026-11-03-01.tar.zst"), m); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreEntry(t *testing.T) {
	dir := t.TempDir()
	work, m := bundleOf(t, dir, "a.jsonl", "b.jsonl")
	dest := filepath.Join(dir, "out.jsonl")
	if err := restoreEntry(t.Context(), work, m.Entries[1].Name, m.Entries[1].SHA256, dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "body of b.jsonl\n" {
		t.Fatalf("restored %q, %v", got, err)
	}
	if err := restoreEntry(t.Context(), work, m.Entries[1].Name, m.Entries[1].SHA256, dest); err == nil {
		t.Fatal("replaced an existing file")
	}
	if err := restoreEntry(t.Context(), work, m.Entries[0].Name, "bad", filepath.Join(dir, "bad")); err == nil {
		t.Fatal("kept a mismatch")
	}
	if _, err := os.Stat(filepath.Join(dir, "bad")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("left a mismatched file")
	}
	if err := restoreEntry(t.Context(), work, "nope", "x", filepath.Join(dir, "no")); err == nil {
		t.Fatal("restored a missing entry")
	}
}

const claudeFixture = `{"type":"queue-operation","operation":"enqueue"}
{"type":"user","timestamp":"2026-11-01T10:00:00Z","cwd":"/work/app","message":{"role":"user","content":"Why is the retention seven days?\nsecond line<system-reminder>planted reminder</system-reminder>"}}
{"type":"assistant","timestamp":"2026-11-01T10:00:05Z","cwd":"/work/app","message":{"content":[{"type":"thinking","thinking":"private"},{"type":"text","text":"Because of cleanupPeriodDays."},{"type":"tool_use","name":"Bash","input":{"command":"grep cleanup settings.json"}}]}}
{"type":"user","timestamp":"2026-11-01T10:00:06Z","message":{"content":[{"type":"tool_result","content":"first result"},{"type":"tool_result","content":[{"type":"text","text":"listed result"}]}]}}
{"type":"attachment","attachment":{"x":1}}
{"type":"api-request-blob","message":{"content":[{"type":"text","text":"never index this"}]}}
{"type":"custom-title","customTitle":"Retention talk"}
{"type":"future-record","anything":true}
{"type":"assistant","timestamp":"2026-11-01T10:01:00Z","mess`

func TestParseClaude(t *testing.T) {
	got, err := parse(strings.NewReader(claudeFixture), "claude/projects/-work-app/abc.jsonl", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if got.Session != "abc" || got.Project != "-work-app" || got.CWD != "/work/app" || got.Title != "Retention talk" {
		t.Fatalf("transcript = %+v", got)
	}
	if got.Started != "2026-11-01T10:00:00Z" || got.Ended != "2026-11-01T10:00:06Z" {
		t.Fatalf("times %s %s", got.Started, got.Ended)
	}
	roles := []string{"user", "assistant", "tool_call", "tool_result", "tool_result"}
	if len(got.Texts) != len(roles) {
		t.Fatalf("texts = %+v", got.Texts)
	}
	for i, role := range roles {
		if got.Texts[i].Role != role || got.Texts[i].Seq != i {
			t.Errorf("text %d = %+v", i, got.Texts[i])
		}
	}
	all, _ := json.Marshal(got.Texts)
	for _, noise := range []string{"planted reminder", "private", "never index"} {
		if strings.Contains(string(all), noise) {
			t.Errorf("kept %q", noise)
		}
	}
	if got.Damaged != 1 || got.Skipped == 0 {
		t.Errorf("damaged %d skipped %d", got.Damaged, got.Skipped)
	}
}

func TestParseClipsToolOutput(t *testing.T) {
	long := strings.Repeat("é", resultLimit)
	line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "content": long}}}})
	got, err := parse(strings.NewReader(string(line)), "claude/projects/p/x.jsonl", "claude")
	if err != nil || len(got.Texts) != 1 || len(got.Texts[0].Text) > resultLimit+len("…") || !strings.HasSuffix(got.Texts[0].Text, "…") {
		t.Fatalf("texts = %d bytes, %v", len(got.Texts[0].Text), err)
	}
}

const codexFixture = `{"timestamp":"2026-10-20T21:44:26Z","type":"session_meta","payload":{"id":"s1","cwd":"/work/sample"}}
{"timestamp":"2026-10-20T21:44:27Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"memory guidance"}]}}
{"timestamp":"2026-10-20T21:44:27Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /work"}]}}
{"timestamp":"2026-10-20T21:44:28Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Fix the watchdog"}]}}
{"timestamp":"2026-10-20T21:44:29Z","type":"response_item","payload":{"type":"reasoning","encrypted_content":"opaque"}}
{"timestamp":"2026-10-20T21:44:30Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","input":"ls"}}
{"timestamp":"2026-10-20T21:44:31Z","type":"response_item","payload":{"type":"custom_tool_call_output","output":[{"type":"input_text","text":"file.go"}]}}
{"timestamp":"2026-10-20T21:44:32Z","type":"response_item","payload":{"type":"function_call","name":"sleep","arguments":"{\"ms\":1}"}}
{"timestamp":"2026-10-20T21:44:33Z","type":"response_item","payload":{"type":"function_call_output","output":"done"}}
{"timestamp":"2026-10-20T21:44:34Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Fixed it"}]}}
{"timestamp":"2026-10-20T21:44:34Z","type":"event_msg","payload":{"type":"token_count"}}
`

func zstdOf(t *testing.T, body string) string {
	t.Helper()
	var out strings.Builder
	w, err := zstd.NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestParseCodex(t *testing.T) {
	got, err := parse(strings.NewReader(zstdOf(t, codexFixture)), "codex/sessions/2026/rollout-x.jsonl.zst", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if got.Session != "rollout-x" || got.CWD != "/work/sample" || got.Title != "Fix the watchdog" {
		t.Fatalf("transcript = %+v", got)
	}
	roles := []string{"user", "tool_call", "tool_result", "tool_call", "tool_result", "assistant"}
	if len(got.Texts) != len(roles) {
		t.Fatalf("texts = %+v", got.Texts)
	}
	for i, role := range roles {
		if got.Texts[i].Role != role {
			t.Errorf("text %d = %+v", i, got.Texts[i])
		}
	}
}

func TestParseDamagedCompression(t *testing.T) {
	if _, err := parse(strings.NewReader("not zstd at all"), "codex/sessions/x.jsonl.zst", "codex"); err == nil {
		t.Fatal("garbage parsed")
	}
}

func indexFixture(t *testing.T) (*App, *Index, File) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "projects")
	m := testApp(t, Source{Kind: "claude", Dir: dir})
	path := filepath.Join(dir, "-work-app", "abc.jsonl")
	writeAged(t, path, claudeFixture, 48*time.Hour)
	found, err := m.scan(t.Context(), emptyKnown(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.index(t.Context(), found.Indexable); err != nil {
		t.Fatal(err)
	}
	ix, err := m.readIndex(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.close() })
	return m, ix, found.Indexable[0]
}

func TestSearch(t *testing.T) {
	_, ix, _ := indexFixture(t)
	hits, err := ix.search(t.Context(), "cleanupPeriodDays", Filter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Session != "abc" || hits[0].Role != "assistant" || !strings.Contains(hits[0].Snippet, "[cleanupPeriodDays]") {
		t.Fatalf("hits = %+v", hits)
	}
	for _, q := range []string{"settings.json", "grep AND", `quote"d`, "--flag", "NEAR(x"} {
		if _, err := ix.search(t.Context(), q, Filter{Limit: 10}); err != nil {
			t.Errorf("%q: %v", q, err)
		}
	}
	for _, f := range []Filter{{Project: "elsewhere"}, {Source: "codex"}, {Since: "2026-12-01"}} {
		f.Limit = 10
		if hits, err := ix.search(t.Context(), "cleanupPeriodDays", f); err != nil || len(hits) != 0 {
			t.Errorf("%+v: %d hits, %v", f, len(hits), err)
		}
	}
	if msgs, err := ix.messages(t.Context(), "abc", 0, 1); err != nil || len(msgs) != 2 {
		t.Fatalf("messages = %+v, %v", msgs, err)
	}
}

func TestIndexIsIncrementalAndReplaces(t *testing.T) {
	m, ix, f := indexFixture(t)
	if ok, err := ix.current(t.Context(), f); err != nil || !ok {
		t.Fatalf("current = %v, %v", ok, err)
	}
	stats, err := m.index(t.Context(), []File{f})
	if err != nil || stats.Indexed != 0 {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
	writeAged(t, f.Path, `{"type":"user","timestamp":"2026-11-02T10:00:00Z","message":{"content":"replacement words"}}`, 30*time.Hour)
	info, err := os.Stat(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	f.Size, f.Modified = info.Size(), info.ModTime()
	if stats, err = m.index(t.Context(), []File{f}); err != nil || stats.Indexed != 1 {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
	if hits, err := ix.search(t.Context(), "cleanupPeriodDays", Filter{Limit: 10}); err != nil || len(hits) != 0 {
		t.Fatalf("old text kept: %+v, %v", hits, err)
	}
	if hits, err := ix.search(t.Context(), "replacement", Filter{Limit: 10}); err != nil || len(hits) != 1 {
		t.Fatalf("new text missing: %+v, %v", hits, err)
	}
}

// Fake credentials in a transcript must not reach the index, whose text goes
// back into new transcripts through search output.
func TestIndexHoldsNoRecognisableSecrets(t *testing.T) {
	fakes := []string{
		"sk-ant-api03-FAKEFAKEFAKEFAKEFAKE", "ghp_FAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKE", "AKIAFAKEFAKEFAKEFAKE",
		"npg_FAKEfakeFAKE", "xoxb-1234-FAKE", "postgresql://app:hunter2fake@db.example.test/main",
	}
	var body strings.Builder
	for _, fake := range fakes {
		line, _ := json.Marshal(map[string]any{"type": "user", "timestamp": "2026-11-01T10:00:00Z", "message": map[string]any{"content": "searchword value " + fake}})
		body.Write(append(line, '\n'))
	}
	dir := filepath.Join(t.TempDir(), "projects")
	m := testApp(t, Source{Kind: "claude", Dir: dir})
	writeAged(t, filepath.Join(dir, "p", "s.jsonl"), body.String(), 48*time.Hour)
	found, err := m.scan(t.Context(), emptyKnown(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.index(t.Context(), found.Indexable); err != nil {
		t.Fatal(err)
	}
	ix, err := m.readIndex(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ix.close() }()
	msgs, err := ix.messages(t.Context(), "s", 0, 100)
	if err != nil || len(msgs) != len(fakes) {
		t.Fatalf("messages = %d, %v", len(msgs), err)
	}
	for _, msg := range msgs {
		for _, fake := range []string{"sk-ant", "ghp_", "AKIA", "npg_", "xoxb", "hunter2fake"} {
			if strings.Contains(msg.Text, fake) {
				t.Errorf("index kept %q in %q", fake, msg.Text)
			}
		}
	}
}

func TestUnreadableFileIsCountedOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	m := testApp(t, Source{Kind: "codex", Dir: dir})
	writeAged(t, filepath.Join(dir, "bad.jsonl.zst"), "not zstd", 48*time.Hour)
	found, err := m.scan(t.Context(), emptyKnown(), false)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := m.index(t.Context(), found.Indexable)
	if err != nil || stats.Unreadable != 1 {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
	if stats, err = m.index(t.Context(), found.Indexable); err != nil || stats.Indexed != 0 {
		t.Fatalf("second pass stats = %+v, %v", stats, err)
	}
}

func TestIndexRebuildsOnANewVersion(t *testing.T) {
	m, ix, _ := indexFixture(t)
	if _, err := ix.db.ExecContext(t.Context(), `PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	if err := ix.close(); err != nil {
		t.Fatal(err)
	}
	fresh, err := m.readIndex(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fresh.close() }()
	if c, err := fresh.counts(t.Context()); err != nil || c.Sessions != 0 {
		t.Fatalf("counts = %+v, %v", c, err)
	}
}

func TestIndexBundleKeepsNewestCopy(t *testing.T) {
	dir := t.TempDir()
	m := testApp(t)
	path, err := m.indexPath()
	if err != nil {
		t.Fatal(err)
	}
	ix, err := openIndex(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ix.close() }()
	var stats IndexStats
	for _, word := range []string{"firstword", "secondword"} {
		src := filepath.Join(dir, "x.jsonl")
		writeAged(t, src, `{"type":"user","timestamp":"2026-11-01T10:00:00Z","message":{"content":"`+word+`"}}`, 48*time.Hour)
		work := filepath.Join(dir, word+bundleExt)
		man, err := writeBundle(t.Context(), work, []File{{Entry: "claude/projects/p/x.jsonl", Path: src}})
		if err != nil {
			t.Fatal(err)
		}
		if err := m.indexBundle(t.Context(), ix, work, man, &stats); err != nil {
			t.Fatal(err)
		}
	}
	if hits, _ := ix.search(t.Context(), "firstword", Filter{Limit: 5}); len(hits) != 0 {
		t.Fatalf("older copy kept: %+v", hits)
	}
	if hits, _ := ix.search(t.Context(), "secondword", Filter{Limit: 5}); len(hits) != 1 {
		t.Fatalf("newest copy missing: %+v", hits)
	}
}

func TestSearchOutputIsLabelled(t *testing.T) {
	m, _, _ := indexFixture(t)
	var out strings.Builder
	if err := m.cmdSearch(t.Context(), &out, []string{"cleanupPeriodDays"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), notice) || !strings.Contains(out.String(), "abc #1 assistant 2026-11-01") {
		t.Fatalf("output = %q", out.String())
	}
	out.Reset()
	if err := m.cmdShow(t.Context(), &out, []string{"abc", "--json"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"notice"`) {
		t.Fatalf("output = %q", out.String())
	}
}

func TestSearchWithoutAnIndexSaysSo(t *testing.T) {
	m := testApp(t)
	if err := m.cmdSearch(t.Context(), io.Discard, []string{"x"}); err == nil || errors.Is(err, errUsage) {
		t.Fatalf("err = %v", err)
	}
}

func TestEachEntryStops(t *testing.T) {
	dir := t.TempDir()
	work, _ := bundleOf(t, dir, "a.jsonl", "b.jsonl")
	seen := 0
	err := eachEntry(context.Background(), work, func(*tar.Header, io.Reader) error {
		seen++
		return errStop
	})
	if err != nil || seen != 1 {
		t.Fatalf("seen %d, err %v", seen, err)
	}
}

func TestKeptLinkOutlivesADelete(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "projects")
	m := testApp(t, Source{Kind: "claude", Dir: dir})
	gone, stays := filepath.Join(dir, "-work-app", "gone.jsonl"), filepath.Join(dir, "-work-app", "stays.jsonl")
	writeAged(t, gone, "{}\n", 48*time.Hour)
	writeAged(t, stays, "{}\n", 48*time.Hour)
	if stats, err := m.keep(t.Context(), false); err != nil || stats.Linked != 2 {
		t.Fatalf("keep = %+v, %v", stats, err)
	}
	if stats, err := m.keep(t.Context(), false); err != nil || stats.Linked != 0 {
		t.Fatalf("second keep = %+v, %v", stats, err)
	}
	// An append after the link lands in the kept copy too.
	f, err := os.OpenFile(gone, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"more\":1}\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	stamp := now.Add(-48 * time.Hour)
	if err := os.Chtimes(gone, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	found, err := m.scan(t.Context(), emptyKnown(), true)
	if err != nil {
		t.Fatal(err)
	}
	if found.Files != 2 || len(found.Pending) != 2 || len(found.Release) != 0 {
		t.Fatalf("found = %+v", found)
	}
	var orphan File
	for _, f := range found.Pending {
		if f.Entry == "claude/projects/-work-app/gone.jsonl" {
			orphan = f
		}
	}
	if body, err := os.ReadFile(orphan.Path); err != nil || string(body) != "{}\n{\"more\":1}\n" {
		t.Fatalf("orphan %+v read %q, %v", orphan, body, err)
	}
	known := emptyKnown()
	known.Entries[orphan.Entry] = Copy{Size: orphan.Size, Modified: orphan.Modified, Archived: now.Add(-releaseAfter)}
	found, err = m.scan(t.Context(), known, true)
	if err != nil || len(found.Release) != 1 || found.Release[0] != orphan.Path {
		t.Fatalf("release = %+v, %v", found.Release, err)
	}
}

func TestKeepDropsTheOldNameOfAMove(t *testing.T) {
	root := t.TempDir()
	sessions, archived := filepath.Join(root, "sessions"), filepath.Join(root, "archived_sessions")
	m := testApp(t, Source{Kind: "codex", Dir: sessions}, Source{Kind: "codex", Dir: archived})
	writeAged(t, filepath.Join(sessions, "a.jsonl"), "one\n", time.Hour)
	if err := os.MkdirAll(archived, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := m.keep(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(sessions, "a.jsonl"), filepath.Join(archived, "a.jsonl")); err != nil {
		t.Fatal(err)
	}
	stats, err := m.keep(t.Context(), false)
	if err != nil || stats.Linked != 1 || stats.Moved != 1 {
		t.Fatalf("keep = %+v, %v", stats, err)
	}
	kept := filepath.Join(m.Config.DataDir, keptDir, "codex")
	if _, err := os.Lstat(filepath.Join(kept, "sessions", "a.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old name kept: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(kept, "archived_sessions", "a.jsonl")); err != nil {
		t.Fatal(err)
	}
}

func TestMissingSourceKeepsItsLinks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	m := testApp(t, Source{Kind: "codex", Dir: dir})
	writeAged(t, filepath.Join(dir, "a.jsonl"), "one\n", 48*time.Hour)
	if _, err := m.keep(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	stats, err := m.keep(t.Context(), false)
	if err != nil || stats.Missing != 1 || stats.Moved != 0 {
		t.Fatalf("keep = %+v, %v", stats, err)
	}
	found, err := m.scan(t.Context(), emptyKnown(), true)
	if err != nil || found.Missing != 1 || len(found.Pending) != 0 {
		t.Fatalf("found = %+v, %v", found, err)
	}
}
