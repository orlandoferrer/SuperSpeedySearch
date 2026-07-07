package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimal = `
roots:
  - id: "docs"
    path: "/tmp/docs"
`

func TestParseDefaults(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.Node.ID == "" || c.Node.Name == "" {
		t.Errorf("expected node id/name defaults, got %+v", c.Node)
	}
	if c.Node.ListenAddr != "0.0.0.0:37373" {
		t.Errorf("listen addr default = %q", c.Node.ListenAddr)
	}
	if !c.Node.AuthRequiredOn() || !c.Node.AdvertiseOn() {
		t.Error("auth and advertise should default on")
	}
	if time.Duration(c.Scan.Interval) != 6*time.Hour {
		t.Errorf("scan interval default = %v", time.Duration(c.Scan.Interval))
	}
	if !c.Scan.Watch.EnabledOn() || c.Scan.Watch.MaxWatchedDirs != 50000 {
		t.Errorf("watch defaults wrong: %+v", c.Scan.Watch)
	}
	if c.Scan.TombstoneRetentionDays != 30 {
		t.Errorf("tombstone retention default = %d", c.Scan.TombstoneRetentionDays)
	}
	r := c.Roots[0]
	if r.ContentSearch.MaxFileSizeMB != 25 {
		t.Errorf("content max size default = %d", r.ContentSearch.MaxFileSizeMB)
	}
	if len(r.Excludes.Paths) == 0 {
		t.Error("expected default excludes")
	}
	if r.DisplayPrefix == "" {
		t.Error("expected derived display prefix")
	}
	if c.ResourceLimits.MaxResultsPerQuery != 500 {
		t.Errorf("max results default = %d", c.ResourceLimits.MaxResultsPerQuery)
	}
}

func TestParseFull(t *testing.T) {
	c, err := Parse([]byte(`
node:
  id: "mac"
  name: "Mac"
  advertise: false
  auth_required: false
scan:
  interval: "2h"
roots:
  - id: "docs"
    path: "/tmp/docs"
    excludes:
      extensions: ["tmp", ".BAK"]
    content_search:
      enabled: true
      include_extensions: ["TXT", ".md"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Node.AdvertiseOn() || c.Node.AuthRequiredOn() {
		t.Error("explicit false should stick")
	}
	got := c.Roots[0].Excludes.Extensions
	if got[0] != ".tmp" || got[1] != ".bak" {
		t.Errorf("extensions not normalized: %v", got)
	}
	inc := c.Roots[0].ContentSearch.IncludeExtensions
	if inc[0] != ".txt" || inc[1] != ".md" {
		t.Errorf("include extensions not normalized: %v", inc)
	}
}

func TestValidation(t *testing.T) {
	cases := []struct {
		name, yaml, wantErr string
	}{
		{"no roots", `node: {id: "x"}`, "at least one root"},
		{"relative root", `roots: [{id: "a", path: "docs"}]`, "must be absolute"},
		{"dup root", `roots: [{id: "a", path: "/x"}, {id: "a", path: "/y"}]`, "duplicate root"},
		{"short interval", "scan: {interval: \"5s\"}\nroots: [{id: \"a\", path: \"/x\"}]", "at least 1m"},
		{"unknown field", "bogus: 1\nroots: [{id: \"a\", path: \"/x\"}]", "field bogus not found"},
	}
	for _, tc := range cases {
		_, err := Parse([]byte(tc.yaml))
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: want error containing %q, got %v", tc.name, tc.wantErr, err)
		}
	}
}

func TestEnsureAuthTokenGeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	c.Database.Path = filepath.Join(dir, "index.db")

	tok1, generated, err := c.EnsureAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	if !generated || len(tok1) < 32 {
		t.Fatalf("expected generated token, got %q generated=%v", tok1, generated)
	}
	if _, err := os.Stat(filepath.Join(dir, "auth_token")); err != nil {
		t.Fatalf("token not persisted: %v", err)
	}

	c2, _ := Parse([]byte(minimal))
	c2.Database.Path = c.Database.Path
	tok2, generated2, err := c2.EnsureAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	if generated2 || tok2 != tok1 {
		t.Errorf("second run should reuse token: got %q generated=%v", tok2, generated2)
	}
}

func TestEnsureAuthTokenDisabled(t *testing.T) {
	c, err := Parse([]byte(minimal + `
node:
  auth_required: false
`))
	if err != nil {
		t.Fatal(err)
	}
	tok, generated, err := c.EnsureAuthToken()
	if err != nil || tok != "" || generated {
		t.Errorf("disabled auth should yield empty token, got %q %v %v", tok, generated, err)
	}
}
