package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
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
	db *sql.DB
}

func Open(path, bucket string) (*Store, error) {
	if bucket == "" {
		return nil, fmt.Errorf("store: bucket must not be empty")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir %s: %w", dir, err)
		}
	}
	db, err := openDB(path, "rwc")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	if err := ensureBucket(db, bucket); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func ensureBucket(db *sql.DB, bucket string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('bucket',?) ON CONFLICT(key) DO NOTHING`, bucket)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('scan_complete', '0')`); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return checkBucket(db, bucket)
}

func checkBucket(db *sql.DB, bucket string) error {
	var existing string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key='bucket'`).Scan(&existing); err != nil {
		return fmt.Errorf("read bucket meta: %w", err)
	}
	if existing != bucket {
		return fmt.Errorf("db_path is bound to bucket %q but config requests %q; use a separate db_path per bucket", existing, bucket)
	}
	return nil
}

// BindEndpoints prevents terminal rows from one migration being reused for
// another. A scan only knows the source. Existing pre-binding databases adopt
// their configured endpoint on first use without discarding completed work.
func (s *Store) BindEndpoints(source, target string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, entry := range []struct{ key, value string }{{"source_endpoint", source}, {"target_endpoint", target}} {
		if entry.value == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO NOTHING`, entry.key, entry.value); err != nil {
			return err
		}
		var bound string
		if err := tx.QueryRow(`SELECT value FROM meta WHERE key=?`, entry.key).Scan(&bound); err != nil {
			return err
		}
		if bound != entry.value {
			return fmt.Errorf("db_path is bound to %s %q, configured %q; use a separate db_path for this migration", entry.key, bound, entry.value)
		}
	}
	return tx.Commit()
}

func (s *Store) Close() error { return s.db.Close() }

func openDB(path, mode string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// SQLite file URIs require a leading slash before Windows drive letters.
	name := filepath.ToSlash(abs)
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	query := url.Values{"mode": {mode}, "_pragma": {"busy_timeout(5000)"}}
	if mode != "ro" {
		query.Add("_pragma", "journal_mode(WAL)")
		query.Add("_pragma", "synchronous(NORMAL)")
	}
	uri := url.URL{Scheme: "file", Path: name, RawQuery: query.Encode()}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	tuneSQLite(db)
	return db, nil
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

// OpenExisting opens a state database read-only for status.
func OpenExisting(path string) (*Store, error) {
	db, err := openDB(path, "ro")
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if _, err := s.BoundBucket(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open existing %s: %w", path, err)
	}
	return s, nil
}

// OpenInventory requires a state DB created by scan, and never creates one.
func OpenInventory(path, bucket string) (*Store, error) {
	db, err := openDB(path, "rw")
	if err != nil {
		return nil, err
	}
	if err := checkBucket(db, bucket); err != nil {
		db.Close()
		return nil, fmt.Errorf("open inventory %s (run scan first): %w", path, err)
	}
	var incomplete bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM meta WHERE key='scan_complete' AND value != '1')`).Scan(&incomplete); err != nil {
		db.Close()
		return nil, err
	}
	if incomplete {
		db.Close()
		return nil, fmt.Errorf("inventory %s is incomplete; run scan again", path)
	}
	return &Store{db: db}, nil
}

// SetScanComplete leaves the last scan marked incomplete until all pages are durable.
func (s *Store) SetScanComplete(complete bool) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO meta(key,value) VALUES('scan_complete',?)`, complete)
	return err
}

func (s *Store) BoundBucket() (string, error) {
	var name string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'bucket'`).Scan(&name)
	return name, err
}

const schema = `
CREATE TABLE IF NOT EXISTS objects (
    key            TEXT PRIMARY KEY,
    size           INTEGER NOT NULL,
    last_modified  INTEGER NOT NULL,
    etag           TEXT,
    status         TEXT NOT NULL DEFAULT 'pending',
    attempts       INTEGER NOT NULL DEFAULT 0,
    last_error     TEXT,
    updated_at     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_objects_status ON objects(status);
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

// BatchUpsertScanned records objects discovered during scan.
// Terminal rows (done/conflict) are left untouched.
// pending/failed/skipped rows are refreshed and reset to pending, so a
// skipped object that reappears on the source is retried.
func (s *Store) BatchUpsertScanned(objs []Object) error {
	if len(objs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`
        INSERT INTO objects(key, size, last_modified, etag, status, updated_at)
        VALUES(?, ?, ?, ?, 'pending', ?)
        ON CONFLICT(key) DO UPDATE SET
            size = excluded.size,
            last_modified = excluded.last_modified,
            etag = excluded.etag,
            updated_at = excluded.updated_at,
            status = 'pending',
            last_error = ''
        WHERE objects.status IN ('pending','failed','skipped')
    `)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	now := time.Now().Unix()
	for _, o := range objs {
		if _, err := stmt.Exec(o.Key, o.Size, o.LastModified.Unix(), o.ETag, now); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

const pendingBatch = `
SELECT key, size, last_modified, etag, status FROM objects
 WHERE status IN ('pending','failed') AND key > ?
 ORDER BY key LIMIT ?
`

// BatchPendingAfter returns up to limit objects still needing work, strictly
// after afterKey (keyset pagination, bounded memory).
func (s *Store) BatchPendingAfter(afterKey string, limit int) ([]Object, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.Query(pendingBatch, afterKey, limit)
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

func (s *Store) CountWork() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM objects WHERE status IN ('pending','failed')`).Scan(&n)
	return n, err
}
