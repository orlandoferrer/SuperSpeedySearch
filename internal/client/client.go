// Package client is the HTTP client for talking to search nodes, shared by
// the GUI and any future clients. It reuses the node's own request/response
// types so the wire format has one definition.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"super-speedy-search/internal/content"
	"super-speedy-search/internal/search"
)

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 15 * time.Second},
	}
}

// Status mirrors GET /v1/status.
type Status struct {
	NodeID             string   `json:"node_id"`
	Name               string   `json:"name"`
	Version            string   `json:"version"`
	StartedAt          string   `json:"started_at"`
	Capabilities       []string `json:"capabilities"`
	IndexedFiles       int64    `json:"indexed_files"`
	LastScanFinishedAt string   `json:"last_scan_finished_at"`
	AuthRequired       bool     `json:"auth_required"`
}

func (c *Client) Status(ctx context.Context) (*Status, error) {
	var s Status
	if err := c.getJSON(ctx, "/v1/status", &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (c *Client) SearchMetadata(ctx context.Context, p search.Params) ([]search.Result, error) {
	var parsed struct {
		Results []search.Result `json:"results"`
	}
	if err := c.postJSON(ctx, "/v1/search/metadata", p, &parsed); err != nil {
		return nil, err
	}
	return parsed.Results, nil
}

// SearchContent streams NDJSON events to fn until the stream ends, fn returns
// an error, or ctx is cancelled.
func (c *Client) SearchContent(ctx context.Context, req content.Request, fn func(content.Event) error) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := c.newRequest(ctx, "POST", "/v1/search/content", bytes.NewReader(body))
	if err != nil {
		return err
	}
	// No overall timeout: streaming searches are bounded by the node's
	// max_search_seconds and by ctx.
	client := &http.Client{}
	resp, err := client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev content.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return fmt.Errorf("bad stream line: %w", err)
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return nil // cancelled by caller; not an error
	}
	return sc.Err()
}

func (c *Client) TriggerScan(ctx context.Context, rootID string) error {
	body := map[string]string{}
	if rootID != "" {
		body["root_id"] = rootID
	}
	return c.postJSON(ctx, "/v1/scan", body, nil)
}

func (c *Client) newRequest(ctx context.Context, method, path string, body *bytes.Reader) (*http.Request, error) {
	var r *http.Request
	var err error
	if body != nil {
		r, err = http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	} else {
		r, err = http.NewRequestWithContext(ctx, method, c.BaseURL+path, nil)
	}
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		r.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return r, nil
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := c.newRequest(ctx, "GET", path, nil)
	if err != nil {
		return err
	}
	return c.doJSON(req, out)
}

func (c *Client) postJSON(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, "POST", path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	return c.doJSON(req, out)
}

func (c *Client) doJSON(req *http.Request, out any) error {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apiError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func apiError(resp *http.Response) error {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if e.Error == "" {
		e.Error = resp.Status
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("unauthorized: check the node's auth token")
	}
	return fmt.Errorf("node error (HTTP %d): %s", resp.StatusCode, e.Error)
}
