// Package roost keeps finished Claude Code and Codex transcripts. It
// hard-links them so a delete in the app cannot outrun the archive, copies
// finished ones one way into an archive folder as compressed bundles, and
// keeps a local search index with recognisable secrets removed.
//
// It only ever adds to the archive folder and only ever reads the transcript
// folders. Its one deletion is a kept link whose bytes the archive holds.
package roost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const (
	// bundleEvery spaces bundles once a full one's worth is not waiting.
	bundleEvery = 20 * time.Hour
	// staleAfter is how long without a complete pass before status fails.
	staleAfter = 48 * time.Hour
	// lagLimit is how long finished files may wait with no new bundle.
	lagLimit = 72 * time.Hour
	// indexShare is how long one pass may spend indexing; the rest of a
	// backfill is picked up by the next pass.
	indexShare = 4 * time.Minute
	// notifyEvery spaces repeated notifications about the same trouble.
	notifyEvery = 6 * time.Hour
	// flushIdle is how idle a file must be for --flush.
	flushIdle = time.Hour
)

// App is roost opened on one configuration.
type App struct {
	Config Config
	Log    *slog.Logger
	Now    func() time.Time
	rec    *Record
}

// Open creates the data folder and opens the record in it.
func Open(ctx context.Context, cfg Config, log *slog.Logger) (*App, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	rec, err := openRecord(ctx, filepath.Join(cfg.DataDir, "roost.db"))
	if err != nil {
		return nil, err
	}
	return &App{Config: cfg, Log: log, Now: time.Now, rec: rec}, nil
}

// Close closes the record.
func (m *App) Close() error { return m.rec.close() }

// lock takes the data folder's lock, so only one pass or reindex writes at a
// time. The lock goes with the process.
func (m *App) lock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(m.Config.DataDir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, errors.New("another roost pass is running")
	}
	return func() { _ = f.Close() }, nil
}

// RunOptions shapes one pass.
type RunOptions struct {
	// Dry builds and checks one bundle locally and changes nothing outside
	// the data folder: no links made or released, nothing published.
	Dry bool
	// Flush bundles everything idle for an hour now, however small.
	Flush bool
	// List calls back with each kept link the pass releases, or would.
	List func(path string)
}

// Stats counts one pass.
type Stats map[string]int

// Run does one pass: keep links, reconcile, scan, release, bundle when due,
// index. A missing source makes the pass partial: what is there still moves,
// and the pass is not recorded as complete.
func (m *App) Run(ctx context.Context, opt RunOptions) (stats Stats, err error) {
	stats = Stats{}
	unlock, err := m.lock()
	if err != nil {
		return stats, err
	}
	defer unlock()
	defer func() {
		if err != nil && !opt.Dry && ctx.Err() == nil {
			_ = m.rec.setMeta(context.WithoutCancel(ctx), "last_error", m.Now().UTC().Format(time.RFC3339)+" "+err.Error())
			m.notify(ctx, "roost pass failed: "+err.Error())
		}
	}()
	kept, err := m.keep(ctx, opt.Dry)
	stats["linked"], stats["moved"] = kept.Linked, kept.Moved
	if err != nil {
		return stats, err
	}
	if info, err := os.Stat(m.Config.ArchiveDir); err != nil || !info.IsDir() {
		return stats, fmt.Errorf("archive folder is not available: %s", m.Config.ArchiveDir)
	}
	n, err := m.reconcile(ctx)
	if err != nil {
		return stats, err
	}
	stats["reconciled"] = n
	known, err := m.rec.Known(ctx)
	if err != nil {
		return stats, err
	}
	idle := time.Duration(m.Config.IdleHours) * time.Hour
	if opt.Flush {
		idle = flushIdle
	}
	found, err := m.scanIdle(ctx, known, true, idle)
	if err != nil {
		return stats, err
	}
	stats["files"], stats["waiting"], stats["missing_sources"], stats["orphans"] = found.Files, len(found.Pending), found.Missing, found.Orphans
	if err := m.rec.Refresh(ctx, found.Same); err != nil {
		return stats, err
	}
	// Released only after Refresh, so a link whose bytes matched by hash waits
	// a pass for its entry to be recorded.
	for _, path := range found.Release {
		if opt.List != nil {
			opt.List(path)
		}
		if opt.Dry {
			continue
		}
		if err := removeKept(path); err != nil {
			return stats, err
		}
	}
	stats["released"] = len(found.Release)
	summary, err := m.rec.Summary(ctx)
	if err != nil {
		return stats, err
	}
	if opt.Flush || due(m.Now(), summary.Last, found.Pending, m.bundleBytes()) {
		if err := m.archive(ctx, stats, found.Pending, opt); err != nil {
			return stats, err
		}
	}
	if !opt.Dry {
		deadline, cancel := context.WithTimeout(ctx, indexShare)
		ix, err := m.index(deadline, found.Indexable)
		cancel()
		stats["indexed"], stats["unreadable"] = ix.Indexed, ix.Unreadable
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			stats["index_deferred"] = 1
		} else if err != nil {
			return stats, err
		}
	}
	if opt.Dry {
		return stats, nil
	}
	if found.Missing > 0 {
		m.Log.Warn("transcript folder missing", "count", found.Missing)
	} else if err := m.rec.setMeta(ctx, "last_complete", m.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return stats, err
	}
	if problems, err := m.check(ctx, found); err == nil && len(problems) > 0 {
		m.notify(ctx, "roost: "+strings.Join(problems, "; "))
	}
	return stats, nil
}

// due says whether to bundle now: a full bundle is waiting, or the last one
// has aged out.
func due(now, last time.Time, pending []File, full int64) bool {
	if len(pending) == 0 {
		return false
	}
	var total int64
	for _, f := range pending {
		total += f.Size
	}
	return total >= full || last.IsZero() || now.Sub(last) >= bundleEvery
}

func (m *App) bundleBytes() int64 { return int64(m.Config.BundleMaxMB) << 20 }

// archive publishes up to max_bundles_per_run bundles, or all with Flush. A
// dry run stops after building and checking one, then deletes it.
func (m *App) archive(ctx context.Context, stats Stats, pending []File, opt RunOptions) error {
	tmp := filepath.Join(m.Config.DataDir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return err
	}
	for i := 0; opt.Flush || i < m.Config.MaxBundlesPerRun; i++ {
		batch, rest := nextBatch(pending, m.bundleBytes())
		if len(batch) == 0 {
			return nil
		}
		pending = rest
		work := filepath.Join(tmp, "bundle.tar.zst")
		manifest, err := writeBundle(ctx, work, batch)
		if err == nil {
			err = verifyBundle(ctx, work, manifest)
		}
		if err != nil {
			_ = os.Remove(work)
			return err
		}
		stats["bundled_files"] += len(manifest.Entries)
		stats["bundled_mb"] += int(manifest.Bytes >> 20)
		if opt.Dry {
			m.Log.Info("dry bundle built and checked", "files", len(manifest.Entries), "bytes", manifest.Bytes)
			return os.Remove(work)
		}
		dest, err := publish(ctx, work, m.Config.ArchiveDir, m.Now(), &manifest)
		_ = os.Remove(work)
		if err != nil {
			return err
		}
		if err := m.rec.Add(ctx, manifest, m.Now()); err != nil {
			return err
		}
		stats["bundles"]++
		m.Log.Info("bundle published", "bundle", manifest.Bundle, "files", len(manifest.Entries), "bytes", manifest.Bytes, "path", dest)
	}
	return nil
}

// check lists what is wrong: no complete pass for staleAfter, or finished
// files waiting lagLimit after the last bundle.
func (m *App) check(ctx context.Context, found Found) ([]string, error) {
	var problems []string
	last, err := m.rec.metaTime(ctx, "last_complete")
	if err != nil {
		return nil, err
	}
	if last.IsZero() || m.Now().Sub(last) > staleAfter {
		problems = append(problems, "no complete pass in 48 hours")
	}
	summary, err := m.rec.Summary(ctx)
	if err != nil {
		return nil, err
	}
	if len(found.Pending) > 0 && !summary.Last.IsZero() && m.Now().Sub(summary.Last) > lagLimit {
		problems = append(problems, fmt.Sprintf("%d finished files waiting and no bundle since %s", len(found.Pending), summary.Last.Local().Format("2 Jan")))
	}
	return problems, nil
}

// notify posts a macOS notification, at most once per notifyEvery.
func (m *App) notify(ctx context.Context, msg string) {
	m.Log.Warn(msg)
	if !m.Config.Notify || runtime.GOOS != "darwin" {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if last, err := m.rec.metaTime(ctx, "last_notified"); err == nil && m.Now().Sub(last) < notifyEvery {
		return
	}
	script := fmt.Sprintf("display notification %q with title %q", msg, "roost")
	if err := exec.CommandContext(ctx, "osascript", "-e", script).Run(); err != nil {
		m.Log.Warn("notification failed", "err", err)
		return
	}
	_ = m.rec.setMeta(ctx, "last_notified", m.Now().UTC().Format(time.RFC3339Nano))
}
