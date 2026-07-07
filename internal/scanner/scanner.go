// Package scanner walks configured roots and reconciles the metadata index.
package scanner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"super-speedy-search/internal/config"
	"super-speedy-search/internal/db"
)

const batchSize = 500

// throttle: brief pause per chunk of entries keeps sustained disk/CPU low.
const (
	throttleEvery = 2000
	throttleSleep = 20 * time.Millisecond
)

var ErrScanRunning = errors.New("a scan is already running")

// Status is a snapshot of the current or last scan, for /v1/scan/current.
type Status struct {
	ScanID       string `json:"scan_id"`
	RootID       string `json:"root_id,omitempty"`
	Running      bool   `json:"running"`
	StartedAt    int64  `json:"started_at"`
	FinishedAt   int64  `json:"finished_at,omitempty"`
	Status       string `json:"status"`
	FilesSeen    int64  `json:"files_seen"`
	FilesUpdated int64  `json:"files_updated"`
	FilesDeleted int64  `json:"files_deleted"`
	Errors       int64  `json:"errors"`
}

type Scanner struct {
	DB  *db.DB
	Cfg *config.Config
	Log *slog.Logger

	mu      sync.Mutex
	running bool
	current Status
}

func New(d *db.DB, cfg *config.Config, log *slog.Logger) *Scanner {
	return &Scanner{DB: d, Cfg: cfg, Log: log}
}

// Current returns the most recent scan status snapshot.
func (s *Scanner) Current() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

// Run scans all enabled roots, or just rootID when non-empty. Only one scan
// runs at a time; concurrent calls get ErrScanRunning.
func (s *Scanner) Run(ctx context.Context, rootID string) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return ErrScanRunning
	}
	scanID := newID()
	started := time.Now()
	s.running = true
	s.current = Status{ScanID: scanID, RootID: rootID, Running: true, StartedAt: started.Unix(), Status: "running"}
	s.mu.Unlock()

	run := db.ScanRun{ID: scanID, RootID: rootID, StartedAt: started.Unix(), Status: "running"}
	if err := s.DB.InsertScanRun(run); err != nil {
		s.finish(run, "failed")
		return err
	}

	var roots []config.Root
	if rootID != "" {
		r, ok := s.Cfg.RootByID(rootID)
		if !ok || !r.EnabledOn() {
			s.finish(run, "failed")
			return errors.New("unknown or disabled root: " + rootID)
		}
		roots = []config.Root{r}
	} else {
		roots = s.Cfg.EnabledRoots()
	}

	status := "completed"
	for _, root := range roots {
		if err := s.scanRoot(ctx, root, scanID, started, &run); err != nil {
			if ctx.Err() != nil {
				status = "cancelled"
				break
			}
			s.Log.Error("scan root failed", "root", root.ID, "err", err)
			run.Errors++
			status = "completed_with_errors"
		}
	}

	cutoff := time.Now().AddDate(0, 0, -s.Cfg.Scan.TombstoneRetentionDays).Unix()
	if n, err := s.DB.PurgeTombstones(cutoff); err != nil {
		s.Log.Error("purge tombstones failed", "err", err)
	} else if n > 0 {
		s.Log.Info("purged tombstones", "rows", n)
	}

	s.finish(run, status)
	s.Log.Info("scan finished", "scan_id", scanID, "status", status,
		"seen", run.FilesSeen, "updated", run.FilesUpdated, "deleted", run.FilesDeleted,
		"errors", run.Errors, "took", time.Since(started).Round(time.Millisecond))
	return nil
}

func (s *Scanner) finish(run db.ScanRun, status string) {
	run.Status = status
	run.FinishedAt = time.Now().Unix()
	if err := s.DB.FinishScanRun(run); err != nil {
		s.Log.Error("record scan finish failed", "err", err)
	}
	s.mu.Lock()
	s.running = false
	s.current.Running = false
	s.current.Status = status
	s.current.FinishedAt = run.FinishedAt
	s.current.FilesSeen = run.FilesSeen
	s.current.FilesUpdated = run.FilesUpdated
	s.current.FilesDeleted = run.FilesDeleted
	s.current.Errors = run.Errors
	s.mu.Unlock()
}

func (s *Scanner) scanRoot(ctx context.Context, root config.Root, scanID string, started time.Time, run *db.ScanRun) error {
	batch := make([]db.FileMeta, 0, batchSize)
	entries := 0

	flush := func() error {
		res, err := s.DB.UpsertFiles(batch, scanID, time.Now().Unix())
		if err != nil {
			return err
		}
		run.FilesUpdated += int64(res.Inserted + res.Updated)
		batch = batch[:0]
		s.mu.Lock()
		s.current.FilesSeen = run.FilesSeen
		s.current.FilesUpdated = run.FilesUpdated
		s.mu.Unlock()
		return nil
	}

	err := filepath.WalkDir(root.Path, func(path string, d fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if walkErr != nil {
			run.Errors++
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if path == root.Path {
			return nil
		}
		rel, err := filepath.Rel(root.Path, path)
		if err != nil {
			run.Errors++
			return nil
		}
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			if Excluded(rel, d.Name(), root.Excludes.Paths) {
				return fs.SkipDir
			}
		} else {
			if Excluded(rel, d.Name(), root.Excludes.Paths) {
				return nil
			}
			if extExcluded(d.Name(), root.Excludes.Extensions) {
				return nil
			}
			if !s.Cfg.Scan.FollowSymlinks && d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
		}

		meta, err := MetaFor(root.ID, rel, d)
		if err != nil {
			run.Errors++
			return nil
		}
		batch = append(batch, meta)
		run.FilesSeen++
		entries++
		if len(batch) >= batchSize {
			if err := flush(); err != nil {
				return err
			}
		}
		if entries%throttleEvery == 0 {
			select {
			case <-time.After(throttleSleep):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	if ferr := flush(); ferr != nil && err == nil {
		err = ferr
	}
	if err != nil {
		return err
	}

	deleted, err := s.DB.MarkMissing(root.ID, scanID, started.Unix(), time.Now().Unix())
	if err != nil {
		return err
	}
	run.FilesDeleted += deleted
	s.mu.Lock()
	s.current.FilesDeleted = run.FilesDeleted
	s.mu.Unlock()
	return nil
}

// MetaFor builds a FileMeta from a directory entry. Exported for the watcher.
func MetaFor(rootID, rel string, d fs.DirEntry) (db.FileMeta, error) {
	info, err := d.Info()
	if err != nil {
		return db.FileMeta{}, err
	}
	return metaFromInfo(rootID, rel, info), nil
}

func metaFromInfo(rootID, rel string, info fs.FileInfo) db.FileMeta {
	name := info.Name()
	ext := ""
	if !info.IsDir() {
		ext = strings.ToLower(filepath.Ext(name))
	}
	return db.FileMeta{
		RootID:       rootID,
		RelativePath: rel,
		Filename:     name,
		Extension:    ext,
		SizeBytes:    info.Size(),
		ModifiedAt:   info.ModTime().Unix(),
		CreatedAt:    createdAt(info),
		IsDir:        info.IsDir(),
	}
}

// MetaFromInfo is the fs.FileInfo variant of MetaFor, for the watcher.
func MetaFromInfo(rootID, rel string, info fs.FileInfo) db.FileMeta {
	return metaFromInfo(rootID, rel, info)
}

// Excluded reports whether a root-relative path should be skipped. Exclude
// entries containing "/" are treated as relative path prefixes; bare entries
// match any single path segment.
func Excluded(rel, name string, excludes []string) bool {
	for _, e := range excludes {
		e = strings.Trim(e, "/")
		if e == "" {
			continue
		}
		if strings.Contains(e, "/") {
			if rel == e || strings.HasPrefix(rel, e+"/") {
				return true
			}
			continue
		}
		if name == e {
			return true
		}
		for _, seg := range strings.Split(rel, "/") {
			if seg == e {
				return true
			}
		}
	}
	return false
}

func extExcluded(name string, exts []string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	for _, e := range exts {
		if ext == e {
			return true
		}
	}
	return false
}

func newID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return time.Now().Format("20060102t150405")
	}
	return time.Now().UTC().Format("20060102t150405") + "-" + hex.EncodeToString(buf[:4])
}
