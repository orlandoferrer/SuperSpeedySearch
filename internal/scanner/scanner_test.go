package scanner

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

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func setup(t *testing.T) (*db.DB, *config.Config, string) {
	t.Helper()
	// Intentionally include a space in the path: the project targets macOS
	// and Synology where spaces in share names are common.
	dir := filepath.Join(t.TempDir(), "root dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(`
node: {id: "test", auth_required: false}
roots:
  - id: "r1"
    path: "` + dir + `"
    excludes:
      paths: [".git", "skipme"]
      extensions: [".tmp"]
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database.Path = filepath.Join(t.TempDir(), "spaced dir", "index.db")
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

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fileState(t *testing.T, d *db.DB, rel string) (found bool, deleted bool, size int64) {
	t.Helper()
	var isDeleted int
	err := d.SQL.QueryRow(`SELECT is_deleted, size_bytes FROM files WHERE root_id = 'r1' AND relative_path = ?`, rel).
		Scan(&isDeleted, &size)
	if err != nil {
		return false, false, 0
	}
	return true, isDeleted != 0, size
}

func TestScanLifecycle(t *testing.T) {
	d, cfg, dir := setup(t)
	s := New(d, cfg, testLogger())
	ctx := context.Background()

	write(t, filepath.Join(dir, "a.txt"), "hello")
	write(t, filepath.Join(dir, "sub", "b.md"), "world")
	write(t, filepath.Join(dir, ".git", "config"), "excluded")
	write(t, filepath.Join(dir, "skipme", "c.txt"), "excluded")
	write(t, filepath.Join(dir, "junk.tmp"), "excluded ext")

	if err := s.Run(ctx, ""); err != nil {
		t.Fatal(err)
	}

	if found, deleted, size := fileState(t, d, "a.txt"); !found || deleted || size != 5 {
		t.Errorf("a.txt: found=%v deleted=%v size=%d", found, deleted, size)
	}
	if found, _, _ := fileState(t, d, "sub/b.md"); !found {
		t.Error("sub/b.md not indexed")
	}
	if found, _, _ := fileState(t, d, "sub"); !found {
		t.Error("directory sub not indexed")
	}
	for _, rel := range []string{".git/config", "skipme/c.txt", "junk.tmp"} {
		if found, _, _ := fileState(t, d, rel); found {
			t.Errorf("%s should have been excluded", rel)
		}
	}

	// Update: content change must be visible after rescan.
	// Backdate mtime tracking by rewriting with different size.
	write(t, filepath.Join(dir, "a.txt"), "hello longer")
	if err := s.Run(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, size := fileState(t, d, "a.txt"); size != 12 {
		t.Errorf("a.txt size after update = %d, want 12", size)
	}

	// Delete: file must be tombstoned after reconciliation.
	if err := os.Remove(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if found, deleted, _ := fileState(t, d, "a.txt"); !found || !deleted {
		t.Errorf("a.txt after delete: found=%v deleted=%v, want tombstoned", found, deleted)
	}
	if _, deleted, _ := fileState(t, d, "sub/b.md"); deleted {
		t.Error("sub/b.md wrongly tombstoned")
	}

	// Re-create: tombstone must be resurrected.
	write(t, filepath.Join(dir, "a.txt"), "back")
	if err := s.Run(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if found, deleted, size := fileState(t, d, "a.txt"); !found || deleted || size != 4 {
		t.Errorf("a.txt resurrect: found=%v deleted=%v size=%d", found, deleted, size)
	}

	runs, err := d.ScanHistory(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 4 {
		t.Errorf("scan history len = %d, want 4", len(runs))
	}
	for _, r := range runs {
		if r.Status != "completed" {
			t.Errorf("run %s status = %q", r.ID, r.Status)
		}
	}
}

// A root that disappears (unmounted disk/volume) must not tombstone its
// previously indexed files.
func TestUnavailableRootPreservesIndex(t *testing.T) {
	d, cfg, dir := setup(t)
	s := New(d, cfg, testLogger())
	ctx := context.Background()

	write(t, filepath.Join(dir, "a.txt"), "hello")
	write(t, filepath.Join(dir, "b.txt"), "world")
	if err := s.Run(ctx, ""); err != nil {
		t.Fatal(err)
	}

	// Root path gone entirely: stat preflight must keep the index.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, deleted, _ := fileState(t, d, "a.txt"); deleted {
		t.Error("missing root tombstoned a.txt")
	}
	if s.Current().Status != "completed_with_errors" {
		t.Errorf("scan status = %q, want completed_with_errors", s.Current().Status)
	}

	// Root present but empty (Docker bind mount vanished): the mass-delete
	// guard must keep the index too.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, deleted, _ := fileState(t, d, "a.txt"); deleted {
		t.Error("empty root tombstoned a.txt")
	}

	// Once the root yields any entry again, normal reconciliation resumes:
	// b.txt is genuinely gone now and must be tombstoned.
	write(t, filepath.Join(dir, "a.txt"), "hello")
	if err := s.Run(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if found, deleted, _ := fileState(t, d, "a.txt"); !found || deleted {
		t.Error("a.txt should be live again")
	}
	if found, deleted, _ := fileState(t, d, "b.txt"); !found || !deleted {
		t.Error("b.txt should be tombstoned once reconciliation resumes")
	}
}

func TestExcluded(t *testing.T) {
	excludes := []string{".git", "node_modules", "sub/private"}
	cases := []struct {
		rel, name string
		want      bool
	}{
		{".git", ".git", true},
		{"a/.git/config", "config", true},
		{"node_modules", "node_modules", true},
		{"deep/node_modules/x.js", "x.js", true},
		{"sub/private", "private", true},
		{"sub/private/f.txt", "f.txt", true},
		{"sub/privateer", "privateer", false},
		{"a.txt", "a.txt", false},
		{"gitignore", "gitignore", false},
	}
	for _, c := range cases {
		if got := Excluded(c.rel, c.name, excludes); got != c.want {
			t.Errorf("Excluded(%q) = %v, want %v", c.rel, got, c.want)
		}
	}
}
