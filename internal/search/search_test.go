package search

import (
	"path/filepath"
	"testing"
	"time"

	"super-speedy-search/internal/config"
	"super-speedy-search/internal/db"
)

func seed(t *testing.T) *Searcher {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	cfg, err := config.Parse([]byte(`
node: {id: "test-node", auth_required: false}
roots:
  - id: "docs"
    path: "/data/docs"
    display_prefix: "NAS:Docs"
    open_uri_prefix: "smb://nas.local/docs"
`))
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().Unix()
	metas := []db.FileMeta{
		{RootID: "docs", RelativePath: "taxes/tax 2024.pdf", Filename: "tax 2024.pdf", Extension: ".pdf", SizeBytes: 100, ModifiedAt: now - 100},
		{RootID: "docs", RelativePath: "taxes/2023/summary.pdf", Filename: "summary.pdf", Extension: ".pdf", SizeBytes: 200, ModifiedAt: now - 200},
		{RootID: "docs", RelativePath: "notes/tax-notes.txt", Filename: "tax-notes.txt", Extension: ".txt", SizeBytes: 50, ModifiedAt: now - 50},
		{RootID: "docs", RelativePath: "misc/receipt_50%_off.txt", Filename: "receipt_50%_off.txt", Extension: ".txt", SizeBytes: 10, ModifiedAt: now - 10},
		{RootID: "docs", RelativePath: "taxes", Filename: "taxes", Extension: "", IsDir: true, ModifiedAt: now},
	}
	if _, err := d.UpsertFiles(metas, "scan1", now); err != nil {
		t.Fatal(err)
	}
	return &Searcher{DB: d, Cfg: cfg, NodeID: "test-node"}
}

func TestRankingAndAssembly(t *testing.T) {
	s := seed(t)
	results, err := s.Search(Params{Query: "tax 2024.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("no results")
	}
	top := results[0]
	if top.MatchType != MatchFilenameExact || top.Filename != "tax 2024.pdf" {
		t.Errorf("top result = %+v, want exact filename match", top)
	}
	if top.DisplayPath != "NAS:Docs/taxes/tax 2024.pdf" {
		t.Errorf("display path = %q", top.DisplayPath)
	}
	if top.OpenURI != "smb://nas.local/docs/taxes/tax%202024.pdf" {
		t.Errorf("open uri = %q", top.OpenURI)
	}
	if top.ParentOpenURI != "smb://nas.local/docs/taxes" {
		t.Errorf("parent open uri = %q", top.ParentOpenURI)
	}
	if top.Path != "/data/docs/taxes/tax 2024.pdf" {
		t.Errorf("path = %q", top.Path)
	}
	if top.NodeID != "test-node" {
		t.Errorf("node id = %q", top.NodeID)
	}
}

func TestFilenameBeatsPath(t *testing.T) {
	s := seed(t)
	results, err := s.Search(Params{Query: "tax"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) < 2 {
		t.Fatalf("want multiple results, got %d", len(results))
	}
	// filename matches (tax 2024.pdf, tax-notes.txt) must precede the
	// path-only match (taxes/2023/summary.pdf)
	seenPathMatch := false
	for _, r := range results {
		if r.MatchType == MatchPath {
			seenPathMatch = true
		}
		if seenPathMatch && r.MatchType != MatchPath {
			t.Errorf("filename match ranked below path match: %+v", results)
		}
	}
}

func TestMultiTermAnd(t *testing.T) {
	s := seed(t)
	results, err := s.Search(Params{Query: "tax notes"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Filename != "tax-notes.txt" {
		t.Errorf("multi-term AND: got %+v", results)
	}
}

func TestExtensionFilter(t *testing.T) {
	s := seed(t)
	results, err := s.Search(Params{Query: "tax", Extensions: []string{"txt"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Extension != ".txt" {
			t.Errorf("extension filter leaked %q", r.Extension)
		}
	}
	if len(results) == 0 {
		t.Error("expected .txt results")
	}
}

func TestLikeEscaping(t *testing.T) {
	s := seed(t)
	// "50%" must match literally, not as a wildcard.
	results, err := s.Search(Params{Query: "50%_off"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Filename != "receipt_50%_off.txt" {
		t.Errorf("escaped query: got %+v", results)
	}
	// A query whose % would match everything if unescaped must not.
	results, err = s.Search(Params{Query: "%pdf%"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("unescaped wildcard leaked: %+v", results)
	}
}

func TestDirsExcludedByDefault(t *testing.T) {
	s := seed(t)
	results, err := s.Search(Params{Query: "taxes"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.IsDir {
			t.Errorf("directory returned without include_dirs: %+v", r)
		}
	}
	results, err = s.Search(Params{Query: "taxes", IncludeDirs: true})
	if err != nil {
		t.Fatal(err)
	}
	foundDir := false
	for _, r := range results {
		if r.IsDir {
			foundDir = true
		}
	}
	if !foundDir {
		t.Error("include_dirs did not return the directory")
	}
}

func TestEmptyQuery(t *testing.T) {
	s := seed(t)
	if _, err := s.Search(Params{Query: "   "}); err == nil {
		t.Error("empty query should error")
	}
}
