// Package db owns the per-node SQLite database: schema, writes, and queries.
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

const schemaVersion = 1

const schema = `
CREATE TABLE IF NOT EXISTS scan_roots (
  id TEXT PRIMARY KEY,
  path TEXT NOT NULL,
  display_prefix TEXT NOT NULL,
  open_uri_prefix TEXT,
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS files (
  id INTEGER PRIMARY KEY,
  root_id TEXT NOT NULL,
  relative_path TEXT NOT NULL,
  filename TEXT NOT NULL,
  filename_lower TEXT NOT NULL,
  extension TEXT NOT NULL,
  size_bytes INTEGER NOT NULL,
  modified_at INTEGER,
  created_at INTEGER,
  indexed_at INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  last_seen_scan_id TEXT,
  is_deleted INTEGER NOT NULL DEFAULT 0,
  deleted_at INTEGER,
  is_dir INTEGER NOT NULL DEFAULT 0,
  error TEXT,
  UNIQUE(root_id, relative_path)
);

CREATE INDEX IF NOT EXISTS idx_files_filename_lower ON files(filename_lower) WHERE is_deleted = 0;
CREATE INDEX IF NOT EXISTS idx_files_extension ON files(extension) WHERE is_deleted = 0;
CREATE INDEX IF NOT EXISTS idx_files_root_id ON files(root_id) WHERE is_deleted = 0;
CREATE INDEX IF NOT EXISTS idx_files_modified_at ON files(modified_at) WHERE is_deleted = 0;
CREATE INDEX IF NOT EXISTS idx_files_last_seen_scan_id ON files(last_seen_scan_id);

CREATE TABLE IF NOT EXISTS scan_runs (
  id TEXT PRIMARY KEY,
  root_id TEXT,
  started_at INTEGER NOT NULL,
  finished_at INTEGER,
  status TEXT NOT NULL,
  files_seen INTEGER NOT NULL DEFAULT 0,
  files_updated INTEGER NOT NULL DEFAULT 0,
  files_deleted INTEGER NOT NULL DEFAULT 0,
  errors INTEGER NOT NULL DEFAULT 0
);
`

// DB wraps database/sql with a write mutex: SQLite allows one writer at a
// time, and serializing writes in-process avoids SQLITE_BUSY churn between
// the scanner and the watcher.
type DB struct {
	SQL     *sql.DB
	writeMu sync.Mutex
}

func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(4)
	if _, err := sqlDB.Exec(schema); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if _, err := sqlDB.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return &DB{SQL: sqlDB}, nil
}

func (d *DB) Close() error { return d.SQL.Close() }

// RootRow mirrors a configured scan root persisted for reference.
type RootRow struct {
	ID            string
	Path          string
	DisplayPrefix string
	OpenURIPrefix string
	Enabled       bool
}

// SyncRoots upserts configured roots and tombstones files belonging to roots
// that are no longer configured.
func (d *DB) SyncRoots(roots []RootRow, now int64) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	tx, err := d.SQL.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	ids := make([]string, 0, len(roots))
	for _, r := range roots {
		ids = append(ids, r.ID)
		_, err := tx.Exec(`
			INSERT INTO scan_roots (id, path, display_prefix, open_uri_prefix, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
			  path = excluded.path,
			  display_prefix = excluded.display_prefix,
			  open_uri_prefix = excluded.open_uri_prefix,
			  enabled = excluded.enabled,
			  updated_at = excluded.updated_at`,
			r.ID, r.Path, r.DisplayPrefix, r.OpenURIPrefix, boolInt(r.Enabled), now, now)
		if err != nil {
			return err
		}
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	if _, err := tx.Exec(`DELETE FROM scan_roots WHERE id NOT IN (`+placeholders+`)`, args...); err != nil {
		return err
	}
	orphanArgs := append([]any{now}, args...)
	if _, err := tx.Exec(`UPDATE files SET is_deleted = 1, deleted_at = ? WHERE is_deleted = 0 AND root_id NOT IN (`+placeholders+`)`, orphanArgs...); err != nil {
		return err
	}
	return tx.Commit()
}

// FileMeta is one filesystem entry observed by the scanner or watcher.
type FileMeta struct {
	RootID       string
	RelativePath string
	Filename     string
	Extension    string
	SizeBytes    int64
	ModifiedAt   int64
	CreatedAt    int64 // 0 when the platform cannot provide it
	IsDir        bool
}

// UpsertResult reports what a batch write changed.
type UpsertResult struct {
	Inserted int
	Updated  int
}

// UpsertFiles writes a batch of observed entries in one transaction. scanID
// may be empty for watcher-originated writes; then last_seen_scan_id is left
// alone so the next reconciliation scan claims the row normally.
func (d *DB) UpsertFiles(metas []FileMeta, scanID string, now int64) (UpsertResult, error) {
	var res UpsertResult
	if len(metas) == 0 {
		return res, nil
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	tx, err := d.SQL.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	sel, err := tx.Prepare(`SELECT id, size_bytes, modified_at, is_deleted FROM files WHERE root_id = ? AND relative_path = ?`)
	if err != nil {
		return res, err
	}
	ins, err := tx.Prepare(`
		INSERT INTO files (root_id, relative_path, filename, filename_lower, extension,
		  size_bytes, modified_at, created_at, indexed_at, last_seen_at, last_seen_scan_id, is_deleted, is_dir, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, NULL)`)
	if err != nil {
		return res, err
	}
	upd, err := tx.Prepare(`
		UPDATE files SET filename = ?, filename_lower = ?, extension = ?, size_bytes = ?,
		  modified_at = ?, created_at = ?, indexed_at = ?, last_seen_at = ?,
		  last_seen_scan_id = COALESCE(?, last_seen_scan_id),
		  is_deleted = 0, deleted_at = NULL, is_dir = ?, error = NULL
		WHERE id = ?`)
	if err != nil {
		return res, err
	}
	touch, err := tx.Prepare(`
		UPDATE files SET last_seen_at = ?, last_seen_scan_id = COALESCE(?, last_seen_scan_id)
		WHERE id = ?`)
	if err != nil {
		return res, err
	}

	var scanIDArg any
	if scanID != "" {
		scanIDArg = scanID
	}
	for _, m := range metas {
		lower := strings.ToLower(m.Filename)
		var (
			id        int64
			size      int64
			modified  sql.NullInt64
			isDeleted int
		)
		err := sel.QueryRow(m.RootID, m.RelativePath).Scan(&id, &size, &modified, &isDeleted)
		switch {
		case err == sql.ErrNoRows:
			if _, err := ins.Exec(m.RootID, m.RelativePath, m.Filename, lower, m.Extension,
				m.SizeBytes, nullInt(m.ModifiedAt), nullInt(m.CreatedAt), now, now, scanIDArg, boolInt(m.IsDir)); err != nil {
				return res, err
			}
			res.Inserted++
		case err != nil:
			return res, err
		case size != m.SizeBytes || modified.Int64 != m.ModifiedAt || isDeleted != 0:
			if _, err := upd.Exec(m.Filename, lower, m.Extension, m.SizeBytes,
				nullInt(m.ModifiedAt), nullInt(m.CreatedAt), now, now, scanIDArg, boolInt(m.IsDir), id); err != nil {
				return res, err
			}
			res.Updated++
		default:
			if _, err := touch.Exec(now, scanIDArg, id); err != nil {
				return res, err
			}
		}
	}
	return res, tx.Commit()
}

// MarkMissing tombstones rows in a root that the scan identified by scanID
// did not touch. The last_seen_at guard protects rows written by the watcher
// while the scan was running; <= (not <) because timestamps have second
// granularity and a scan can start in the same second the previous one
// touched rows. A watcher write in the exact start second may be tombstoned
// spuriously, which self-heals on the next event or scan.
func (d *DB) MarkMissing(rootID, scanID string, scanStartedAt, now int64) (int64, error) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	r, err := d.SQL.Exec(`
		UPDATE files SET is_deleted = 1, deleted_at = ?
		WHERE root_id = ? AND is_deleted = 0
		  AND (last_seen_scan_id IS NULL OR last_seen_scan_id <> ?)
		  AND last_seen_at <= ?`,
		now, rootID, scanID, scanStartedAt)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// MarkDeletedByPath tombstones a single path and everything under it.
// Used by the watcher for remove/rename events.
func (d *DB) MarkDeletedByPath(rootID, relPath string, now int64) (int64, error) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	r, err := d.SQL.Exec(`
		UPDATE files SET is_deleted = 1, deleted_at = ?
		WHERE root_id = ? AND is_deleted = 0
		  AND (relative_path = ? OR relative_path LIKE ? ESCAPE '\')`,
		now, rootID, relPath, EscapeLike(relPath)+`/%`)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// PurgeTombstones deletes rows tombstoned before the cutoff.
func (d *DB) PurgeTombstones(deletedBefore int64) (int64, error) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	r, err := d.SQL.Exec(`DELETE FROM files WHERE is_deleted = 1 AND deleted_at < ?`, deletedBefore)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

func (d *DB) CountFiles() (int64, error) {
	var n int64
	err := d.SQL.QueryRow(`SELECT COUNT(*) FROM files WHERE is_deleted = 0`).Scan(&n)
	return n, err
}

// ScanRun is one row of scan history.
type ScanRun struct {
	ID           string `json:"id"`
	RootID       string `json:"root_id,omitempty"`
	StartedAt    int64  `json:"started_at"`
	FinishedAt   int64  `json:"finished_at,omitempty"`
	Status       string `json:"status"`
	FilesSeen    int64  `json:"files_seen"`
	FilesUpdated int64  `json:"files_updated"`
	FilesDeleted int64  `json:"files_deleted"`
	Errors       int64  `json:"errors"`
}

func (d *DB) InsertScanRun(run ScanRun) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_, err := d.SQL.Exec(`INSERT INTO scan_runs (id, root_id, started_at, status) VALUES (?, ?, ?, ?)`,
		run.ID, nullStr(run.RootID), run.StartedAt, run.Status)
	return err
}

func (d *DB) FinishScanRun(run ScanRun) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_, err := d.SQL.Exec(`
		UPDATE scan_runs SET finished_at = ?, status = ?, files_seen = ?, files_updated = ?, files_deleted = ?, errors = ?
		WHERE id = ?`,
		run.FinishedAt, run.Status, run.FilesSeen, run.FilesUpdated, run.FilesDeleted, run.Errors, run.ID)
	return err
}

func (d *DB) ScanHistory(limit int) ([]ScanRun, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := d.SQL.Query(`
		SELECT id, COALESCE(root_id, ''), started_at, COALESCE(finished_at, 0), status,
		       files_seen, files_updated, files_deleted, errors
		FROM scan_runs ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScanRun
	for rows.Next() {
		var r ScanRun
		if err := rows.Scan(&r.ID, &r.RootID, &r.StartedAt, &r.FinishedAt, &r.Status,
			&r.FilesSeen, &r.FilesUpdated, &r.FilesDeleted, &r.Errors); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastScanFinishedAt returns the newest finished_at across completed runs.
func (d *DB) LastScanFinishedAt() (int64, error) {
	var v sql.NullInt64
	err := d.SQL.QueryRow(`SELECT MAX(finished_at) FROM scan_runs WHERE status = 'completed'`).Scan(&v)
	return v.Int64, err
}

// FileRow is a metadata search hit before ranking and URI assembly.
type FileRow struct {
	RootID        string
	RelativePath  string
	Filename      string
	FilenameLower string
	Extension     string
	SizeBytes     int64
	ModifiedAt    int64
	IsDir         bool
}

// SearchFilter narrows a metadata search. Terms are matched with AND
// semantics against filename or relative path.
type SearchFilter struct {
	Terms       []string
	Extensions  []string
	RootIDs     []string
	IncludeDirs bool
	Cap         int // maximum candidates fetched before ranking
}

func (d *DB) SearchMetadata(f SearchFilter) ([]FileRow, error) {
	if f.Cap <= 0 {
		f.Cap = 2000
	}
	var (
		where []string
		args  []any
	)
	where = append(where, "is_deleted = 0")
	if !f.IncludeDirs {
		where = append(where, "is_dir = 0")
	}
	for _, t := range f.Terms {
		where = append(where, `(filename_lower LIKE ? ESCAPE '\' OR LOWER(relative_path) LIKE ? ESCAPE '\')`)
		pat := "%" + EscapeLike(strings.ToLower(t)) + "%"
		args = append(args, pat, pat)
	}
	if len(f.Extensions) > 0 {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(f.Extensions)), ",")
		where = append(where, "extension IN ("+ph+")")
		for _, e := range f.Extensions {
			args = append(args, strings.ToLower(e))
		}
	}
	if len(f.RootIDs) > 0 {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(f.RootIDs)), ",")
		where = append(where, "root_id IN ("+ph+")")
		for _, r := range f.RootIDs {
			args = append(args, r)
		}
	}
	args = append(args, f.Cap)
	rows, err := d.SQL.Query(`
		SELECT root_id, relative_path, filename, filename_lower, extension, size_bytes,
		       COALESCE(modified_at, 0), is_dir
		FROM files WHERE `+strings.Join(where, " AND ")+`
		ORDER BY modified_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileRow
	for rows.Next() {
		var r FileRow
		var isDir int
		if err := rows.Scan(&r.RootID, &r.RelativePath, &r.Filename, &r.FilenameLower,
			&r.Extension, &r.SizeBytes, &r.ModifiedAt, &isDir); err != nil {
			return nil, err
		}
		r.IsDir = isDir != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// Candidate is a file eligible for live content search.
type Candidate struct {
	RootID       string
	RelativePath string
	Filename     string
	Extension    string
	SizeBytes    int64
}

// ContentCandidates streams non-deleted files in a root matching the
// extension allowlist and size cap, ordered by recency.
func (d *DB) ContentCandidates(rootID string, extensions []string, maxSizeBytes int64, fn func(Candidate) error) error {
	if len(extensions) == 0 {
		return nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(extensions)), ",")
	args := []any{rootID, maxSizeBytes}
	for _, e := range extensions {
		args = append(args, strings.ToLower(e))
	}
	rows, err := d.SQL.Query(`
		SELECT root_id, relative_path, filename, extension, size_bytes
		FROM files
		WHERE root_id = ? AND is_deleted = 0 AND is_dir = 0 AND size_bytes <= ?
		  AND extension IN (`+ph+`)
		ORDER BY modified_at DESC`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(&c.RootID, &c.RelativePath, &c.Filename, &c.Extension, &c.SizeBytes); err != nil {
			return err
		}
		if err := fn(c); err != nil {
			return err
		}
	}
	return rows.Err()
}

// EscapeLike escapes %, _ and \ for use in a LIKE pattern with ESCAPE '\'.
func EscapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
