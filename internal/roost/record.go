package roost

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"time"

	_ "modernc.org/sqlite" // the database/sql driver
)

// Record notes what the archive holds. It keeps paths and hashes, never
// transcript text, and can always be rebuilt from the manifests in the
// archive. Times are stored as nanoseconds: a coarser clock than the file
// system's makes every unchanged file look changed.
type Record struct{ db *sql.DB }

const recordSchema = `
CREATE TABLE IF NOT EXISTS files (
 entry TEXT NOT NULL,
 sha256 TEXT NOT NULL,
 size INTEGER NOT NULL,
 modified INTEGER NOT NULL,
 bundle TEXT NOT NULL,
 archived INTEGER NOT NULL,
 PRIMARY KEY (entry, sha256)
);
CREATE INDEX IF NOT EXISTS files_bundle ON files (bundle);
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`

func openSQLite(ctx context.Context, path string) (*sql.DB, error) {
	dsn := url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)"}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func openRecord(ctx context.Context, path string) (*Record, error) {
	db, err := openSQLite(ctx, path)
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, recordSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Record{db: db}, nil
}

func (r *Record) close() error { return r.db.Close() }

// Copy is the newest archived copy of one entry.
type Copy struct {
	Size     int64
	Modified time.Time
	SHA256   string
	Bundle   string
	Archived time.Time
}

// Known is what the archive already holds.
type Known struct {
	// Entries is each entry's newest copy.
	Entries map[string]Copy
	// Hashes maps a kind and content hash to a copy, so a file Codex moves to
	// archived_sessions is not stored twice.
	Hashes map[string]Copy
	// Bundles is every recorded bundle.
	Bundles map[string]bool
}

func hashKey(kind, sha string) string { return kind + ":" + sha }

func emptyKnown() Known {
	return Known{Entries: map[string]Copy{}, Hashes: map[string]Copy{}, Bundles: map[string]bool{}}
}

// Known reads the whole table, a few thousand rows.
func (r *Record) Known(ctx context.Context) (Known, error) {
	k := emptyKnown()
	rows, err := r.db.QueryContext(ctx, `SELECT entry, sha256, size, modified, bundle, archived FROM files ORDER BY archived, entry`)
	if err != nil {
		return k, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var entry string
		var c Copy
		var modified, archived int64
		if err := rows.Scan(&entry, &c.SHA256, &c.Size, &modified, &c.Bundle, &archived); err != nil {
			return k, err
		}
		c.Modified, c.Archived = time.Unix(0, modified), time.Unix(0, archived)
		k.Entries[entry] = c
		k.Hashes[hashKey(kindOf(entry), c.SHA256)] = c
		k.Bundles[c.Bundle] = true
	}
	return k, rows.Err()
}

func (r *Record) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Add stores a published bundle's entries. Rows already there stay as they
// are, so reconciling a manifest twice changes nothing.
func (r *Record) Add(ctx context.Context, m Manifest, at time.Time) error {
	return r.inTx(ctx, func(tx *sql.Tx) error {
		for _, e := range m.Entries {
			if _, err := tx.ExecContext(ctx, `INSERT INTO files (entry, sha256, size, modified, bundle, archived)
				VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, e.Name, e.SHA256, e.Size, e.Modified.UnixNano(), m.Bundle, at.UnixNano()); err != nil {
				return err
			}
		}
		return nil
	})
}

// Refresh notes files whose bytes the archive already holds: a touched file,
// or one moved between sources. Its row points at the bundle that has them.
func (r *Record) Refresh(ctx context.Context, same []Same) error {
	if len(same) == 0 {
		return nil
	}
	return r.inTx(ctx, func(tx *sql.Tx) error {
		for _, f := range same {
			if _, err := tx.ExecContext(ctx, `INSERT INTO files (entry, sha256, size, modified, bundle, archived)
				VALUES (?, ?, ?, ?, ?, ?)
				ON CONFLICT (entry, sha256) DO UPDATE SET modified = excluded.modified`, f.Entry, f.SHA256, f.Size, f.Modified.UnixNano(), f.Bundle, f.Archived.UnixNano()); err != nil {
				return err
			}
		}
		return nil
	})
}

// Summary is the archive at a glance.
type Summary struct {
	Files   int       `json:"files"`
	Bundles int       `json:"bundles"`
	Bytes   int64     `json:"source_bytes"`
	Last    time.Time `json:"last_bundle"`
}

// Summary counts the table.
func (r *Record) Summary(ctx context.Context) (Summary, error) {
	var out Summary
	var last sql.NullInt64
	err := r.db.QueryRowContext(ctx, `SELECT count(DISTINCT entry), count(DISTINCT bundle), COALESCE(sum(size), 0), max(archived) FROM files`).
		Scan(&out.Files, &out.Bundles, &out.Bytes, &last)
	if last.Valid {
		out.Last = time.Unix(0, last.Int64)
	}
	return out, err
}

// errNotArchived is a session id the record has no copy of.
var errNotArchived = errors.New("no archived copy")

// Locate finds the bundle holding a session's newest copy by its id, the
// file name without extensions.
func (r *Record) Locate(ctx context.Context, id string) (entry, sha, bundle string, err error) {
	err = r.db.QueryRowContext(ctx, `SELECT entry, sha256, bundle FROM files
		WHERE entry LIKE '%/' || ? || '.jsonl' OR entry LIKE '%/' || ? || '.jsonl.zst'
		ORDER BY archived DESC, bundle DESC LIMIT 1`, id, id).Scan(&entry, &sha, &bundle)
	if errors.Is(err, sql.ErrNoRows) {
		err = errNotArchived
	}
	return entry, sha, bundle, err
}

func (r *Record) meta(ctx context.Context, key string) (string, error) {
	var v string
	err := r.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (r *Record) setMeta(ctx context.Context, key, value string) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (r *Record) metaTime(ctx context.Context, key string) (time.Time, error) {
	v, err := r.meta(ctx, key)
	if err != nil || v == "" {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, v)
}
