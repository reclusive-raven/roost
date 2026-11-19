package roost

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// keptDir holds a hard link to every source file. Deleting a session in the
// Claude or Codex app then removes only one of the file's names, and the
// archive bundles the bytes from here. A link costs no space while its source
// exists, and appends land in both. It must share a volume with the sources.
const keptDir = "kept"

func (m *App) keptRoot() (string, error) {
	root := filepath.Join(m.Config.DataDir, keptDir)
	return root, os.MkdirAll(root, 0o700)
}

type inode struct{ dev, ino uint64 }

func inodeOf(info fs.FileInfo) (inode, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return inode{}, false
	}
	return inode{uint64(st.Dev), st.Ino}, true //nolint:gosec // dev is a device number, never negative
}

type keepStats struct{ Linked, Moved, Missing int }

func (m *App) keep(ctx context.Context, dry bool) (keepStats, error) {
	var stats keepStats
	root, err := m.keptRoot()
	if err != nil {
		return stats, err
	}
	live := map[inode]bool{}
	type pair struct{ src, mirror string }
	var present []pair
	for _, src := range m.Config.Sources {
		dir, err := filepath.EvalSymlinks(src.Dir)
		if err != nil {
			// Leave a missing source's links alone: they may be all that is left.
			stats.Missing++
			continue
		}
		mirror := filepath.Join(root, filepath.FromSlash(src.Name()))
		present = append(present, pair{dir, mirror})
		err = walkFiles(ctx, dir, func(path, rel string, info fs.FileInfo) error {
			if src.Kind == "claude" && m.excluded(projectOf(rel)) {
				return nil
			}
			id, ok := inodeOf(info)
			if !ok {
				return nil
			}
			live[id] = true
			dst := filepath.Join(mirror, rel)
			if have, err := os.Lstat(dst); err == nil && os.SameFile(have, info) {
				return nil
			}
			stats.Linked++
			if dry {
				return nil
			}
			return link(path, dst)
		})
		if err != nil {
			return stats, err
		}
	}
	for _, p := range present {
		err := walkFiles(ctx, p.mirror, func(path, rel string, info fs.FileInfo) error {
			if _, err := os.Lstat(filepath.Join(p.src, rel)); !errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if id, ok := inodeOf(info); !ok || !live[id] {
				return nil
			}
			stats.Moved++
			if dry {
				return nil
			}
			return removeKept(path)
		})
		if err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// link points dst at src's inode, replacing an older link in one rename. A
// source gone since the walk is skipped.
func link(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp := dst + ".roost-keep"
	_ = os.Remove(tmp)
	if err := os.Link(src, tmp); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// removeKept drops one link.
// shortcut: emptied folders stay behind; they cost a directory entry each,
// prune them if the kept tree ever gets slow to walk.
func removeKept(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// walkFiles visits the regular files under root the way the scan does,
// skipping dot names. A missing root is empty.
func walkFiles(ctx context.Context, root string, visit func(path, rel string, info fs.FileInfo) error) error {
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".") && path != root {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasSuffix(path, ".roost-keep") {
			return nil
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		return visit(path, rel, info)
	})
	return err
}
