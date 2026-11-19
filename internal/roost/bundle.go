package roost

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	bundleExt   = ".tar.zst"
	manifestExt = ".manifest.json"
)

// Entry is one file in a bundle.
type Entry struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	SHA256   string    `json:"sha256"`
}

// Manifest describes a published bundle. It sits beside the bundle, holds no
// transcript text and is written last, so its presence means the bundle is
// complete.
type Manifest struct {
	Version int       `json:"version"`
	Bundle  string    `json:"bundle"` // YYYY/MM/YYYY-MM-DD-NN under the archive folder
	Created time.Time `json:"created"`
	SHA256  string    `json:"sha256"`
	Bytes   int64     `json:"bytes"`
	Entries []Entry   `json:"entries"`
}

// writeBundle packs files into path and returns their entries as read, so a
// file still changing is recorded as it was and caught by the next scan. A
// file gone since the scan is left out.
func writeBundle(ctx context.Context, path string, files []File) (Manifest, error) {
	m := Manifest{Version: 1}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return m, err
	}
	defer func() { _ = out.Close() }()
	zw, err := zstd.NewWriter(out, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(2))
	if err != nil {
		return m, err
	}
	tw := tar.NewWriter(zw)
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return m, err
		}
		e, err := addFile(tw, f)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return m, err
		}
		m.Entries = append(m.Entries, e)
	}
	if err := tw.Close(); err != nil {
		return m, err
	}
	if err := zw.Close(); err != nil {
		return m, err
	}
	if err := out.Sync(); err != nil {
		return m, err
	}
	info, err := out.Stat()
	if err != nil {
		return m, err
	}
	m.Bytes = info.Size()
	return m, out.Close()
}

func addFile(tw *tar.Writer, f File) (Entry, error) {
	in, err := os.Open(f.Path)
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return Entry{}, err
	}
	e := Entry{Name: f.Entry, Size: info.Size(), Modified: info.ModTime()}
	if err := tw.WriteHeader(&tar.Header{Name: e.Name, Mode: 0o600, Size: e.Size, ModTime: e.Modified, Format: tar.FormatPAX}); err != nil {
		return e, err
	}
	h := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(tw, h), in, e.Size); err != nil {
		return e, fmt.Errorf("%s changed while it was read: %w", e.Name, err)
	}
	e.SHA256 = hex.EncodeToString(h.Sum(nil))
	return e, nil
}

// eachEntry reads a bundle in order. fn may stop early with errStop.
func eachEntry(ctx context.Context, path string, fn func(*tar.Header, io.Reader) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	zr, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(h, tr); err != nil {
			if errors.Is(err, errStop) {
				return nil
			}
			return err
		}
	}
}

var errStop = errors.New("stop")

// verifyBundle reads a bundle back and checks every entry against m.
func verifyBundle(ctx context.Context, path string, m Manifest) error {
	want := make(map[string]string, len(m.Entries))
	for _, e := range m.Entries {
		want[e.Name] = e.SHA256
	}
	seen := 0
	err := eachEntry(ctx, path, func(hdr *tar.Header, r io.Reader) error {
		h := sha256.New()
		if _, err := io.Copy(h, r); err != nil {
			return err
		}
		if sum := hex.EncodeToString(h.Sum(nil)); want[hdr.Name] != sum {
			return fmt.Errorf("bundle check failed: %s", hdr.Name)
		}
		seen++
		return nil
	})
	if err != nil {
		return err
	}
	if seen != len(m.Entries) {
		return fmt.Errorf("bundle check failed: %d of %d entries", seen, len(m.Entries))
	}
	return nil
}

// publish copies a checked bundle into the archive folder, verifies the copy
// and only then gives it its name, then writes the manifest. It adds files and
// never replaces one. The folder must already exist: a Drive that is not
// mounted fails here rather than filling a local look-alike.
func publish(ctx context.Context, work, root string, now time.Time, m *Manifest) (string, error) {
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return "", fmt.Errorf("archive folder is not available: %w", errors.Join(err, fs.ErrNotExist))
	}
	dir := filepath.Join(root, now.Format("2006"), now.Format("01"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name, err := nextName(dir, now.Format("2006-01-02"))
	if err != nil {
		return "", err
	}
	dest := filepath.Join(dir, name+bundleExt)
	if _, err := os.Lstat(dest); err == nil {
		return "", fmt.Errorf("refusing to replace %s", name)
	}
	partial := dest + ".partial"
	defer func() { _ = os.Remove(partial) }()
	sum, err := copyFile(work, partial)
	if err != nil {
		return "", err
	}
	if got, err := hashFile(partial); err != nil || got != sum {
		return "", errors.Join(errors.New("published copy does not match its bundle"), err)
	}
	if err := os.Rename(partial, dest); err != nil {
		return "", err
	}
	m.Bundle = now.Format("2006") + "/" + now.Format("01") + "/" + name
	m.Created = now
	m.SHA256 = sum
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	if err := writeAtomic(filepath.Join(dir, name+manifestExt), data); err != nil {
		return "", err
	}
	return dest, nil
}

func copyFile(from, to string) (string, error) {
	in, err := os.Open(from)
	if err != nil {
		return "", err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer func() { _ = out.Close() }()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), in); err != nil {
		return "", err
	}
	if err := out.Sync(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), out.Close()
}

// nextName is the day's next free bundle number.
func nextName(dir, day string) (string, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	next := 1
	for _, f := range files {
		var n int
		if _, err := fmt.Sscanf(strings.TrimPrefix(f.Name(), day+"-"), "%d", &n); err == nil && strings.HasPrefix(f.Name(), day+"-") && strings.HasSuffix(f.Name(), bundleExt) {
			next = max(next, n+1)
		}
	}
	return fmt.Sprintf("%s-%02d", day, next), nil
}

// manifests reads every manifest in the archive folder, oldest first. skip
// names bundles to leave unread, so a pass stays cheap on a streamed Drive.
func manifests(root string, skip map[string]bool) ([]Manifest, error) {
	var out []Manifest
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), manifestExt) {
			return nil
		}
		rel, err := filepath.Rel(root, strings.TrimSuffix(path, manifestExt))
		if err != nil {
			return err
		}
		if skip[filepath.ToSlash(rel)] {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var m Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			return fmt.Errorf("%s: %w", d.Name(), err)
		}
		out = append(out, m)
		return nil
	})
	slices.SortFunc(out, func(a, b Manifest) int { return strings.Compare(a.Bundle, b.Bundle) })
	return out, err
}

// reconcile records bundles published by a run that stopped before recording
// them.
func (m *App) reconcile(ctx context.Context) (int, error) {
	known, err := m.rec.Known(ctx)
	if err != nil {
		return 0, err
	}
	found, err := manifests(m.Config.ArchiveDir, known.Bundles)
	if err != nil {
		return 0, err
	}
	for _, man := range found {
		if err := m.rec.Add(ctx, man, man.Created); err != nil {
			return 0, err
		}
	}
	return len(found), nil
}

// writeAtomic writes data beside path and renames it into place, so a
// manifest is either whole or absent.
func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".roost-"+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// checkPublished reads a published bundle once, checking the whole file
// against its manifest's hash and size and every entry against its own hash.
func checkPublished(ctx context.Context, path string, m Manifest) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	whole := sha256.New()
	counted := &countWriter{}
	in := io.TeeReader(f, io.MultiWriter(whole, counted))
	zr, err := zstd.NewReader(in, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return err
	}
	defer zr.Close()
	want := make(map[string]string, len(m.Entries))
	for _, e := range m.Entries {
		want[e.Name] = e.SHA256
	}
	seen := 0
	tr := tar.NewReader(zr)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		sum := sha256.New()
		if _, err := io.Copy(sum, tr); err != nil {
			return err
		}
		if hex.EncodeToString(sum.Sum(nil)) != want[h.Name] {
			return fmt.Errorf("entry %s does not match", h.Name)
		}
		seen++
	}
	if _, err := io.Copy(io.Discard, in); err != nil {
		return err
	}
	switch {
	case seen != len(m.Entries):
		return fmt.Errorf("%d of %d entries", seen, len(m.Entries))
	case counted.n != m.Bytes:
		return fmt.Errorf("%d bytes, manifest says %d", counted.n, m.Bytes)
	case hex.EncodeToString(whole.Sum(nil)) != m.SHA256:
		return errors.New("bundle hash does not match its manifest")
	}
	return nil
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }
