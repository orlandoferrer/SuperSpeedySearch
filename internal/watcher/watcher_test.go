package watcher

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"super-speedy-search/internal/config"
	"super-speedy-search/internal/db"
)

func setup(t *testing.T) (*db.DB, *config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Parse([]byte(`
node: {id: "test", auth_required: false}
scan:
  watch:
    debounce_ms: 50
roots:
  - id: "r1"
    path: "` + dir + `"
    excludes:
      paths: [".git"]
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database.Path = filepath.Join(t.TempDir(), "index.db")
	d, err := db.Open(cfg.Database.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.SyncRoots([]db.RootRow{{ID: "r1", Path: dir, DisplayPrefix: "T:r1", Enabled: true}}, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	return d, cfg, dir
}

func waitFor(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

func rowState(d *db.DB, rel string) (found, deleted bool) {
	var isDeleted int
	err := d.SQL.QueryRow(`SELECT is_deleted FROM files WHERE root_id = 'r1' AND relative_path = ?`, rel).Scan(&isDeleted)
	if err != nil {
		return false, false
	}
	return true, isDeleted != 0
}

func TestWatcherLifecycle(t *testing.T) {
	d, cfg, dir := setup(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := New(d, cfg, log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// Create: file appears in the index without any scan.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a.txt indexed", func() bool {
		found, deleted := rowState(d, "a.txt")
		return found && !deleted
	})

	// New directory with a file: subtree gets indexed and watched.
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "sub indexed", func() bool {
		found, _ := rowState(d, "sub")
		return found
	})
	if err := os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("nested"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "sub/b.txt indexed", func() bool {
		found, deleted := rowState(d, "sub/b.txt")
		return found && !deleted
	})

	// Delete: tombstoned.
	if err := os.Remove(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a.txt tombstoned", func() bool {
		found, deleted := rowState(d, "a.txt")
		return found && deleted
	})

	// Excluded path: never indexed.
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // give it a chance to (wrongly) index
	if found, _ := rowState(d, ".git/config"); found {
		t.Error(".git/config should have been excluded")
	}
}

func TestRootFor(t *testing.T) {
	d, cfg, dir := setup(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := New(d, cfg, log)
	root, rel, ok := w.rootFor(filepath.Join(dir, "sub", "x.txt"))
	if !ok || root.ID != "r1" || rel != "sub/x.txt" {
		t.Errorf("rootFor = %v %q %v", root.ID, rel, ok)
	}
	if _, _, ok := w.rootFor("/somewhere/else"); ok {
		t.Error("path outside roots should not match")
	}
}
