// Package watcher provides best-effort near-real-time index updates via
// fsnotify. It is an acceleration layer only: the periodic reconciliation
// scan remains authoritative, so any missed event heals on the next scan.
package watcher

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"super-speedy-search/internal/config"
	"super-speedy-search/internal/db"
	"super-speedy-search/internal/scanner"
)

type Watcher struct {
	DB  *db.DB
	Cfg *config.Config
	Log *slog.Logger

	fsw      *fsnotify.Watcher
	mu       sync.Mutex
	pending  map[string]time.Time // abs path -> last event time
	watched  int
	disabled map[string]bool // root ID -> watching disabled (budget/error)
}

func New(d *db.DB, cfg *config.Config, log *slog.Logger) *Watcher {
	return &Watcher{DB: d, Cfg: cfg, Log: log,
		pending: map[string]time.Time{}, disabled: map[string]bool{}}
}

// Start begins watching all enabled roots. Returns an error only when the
// watcher cannot be created at all; per-root failures degrade to scans.
func (w *Watcher) Start(ctx context.Context) error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	w.fsw = fsw

	for _, root := range w.Cfg.EnabledRoots() {
		// fsnotify watches directories, not whole recursive trees. We add every
		// existing directory up front and add newly-created directories later in
		// handle().
		if err := w.addRecursive(root, root.Path); err != nil {
			w.disableRoot(root, err)
		}
	}
	w.Log.Info("filesystem watcher started", "watched_dirs", w.watched)

	go w.loop(ctx)
	return nil
}

func (w *Watcher) disableRoot(root config.Root, err error) {
	w.mu.Lock()
	w.disabled[root.ID] = true
	w.mu.Unlock()
	w.Log.Warn("watching disabled for root; relying on periodic scans",
		"root", root.ID, "reason", err)
	// Best effort: drop this root's watches to free descriptors.
	for _, path := range w.fsw.WatchList() {
		if path == root.Path || strings.HasPrefix(path, root.Path+string(filepath.Separator)) {
			_ = w.fsw.Remove(path)
		}
	}
}

var errBudget = budgetErr{}

type budgetErr struct{}

func (budgetErr) Error() string { return "watched directory budget exceeded" }

func (w *Watcher) addRecursive(root config.Root, dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // unreadable subtree: reconciliation scan covers it
		}
		if !d.IsDir() {
			return nil
		}
		if path != root.Path {
			rel, err := filepath.Rel(root.Path, path)
			if err != nil {
				return nil
			}
			if scanner.Excluded(filepath.ToSlash(rel), d.Name(), root.Excludes.Paths) {
				return fs.SkipDir
			}
		}
		w.mu.Lock()
		if w.watched >= w.Cfg.Scan.Watch.MaxWatchedDirs {
			w.mu.Unlock()
			return errBudget
		}
		w.mu.Unlock()
		if err := w.fsw.Add(path); err != nil {
			return err
		}
		w.mu.Lock()
		w.watched++
		w.mu.Unlock()
		return nil
	})
}

func (w *Watcher) loop(ctx context.Context) {
	debounce := time.Duration(w.Cfg.Scan.Watch.DebounceMs) * time.Millisecond
	ticker := time.NewTicker(debounce / 2)
	defer ticker.Stop()
	defer w.fsw.Close()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			// Editors often save by writing temp files, renaming, and touching
			// directories in quick bursts. Store the latest event time and let
			// flush process the stable path after the debounce window.
			w.mu.Lock()
			w.pending[ev.Name] = time.Now()
			w.mu.Unlock()
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			w.Log.Warn("watcher error", "err", err)
		case <-ticker.C:
			w.flush(debounce)
		}
	}
}

// flush processes pending paths whose last event is older than the debounce
// window, batching sibling events from bulk operations.
func (w *Watcher) flush(debounce time.Duration) {
	now := time.Now()
	var ready []string
	w.mu.Lock()
	for p, t := range w.pending {
		if now.Sub(t) >= debounce {
			ready = append(ready, p)
			delete(w.pending, p)
		}
	}
	w.mu.Unlock()
	sort.Strings(ready)
	for _, p := range ready {
		w.handle(p)
	}
}

func (w *Watcher) handle(absPath string) {
	root, rel, ok := w.rootFor(absPath)
	if !ok {
		return
	}
	w.mu.Lock()
	disabled := w.disabled[root.ID]
	w.mu.Unlock()
	if disabled {
		return
	}
	if scanner.Excluded(rel, filepath.Base(absPath), root.Excludes.Paths) {
		return
	}

	info, err := os.Lstat(absPath)
	if err != nil {
		// Gone: tombstone the path and any children. The periodic scanner will
		// later confirm the state, but this makes deletes disappear quickly.
		if n, err := w.DB.MarkDeletedByPath(root.ID, rel, time.Now().Unix()); err != nil {
			w.Log.Warn("watcher delete failed", "path", rel, "err", err)
		} else if n > 0 {
			w.Log.Debug("watcher tombstoned", "path", rel, "rows", n)
		}
		return
	}

	if info.IsDir() {
		// New or moved-in directory: watch it and index its subtree.
		if err := w.addRecursive(root, absPath); err != nil {
			w.disableRoot(root, err)
			return
		}
		w.indexSubtree(root, absPath)
		return
	}
	if info.Mode()&fs.ModeSymlink != 0 && !w.Cfg.Scan.FollowSymlinks {
		return
	}
	if !scanner.ShouldIndexFile(rel, filepath.Base(absPath), root) {
		return
	}
	meta := scanner.MetaFromInfo(root.ID, rel, info)
	if _, err := w.DB.UpsertFiles([]db.FileMeta{meta}, "", time.Now().Unix()); err != nil {
		w.Log.Warn("watcher upsert failed", "path", rel, "err", err)
	}
}

func (w *Watcher) indexSubtree(root config.Root, dir string) {
	var batch []db.FileMeta
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		rel, err := filepath.Rel(root.Path, path)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if d.IsDir() {
			if path != dir && scanner.Excluded(relSlash, d.Name(), root.Excludes.Paths) {
				return fs.SkipDir
			}
		} else {
			if !scanner.ShouldIndexFile(relSlash, d.Name(), root) {
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 && !w.Cfg.Scan.FollowSymlinks {
				return nil
			}
		}
		meta, err := scanner.MetaFor(root.ID, relSlash, d)
		if err != nil {
			return nil
		}
		// A moved-in directory can contain many files. Batch the subtree into a
		// single write so the watcher does not do one SQLite transaction per file.
		batch = append(batch, meta)
		return nil
	})
	if _, err := w.DB.UpsertFiles(batch, "", time.Now().Unix()); err != nil {
		w.Log.Warn("watcher subtree index failed", "dir", dir, "err", err)
	}
}

// rootFor maps an absolute event path to its configured root (longest prefix
// wins) and the slash-separated relative path.
func (w *Watcher) rootFor(absPath string) (config.Root, string, bool) {
	var best config.Root
	bestLen := -1
	for _, r := range w.Cfg.EnabledRoots() {
		if absPath == r.Path || strings.HasPrefix(absPath, r.Path+string(filepath.Separator)) {
			if len(r.Path) > bestLen {
				best = r
				bestLen = len(r.Path)
			}
		}
	}
	if bestLen < 0 {
		return config.Root{}, "", false
	}
	rel, err := filepath.Rel(best.Path, absPath)
	if err != nil || rel == "." {
		return config.Root{}, "", false
	}
	return best, filepath.ToSlash(rel), true
}
