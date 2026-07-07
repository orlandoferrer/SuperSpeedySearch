// Package config loads and validates the per-node YAML configuration.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Node           Node           `yaml:"node" json:"node"`
	Database       Database       `yaml:"database" json:"database"`
	Scan           Scan           `yaml:"scan" json:"scan"`
	Roots          []Root         `yaml:"roots" json:"roots"`
	Content        Content        `yaml:"content" json:"content"`
	ResourceLimits ResourceLimits `yaml:"resource_limits" json:"resource_limits"`
}

type Node struct {
	ID           string `yaml:"id" json:"id"`
	Name         string `yaml:"name" json:"name"`
	ListenAddr   string `yaml:"listen_addr" json:"listen_addr"`
	Advertise    *bool  `yaml:"advertise" json:"advertise"`
	AuthRequired *bool  `yaml:"auth_required" json:"auth_required"`
	AuthToken    string `yaml:"auth_token" json:"-"`
}

func (n Node) AdvertiseOn() bool    { return n.Advertise == nil || *n.Advertise }
func (n Node) AuthRequiredOn() bool { return n.AuthRequired == nil || *n.AuthRequired }

type Database struct {
	Path string `yaml:"path" json:"path"`
}

type Scan struct {
	Interval               Duration `yaml:"interval" json:"interval"`
	WorkerCount            int      `yaml:"worker_count" json:"worker_count"`
	FollowSymlinks         bool     `yaml:"follow_symlinks" json:"follow_symlinks"`
	TombstoneRetentionDays int      `yaml:"tombstone_retention_days" json:"tombstone_retention_days"`
	Watch                  Watch    `yaml:"watch" json:"watch"`
}

type Watch struct {
	Enabled        *bool `yaml:"enabled" json:"enabled"`
	MaxWatchedDirs int   `yaml:"max_watched_dirs" json:"max_watched_dirs"`
	DebounceMs     int   `yaml:"debounce_ms" json:"debounce_ms"`
}

func (w Watch) EnabledOn() bool { return w.Enabled == nil || *w.Enabled }

type Root struct {
	ID            string        `yaml:"id" json:"id"`
	Path          string        `yaml:"path" json:"path"`
	DisplayPrefix string        `yaml:"display_prefix" json:"display_prefix"`
	OpenURIPrefix string        `yaml:"open_uri_prefix" json:"open_uri_prefix"`
	Enabled       *bool         `yaml:"enabled" json:"enabled"`
	Excludes      Excludes      `yaml:"excludes" json:"excludes"`
	ContentSearch ContentSearch `yaml:"content_search" json:"content_search"`
}

func (r Root) EnabledOn() bool { return r.Enabled == nil || *r.Enabled }

type Excludes struct {
	// Paths entries are either bare names matched against any path segment
	// (".git", "node_modules") or root-relative path prefixes ("sub/dir").
	Paths      []string `yaml:"paths" json:"paths"`
	Extensions []string `yaml:"extensions" json:"extensions"`
}

type ContentSearch struct {
	Enabled           bool     `yaml:"enabled" json:"enabled"`
	MaxFileSizeMB     int      `yaml:"max_file_size_mb" json:"max_file_size_mb"`
	IncludeExtensions []string `yaml:"include_extensions" json:"include_extensions"`
}

type Content struct {
	PDF PDF `yaml:"pdf" json:"pdf"`
}

type PDF struct {
	Enabled       bool   `yaml:"enabled" json:"enabled"`
	PdftotextPath string `yaml:"pdftotext_path" json:"pdftotext_path"`
}

type ResourceLimits struct {
	MaxParallelContentSearches int `yaml:"max_parallel_content_searches" json:"max_parallel_content_searches"`
	MaxSearchSeconds           int `yaml:"max_search_seconds" json:"max_search_seconds"`
	MaxResultsPerQuery         int `yaml:"max_results_per_query" json:"max_results_per_query"`
}

// Duration wraps time.Duration for YAML strings like "6h" or "90s".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	dd, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(dd)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Duration(d).String() + `"`), nil
}

// DefaultExcludedNames are directory names skipped in every root unless the
// root config overrides excludes entirely.
var DefaultExcludedNames = []string{
	".git", "node_modules", ".Trash", "#recycle", "@eaDir", ".cache",
	"__pycache__", ".venv", "Library/Caches",
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

func Parse(raw []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.applyDefaults(); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() error {
	if c.Node.ID == "" {
		host, err := os.Hostname()
		if err != nil || host == "" {
			host = "node"
		}
		c.Node.ID = sanitizeID(host)
	}
	if c.Node.Name == "" {
		c.Node.Name = c.Node.ID
	}
	if c.Node.ListenAddr == "" {
		c.Node.ListenAddr = "0.0.0.0:37373"
	}
	if c.Database.Path == "" {
		c.Database.Path = "data/index.db"
	}
	p, err := expandPath(c.Database.Path)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(p) {
		if p, err = filepath.Abs(p); err != nil {
			return err
		}
	}
	c.Database.Path = p

	if c.Scan.Interval == 0 {
		c.Scan.Interval = Duration(6 * time.Hour)
	}
	if c.Scan.WorkerCount <= 0 {
		c.Scan.WorkerCount = 2
	}
	if c.Scan.TombstoneRetentionDays <= 0 {
		c.Scan.TombstoneRetentionDays = 30
	}
	if c.Scan.Watch.MaxWatchedDirs <= 0 {
		c.Scan.Watch.MaxWatchedDirs = 50000
	}
	if c.Scan.Watch.DebounceMs <= 0 {
		c.Scan.Watch.DebounceMs = 500
	}

	if c.ResourceLimits.MaxParallelContentSearches <= 0 {
		c.ResourceLimits.MaxParallelContentSearches = 2
	}
	if c.ResourceLimits.MaxSearchSeconds <= 0 {
		c.ResourceLimits.MaxSearchSeconds = 60
	}
	if c.ResourceLimits.MaxResultsPerQuery <= 0 {
		c.ResourceLimits.MaxResultsPerQuery = 500
	}

	for i := range c.Roots {
		r := &c.Roots[i]
		rp, err := expandPath(r.Path)
		if err != nil {
			return err
		}
		r.Path = filepath.Clean(rp)
		if r.DisplayPrefix == "" {
			r.DisplayPrefix = c.Node.Name + ":" + r.ID
		}
		if r.ContentSearch.MaxFileSizeMB <= 0 {
			r.ContentSearch.MaxFileSizeMB = 25
		}
		if len(r.Excludes.Paths) == 0 {
			r.Excludes.Paths = append([]string(nil), DefaultExcludedNames...)
		}
		for j, e := range r.Excludes.Extensions {
			r.Excludes.Extensions[j] = normalizeExt(e)
		}
		for j, e := range r.ContentSearch.IncludeExtensions {
			r.ContentSearch.IncludeExtensions[j] = normalizeExt(e)
		}
	}
	return nil
}

func (c *Config) validate() error {
	if len(c.Roots) == 0 {
		return errors.New("config: at least one root is required")
	}
	seen := map[string]bool{}
	for _, r := range c.Roots {
		if r.ID == "" {
			return errors.New("config: every root needs an id")
		}
		if seen[r.ID] {
			return fmt.Errorf("config: duplicate root id %q", r.ID)
		}
		seen[r.ID] = true
		if r.Path == "" || !filepath.IsAbs(r.Path) {
			return fmt.Errorf("config: root %q path must be absolute, got %q", r.ID, r.Path)
		}
	}
	if time.Duration(c.Scan.Interval) < time.Minute {
		return fmt.Errorf("config: scan interval must be at least 1m, got %s", time.Duration(c.Scan.Interval))
	}
	return nil
}

// EnsureAuthToken returns the effective auth token, generating and persisting
// one next to the database on first run when auth is required but no token is
// configured. Returns ("", false) when auth is disabled.
func (c *Config) EnsureAuthToken() (token string, generated bool, err error) {
	if !c.Node.AuthRequiredOn() {
		return "", false, nil
	}
	if c.Node.AuthToken != "" {
		return c.Node.AuthToken, false, nil
	}
	tokenPath := filepath.Join(filepath.Dir(c.Database.Path), "auth_token")
	if raw, err := os.ReadFile(tokenPath); err == nil {
		t := strings.TrimSpace(string(raw))
		if t != "" {
			c.Node.AuthToken = t
			return t, false, nil
		}
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", false, err
	}
	t := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o755); err != nil {
		return "", false, err
	}
	if err := os.WriteFile(tokenPath, []byte(t+"\n"), 0o600); err != nil {
		return "", false, err
	}
	c.Node.AuthToken = t
	return t, true, nil
}

// TokenPath is where a generated token is persisted.
func (c *Config) TokenPath() string {
	return filepath.Join(filepath.Dir(c.Database.Path), "auth_token")
}

// RootByID returns the configured root with the given id.
func (c *Config) RootByID(id string) (Root, bool) {
	for _, r := range c.Roots {
		if r.ID == id {
			return r, true
		}
	}
	return Root{}, false
}

// EnabledRoots returns roots with enabled != false.
func (c *Config) EnabledRoots() []Root {
	var out []Root
	for _, r := range c.Roots {
		if r.EnabledOn() {
			out = append(out, r)
		}
	}
	return out
}

func expandPath(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
	}
	return p, nil
}

func normalizeExt(e string) string {
	e = strings.ToLower(strings.TrimSpace(e))
	if e != "" && !strings.HasPrefix(e, ".") {
		e = "." + e
	}
	return e
}

func sanitizeID(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == '.', r == ' ':
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "node"
	}
	return b.String()
}
