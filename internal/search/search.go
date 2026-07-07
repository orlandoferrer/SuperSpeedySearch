// Package search implements metadata search semantics: term matching,
// ranking, and assembly of display paths and open URIs from root config.
package search

import (
	"errors"
	"net/url"
	"path"
	"strings"
	"time"

	"super-speedy-search/internal/config"
	"super-speedy-search/internal/db"
)

type Params struct {
	Query       string   `json:"query"`
	Extensions  []string `json:"extensions,omitempty"`
	RootIDs     []string `json:"root_ids,omitempty"`
	Limit       int      `json:"limit,omitempty"`
	IncludeDirs bool     `json:"include_dirs,omitempty"`
}

// Result carries raw match signals (MatchType, ModifiedAt) so a GUI can rank
// results from several nodes globally.
type Result struct {
	NodeID        string `json:"node_id"`
	RootID        string `json:"root_id"`
	Path          string `json:"path"`
	RelativePath  string `json:"relative_path"`
	DisplayPath   string `json:"display_path"`
	OpenURI       string `json:"open_uri,omitempty"`
	ParentOpenURI string `json:"parent_open_uri,omitempty"`
	Filename      string `json:"filename"`
	Extension     string `json:"extension,omitempty"`
	SizeBytes     int64  `json:"size_bytes"`
	ModifiedAt    string `json:"modified_at,omitempty"`
	IsDir         bool   `json:"is_dir,omitempty"`
	MatchType     string `json:"match_type"`
}

const (
	MatchFilenameExact = "filename_exact"
	MatchFilename      = "filename"
	MatchPath          = "path"
)

type Searcher struct {
	DB     *db.DB
	Cfg    *config.Config
	NodeID string
}

func (s *Searcher) Search(p Params) ([]Result, error) {
	query := strings.TrimSpace(p.Query)
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return nil, errors.New("query is required")
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > s.Cfg.ResourceLimits.MaxResultsPerQuery {
		limit = s.Cfg.ResourceLimits.MaxResultsPerQuery
	}

	// Ranking happens in SQL (ORDER BY match class, then recency) so the row
	// cap cannot evict a better match — see db.SearchMetadata.
	rows, err := s.DB.SearchMetadata(db.SearchFilter{
		Query:       query,
		Terms:       terms,
		Extensions:  normalizeExts(p.Extensions),
		RootIDs:     p.RootIDs,
		IncludeDirs: p.IncludeDirs,
		Cap:         limit,
	})
	if err != nil {
		return nil, err
	}

	roots := map[string]config.Root{}
	for _, r := range s.Cfg.Roots {
		roots[r.ID] = r
	}
	out := make([]Result, 0, len(rows))
	for _, r := range rows {
		out = append(out, s.assemble(r, roots[r.RootID], matchTypeForScore(r.Score)))
	}
	return out, nil
}

func matchTypeForScore(score int) string {
	switch score {
	case 3:
		return MatchFilenameExact
	case 2:
		return MatchFilename
	default:
		return MatchPath
	}
}

func (s *Searcher) assemble(row db.FileRow, root config.Root, matchType string) Result {
	res := Result{
		NodeID:       s.NodeID,
		RootID:       row.RootID,
		Path:         path.Join(root.Path, row.RelativePath),
		RelativePath: row.RelativePath,
		DisplayPath:  joinDisplay(root.DisplayPrefix, row.RelativePath),
		Filename:     row.Filename,
		Extension:    row.Extension,
		SizeBytes:    row.SizeBytes,
		IsDir:        row.IsDir,
		MatchType:    matchType,
	}
	if row.ModifiedAt > 0 {
		res.ModifiedAt = time.Unix(row.ModifiedAt, 0).UTC().Format(time.RFC3339)
	}
	if root.OpenURIPrefix != "" {
		res.OpenURI = JoinOpenURI(root.OpenURIPrefix, row.RelativePath)
		if parent := path.Dir(row.RelativePath); parent == "." {
			res.ParentOpenURI = root.OpenURIPrefix
		} else {
			res.ParentOpenURI = JoinOpenURI(root.OpenURIPrefix, parent)
		}
	}
	return res
}

func joinDisplay(prefix, rel string) string {
	if prefix == "" {
		return rel
	}
	return strings.TrimSuffix(prefix, "/") + "/" + rel
}

// JoinOpenURI appends a root-relative path to a URI prefix, percent-escaping
// each segment so spaces and special characters survive.
func JoinOpenURI(prefix, rel string) string {
	segs := strings.Split(rel, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.TrimSuffix(prefix, "/") + "/" + strings.Join(segs, "/")
}

func normalizeExts(exts []string) []string {
	out := make([]string, 0, len(exts))
	for _, e := range exts {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		out = append(out, e)
	}
	return out
}
