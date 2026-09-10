//! Local SQLite state store: scan inventory + per-object migration status.
//! Same schema and semantics as the Go original, so a DB written by either
//! tool can be read by the other.

use std::path::Path;
use std::sync::Mutex;

use anyhow::{Context, Result, bail};
use chrono::Utc;
use rusqlite::{Connection, OptionalExtension, params};

pub const STATUS_PENDING: &str = "pending";
pub const STATUS_DONE: &str = "done";
pub const STATUS_CONFLICT: &str = "conflict";
pub const STATUS_SKIPPED: &str = "skipped";
pub const STATUS_FAILED: &str = "failed";

#[derive(Debug, Clone)]
pub struct Object {
    pub key: String,
    pub size: i64,
    /// Unix seconds.
    pub last_modified: i64,
    pub etag: String,
}

#[derive(Debug, Clone, Copy, Default, PartialEq)]
pub struct Counts {
    pub total: i64,
    pub pending: i64,
    pub done: i64,
    pub conflict: i64,
    pub skipped: i64,
    pub failed: i64,
}

const SCHEMA: &str = "
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
";

pub struct Store {
    conn: Mutex<Connection>,
}

impl Store {
    /// Open (creating if needed) the state DB and bind it to `bucket`.
    /// The binding persists: reusing one DB file for another bucket fails
    /// fast instead of mixing inventories.
    pub fn open(path: &Path, bucket: &str) -> Result<Self> {
        if bucket.is_empty() {
            bail!("store: bucket must not be empty");
        }
        if let Some(dir) = path.parent() {
            if !dir.as_os_str().is_empty() {
                std::fs::create_dir_all(dir)
                    .with_context(|| format!("create dir {}", dir.display()))?;
            }
        }
        let conn = Connection::open(path)
            .with_context(|| format!("open sqlite {}", path.display()))?;
        conn.busy_timeout(std::time::Duration::from_millis(5000))?;
        conn.pragma_update(None, "journal_mode", "WAL")?;
        conn.pragma_update(None, "synchronous", "NORMAL")?;
        conn.execute_batch(SCHEMA)
            .context("init schema")?;
        {
            let existing: Option<String> = conn
                .query_row(
                    "SELECT value FROM meta WHERE key = 'bucket'",
                    [],
                    |row| row.get(0),
                )
                .optional()?;
            match existing {
                None => {
                    conn.execute(
                        "INSERT INTO meta(key, value) VALUES('bucket', ?1)",
                        params![bucket],
                    )?;
                }
                Some(existing) if existing != bucket => {
                    bail!(
                        "db_path is bound to bucket {existing:?} but config requests {bucket:?}; use a separate db_path per bucket"
                    );
                }
                Some(_) => {}
            }
        }
        Ok(Store { conn: Mutex::new(conn) })
    }

    /// Record a batch of scanned objects. Rows already in a terminal state
    /// (done/conflict/skipped) are left untouched, so re-running a scan is
    /// safe and never resurrects finished work.
    pub fn batch_upsert_scanned(&self, objs: &[Object]) -> Result<()> {
        if objs.is_empty() {
            return Ok(());
        }
        let mut conn = self.conn.lock().unwrap();
        let tx = conn.transaction()?;
        {
            let mut stmt = tx.prepare_cached(
                "INSERT INTO objects(key, size, last_modified, etag, status, updated_at)
                 VALUES(?1, ?2, ?3, ?4, 'pending', ?5)
                 ON CONFLICT(key) DO UPDATE SET
                     size = excluded.size,
                     last_modified = excluded.last_modified,
                     etag = excluded.etag,
                     updated_at = excluded.updated_at
                 WHERE objects.status = 'pending' OR objects.status = 'failed'",
            )?;
            let now = Utc::now().timestamp();
            for o in objs {
                stmt.execute(params![o.key, o.size, o.last_modified, o.etag, now])?;
            }
        }
        tx.commit()?;
        Ok(())
    }

    /// Fetch one batch of objects still needing work, strictly after
    /// `after_key` (keyset pagination so memory stays bounded even for
    /// hundreds of millions of objects).
    pub fn batch_pending_after(&self, after_key: &str, limit: i64) -> Result<Vec<Object>> {
        let conn = self.conn.lock().unwrap();
        let mut stmt = conn.prepare_cached(
            "SELECT key, size, last_modified, etag, status FROM objects
             WHERE status IN ('pending','failed') AND key > ?1
             ORDER BY key LIMIT ?2",
        )?;
        let rows = stmt.query_map(params![after_key, limit], |row| {
            Ok(Object {
                key: row.get(0)?,
                size: row.get(1)?,
                last_modified: row.get(2)?,
                etag: row.get(3)?,
            })
        })?;
        Ok(rows.collect::<std::result::Result<Vec<_>, _>>()?)
    }

    pub fn mark_status(&self, key: &str, status: &str, err_msg: &str) -> Result<()> {
        let conn = self.conn.lock().unwrap();
        conn.execute(
            "UPDATE objects
             SET status = ?1, last_error = ?2, attempts = attempts + 1, updated_at = ?3
             WHERE key = ?4",
            params![status, err_msg, Utc::now().timestamp(), key],
        )?;
        Ok(())
    }

    pub fn counts(&self) -> Result<Counts> {
        let conn = self.conn.lock().unwrap();
        let mut stmt = conn.prepare("SELECT status, COUNT(*) FROM objects GROUP BY status")?;
        let rows = stmt.query_map([], |row| Ok((row.get::<_, String>(0)?, row.get::<_, i64>(1)?)))?;
        let mut c = Counts::default();
        for row in rows {
            let (status, n) = row?;
            c.total += n;
            match status.as_str() {
                STATUS_PENDING => c.pending = n,
                STATUS_DONE => c.done = n,
                STATUS_CONFLICT => c.conflict = n,
                STATUS_SKIPPED => c.skipped = n,
                STATUS_FAILED => c.failed = n,
                _ => {}
            }
        }
        Ok(c)
    }

    /// Number of rows that still need work (pending or failed).
    pub fn count_work(&self) -> Result<i64> {
        let conn = self.conn.lock().unwrap();
        let n: i64 = conn.query_row(
            "SELECT COUNT(*) FROM objects WHERE status IN ('pending','failed')",
            [],
            |row| row.get(0),
        )?;
        Ok(n)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn store(tmp: &Path) -> Store {
        Store::open(&tmp.join("test.db"), "photos").unwrap()
    }

    #[test]
    fn refuses_foreign_bucket() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("bound.db");
        Store::open(&path, "a").unwrap();
        let err = Store::open(&path, "b").err().expect("must refuse foreign bucket");
        assert!(err.to_string().contains("bound to bucket"));
    }

    #[test]
    fn upsert_never_resurrects_terminal_rows() {
        let dir = tempfile::tempdir().unwrap();
        let st = store(dir.path());
        st.batch_upsert_scanned(&[Object {
            key: "k".into(),
            size: 1,
            last_modified: 0,
            etag: String::new(),
        }])
        .unwrap();
        st.mark_status("k", STATUS_DONE, "").unwrap();

        // Re-scan must not reset the done row to pending.
        st.batch_upsert_scanned(&[Object {
            key: "k".into(),
            size: 2,
            last_modified: 5,
            etag: String::new(),
        }])
        .unwrap();
        let c = st.counts().unwrap();
        assert_eq!((c.total, c.done, c.pending), (1, 1, 0));

        // But failed rows do get refreshed and are eligible again.
        st.mark_status("k", STATUS_FAILED, "boom").unwrap();
        st.batch_upsert_scanned(&[Object {
            key: "k".into(),
            size: 2,
            last_modified: 6,
            etag: String::new(),
        }])
        .unwrap();
        // Row metadata is refreshed but the status stays `failed`; a
        // re-run of migrate is what retries it.
        let c = st.counts().unwrap();
        assert_eq!((c.pending, c.failed), (0, 1));
    }

    #[test]
    fn keyset_pagination_walks_all_pending() {
        let dir = tempfile::tempdir().unwrap();
        let st = store(dir.path());
        let objs: Vec<Object> = (0..7)
            .map(|i| Object {
                key: format!("key-{i:02}"),
                size: i,
                last_modified: 0,
                etag: String::new(),
            })
            .collect();
        st.batch_upsert_scanned(&objs).unwrap();
        st.mark_status("key-00", STATUS_DONE, "").unwrap(); // skipped in iteration

        let mut seen = Vec::new();
        let mut after = String::new();
        loop {
            let batch = st.batch_pending_after(&after, 3).unwrap();
            if batch.is_empty() {
                break;
            }
            after = batch.last().unwrap().key.clone();
            seen.extend(batch.iter().map(|o| o.key.clone()));
        }
        assert_eq!(seen, vec!["key-01", "key-02", "key-03", "key-04", "key-05", "key-06"]);
    }
}
