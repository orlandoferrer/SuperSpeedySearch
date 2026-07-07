// Package content implements live (unindexed) content search: candidates come
// from the metadata index, file bodies are scanned on demand, and results
// stream incrementally.
package content

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"super-speedy-search/internal/config"
	"super-speedy-search/internal/db"
	"super-speedy-search/internal/search"
)

var ErrBusy = errors.New("too many concurrent content searches")

const (
	maxLineBytes      = 1 << 20 // lines longer than 1 MiB are truncated
	sniffBytes        = 8000
	maxMatchesPerFile = 5
	snippetRadius     = 100
)

type Request struct {
	Query      string   `json:"query"`
	Extensions []string `json:"extensions,omitempty"`
	RootIDs    []string `json:"root_ids,omitempty"`
	Limit      int      `json:"limit,omitempty"`
	MaxSeconds int      `json:"max_seconds,omitempty"`
}

type Result struct {
	NodeID       string `json:"node_id"`
	RootID       string `json:"root_id"`
	Path         string `json:"path"`
	RelativePath string `json:"relative_path"`
	DisplayPath  string `json:"display_path"`
	OpenURI      string `json:"open_uri,omitempty"`
	Filename     string `json:"filename"`
	MatchType    string `json:"match_type"`
	Snippet      string `json:"snippet"`
	Line         int    `json:"line,omitempty"`
}

type Summary struct {
	SearchedFiles int64 `json:"searched_files"`
	SkippedFiles  int64 `json:"skipped_files"`
	Errors        int64 `json:"errors"`
	TimedOut      bool  `json:"timed_out"`
	Truncated     bool  `json:"truncated"`
}

// Event is one NDJSON line in the streaming response.
type Event struct {
	Type    string   `json:"type"` // "result" | "summary" | "error"
	Result  *Result  `json:"result,omitempty"`
	Summary *Summary `json:"summary,omitempty"`
	Message string   `json:"message,omitempty"`
}

type Searcher struct {
	DB     *db.DB
	Cfg    *config.Config
	NodeID string
	Log    *slog.Logger
	PDF    Extractor // nil when PDF search is disabled

	semOnce sync.Once
	sem     chan struct{}
}

// Search streams events to emit. emit is called from one goroutine at a time.
func (s *Searcher) Search(ctx context.Context, req Request, emit func(Event) error) error {
	s.semOnce.Do(func() {
		s.sem = make(chan struct{}, s.Cfg.ResourceLimits.MaxParallelContentSearches)
	})
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		return ErrBusy
	}

	query := strings.ToLower(strings.TrimSpace(req.Query))
	if query == "" {
		return errors.New("query is required")
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > s.Cfg.ResourceLimits.MaxResultsPerQuery {
		limit = s.Cfg.ResourceLimits.MaxResultsPerQuery
	}
	maxSeconds := s.Cfg.ResourceLimits.MaxSearchSeconds
	if req.MaxSeconds > 0 && req.MaxSeconds < maxSeconds {
		maxSeconds = req.MaxSeconds
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(maxSeconds)*time.Second)
	defer cancel()

	var (
		sum      Summary
		searched atomic.Int64
		skipped  atomic.Int64
		errCount atomic.Int64
		emitted  atomic.Int64
		emitMu   sync.Mutex
	)
	emitResult := func(r Result) error {
		emitMu.Lock()
		defer emitMu.Unlock()
		if emitted.Load() >= int64(limit) {
			return nil
		}
		if err := emit(Event{Type: "result", Result: &r}); err != nil {
			cancel()
			return err
		}
		if emitted.Add(1) >= int64(limit) {
			sum.Truncated = true
			cancel()
		}
		return nil
	}

	type job struct {
		root config.Root
		cand db.Candidate
	}
	jobs := make(chan job)
	workers := min(s.Cfg.ResourceLimits.MaxParallelContentSearches, 2)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					continue
				}
				n, err := s.searchFile(ctx, j.root, j.cand, query, emitResult)
				switch {
				case errors.Is(err, errSkipped):
					skipped.Add(1)
				case err != nil && ctx.Err() == nil:
					errCount.Add(1)
					s.Log.Debug("content search file failed", "path", j.cand.RelativePath, "err", err)
				default:
					_ = n
					searched.Add(1)
				}
			}
		}()
	}

	// Producer: eligible roots ∩ requested roots; extensions are the
	// intersection of the root allowlist and the request.
feed:
	for _, root := range s.Cfg.EnabledRoots() {
		if !root.ContentSearch.Enabled {
			continue
		}
		if len(req.RootIDs) > 0 && !contains(req.RootIDs, root.ID) {
			continue
		}
		exts := effectiveExtensions(root, req.Extensions, s.PDF != nil)
		if len(exts) == 0 {
			continue
		}
		maxSize := int64(root.ContentSearch.MaxFileSizeMB) * 1024 * 1024
		err := s.DB.ContentCandidates(root.ID, exts, maxSize, func(c db.Candidate) error {
			select {
			case jobs <- job{root: root, cand: c}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if err != nil {
			if ctx.Err() != nil {
				break feed
			}
			errCount.Add(1)
			s.Log.Error("candidate query failed", "root", root.ID, "err", err)
		}
	}
	close(jobs)
	wg.Wait()

	sum.SearchedFiles = searched.Load()
	sum.SkippedFiles = skipped.Load()
	sum.Errors = errCount.Load()
	sum.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded) && !sum.Truncated
	emitMu.Lock()
	defer emitMu.Unlock()
	return emit(Event{Type: "summary", Summary: &sum})
}

var errSkipped = errors.New("skipped")

func (s *Searcher) searchFile(ctx context.Context, root config.Root, c db.Candidate, query string, emitResult func(Result) error) (int, error) {
	absPath := filepath.Join(root.Path, filepath.FromSlash(c.RelativePath))

	var reader io.ReadCloser
	if c.Extension == ".pdf" {
		if s.PDF == nil {
			return 0, errSkipped
		}
		r, err := s.PDF.Extract(ctx, absPath)
		if err != nil {
			return 0, err
		}
		reader = r
	} else {
		f, err := os.Open(absPath)
		if err != nil {
			return 0, err
		}
		buf := make([]byte, sniffBytes)
		n, _ := io.ReadFull(f, buf)
		if bytes.IndexByte(buf[:n], 0) >= 0 {
			f.Close()
			return 0, errSkipped // binary
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			f.Close()
			return 0, err
		}
		reader = f
	}
	defer reader.Close()

	matches := 0
	line := 0
	br := bufio.NewReaderSize(reader, 64*1024)
	for {
		if ctx.Err() != nil {
			return matches, nil
		}
		text, err := readLine(br)
		if text == "" && err != nil {
			if err == io.EOF {
				return matches, nil
			}
			return matches, err
		}
		line++
		if idx := strings.Index(strings.ToLower(text), query); idx >= 0 {
			res := Result{
				NodeID:       s.NodeID,
				RootID:       c.RootID,
				Path:         absPath,
				RelativePath: c.RelativePath,
				DisplayPath:  strings.TrimSuffix(root.DisplayPrefix, "/") + "/" + c.RelativePath,
				Filename:     c.Filename,
				MatchType:    "content",
				Snippet:      snippet(text, idx, len(query)),
				Line:         line,
			}
			if root.OpenURIPrefix != "" {
				res.OpenURI = search.JoinOpenURI(root.OpenURIPrefix, c.RelativePath)
			}
			if err := emitResult(res); err != nil {
				return matches, err
			}
			matches++
			if matches >= maxMatchesPerFile {
				return matches, nil
			}
		}
		if err == io.EOF {
			return matches, nil
		}
	}
}

// readLine reads one line, truncating pathological lines at maxLineBytes so a
// minified single-line file cannot balloon memory.
func readLine(br *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		chunk, err := br.ReadSlice('\n')
		if b.Len()+len(chunk) > maxLineBytes {
			chunk = chunk[:maxLineBytes-b.Len()]
		}
		b.Write(chunk)
		if err == bufio.ErrBufferFull && b.Len() < maxLineBytes {
			continue
		}
		if err == bufio.ErrBufferFull {
			// discard the rest of the oversized line
			for err == bufio.ErrBufferFull {
				_, err = br.ReadSlice('\n')
			}
			return strings.TrimRight(b.String(), "\n"), err
		}
		return strings.TrimRight(b.String(), "\r\n"), err
	}
}

func snippet(line string, idx, qlen int) string {
	start := max(idx-snippetRadius, 0)
	end := min(idx+qlen+snippetRadius, len(line))
	// avoid splitting UTF-8 runes at the boundaries
	for start > 0 && start < len(line) && line[start]&0xC0 == 0x80 {
		start--
	}
	for end < len(line) && line[end]&0xC0 == 0x80 {
		end++
	}
	s := strings.TrimSpace(line[start:end])
	if start > 0 {
		s = "..." + s
	}
	if end < len(line) {
		s += "..."
	}
	return s
}

func effectiveExtensions(root config.Root, requested []string, pdfEnabled bool) []string {
	allowed := root.ContentSearch.IncludeExtensions
	var out []string
	for _, e := range allowed {
		if e == ".pdf" && !pdfEnabled {
			continue
		}
		if len(requested) == 0 || containsExt(requested, e) {
			out = append(out, e)
		}
	}
	return out
}

func containsExt(exts []string, want string) bool {
	for _, e := range exts {
		e = strings.ToLower(strings.TrimSpace(e))
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		if e == want {
			return true
		}
	}
	return false
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
