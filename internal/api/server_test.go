package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"super-speedy-search/internal/config"
	"super-speedy-search/internal/content"
	"super-speedy-search/internal/db"
	"super-speedy-search/internal/scanner"
	"super-speedy-search/internal/search"
)

func newTestServer(t *testing.T, authToken string) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	authRequired := "false"
	if authToken != "" {
		authRequired = "true"
	}
	cfg, err := config.Parse([]byte(`
node:
  id: "test-node"
  auth_required: ` + authRequired + `
  auth_token: "` + authToken + `"
roots:
  - id: "r1"
    path: "` + dir + `"
    display_prefix: "Test:r1"
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
	if err := d.SyncRoots([]db.RootRow{{ID: "r1", Path: dir, DisplayPrefix: "Test:r1", Enabled: true}}, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "hello world.txt"), []byte("searchable content needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	scn := scanner.New(d, cfg, log)
	if err := scn.Run(context.Background(), ""); err != nil {
		t.Fatal(err)
	}

	srv := &Server{
		Cfg:       cfg,
		DB:        d,
		Scanner:   scn,
		Searcher:  &search.Searcher{DB: d, Cfg: cfg, NodeID: "test-node"},
		Content:   &content.Searcher{DB: d, Cfg: cfg, NodeID: "test-node", Log: log},
		Version:   "test",
		StartedAt: time.Now(),
		Log:       log,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, dir
}

func doJSON(t *testing.T, method, url, token string, body any) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestAuthRequired(t *testing.T) {
	ts, _ := newTestServer(t, "sekrit")
	if resp := doJSON(t, "GET", ts.URL+"/v1/status", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", resp.StatusCode)
	}
	if resp := doJSON(t, "GET", ts.URL+"/v1/status", "wrong", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad token: status = %d, want 401", resp.StatusCode)
	}
	if resp := doJSON(t, "GET", ts.URL+"/v1/status", "sekrit", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("good token: status = %d, want 200", resp.StatusCode)
	}
}

func TestStatus(t *testing.T) {
	ts, _ := newTestServer(t, "")
	resp := doJSON(t, "GET", ts.URL+"/v1/status", "", nil)
	var body struct {
		NodeID       string   `json:"node_id"`
		IndexedFiles int64    `json:"indexed_files"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.NodeID != "test-node" || body.IndexedFiles != 1 {
		t.Errorf("status = %+v", body)
	}
	if !strings.Contains(strings.Join(body.Capabilities, ","), "metadata_search") {
		t.Errorf("capabilities = %v", body.Capabilities)
	}
}

func TestMetadataSearchEndpoint(t *testing.T) {
	ts, _ := newTestServer(t, "")
	resp := doJSON(t, "POST", ts.URL+"/v1/search/metadata", "", map[string]any{"query": "hello world"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Results []search.Result `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Results) != 1 || body.Results[0].Filename != "hello world.txt" {
		t.Errorf("results = %+v", body.Results)
	}
	if body.Results[0].DisplayPath != "Test:r1/hello world.txt" {
		t.Errorf("display path = %q", body.Results[0].DisplayPath)
	}
}

func TestContentSearchEndpointStreamsNDJSON(t *testing.T) {
	ts, _ := newTestServer(t, "")
	resp := doJSON(t, "POST", ts.URL+"/v1/search/content", "", map[string]any{"query": "needle"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("content type = %q", ct)
	}
	var types []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var ev content.Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("bad NDJSON line %q: %v", sc.Text(), err)
		}
		types = append(types, ev.Type)
		if ev.Type == "result" && !strings.Contains(ev.Result.Snippet, "needle") {
			t.Errorf("snippet = %q", ev.Result.Snippet)
		}
	}
	if len(types) != 2 || types[0] != "result" || types[1] != "summary" {
		t.Errorf("event sequence = %v, want [result summary]", types)
	}
}

func TestScanEndpoints(t *testing.T) {
	ts, dir := newTestServer(t, "")
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp := doJSON(t, "POST", ts.URL+"/v1/scan", "", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("scan start status = %d", resp.StatusCode)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r := doJSON(t, "GET", ts.URL+"/v1/scan/current", "", nil)
		var cur struct {
			Running bool `json:"running"`
		}
		_ = json.NewDecoder(r.Body).Decode(&cur)
		if !cur.Running {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	r := doJSON(t, "GET", ts.URL+"/v1/scan/history", "", nil)
	var hist struct {
		Runs []db.ScanRun `json:"runs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&hist); err != nil {
		t.Fatal(err)
	}
	if len(hist.Runs) < 2 {
		t.Errorf("history = %+v, want at least 2 runs", hist.Runs)
	}
}

func TestConfigRedactsToken(t *testing.T) {
	ts, _ := newTestServer(t, "sekrit")
	resp := doJSON(t, "GET", ts.URL+"/v1/config", "sekrit", nil)
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), "sekrit") {
		t.Errorf("config response leaked the auth token: %s", raw)
	}
	if !strings.Contains(string(raw), "test-node") {
		t.Errorf("config response missing expected content: %s", raw)
	}
}

func TestUnknownRootScan(t *testing.T) {
	ts, _ := newTestServer(t, "")
	resp := doJSON(t, "POST", ts.URL+"/v1/scan", "", map[string]any{"root_id": "nope"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown root: status = %d, want 400", resp.StatusCode)
	}
}

// Malformed JSON must be a 400, not a silent full scan; an empty body still
// means "scan all roots".
func TestScanBodyValidation(t *testing.T) {
	ts, _ := newTestServer(t, "")
	req, err := http.NewRequest("POST", ts.URL+"/v1/scan", strings.NewReader(`{"root_id": `))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed JSON: status = %d, want 400", resp.StatusCode)
	}
	if resp := doJSON(t, "POST", ts.URL+"/v1/scan", "", nil); resp.StatusCode != http.StatusAccepted &&
		resp.StatusCode != http.StatusConflict {
		t.Errorf("empty body: status = %d, want 202 (or 409 if a scan is running)", resp.StatusCode)
	}
}
