package content

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"super-speedy-search/internal/config"
	"super-speedy-search/internal/db"
	"super-speedy-search/internal/scanner"
)

func setup(t *testing.T) (*Searcher, string) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Parse([]byte(`
node: {id: "test", auth_required: false}
roots:
  - id: "r1"
    path: "` + dir + `"
    content_search:
      enabled: true
      max_file_size_mb: 1
      include_extensions: [".txt", ".md"]
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
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Searcher{DB: d, Cfg: cfg, NodeID: "test", Log: log}
	return s, dir
}

func write(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func scan(t *testing.T, s *Searcher) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := scanner.New(s.DB, s.Cfg, log).Run(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
}

func collect(t *testing.T, s *Searcher, req Request) ([]Result, Summary) {
	t.Helper()
	var results []Result
	var summary Summary
	err := s.Search(context.Background(), req, func(ev Event) error {
		switch ev.Type {
		case "result":
			results = append(results, *ev.Result)
		case "summary":
			summary = *ev.Summary
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return results, summary
}

func TestContentSearch(t *testing.T) {
	s, dir := setup(t)
	write(t, filepath.Join(dir, "notes.txt"), []byte("line one\nthe property tax statement arrived\nline three\n"))
	write(t, filepath.Join(dir, "other.md"), []byte("# nothing relevant here\n"))
	write(t, filepath.Join(dir, "binary.txt"), append([]byte("PK\x00\x03"), []byte("property tax hidden in binary")...))
	write(t, filepath.Join(dir, "skipped.pdf"), []byte("property tax in pdf, no extractor"))
	scan(t, s)

	results, summary := collect(t, s, Request{Query: "Property TAX"})
	if len(results) != 1 {
		t.Fatalf("results = %+v, want exactly 1", results)
	}
	r := results[0]
	if r.RelativePath != "notes.txt" || r.Line != 2 {
		t.Errorf("match = %+v", r)
	}
	if !strings.Contains(r.Snippet, "property tax statement") {
		t.Errorf("snippet = %q", r.Snippet)
	}
	if r.MatchType != "content" {
		t.Errorf("match type = %q", r.MatchType)
	}
	// binary.txt skipped via NUL sniff; .pdf not in allowlist at all
	if summary.SkippedFiles != 1 {
		t.Errorf("skipped = %d, want 1 (binary)", summary.SkippedFiles)
	}
	if summary.SearchedFiles != 2 {
		t.Errorf("searched = %d, want 2", summary.SearchedFiles)
	}
	if summary.Errors != 0 || summary.TimedOut {
		t.Errorf("summary = %+v", summary)
	}
}

func TestContentSearchLimit(t *testing.T) {
	s, dir := setup(t)
	for i := range 20 {
		write(t, filepath.Join(dir, "f"+string(rune('a'+i))+".txt"), []byte("needle here\n"))
	}
	scan(t, s)
	results, summary := collect(t, s, Request{Query: "needle", Limit: 5})
	if len(results) != 5 {
		t.Errorf("limit not applied: %d results", len(results))
	}
	if !summary.Truncated {
		t.Error("summary should report truncation")
	}
}

func TestContentSearchCancellation(t *testing.T) {
	s, dir := setup(t)
	write(t, filepath.Join(dir, "a.txt"), []byte("needle\n"))
	scan(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.Search(ctx, Request{Query: "needle"}, func(ev Event) error { return nil })
	if err != nil {
		t.Fatalf("cancelled search should still emit summary cleanly, got %v", err)
	}
}

func TestMaxFileSize(t *testing.T) {
	s, dir := setup(t)
	big := make([]byte, 2*1024*1024) // over the 1 MB root cap
	for i := range big {
		big[i] = 'a'
	}
	copy(big[1024:], []byte("needle"))
	write(t, filepath.Join(dir, "big.txt"), big)
	scan(t, s)
	results, _ := collect(t, s, Request{Query: "needle"})
	if len(results) != 0 {
		t.Errorf("oversized file should not be searched, got %+v", results)
	}
}

func TestSnippetTruncation(t *testing.T) {
	long := strings.Repeat("x", 500) + " needle " + strings.Repeat("y", 500)
	s := snippet(long, strings.Index(long, "needle"), len("needle"))
	if len(s) > 2*snippetRadius+len("needle")+10 {
		t.Errorf("snippet too long: %d bytes", len(s))
	}
	if !strings.Contains(s, "needle") || !strings.HasPrefix(s, "...") || !strings.HasSuffix(s, "...") {
		t.Errorf("snippet = %q", s)
	}
}
