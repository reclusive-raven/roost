package roost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clock lets a test move the app's time forward.
func clock(m *App, at *time.Time) { m.Now = func() time.Time { return *at } }

func listDir(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			out = append(out, rel)
		}
		return nil
	})
	return out
}

// A file system mtime has nanoseconds. The record must keep them all, or an
// unchanged file never matches and its kept link is never released.
func TestUnchangedFileMatchesItsRecordToTheNanosecond(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "projects")
	m := testApp(t, Source{Kind: "claude", Dir: dir})
	path := filepath.Join(dir, "p", "a.jsonl")
	writeAged(t, path, "{}\n", 48*time.Hour)
	stamp := now.Add(-48*time.Hour + 123456789*time.Nanosecond + 7)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Run(t.Context(), RunOptions{}); err != nil {
		t.Fatal(err)
	}
	known, err := m.rec.Known(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found, err := m.scan(t.Context(), known, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Pending) != 0 || len(found.Same) != 0 {
		t.Fatalf("unchanged file looked changed: pending %d same %d", len(found.Pending), len(found.Same))
	}
}

// The whole life of a Codex session: archived, moved to archived_sessions,
// deleted in the app, restored from its bundle under its earlier name, and
// its kept link released only once the bundle is a day old.
func TestSessionLifecycle(t *testing.T) {
	root := t.TempDir()
	sessions, archived := filepath.Join(root, "sessions"), filepath.Join(root, "archived_sessions")
	m := testApp(t, Source{Kind: "codex", Dir: sessions}, Source{Kind: "codex", Dir: archived})
	at := now
	clock(m, &at)
	body := "one\ntwo\n"
	writeAged(t, filepath.Join(sessions, "rollout-a.jsonl"), body, 48*time.Hour)
	if err := os.MkdirAll(archived, 0o700); err != nil {
		t.Fatal(err)
	}
	stats, err := m.Run(t.Context(), RunOptions{})
	if err != nil || stats["bundles"] != 1 || stats["linked"] != 1 {
		t.Fatalf("first pass %v, %v", stats, err)
	}
	// Codex moves it, then the app deletes it.
	if err := os.Rename(filepath.Join(sessions, "rollout-a.jsonl"), filepath.Join(archived, "rollout-a.jsonl")); err != nil {
		t.Fatal(err)
	}
	if stats, err = m.Run(t.Context(), RunOptions{}); err != nil || stats["bundles"] != 0 || stats["moved"] != 1 {
		t.Fatalf("move pass %v, %v", stats, err)
	}
	if err := os.Remove(filepath.Join(archived, "rollout-a.jsonl")); err != nil {
		t.Fatal(err)
	}
	if stats, err = m.Run(t.Context(), RunOptions{}); err != nil || stats["orphans"] != 1 || stats["released"] != 0 {
		t.Fatalf("held before a day: %v, %v", stats, err)
	}
	out := t.TempDir()
	var buf strings.Builder
	if err := m.cmdRestore(t.Context(), &buf, []string{"rollout-a", "--to", out}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(out, "rollout-a.jsonl")); err != nil || string(got) != body {
		t.Fatalf("restored %q, %v", got, err)
	}
	known, err := m.rec.Known(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	moved := known.Entries["codex/archived_sessions/rollout-a.jsonl"]
	manifestPath := filepath.Join(m.Config.ArchiveDir, filepath.FromSlash(moved.Bundle)+manifestExt)
	if name, err := nameInBundle(manifestPath, "codex/archived_sessions/rollout-a.jsonl", moved.SHA256); err != nil || name != "codex/sessions/rollout-a.jsonl" {
		t.Fatalf("moved entry resolves to %q, %v", name, err)
	}
	at = now.Add(25 * time.Hour)
	m.Config.Release = false
	if stats, err = m.Run(t.Context(), RunOptions{}); err != nil || stats["released"] != 0 {
		t.Fatalf("released while switched off: %v, %v", stats, err)
	}
	m.Config.Release = true
	var listed []string
	if stats, err = m.Run(t.Context(), RunOptions{List: func(p string) { listed = append(listed, p) }}); err != nil || stats["released"] != 1 || len(listed) != 1 {
		t.Fatalf("release pass %v, %v", stats, err)
	}
	if left := listDir(t, filepath.Join(m.Config.DataDir, keptDir)); len(left) != 0 {
		t.Fatalf("kept tree still holds %v", left)
	}
}

// Manifests written by roost's predecessor carry an offset and nanoseconds.
// Reconciling them must make the files they hold look unchanged.
func TestReconcileReadsAnOlderManifest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "projects")
	m := testApp(t, Source{Kind: "claude", Dir: dir})
	path := filepath.Join(dir, "p", "a.jsonl")
	writeAged(t, path, "{}\n", 48*time.Hour)
	stamp := time.Date(2026, 11, 1, 9, 8, 7, 654321987, time.FixedZone("", 19800))
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	month := filepath.Join(m.Config.ArchiveDir, "2026", "11")
	if err := os.MkdirAll(month, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"version": 1, "bundle": "2026/11/2026-11-02-01", "created": "2026-11-02T04:05:52.859843+05:30",
 "sha256": "x", "bytes": 1, "entries": [{"name": "claude/projects/p/a.jsonl", "size": 3,
 "modified": "2026-11-01T09:08:07.654321987+05:30",
 "sha256": "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"}]}`
	if err := os.WriteFile(filepath.Join(month, "2026-11-02-01.manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := m.reconcile(t.Context()); err != nil || n != 1 {
		t.Fatalf("reconciled %d, %v", n, err)
	}
	known, err := m.rec.Known(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found, err := m.scan(t.Context(), known, true)
	if err != nil || len(found.Pending) != 0 || len(found.Same) != 0 {
		t.Fatalf("found %+v, %v", found, err)
	}
}

func TestDryRunChangesNothingOutsideTheDataFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "projects")
	m := testApp(t, Source{Kind: "claude", Dir: dir})
	writeAged(t, filepath.Join(dir, "p", "a.jsonl"), "{}\n", 48*time.Hour)
	stats, err := m.Run(t.Context(), RunOptions{Dry: true})
	if err != nil || stats["linked"] != 1 || stats["bundled_files"] != 1 {
		t.Fatalf("dry %v, %v", stats, err)
	}
	if got := listDir(t, m.Config.ArchiveDir); len(got) != 0 {
		t.Fatalf("archive written: %v", got)
	}
	if got := listDir(t, filepath.Join(m.Config.DataDir, keptDir)); len(got) != 0 {
		t.Fatalf("links made: %v", got)
	}
	if got := listDir(t, filepath.Join(m.Config.DataDir, "tmp")); len(got) != 0 {
		t.Fatalf("dry bundle left: %v", got)
	}
}

func TestFlushBundlesAtOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "projects")
	m := testApp(t, Source{Kind: "claude", Dir: dir})
	writeAged(t, filepath.Join(dir, "p", "a.jsonl"), "{}\n", 48*time.Hour)
	if _, err := m.Run(t.Context(), RunOptions{}); err != nil {
		t.Fatal(err)
	}
	writeAged(t, filepath.Join(dir, "p", "b.jsonl"), "{}\n{}\n", 2*time.Hour)
	if stats, err := m.Run(t.Context(), RunOptions{}); err != nil || stats["bundles"] != 0 {
		t.Fatalf("not idle yet: %v, %v", stats, err)
	}
	if stats, err := m.Run(t.Context(), RunOptions{Flush: true}); err != nil || stats["bundles"] != 1 {
		t.Fatalf("flush %v, %v", stats, err)
	}
}

func TestOnePassAtATime(t *testing.T) {
	m := testApp(t, Source{Kind: "claude", Dir: t.TempDir()})
	unlock, err := m.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := m.Run(t.Context(), RunOptions{}); err == nil || !strings.Contains(err.Error(), "another roost") {
		t.Fatalf("err = %v", err)
	}
}

func TestMissingArchiveFolderFailsAfterLinking(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "projects")
	m := testApp(t, Source{Kind: "claude", Dir: dir})
	m.Config.ArchiveDir = filepath.Join(t.TempDir(), "not-mounted")
	writeAged(t, filepath.Join(dir, "p", "a.jsonl"), "{}\n", time.Hour)
	stats, err := m.Run(t.Context(), RunOptions{})
	if err == nil || stats["linked"] != 1 {
		t.Fatalf("stats %v, err %v", stats, err)
	}
	if _, err := os.Stat(m.Config.ArchiveDir); err == nil {
		t.Fatal("created the archive folder")
	}
}

func TestVerifyCatchesADamagedBundle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "projects")
	m := testApp(t, Source{Kind: "claude", Dir: dir})
	writeAged(t, filepath.Join(dir, "p", "a.jsonl"), strings.Repeat("{}\n", 1000), 48*time.Hour)
	if _, err := m.Run(t.Context(), RunOptions{}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := m.cmdVerify(t.Context(), &out, nil); err != nil || !strings.Contains(out.String(), "1 bundles checked, 0 failed") {
		t.Fatalf("clean verify %q, %v", out.String(), err)
	}
	paths, _ := filepath.Glob(filepath.Join(m.Config.ArchiveDir, "*", "*", "*.tar.zst"))
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(paths[0], data, 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := m.cmdVerify(t.Context(), &out, nil); err == nil || !strings.Contains(out.String(), "FAIL") {
		t.Fatalf("damaged verify %q, %v", out.String(), err)
	}
}

func TestStatusCheckFailsWhenStale(t *testing.T) {
	m := testApp(t, Source{Kind: "claude", Dir: t.TempDir()})
	var out strings.Builder
	if err := m.cmdStatus(t.Context(), &out, []string{"--check"}); err != errCheck {
		t.Fatalf("never ran: %v", err)
	}
	if _, err := m.Run(t.Context(), RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := m.cmdStatus(t.Context(), &out, []string{"--check"}); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	m.Now = func() time.Time { return now.Add(staleAfter + time.Hour) }
	if err := m.cmdStatus(t.Context(), &out, []string{"--check"}); err != errCheck {
		t.Fatalf("stale: %v", err)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		path := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(dir, "missing-codex"))
	if err := os.MkdirAll(filepath.Join(dir, "claude", "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(write(`archive_dir = "` + filepath.Join(dir, "archive") + `"`))
	if err != nil || len(cfg.Sources) != 1 || cfg.Sources[0].Name() != "claude/projects" || !cfg.Release {
		t.Fatalf("defaults %+v, %v", cfg, err)
	}
	cfg, err = Load(write("archive_dir = \"/a\"\nrelease = false\n[[sources]]\nkind = \"codex\"\ndir = \"/x/sessions\"\n"))
	if err != nil || len(cfg.Sources) != 1 || cfg.Sources[0].Dir != "/x/sessions" || cfg.Release {
		t.Fatalf("explicit %+v, %v", cfg, err)
	}
	for _, bad := range []string{`archive_dir = "/a"` + "\nrelaese = true", ``, `archive_dir = "relative"`, `archive_dir = "/a"` + "\nidle_hours = 0"} {
		if _, err := Load(write(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := Load(filepath.Join(dir, "none.toml")); err == nil || !strings.Contains(err.Error(), "archive_dir") {
		t.Errorf("missing file: %v", err)
	}
}
