package client

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"super-speedy-search/internal/api"
	"super-speedy-search/internal/config"
	"super-speedy-search/internal/content"
	"super-speedy-search/internal/db"
	"super-speedy-search/internal/scanner"
	"super-speedy-search/internal/search"
)

// newTestNode spins up a real node API over httptest so the client is tested
// against the actual server implementation, not a mock.
func newTestNode(t *testing.T, nodeID, token string, files map[string]string) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	authRequired := "false"
	if token != "" {
		authRequired = "true"
	}
	cfg, err := config.Parse([]byte(`
node:
  id: "` + nodeID + `"
  auth_required: ` + authRequired + `
  auth_token: "` + token + `"
roots:
  - id: "docs"
    path: "` + dir + `"
    display_prefix: "` + nodeID + `:Docs"
    content_search:
      enabled: true
      include_extensions: [".txt"]
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
	if err := d.SyncRoots([]db.RootRow{{ID: "docs", Path: dir, DisplayPrefix: nodeID + ":Docs", Enabled: true}}, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	scn := scanner.New(d, cfg, log)
	if err := scn.Run(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	srv := &api.Server{
		Cfg: cfg, DB: d, Scanner: scn,
		Searcher:  &search.Searcher{DB: d, Cfg: cfg, NodeID: nodeID},
		Content:   &content.Searcher{DB: d, Cfg: cfg, NodeID: nodeID, Log: log},
		Version:   "test",
		StartedAt: time.Now(),
		Log:       log,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestStatusAndAuth(t *testing.T) {
	ts := newTestNode(t, "node-a", "tok", map[string]string{"a.txt": "x"})

	c := New(ts.URL, "tok")
	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.NodeID != "node-a" || st.IndexedFiles != 1 || !st.AuthRequired {
		t.Errorf("status = %+v", st)
	}

	bad := New(ts.URL, "wrong")
	if _, err := bad.Status(context.Background()); err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("wrong token: err = %v", err)
	}
}

func TestSearchContentStreaming(t *testing.T) {
	ts := newTestNode(t, "node-a", "", map[string]string{
		"notes.txt": "nothing\nfind the needle here\n",
	})
	c := New(ts.URL, "")
	var results []content.Result
	var summaries int
	err := c.SearchContent(context.Background(), content.Request{Query: "needle"}, func(ev content.Event) error {
		switch ev.Type {
		case "result":
			results = append(results, *ev.Result)
		case "summary":
			summaries++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Line != 2 || summaries != 1 {
		t.Errorf("results = %+v summaries = %d", results, summaries)
	}
}

func TestFanOutMetadata(t *testing.T) {
	now := time.Now()
	tsA := newTestNode(t, "node-a", "", map[string]string{
		"report.txt":        "x",
		"old/tax notes.txt": "x",
	})
	tsB := newTestNode(t, "node-b", "tok-b", map[string]string{
		"tax.txt": "x",
	})
	_ = now

	clients := map[string]*Client{
		tsA.URL: New(tsA.URL, ""),
		tsB.URL: New(tsB.URL, "tok-b"),
	}
	results, errs := FanOutMetadata(context.Background(), clients, search.Params{Query: "tax"})
	if len(errs) != 0 {
		t.Fatalf("errs = %+v", errs)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want 2", results)
	}
	// exact filename match ("tax.txt" is not exact for query "tax", but both
	// are filename matches) — verify both nodes contributed
	nodes := map[string]bool{}
	for _, r := range results {
		nodes[r.NodeID] = true
	}
	if !nodes["node-a"] || !nodes["node-b"] {
		t.Errorf("missing node in merged results: %+v", results)
	}
}

func TestFanOutPartialFailure(t *testing.T) {
	ts := newTestNode(t, "node-a", "", map[string]string{"tax.txt": "x"})
	clients := map[string]*Client{
		ts.URL:               New(ts.URL, ""),
		"http://127.0.0.1:1": New("http://127.0.0.1:1", ""), // nothing listens here
	}
	results, errs := FanOutMetadata(context.Background(), clients, search.Params{Query: "tax"})
	if len(results) != 1 {
		t.Errorf("good node's results lost: %+v", results)
	}
	if len(errs) != 1 || errs[0].NodeURL != "http://127.0.0.1:1" {
		t.Errorf("errs = %+v", errs)
	}
}

func TestRankGlobal(t *testing.T) {
	results := []search.Result{
		{Filename: "c", MatchType: search.MatchPath, ModifiedAt: "2026-01-03T00:00:00Z"},
		{Filename: "a", MatchType: search.MatchFilename, ModifiedAt: "2026-01-01T00:00:00Z"},
		{Filename: "b", MatchType: search.MatchFilename, ModifiedAt: "2026-01-02T00:00:00Z"},
		{Filename: "d", MatchType: search.MatchFilenameExact, ModifiedAt: "2025-01-01T00:00:00Z"},
	}
	RankGlobal(results)
	got := []string{results[0].Filename, results[1].Filename, results[2].Filename, results[3].Filename}
	want := []string{"d", "b", "a", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}
