package roost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

var punctuation = regexp.MustCompile(`[^\pL\pN]`)

// releaseAfter is how long a bundle must have been published before a kept
// link to its bytes may go.
const releaseAfter = 24 * time.Hour

// File is one transcript file on disk.
type File struct {
	// Entry is the file's name in the archive: its source's name, then its
	// path under the source.
	Entry    string
	Path     string
	Kind     string
	Size     int64
	Modified time.Time
	SHA256   string
}

// Same is a file whose bytes the archive already holds.
type Same struct {
	Entry, SHA256, Bundle string
	Size                  int64
	Modified, Archived    time.Time
}

// Found is one pass over the sources.
type Found struct {
	Files     int
	Missing   int
	Orphans   int    // kept links whose source is gone
	Pending   []File // to publish, oldest first
	Same      []Same
	Indexable []File   // finished transcripts, archived or not
	Release   []string // orphans whose bytes are archived and may go
}

func kindOf(entry string) string {
	kind, _, _ := strings.Cut(entry, "/")
	return kind
}

func isTranscript(name string) bool {
	return strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".jsonl.zst")
}

// idOf is a session's id: its file name without the extensions.
func idOf(entry string) string {
	base := filepath.Base(entry)
	return strings.TrimSuffix(strings.TrimSuffix(base, ".zst"), ".jsonl")
}

// projectOf is a Claude file's project folder: the first path element under
// its source. Codex keeps files by date, so its project comes from the file.
func projectOf(rel string) string {
	project, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
	return project
}

// excluded compares in the form Claude names its project folders, every
// punctuation mark a dash, so one exclude matches a folder and a working
// directory alike and a file can never be left out of the index but archived.
func (m *App) excluded(names ...string) bool {
	for _, want := range m.Config.ExcludeProjects {
		for _, name := range names {
			if want != "" && strings.Contains(folded(name), folded(want)) {
				return true
			}
		}
	}
	return false
}

func folded(s string) string {
	return strings.ToLower(punctuation.ReplaceAllString(s, "-"))
}

// scan walks every source, and the kept links of files since deleted there,
// and sorts the finished files. It only reads. With
// hash false a changed file is counted as pending without reading it, which
// is enough for the doctor.
func (m *App) scan(ctx context.Context, known Known, hash bool) (Found, error) {
	return m.scanIdle(ctx, known, hash, time.Duration(m.Config.IdleHours)*time.Hour)
}

// scanIdle is scan with files counted finished after idle.
func (m *App) scanIdle(ctx context.Context, known Known, hash bool, idle time.Duration) (Found, error) {
	var found Found
	cutoff := m.Now().Add(-idle)
	for _, src := range m.Config.Sources {
		root, err := filepath.EvalSymlinks(src.Dir)
		if err != nil {
			found.Missing++
			continue
		}
		if err := m.walk(ctx, src, root, "", cutoff, known, hash, &found); err != nil {
			return found, err
		}
		mirror := filepath.Join(m.Config.DataDir, keptDir, filepath.FromSlash(src.Name()))
		if err := m.walk(ctx, src, mirror, root, cutoff, known, hash, &found); err != nil {
			return found, err
		}
	}
	slices.SortFunc(found.Pending, func(a, b File) int { return a.Modified.Compare(b.Modified) })
	return found, nil
}

// walk sorts one tree's files into found. With live set, root is a kept
// mirror and only files gone from live are its own; the rest are scanned at
// their source. Such an orphan's link is released once the archive holds it.
func (m *App) walk(ctx context.Context, src Source, root, live string, cutoff time.Time, known Known, hash bool, found *Found) error {
	return walkFiles(ctx, root, func(path, rel string, info fs.FileInfo) error {
		orphan := live != ""
		if orphan {
			if _, err := os.Lstat(filepath.Join(live, rel)); !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		if orphan {
			found.Orphans++
		}
		// release lets go of a kept link whose source is gone once the archive
		// has held its bytes for releaseAfter, time for a synced folder to
		// upload them. An excluded file has no archived copy to wait for.
		release := func(archived time.Time) error {
			if orphan && m.Config.Release && m.Now().Sub(archived) >= releaseAfter {
				found.Release = append(found.Release, path)
			}
			return nil
		}
		found.Files++
		if info.ModTime().After(cutoff) {
			return nil
		}
		f := File{Entry: src.Name() + "/" + filepath.ToSlash(rel), Path: path, Kind: src.Kind, Size: info.Size(), Modified: info.ModTime()}
		if src.Kind == "claude" && m.excluded(projectOf(rel)) {
			return release(time.Time{})
		}
		if isTranscript(f.Entry) {
			found.Indexable = append(found.Indexable, f)
		}
		last, ok := known.Entries[f.Entry]
		if ok && last.Size == f.Size && last.Modified.Equal(f.Modified) {
			return release(last.Archived)
		}
		if src.Kind == "codex" && isTranscript(f.Entry) {
			if meta, err := codexMeta(path); err == nil && m.excluded(meta.CWD) {
				return release(time.Time{})
			}
		}
		var err error
		if hash {
			if f.SHA256, err = hashFile(path); errors.Is(err, fs.ErrNotExist) {
				return nil
			} else if err != nil {
				return err
			}
			if c, ok := known.Hashes[hashKey(f.Kind, f.SHA256)]; ok {
				found.Same = append(found.Same, Same{Entry: f.Entry, SHA256: f.SHA256, Bundle: c.Bundle, Size: f.Size, Modified: f.Modified, Archived: c.Archived})
				return nil
			}
		}
		found.Pending = append(found.Pending, f)
		return nil
	})
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// nextBatch takes files up to max source bytes, at least one, so a file
// larger than a bundle still goes out alone.
func nextBatch(pending []File, limit int64) (batch, rest []File) {
	var total int64
	for i, f := range pending {
		if i > 0 && total+f.Size > limit {
			return pending[:i], pending[i:]
		}
		total += f.Size
	}
	return pending, nil
}
