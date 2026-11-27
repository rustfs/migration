package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rustfs/migration/internal/config"
	"modernc.org/sqlite"
)

const (
	StatusPending  = "pending"
	StatusDone     = "done"
	StatusConflict = "conflict"
	StatusSkipped  = "skipped"
	StatusFailed   = "failed"
)

type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
	ETag         string
	Status       string
}

type Store struct {
	db        *sql.DB
	lock      *sql.DB
	closeOnce sync.Once
	closeErr  error
}

func Open(path, bucket string) (*Store, error) {
	return open(path, bucket, "rwc")
}

func open(path, bucket, mode string) (*Store, error) {
	if bucket == "" {
		return nil, fmt.Errorf("store: bucket must not be empty")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir %s: %w", dir, err)
		}
	}
	// File aliases must share a task lock as well as SQLite's data locks.
	if canonical, err := filepath.EvalSymlinks(path); err == nil {
		path = canonical
	} else if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		path = filepath.Join(dir, filepath.Base(path))
	}
	// SQLite otherwise creates files according to the process umask, which can
	// expose object names and errors to other local users. Preserve permissions
	// on existing databases, which may intentionally be shared.
	for _, name := range []string{path + ".lock", path} {
		f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil && !os.IsExist(err) {
			return nil, fmt.Errorf("create state file %s: %w", name, err)
		}
		if err == nil {
			if err := f.Close(); err != nil {
				return nil, fmt.Errorf("close state file %s: %w", name, err)
			}
		}
	}
	// A separate SQLite pager lock is held for the whole writer lifetime. It
	// protects the migration task, not just individual status transactions,
	// and the OS releases it automatically if the process crashes.
	lock, err := sql.Open("sqlite", SQLiteURI(path+".lock", "rwc", "busy_timeout(0)"))
	if err != nil {
		return nil, err
	}
	tuneSQLite(lock)
	if _, err := lock.Exec(`BEGIN EXCLUSIVE`); err != nil {
		lock.Close()
		var sqliteErr *sqlite.Error
		if errors.As(err, &sqliteErr) && (sqliteErr.Code()&0xff == 5 || sqliteErr.Code()&0xff == 6) {
			return nil, fmt.Errorf("state DB %s is in use by another writer: %w", path, err)
		}
		return nil, fmt.Errorf("acquire state DB lock %s: %w", path, err)
	}
	unlock := func() { _, _ = lock.Exec(`ROLLBACK`); _ = lock.Close() }
	db, err := sql.Open("sqlite", SQLiteURI(path, mode, "journal_mode(WAL)", "synchronous(FULL)", "busy_timeout(5000)"))
	if err != nil {
		unlock()
		return nil, err
	}
	tuneSQLite(db)
	var existingSchema int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='objects'`).Scan(&existingSchema); err != nil {
		db.Close()
		unlock()
		return nil, fmt.Errorf("inspect schema: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		unlock()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	if err := ensureSeenGenerationColumn(db); err != nil {
		db.Close()
		unlock()
		return nil, fmt.Errorf("upgrade schema: %w", err)
	}
	if existingSchema == 0 {
		if _, err := db.Exec(`INSERT INTO meta(key,value) VALUES('state_format','2')`); err != nil {
			db.Close()
			unlock()
			return nil, fmt.Errorf("initialize state format: %w", err)
		}
	}
	if err := ensureBucket(db, bucket); err != nil {
		db.Close()
		unlock()
		return nil, err
	}
	return &Store{db: db, lock: lock}, nil
}

func ensureSeenGenerationColumn(db *sql.DB) error {
	var hasCol int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('objects') WHERE name='seen_generation'`).Scan(&hasCol); err != nil {
		return err
	}
	if hasCol == 0 {
		if _, err := db.Exec(`ALTER TABLE objects ADD COLUMN seen_generation INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	return nil
}

// SQLiteURI builds a file: URI that safely escapes special characters (#, ?, %, spaces)
// and handles Windows drive letters and UNC paths.
func SQLiteURI(path, mode string, pragmas ...string) string {
	return sqliteURIWithOptions(path, mode, false, pragmas...)
}

func sqliteURIWithOptions(path, mode string, immutable bool, pragmas ...string) string {
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	u := url.URL{Scheme: "file", Path: sqliteURIPath(filepath.ToSlash(path), runtime.GOOS == "windows")}
	q := url.Values{"mode": {mode}}
	if immutable {
		q.Set("immutable", "1")
	}
	for _, pragma := range pragmas {
		q.Add("_pragma", pragma)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// Drive-letter paths need an absolute URI path. Without the leading slash,
// net/url formats file://C:/... with C: as the authority, not the path.
// UNC paths already start with // and must retain both leading slashes.
func sqliteURIPath(path string, windows bool) string {
	if windows && len(path) >= 2 && path[1] == ':' && !strings.HasPrefix(path, "/") {
		return "/" + path
	}
	return path
}

func ensureBucket(db *sql.DB, bucket string) error {
	var existing string
	err := db.QueryRow(`SELECT value FROM meta WHERE key = 'bucket'`).Scan(&existing)
	switch {
	case err == sql.ErrNoRows:
		_, err := db.Exec(`INSERT INTO meta(key, value) VALUES('bucket', ?)`, bucket)
		return err
	case err != nil:
		return fmt.Errorf("read bucket meta: %w", err)
	case existing != bucket:
		return fmt.Errorf("db_path is bound to bucket %q but config requests %q; use a separate db_path per bucket", existing, bucket)
	}
	return nil
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.db.Close()
		if s.lock != nil {
			_, _ = s.lock.Exec(`ROLLBACK`)
			if err := s.lock.Close(); s.closeErr == nil {
				s.closeErr = err
			}
		}
	})
	return s.closeErr
}

// tuneSQLite keeps a single connection. WAL still allows a writer plus
// readers, but database/sql would otherwise open one connection per worker.
// Those connections just queue on SQLite's single writer and surface as
// "database is locked" once --workers is raised. Status updates stay
// serialized here; object bytes stay on the S3 worker pool.
func tuneSQLite(db *sql.DB) {
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
}

// OpenExisting opens a state DB read-only without taking the writer lock.
// If the parent directory or snapshot is read-only after a clean writer close
// (so WAL header flags remain set but no uncheckpointed -wal exists), it falls
// back to immutable read-only mode instead of failing to create -shm.
func OpenExisting(path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("open existing %s: %w", path, err)
	}
	db, err := sql.Open("sqlite", SQLiteURI(path, "ro", "busy_timeout(5000)"))
	if err != nil {
		return nil, err
	}
	tuneSQLite(db)
	s := &Store{db: db}
	if _, err := s.BoundBucket(); err != nil {
		db.Close()
		if isReadOnlyWALInitError(err) && hasNoUncheckpointedWAL(path) {
			immDB, immErr := sql.Open("sqlite", sqliteURIWithOptions(path, "ro", true))
			if immErr == nil {
				tuneSQLite(immDB)
				immStore := &Store{db: immDB}
				if _, bErr := immStore.BoundBucket(); bErr == nil {
					return immStore, nil
				}
				immDB.Close()
			}
		}
		return nil, fmt.Errorf("open existing %s: %w", path, err)
	}
	return s, nil
}

func isReadOnlyWALInitError(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	primary := sqliteErr.Code() & 0xff
	// SQLITE_READONLY (8, e.g. SQLITE_READONLY_CANTINIT=1544) or SQLITE_CANTOPEN (14).
	return primary == 8 || primary == 14
}

func hasNoUncheckpointedWAL(path string) bool {
	info, err := os.Stat(path + "-wal")
	if os.IsNotExist(err) {
		return true
	}
	return err == nil && info.Size() == 0
}

// BindMigration prevents a checkpoint being reused against another source or
// target. A source-only scan may omit target and bind it on the first copy.
// Credentials are deliberately not persisted in the state database.
func (s *Store) BindMigration(source, target string) error {
	source, target = normalizeEndpoint(source), normalizeEndpoint(target)
	if source == "" {
		return fmt.Errorf("migration source identity is required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var boundSource, boundTarget, format string
	read := func(key string, dst *string) error {
		err := tx.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(dst)
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	if err := read("source_endpoint", &boundSource); err != nil {
		return err
	}
	if err := read("target_endpoint", &boundTarget); err != nil {
		return err
	}
	if boundSource != "" && boundSource != source {
		return fmt.Errorf("state DB source mismatch: bound to %q, requested %q; use a new state directory", boundSource, source)
	}
	if target != "" && boundTarget != "" && boundTarget != target {
		return fmt.Errorf("state DB target mismatch: bound to %q, requested %q; use a new state directory", boundTarget, target)
	}
	if boundSource == "" || (target != "" && boundTarget == "") {
		if err := read("state_format", &format); err != nil {
			return err
		}
		var unsafeRows int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM objects WHERE ? != '2' OR attempts > 0 OR status != 'pending'`, format).Scan(&unsafeRows); err != nil {
			return err
		}
		if unsafeRows > 0 {
			return fmt.Errorf("unbound legacy or previously migrated state cannot be adopted safely; use a new state directory and rescan")
		}
		if format != "2" {
			if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('state_format','2') ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('source_endpoint',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, source); err != nil {
		return err
	}
	if target != "" {
		if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('target_endpoint',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, target); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func normalizeEndpoint(endpoint string) string {
	return config.NormalizeEndpoint(endpoint)
}

// BeginScan invalidates the previous completeness certificate before any
// network request. An interrupted scan must never authorize migration.
func (s *Store) BeginScan() error {
	_, err := s.db.Exec(`INSERT INTO meta(key,value) VALUES('scan_generation','1') ON CONFLICT(key) DO UPDATE SET value=CAST(value AS INTEGER)+1`)
	return err
}

func (s *Store) CompleteScan() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var gen int64
	err = tx.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key='scan_generation'`).Scan(&gen)
	if err == sql.ErrNoRows || (err == nil && gen <= 0) {
		return fmt.Errorf("cannot complete a scan that was never started")
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM objects WHERE seen_generation < ?`, gen); err != nil {
		return fmt.Errorf("prune stale inventory rows: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('scan_complete_generation',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, strconv.FormatInt(gen, 10)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RequireCompleteScan() error {
	var complete int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM meta current JOIN meta completed ON completed.key='scan_complete_generation' AND completed.value=current.value WHERE current.key='scan_generation' AND CAST(current.value AS INTEGER)>0`).Scan(&complete)
	if err != nil {
		return err
	}
	if complete != 1 {
		return fmt.Errorf("state DB has no complete inventory; run scan or run successfully before migrate")
	}
	return nil
}

func (s *Store) BoundBucket() (string, error) {
	var name string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'bucket'`).Scan(&name)
	return name, err
}

const schema = `
CREATE TABLE IF NOT EXISTS objects (
    key             TEXT PRIMARY KEY,
    size            INTEGER NOT NULL,
    last_modified   INTEGER NOT NULL,
    etag            TEXT,
    status          TEXT NOT NULL DEFAULT 'pending',
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT,
    updated_at      INTEGER NOT NULL DEFAULT 0,
    seen_generation INTEGER NOT NULL DEFAULT 0
);
DROP INDEX IF EXISTS idx_objects_status;
CREATE INDEX IF NOT EXISTS idx_objects_status_key ON objects(status, key);
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

// BatchUpsertScanned records objects discovered during scan.
// Changed source identities invalidate previous completion and conflicts.
// Rechecking a changed key never authorizes overwriting a target object.
func (s *Store) BatchUpsertScanned(objs []Object) error {
	if len(objs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	var gen int64
	if err := tx.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key='scan_generation'`).Scan(&gen); err != nil && err != sql.ErrNoRows {
		tx.Rollback()
		return err
	}
	stmt, err := tx.Prepare(`
        INSERT INTO objects(key, size, last_modified, etag, status, updated_at, seen_generation)
        VALUES(?, ?, ?, ?, 'pending', ?, ?)
        ON CONFLICT(key) DO UPDATE SET
            seen_generation = excluded.seen_generation,
            status = CASE
                WHEN objects.status IN ('pending','failed','skipped')
                  OR objects.size != excluded.size
                  OR objects.last_modified != excluded.last_modified
                  OR COALESCE(objects.etag,'') != COALESCE(excluded.etag,'')
                THEN 'pending'
                ELSE objects.status
            END,
            last_error = CASE
                WHEN objects.status IN ('pending','failed','skipped')
                  OR objects.size != excluded.size
                  OR objects.last_modified != excluded.last_modified
                  OR COALESCE(objects.etag,'') != COALESCE(excluded.etag,'')
                THEN ''
                ELSE objects.last_error
            END,
            updated_at = CASE
                WHEN objects.status IN ('pending','failed','skipped')
                  OR objects.size != excluded.size
                  OR objects.last_modified != excluded.last_modified
                  OR COALESCE(objects.etag,'') != COALESCE(excluded.etag,'')
                THEN excluded.updated_at
                ELSE objects.updated_at
            END,
            size = excluded.size,
            last_modified = excluded.last_modified,
            etag = excluded.etag
    `)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	now := time.Now().Unix()
	for _, o := range objs {
		if _, err := stmt.Exec(o.Key, o.Size, o.LastModified.Unix(), o.ETag, now, gen); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// pendingBatch merges the top-K keys from the ordered (status, key) index for
// 'pending' and 'failed' separately, avoiding a full-table TEMP B-TREE sort.
const pendingBatch = `
SELECT key, size, last_modified, etag, status FROM (
    SELECT key, size, last_modified, etag, status FROM (
        SELECT key, size, last_modified, etag, status FROM objects
         WHERE status = 'pending' AND key > ? ORDER BY key LIMIT ?
    )
    UNION ALL
    SELECT key, size, last_modified, etag, status FROM (
        SELECT key, size, last_modified, etag, status FROM objects
         WHERE status = 'failed' AND key > ? ORDER BY key LIMIT ?
    )
) ORDER BY key LIMIT ?
`

// BatchPendingAfter returns up to limit objects still needing work, strictly
// after afterKey (keyset pagination, bounded memory).
func (s *Store) BatchPendingAfter(afterKey string, limit int) ([]Object, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.Query(pendingBatch, afterKey, limit, afterKey, limit, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Object, 0, limit)
	for rows.Next() {
		var o Object
		var ts int64
		if err := rows.Scan(&o.Key, &o.Size, &ts, &o.ETag, &o.Status); err != nil {
			return nil, err
		}
		o.LastModified = time.Unix(ts, 0).UTC()
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) MarkStatus(key, status, errMsg string) error {
	_, err := s.db.Exec(`
        UPDATE objects
        SET status = ?, last_error = ?, attempts = attempts + 1, updated_at = ?
        WHERE key = ?
    `, status, errMsg, time.Now().Unix(), key)
	return err
}

type Counts struct {
	Total, Pending, Done, Conflict, Skipped, Failed int64
	BytesTotal, BytesPending, BytesDone             int64
}

func (s *Store) Counts() (Counts, error) {
	var c Counts
	rows, err := s.db.Query(`SELECT status, COUNT(*), COALESCE(SUM(size),0) FROM objects GROUP BY status`)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n, bytes int64
		if err := rows.Scan(&st, &n, &bytes); err != nil {
			return c, err
		}
		c.Total += n
		c.BytesTotal += bytes
		switch st {
		case StatusPending:
			c.Pending = n
			c.BytesPending = bytes
		case StatusDone:
			c.Done = n
			c.BytesDone = bytes
		case StatusConflict:
			c.Conflict = n
		case StatusSkipped:
			c.Skipped = n
		case StatusFailed:
			c.Failed = n
		}
	}
	return c, rows.Err()
}

// OpenInventory opens an existing, completed inventory under the writer lock.
func OpenInventory(path, bucket string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("open inventory %s: %w", path, err)
	}
	st, err := open(path, bucket, "rw")
	if err != nil {
		return nil, err
	}
	if err := st.RequireCompleteScan(); err != nil {
		st.Close()
		return nil, err
	}
	return st, nil
}

// SetScanComplete is a compatibility wrapper for the generation-based certificate.
func (s *Store) SetScanComplete(complete bool) error {
	if complete {
		return s.CompleteScan()
	}
	return s.BeginScan()
}
