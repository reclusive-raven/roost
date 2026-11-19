package roost

import (
	"archive/tar"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/reclusive-raven/roost/internal/redact"
	_ "modernc.org/sqlite" // the database/sql driver
)

// indexVersion rebuilds the index when the schema or what it keeps changes.
const indexVersion = 1

const schema = `
CREATE TABLE sessions (
 id INTEGER PRIMARY KEY,
 entry TEXT NOT NULL UNIQUE,
 session TEXT NOT NULL,
 kind TEXT NOT NULL,
 project TEXT NOT NULL,
 cwd TEXT NOT NULL,
 title TEXT NOT NULL,
 started TEXT NOT NULL,
 ended TEXT NOT NULL,
 size INTEGER NOT NULL,
 modified TEXT NOT NULL,
 skipped INTEGER NOT NULL,
 damaged INTEGER NOT NULL
);
CREATE INDEX sessions_session ON sessions (session);
CREATE TABLE messages (
 id INTEGER PRIMARY KEY,
 session_id INTEGER NOT NULL,
 seq INTEGER NOT NULL,
 role TEXT NOT NULL,
 ts TEXT NOT NULL,
 text TEXT NOT NULL
);
CREATE INDEX messages_session ON messages (session_id, seq);
CREATE VIRTUAL TABLE fts USING fts5(text, content='messages', content_rowid='id', tokenize='unicode61');
CREATE TRIGGER messages_ai AFTER INSERT ON messages BEGIN
 INSERT INTO fts(rowid, text) VALUES (new.id, new.text);
END;
CREATE TRIGGER messages_ad AFTER DELETE ON messages BEGIN
 INSERT INTO fts(fts, rowid, text) VALUES ('delete', old.id, old.text);
END;
`

// Index is the local search index. Everything in it passes through redaction
// first, and it can always be rebuilt from the bundles.
type Index struct{ db *sql.DB }

func (m *App) indexPath() (string, error) {
	return filepath.Join(m.Config.DataDir, "index.sqlite"), nil
}

// openIndex opens or creates the index at path, starting afresh when it was
// built by another version.
func openIndex(ctx context.Context, path string) (*Index, error) {
	dsn := url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)"}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		_ = db.Close()
		return nil, err
	}
	if version != indexVersion {
		_, err = db.ExecContext(ctx, `DROP TRIGGER IF EXISTS messages_ai; DROP TRIGGER IF EXISTS messages_ad;
			DROP TABLE IF EXISTS fts; DROP TABLE IF EXISTS messages; DROP TABLE IF EXISTS sessions;`+schema+
			fmt.Sprintf(`PRAGMA user_version = %d;`, indexVersion))
		if err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return &Index{db: db}, nil
}

func (ix *Index) close() error { return ix.db.Close() }

// current reports whether the index already has this file as it stands.
func (ix *Index) current(ctx context.Context, f File) (bool, error) {
	var size int64
	var modified string
	err := ix.db.QueryRowContext(ctx, `SELECT size, modified FROM sessions WHERE entry = ?`, f.Entry).Scan(&size, &modified)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return size == f.Size && modified == stamp(f.Modified), err
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// put replaces one file's rows in a single transaction.
func (ix *Index) put(ctx context.Context, f File, t Transcript) error {
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE session_id = (SELECT id FROM sessions WHERE entry = ?)`, f.Entry); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE entry = ?`, f.Entry); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO sessions (entry, session, kind, project, cwd, title, started, ended, size, modified, skipped, damaged)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, f.Entry, t.Session, f.Kind, t.Project, t.CWD, redact.Text(t.Title), t.Started, t.Ended, f.Size, stamp(f.Modified), t.Skipped, t.Damaged)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	insert, err := tx.PrepareContext(ctx, `INSERT INTO messages (session_id, seq, role, ts, text) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = insert.Close() }()
	for _, msg := range t.Texts {
		if _, err := insert.ExecContext(ctx, id, msg.Seq, msg.Role, msg.TS, redact.Text(msg.Text)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// IndexStats counts one indexing pass.
type IndexStats struct {
	Indexed    int
	Unreadable int
}

// add parses r into the index. A file that cannot be read is kept as a row
// with what came before the damage, so it is counted once rather than retried
// every hour.
func (m *App) add(ctx context.Context, ix *Index, f File, r io.Reader, stats *IndexStats) error {
	t, err := parse(r, f.Entry, f.Kind)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		m.Log.Warn("session transcript unreadable", "entry", f.Entry, "err", err)
		stats.Unreadable++
		t.Session = idOf(f.Entry)
		t.Damaged++
	}
	if m.excluded(t.CWD) {
		t = Transcript{Session: t.Session}
	}
	stats.Indexed++
	return ix.put(ctx, f, t)
}

// index brings the local index up to date with files.
func (m *App) index(ctx context.Context, files []File) (IndexStats, error) {
	var stats IndexStats
	path, err := m.indexPath()
	if err != nil {
		return stats, err
	}
	ix, err := openIndex(ctx, path)
	if err != nil {
		return stats, err
	}
	defer func() { _ = ix.close() }()
	return stats, m.indexFiles(ctx, ix, files, &stats)
}

func (m *App) indexFiles(ctx context.Context, ix *Index, files []File, stats *IndexStats) error {
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		ok, err := ix.current(ctx, f)
		if err != nil {
			return err
		}
		if ok {
			continue
		}
		in, err := os.Open(f.Path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		err = m.add(ctx, ix, f, in, stats)
		_ = in.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// rebuild writes a new index from every bundle, oldest first so the newest
// copy wins, then from the files on disk, and swaps it in. A pass writing to
// the old index meanwhile loses nothing: its next run sees those files as
// missing and adds them.
func (m *App) rebuild(ctx context.Context) (IndexStats, error) {
	var stats IndexStats
	path, err := m.indexPath()
	if err != nil {
		return stats, err
	}
	next := path + ".new"
	removeIndex(next)
	ix, err := openIndex(ctx, next)
	if err != nil {
		return stats, err
	}
	defer removeIndex(next)
	defer func() { _ = ix.close() }()
	if root := m.Config.ArchiveDir; root != "" {
		found, err := manifests(root, nil)
		if err != nil {
			return stats, err
		}
		for _, man := range found {
			if err := m.indexBundle(ctx, ix, filepath.Join(root, filepath.FromSlash(man.Bundle)+bundleExt), man, &stats); err != nil {
				return stats, err
			}
		}
	}
	known, err := m.rec.Known(ctx)
	if err != nil {
		return stats, err
	}
	local, err := m.scan(ctx, known, false)
	if err != nil {
		return stats, err
	}
	if err := m.indexFiles(ctx, ix, local.Indexable, &stats); err != nil {
		return stats, err
	}
	if err := ix.close(); err != nil {
		return stats, err
	}
	removeIndex(path)
	return stats, os.Rename(next, path)
}

func (m *App) indexBundle(ctx context.Context, ix *Index, path string, man Manifest, stats *IndexStats) error {
	entries := make(map[string]Entry, len(man.Entries))
	for _, e := range man.Entries {
		entries[e.Name] = e
	}
	return eachEntry(ctx, path, func(h *tar.Header, r io.Reader) error {
		e, ok := entries[h.Name]
		if !ok || !isTranscript(h.Name) {
			return nil
		}
		return m.add(ctx, ix, File{Entry: e.Name, Kind: kindOf(e.Name), Size: e.Size, Modified: e.Modified}, r, stats)
	})
}

func removeIndex(path string) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
}

// Hit is one search result.
type Hit struct {
	Session string `json:"session"`
	Kind    string `json:"kind"`
	CWD     string `json:"cwd"`
	Title   string `json:"title"`
	Seq     int    `json:"seq"`
	Role    string `json:"role"`
	TS      string `json:"ts"`
	Snippet string `json:"snippet"`
}

// Filter narrows a search.
type Filter struct {
	Project string
	Source  string
	Since   string
	Limit   int
}

// ftsQuery quotes every word, so a search for a flag or a path is taken
// literally rather than as FTS5 syntax. All words must appear.
func ftsQuery(q string) string {
	words := strings.Fields(q)
	for i, w := range words {
		words[i] = `"` + strings.ReplaceAll(w, `"`, `""`) + `"`
	}
	return strings.Join(words, " ")
}

func (ix *Index) search(ctx context.Context, q string, f Filter) ([]Hit, error) {
	query := `SELECT s.session, s.kind, s.cwd, s.title, m.seq, m.role, m.ts, snippet(fts, 0, '[', ']', '…', 32)
		FROM fts JOIN messages m ON m.id = fts.rowid JOIN sessions s ON s.id = m.session_id
		WHERE fts MATCH ?`
	args := []any{ftsQuery(q)}
	if f.Project != "" {
		query += ` AND (s.project LIKE ? OR s.cwd LIKE ?)`
		args = append(args, "%"+f.Project+"%", "%"+f.Project+"%")
	}
	if f.Source != "" {
		query += ` AND s.kind = ?`
		args = append(args, f.Source)
	}
	if f.Since != "" {
		query += ` AND s.ended >= ?`
		args = append(args, f.Since)
	}
	rows, err := ix.db.QueryContext(ctx, query+` ORDER BY rank LIMIT ?`, append(args, f.Limit)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var hits []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.Session, &h.Kind, &h.CWD, &h.Title, &h.Seq, &h.Role, &h.TS, &h.Snippet); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

func (ix *Index) messages(ctx context.Context, id string, from, to int) ([]Message, error) {
	rows, err := ix.db.QueryContext(ctx, `SELECT m.seq, m.role, m.ts, m.text FROM messages m JOIN sessions s ON s.id = m.session_id
		WHERE s.session = ? AND m.seq BETWEEN ? AND ? ORDER BY s.entry, m.seq`, id, from, to)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Message
	for rows.Next() {
		var msg Message
		if err := rows.Scan(&msg.Seq, &msg.Role, &msg.TS, &msg.Text); err != nil {
			return nil, err
		}
		out = append(out, msg)
	}
	return out, rows.Err()
}

// IndexCounts is the index at a glance.
type IndexCounts struct {
	Sessions int `json:"sessions"`
	Messages int `json:"messages"`
	Skipped  int `json:"skipped_records"`
	Damaged  int `json:"damaged_records"`
}

func (ix *Index) counts(ctx context.Context) (IndexCounts, error) {
	var c IndexCounts
	err := ix.db.QueryRowContext(ctx, `SELECT count(*), COALESCE(sum(skipped), 0), COALESCE(sum(damaged), 0) FROM sessions`).Scan(&c.Sessions, &c.Skipped, &c.Damaged)
	if err != nil {
		return c, err
	}
	return c, ix.db.QueryRowContext(ctx, `SELECT count(*) FROM messages`).Scan(&c.Messages)
}
