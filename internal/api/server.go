// Package api exposes the node HTTP JSON API.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"super-speedy-search/internal/config"
	"super-speedy-search/internal/content"
	"super-speedy-search/internal/db"
	"super-speedy-search/internal/scanner"
	"super-speedy-search/internal/search"
)

type Server struct {
	Cfg       *config.Config
	DB        *db.DB
	Scanner   *scanner.Scanner
	Searcher  *search.Searcher
	Content   *content.Searcher
	Version   string
	StartedAt time.Time
	Log       *slog.Logger
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// All node behavior is exposed through versioned /v1 routes. Keeping the
	// API small and JSON-based makes it easy to test with curl and easy for the
	// desktop GUI, CLI, or future mobile/web clients to share.
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("GET /v1/roots", s.handleRoots)
	mux.HandleFunc("POST /v1/search/metadata", s.handleMetadataSearch)
	mux.HandleFunc("POST /v1/search/content", s.handleContentSearch)
	mux.HandleFunc("POST /v1/scan", s.handleScanStart)
	mux.HandleFunc("GET /v1/scan/current", s.handleScanCurrent)
	mux.HandleFunc("GET /v1/scan/history", s.handleScanHistory)
	mux.HandleFunc("GET /v1/config", s.handleConfig)
	return s.logMiddleware(s.authMiddleware(mux))
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := s.Cfg.Node.AuthToken
		if !s.Cfg.Node.AuthRequiredOn() || token == "" {
			next.ServeHTTP(w, r)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		// ConstantTimeCompare avoids leaking how many prefix characters matched.
		// That is a small thing on a home LAN, but cheap and correct.
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		s.Log.Debug("http", "method", r.Method, "path", r.URL.Path,
			"remote", r.RemoteAddr, "took", time.Since(start).Round(time.Millisecond))
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	count, err := s.DB.CountFiles()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	lastScan, _ := s.DB.LastScanFinishedAt()
	caps := []string{"metadata_search", "live_content_search"}
	if s.Cfg.Node.AdvertiseOn() {
		caps = append(caps, "mdns")
	}
	for _, root := range s.Cfg.Roots {
		if root.OpenURIPrefix != "" {
			caps = append(caps, "open_uri")
			break
		}
	}
	resp := map[string]any{
		"node_id":       s.Cfg.Node.ID,
		"name":          s.Cfg.Node.Name,
		"version":       s.Version,
		"started_at":    s.StartedAt.UTC().Format(time.RFC3339),
		"capabilities":  caps,
		"indexed_files": count,
		"auth_required": s.Cfg.Node.AuthRequiredOn(),
	}
	if lastScan > 0 {
		resp["last_scan_finished_at"] = time.Unix(lastScan, 0).UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleRoots(w http.ResponseWriter, r *http.Request) {
	type rootInfo struct {
		ID            string `json:"id"`
		Path          string `json:"path"`
		DisplayPrefix string `json:"display_prefix"`
		OpenURIPrefix string `json:"open_uri_prefix,omitempty"`
		Enabled       bool   `json:"enabled"`
		ContentSearch bool   `json:"content_search"`
	}
	out := make([]rootInfo, 0, len(s.Cfg.Roots))
	for _, root := range s.Cfg.Roots {
		out = append(out, rootInfo{
			ID:            root.ID,
			Path:          root.Path,
			DisplayPrefix: root.DisplayPrefix,
			OpenURIPrefix: root.OpenURIPrefix,
			Enabled:       root.EnabledOn(),
			ContentSearch: root.ContentSearch.Enabled,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"roots": out})
}

func (s *Server) handleMetadataSearch(w http.ResponseWriter, r *http.Request) {
	var p search.Params
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	results, err := s.Searcher.Search(p)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if results == nil {
		results = []search.Result{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (s *Server) handleContentSearch(w http.ResponseWriter, r *http.Request) {
	var req content.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	emit := func(ev content.Event) error {
		// NDJSON is one JSON object per line. It lets clients display matches as
		// soon as each file is searched instead of waiting for the whole deep
		// search to finish.
		if err := enc.Encode(ev); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	err := s.Content.Search(r.Context(), req, emit)
	switch {
	case errors.Is(err, content.ErrBusy):
		// headers not written yet only if no events were emitted; ErrBusy
		// happens before any emit, so a proper status is still possible.
		writeError(w, http.StatusTooManyRequests, err.Error())
	case err != nil && r.Context().Err() == nil:
		// stream already started; report the error as a final event
		_ = emit(content.Event{Type: "error", Message: err.Error()})
	}
}

func (s *Server) handleScanStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RootID string `json:"root_id"`
	}
	// An empty body means "scan everything", but malformed JSON must be
	// rejected — silently ignoring it could run a full scan when the client
	// believes it requested a single root.
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if body.RootID != "" {
		if _, ok := s.Cfg.RootByID(body.RootID); !ok {
			writeError(w, http.StatusBadRequest, "unknown root: "+body.RootID)
			return
		}
	}
	if s.Scanner.Current().Running {
		writeError(w, http.StatusConflict, "a scan is already running")
		return
	}
	go func() {
		if err := s.Scanner.Run(context.Background(), body.RootID); err != nil && !errors.Is(err, scanner.ErrScanRunning) {
			s.Log.Error("scan failed", "err", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"started": true})
}

func (s *Server) handleScanCurrent(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Scanner.Current())
}

func (s *Server) handleScanHistory(w http.ResponseWriter, r *http.Request) {
	runs, err := s.DB.ScanHistory(20)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if runs == nil {
		runs = []db.ScanRun{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

// handleConfig returns the effective config. The auth token is redacted via
// the `json:"-"` tag on Node.AuthToken.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Cfg)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
