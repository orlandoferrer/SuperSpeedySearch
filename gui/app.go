package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"super-speedy-search/internal/client"
	"super-speedy-search/internal/content"
	"super-speedy-search/internal/discovery"
	"super-speedy-search/internal/search"
)

// App is the Wails-bound backend. Its exported methods are callable from the
// frontend as window.go.main.App.<Method>.
type App struct {
	ctx      context.Context
	settings *Settings

	mu            sync.Mutex
	contentCancel context.CancelFunc
}

func NewApp() (*App, error) {
	settings, err := LoadSettings()
	if err != nil {
		return nil, err
	}
	return &App{settings: settings}, nil
}

func (a *App) startup(ctx context.Context) { a.ctx = ctx }

// NodeInfo is what the node list renders.
type NodeInfo struct {
	URL          string `json:"url"`
	ID           string `json:"id"`
	Name         string `json:"name"`
	Version      string `json:"version"`
	Source       string `json:"source"` // "mdns" | "manual"
	Online       bool   `json:"online"`
	AuthRequired bool   `json:"auth_required"`
	HasToken     bool   `json:"has_token"`
	IndexedFiles int64  `json:"indexed_files"`
	LastScan     string `json:"last_scan"`
	Error        string `json:"error,omitempty"`
}

// Discover browses mDNS, merges manual nodes, and probes each node's status
// concurrently.
func (a *App) Discover() []NodeInfo {
	type source struct{ url, kind string }
	seen := map[string]source{}

	browseCtx, cancel := context.WithTimeout(a.ctx, 5*time.Second)
	defer cancel()
	if found, err := discovery.Browse(browseCtx, 2*time.Second); err == nil {
		for _, n := range found {
			seen[n.URL()] = source{url: n.URL(), kind: "mdns"}
		}
	}
	for _, u := range a.settings.ManualNodeList() {
		if _, ok := seen[u]; !ok {
			seen[u] = source{url: u, kind: "manual"}
		} else {
			seen[u] = source{url: u, kind: "manual"} // manual wins: it's removable
		}
	}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		nodes []NodeInfo
	)
	for _, src := range seen {
		wg.Add(1)
		go func(src source) {
			defer wg.Done()
			info := NodeInfo{URL: src.url, Source: src.kind, HasToken: a.settings.HasSpecificToken(src.url)}
			c := client.New(src.url, a.settings.TokenFor(src.url))
			c.HTTP.Timeout = 3 * time.Second
			ctx, cancel := context.WithTimeout(a.ctx, 3*time.Second)
			defer cancel()
			st, err := c.Status(ctx)
			if err != nil {
				info.Error = err.Error()
			} else {
				info.Online = true
				info.ID = st.NodeID
				info.Name = st.Name
				info.Version = st.Version
				info.AuthRequired = st.AuthRequired
				info.IndexedFiles = st.IndexedFiles
				info.LastScan = st.LastScanFinishedAt
			}
			mu.Lock()
			nodes = append(nodes, info)
			mu.Unlock()
		}(src)
	}
	wg.Wait()
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Online != nodes[j].Online {
			return nodes[i].Online
		}
		return nodes[i].URL < nodes[j].URL
	})
	return nodes
}

func (a *App) AddManualNode(rawURL string) error {
	rawURL = strings.TrimSpace(strings.TrimSuffix(rawURL, "/"))
	if rawURL == "" {
		return errors.New("node URL is required")
	}
	if !strings.Contains(rawURL, "://") {
		rawURL = "http://" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("not a valid URL: %q", rawURL)
	}
	if u.Port() == "" {
		u.Host += ":37373"
	}
	return a.settings.AddManualNode(u.Scheme + "://" + u.Host)
}

func (a *App) RemoveManualNode(url string) error { return a.settings.RemoveManualNode(url) }
func (a *App) SetNodeToken(url, token string) error {
	return a.settings.SetToken(url, strings.TrimSpace(token))
}
func (a *App) SetDefaultToken(token string) error {
	return a.settings.SetDefaultToken(strings.TrimSpace(token))
}
func (a *App) GetDefaultTokenSet() bool { return a.settings.DefaultToken != "" }

func (a *App) clientsFor(nodeURLs []string) map[string]*client.Client {
	clients := make(map[string]*client.Client, len(nodeURLs))
	for _, u := range nodeURLs {
		clients[u] = client.New(u, a.settings.TokenFor(u))
	}
	return clients
}

// MetaSearchResponse is the merged fan-out result set.
type MetaSearchResponse struct {
	Results []search.Result    `json:"results"`
	Errors  []client.NodeError `json:"errors"`
}

func (a *App) SearchMetadata(query string, extensions []string, nodeURLs []string, limit int) (MetaSearchResponse, error) {
	if strings.TrimSpace(query) == "" {
		return MetaSearchResponse{}, errors.New("query is required")
	}
	if len(nodeURLs) == 0 {
		return MetaSearchResponse{}, errors.New("no nodes selected")
	}
	if limit <= 0 {
		limit = 100
	}
	ctx, cancel := context.WithTimeout(a.ctx, 20*time.Second)
	defer cancel()
	results, errs := client.FanOutMetadata(ctx, a.clientsFor(nodeURLs), search.Params{
		Query: query, Extensions: extensions, Limit: limit,
	})
	if results == nil {
		results = []search.Result{}
	}
	if errs == nil {
		errs = []client.NodeError{}
	}
	return MetaSearchResponse{Results: results, Errors: errs}, nil
}

// ContentEvent is pushed to the frontend over the Wails event bus while a
// deep search runs. Event names: "content" (per event), "content_done".
type ContentEvent struct {
	NodeURL string           `json:"node_url"`
	NodeID  string           `json:"node_id,omitempty"`
	Type    string           `json:"type"`
	Result  *content.Result  `json:"result,omitempty"`
	Summary *content.Summary `json:"summary,omitempty"`
	Message string           `json:"message,omitempty"`
}

// StartContentSearch begins a streaming deep search across the given nodes.
// Any previous deep search is cancelled first.
func (a *App) StartContentSearch(query string, extensions []string, nodeURLs []string, limit int) error {
	if strings.TrimSpace(query) == "" {
		return errors.New("query is required")
	}
	if len(nodeURLs) == 0 {
		return errors.New("no nodes selected")
	}
	if limit <= 0 {
		limit = 100
	}

	a.mu.Lock()
	if a.contentCancel != nil {
		a.contentCancel()
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.contentCancel = cancel
	a.mu.Unlock()

	clients := a.clientsFor(nodeURLs)
	go func() {
		var wg sync.WaitGroup
		for nodeURL, c := range clients {
			wg.Add(1)
			go func(nodeURL string, c *client.Client) {
				defer wg.Done()
				err := c.SearchContent(ctx, content.Request{
					Query: query, Extensions: extensions, Limit: limit,
				}, func(ev content.Event) error {
					out := ContentEvent{NodeURL: nodeURL, Type: ev.Type, Result: ev.Result, Summary: ev.Summary, Message: ev.Message}
					if ev.Result != nil {
						out.NodeID = ev.Result.NodeID
					}
					wruntime.EventsEmit(a.ctx, "content", out)
					return nil
				})
				if err != nil && ctx.Err() == nil {
					wruntime.EventsEmit(a.ctx, "content", ContentEvent{
						NodeURL: nodeURL, Type: "error", Message: err.Error(),
					})
				}
			}(nodeURL, c)
		}
		wg.Wait()
		wruntime.EventsEmit(a.ctx, "content_done", ctx.Err() != nil)
	}()
	return nil
}

func (a *App) CancelContentSearch() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.contentCancel != nil {
		a.contentCancel()
		a.contentCancel = nil
	}
}

func (a *App) TriggerScan(nodeURL string) error {
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	return client.New(nodeURL, a.settings.TokenFor(nodeURL)).TriggerScan(ctx, "")
}

// OpenURI hands a result URI (smb://, file://, http://) to the OS.
func (a *App) OpenURI(uri string) error {
	if uri == "" {
		return errors.New("no URI for this result")
	}
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", uri).Run()
	case "linux":
		return exec.Command("xdg-open", uri).Run()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", uri).Run()
	}
	return fmt.Errorf("unsupported platform %s", runtime.GOOS)
}

// RevealFileURI shows a local file:// result in the file manager.
func (a *App) RevealFileURI(uri string) error {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return errors.New("reveal only works for local file:// results")
	}
	path, err := url.PathUnescape(u.Path)
	if err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", "-R", path).Run()
	case "linux":
		// no portable "reveal": open the containing directory
		return exec.Command("xdg-open", path[:strings.LastIndex(path, "/")]).Run()
	}
	return fmt.Errorf("unsupported platform %s", runtime.GOOS)
}

// ClipboardSet copies text (result paths) to the system clipboard.
func (a *App) ClipboardSet(text string) error {
	return wruntime.ClipboardSetText(a.ctx, text)
}
