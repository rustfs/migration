package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
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
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	tuneSQLite(db)
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

func (s *Store) Close() error { return s.db.Close() }

// tuneSQLite keeps a single connection. WAL still allows a writer plus
// readers, but database/sql would otherwise open one connection per worker.
// Those connections just queue on SQLite's single writer and surface as
// "database is locked" once --workers is raised. Status updates stay
// serialized here; object bytes stay on the S3 worker pool.
func tuneSQLite(db *sql.DB) {
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
}

// OpenExisting opens a state DB without rebinding it to a bucket.
func OpenExisting(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	tuneSQLite(db)
	s := &Store{db: db}
	if _, err := s.BoundBucket(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open existing %s: %w", path, err)
	}
	return s, nil
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
