package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Status values used in the objects table.
const (
	StatusPending  = "pending"
	StatusDone     = "done"
	StatusConflict = "conflict"
	StatusSkipped  = "skipped" // source 404 etc.
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
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
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

// ensureBucket records the bucket the DB is scoped to on first open and
// refuses subsequent opens that would mix data from a different bucket into
// the same state file.
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

const schema = `
CREATE TABLE IF NOT EXISTS objects (
    key            TEXT PRIMARY KEY,
    size           INTEGER NOT NULL,
    last_modified  INTEGER NOT NULL, -- unix seconds
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

// UpsertScanned records an object discovered during scan. If the row already
// exists in a terminal state (done/conflict/skipped), it is left untouched so
// reruns of scan are safe.
func (s *Store) UpsertScanned(o Object) error {
	_, err := s.db.Exec(`
        INSERT INTO objects(key, size, last_modified, etag, status, updated_at)
        VALUES(?, ?, ?, ?, 'pending', ?)
        ON CONFLICT(key) DO UPDATE SET
            size = excluded.size,
            last_modified = excluded.last_modified,
            etag = excluded.etag,
            updated_at = excluded.updated_at
        WHERE objects.status = 'pending' OR objects.status = 'failed'
    `, o.Key, o.Size, o.LastModified.Unix(), o.ETag, time.Now().Unix())
	return err
}

// BatchUpsertScanned wraps many UpsertScanned in a single transaction.
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
            updated_at = excluded.updated_at
        WHERE objects.status = 'pending' OR objects.status = 'failed'
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

// IteratePending streams every object that still needs work into fn.
func (s *Store) IteratePending(fn func(Object) error) error {
	rows, err := s.db.Query(`SELECT key, size, last_modified, etag, status FROM objects WHERE status IN ('pending','failed') ORDER BY key`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var o Object
		var ts int64
		if err := rows.Scan(&o.Key, &o.Size, &ts, &o.ETag, &o.Status); err != nil {
			return err
		}
		o.LastModified = time.Unix(ts, 0).UTC()
		if err := fn(o); err != nil {
			return err
		}
	}
	return rows.Err()
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
}

func (s *Store) Counts() (Counts, error) {
	var c Counts
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM objects GROUP BY status`)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int64
		if err := rows.Scan(&st, &n); err != nil {
			return c, err
		}
		c.Total += n
		switch st {
		case StatusPending:
			c.Pending = n
		case StatusDone:
			c.Done = n
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
