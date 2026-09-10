//! Per-bucket migration pipeline: scan (read-only inventory into SQLite)
//! then copy (HEAD-new precheck -> GET-old -> PUT-new) with verification,
//! conflict logging and live progress bars.

use std::collections::HashMap;
use std::io::{IsTerminal, Write};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use anyhow::{Context, Result, anyhow, bail};
use aws_sdk_s3::error::SdkError;
use aws_sdk_s3::primitives::{ByteStream, DateTime};
use aws_sdk_s3::types::{CompletedMultipartUpload, CompletedPart};
use aws_sdk_s3::Client;
use chrono::{SecondsFormat, Utc};
use futures::{StreamExt, TryStreamExt};
use indicatif::{MultiProgress, ProgressBar, ProgressStyle};
use tokio::io::AsyncWriteExt as _;
use tokio::sync::Semaphore;
use tokio_util::sync::CancellationToken;

use crate::config::Config;
use crate::s3::{etag_kind, is_not_found};
use crate::store::{Object, Store, STATUS_CONFLICT, STATUS_DONE, STATUS_FAILED, STATUS_SKIPPED};

const SCAN_BATCH: usize = 500;
const WORK_BATCH: i64 = 1000;

/// Aggregate counters shared between workers and the progress display.
#[derive(Default)]
pub struct Counters {
    processed: AtomicU64,
    copied: AtomicU64,
    conflicts: AtomicU64,
    skipped: AtomicU64,
    failed: AtomicU64,
    bytes: AtomicU64,
}

impl Counters {
    fn migrate_msg(&self) -> String {
        format!(
            "copied={} conflict={} skipped={} failed={} bytes={}",
            self.copied.load(Ordering::Relaxed),
            self.conflicts.load(Ordering::Relaxed),
            self.skipped.load(Ordering::Relaxed),
            self.failed.load(Ordering::Relaxed),
            fmt_bytes(self.bytes.load(Ordering::Relaxed)),
        )
    }
}

pub struct BucketSummary {
    pub bucket: String,
    pub scanned: u64,
    pub copied: u64,
    pub conflicts: u64,
    pub skipped: u64,
    pub failed: u64,
    pub bytes: u64,
}

/// Print a log line above the progress bars. indicatif swallows `println`
/// when the draw target is hidden (stderr not a TTY), so fall back to plain
/// eprintln in that case.
pub fn log_line(mp: &MultiProgress, line: String) {
    if std::io::stderr().is_terminal() {
        let _ = mp.println(line);
    } else {
        eprintln!("{line}");
    }
}

fn bar_style(template: &str) -> ProgressStyle {
    ProgressStyle::with_template(template)
        .unwrap_or_else(|_| ProgressStyle::with_template("{spinner} {msg}").unwrap())
}

/// Migrate one bucket from `old` to `new`, end to end:
/// scan -> copy pending/failed objects -> summary.
pub async fn migrate_bucket(
    cfg: &Arc<Config>,
    old: &Client,
    new: &Client,
    bucket: &str,
    token: &CancellationToken,
    mp: &MultiProgress,
    overall: Option<&ProgressBar>,
) -> Result<BucketSummary> {
    // Fail loud and early when the bucket is mis-routed (typo, missing
    // permission) instead of migrating whatever the server returns.
    old.head_bucket()
        .bucket(bucket)
        .send()
        .await
        .with_context(|| format!("head bucket {bucket:?} on source"))?;

    ensure_target_bucket(new, bucket).await?;

    let db_path = cfg.db_path_for(bucket);
    let store = Arc::new(Store::open(&db_path, bucket)?);
    let conflict_path = cfg.conflict_log_for(bucket);
    let conflict = ConflictLog::open(&conflict_path)?;

    let scanned = scan(cfg, old, bucket, &store, token, mp).await?;

    let counters = Arc::new(Counters::default());
    let work_total = store.count_work()?;
    let bar = mp.add(
        ProgressBar::new(work_total.max(0) as u64).with_style(bar_style(
            "  {spinner:.green} [{elapsed_precise}] {bar:40.cyan/blue} {pos}/{len} {msg}",
        )),
    );
    bar.set_message(format!("{bucket}: preparing…"));

    if work_total > 0 {
        copy_all(cfg, old, new, bucket, &store, &conflict, &counters, token, mp, &bar).await?;
    }
    bar.finish_and_clear();

    let summary = BucketSummary {
        bucket: bucket.to_string(),
        scanned,
        copied: counters.copied.load(Ordering::Relaxed),
        conflicts: counters.conflicts.load(Ordering::Relaxed),
        skipped: counters.skipped.load(Ordering::Relaxed),
        failed: counters.failed.load(Ordering::Relaxed),
        bytes: counters.bytes.load(Ordering::Relaxed),
    };
    log_line(
        mp,
        format!(
            "✔ {bucket}: scanned={} copied={} conflict={} skipped={} failed={} ({} transferred)",
        summary.scanned,
        summary.copied,
        summary.conflicts,
        summary.skipped,
        summary.failed,
            fmt_bytes(summary.bytes),
        ),
    );
    if let Some(o) = overall {
        o.inc(1);
    }
    Ok(summary)
}

/// Create the bucket on the target cluster when it does not exist yet
/// (needed for whole-instance migration). Never touches an existing bucket.
async fn ensure_target_bucket(new: &Client, bucket: &str) -> Result<()> {
    match new.head_bucket().bucket(bucket).send().await {
        Ok(_) => Ok(()),
        Err(err) if is_not_found(&err) => {
            new.create_bucket().bucket(bucket).send().await.with_context(|| {
                format!("create bucket {bucket:?} on target (credentials need s3:CreateBucket)")
            })?;
            Ok(())
        }
        Err(err) => Err(anyhow!("head bucket {bucket:?} on target: {err}")),
    }
}

/// ListObjectsV2 pagination -> batched upserts into the state DB.
/// Re-runs only refresh pending/failed rows; terminal rows survive.
async fn scan(
    cfg: &Arc<Config>,
    old: &Client,
    bucket: &str,
    store: &Arc<Store>,
    token: &CancellationToken,
    mp: &MultiProgress,
) -> Result<u64> {
    let bar = mp.add(
        ProgressBar::new_spinner().with_style(bar_style(
            "  {spinner:.blue} [{elapsed_precise}] scan {msg} {pos} objects",
        )),
    );
    bar.enable_steady_tick(Duration::from_millis(200));
    bar.set_message(bucket.to_string());

    let mut total: u64 = 0;
    let mut buf: Vec<Object> = Vec::with_capacity(SCAN_BATCH);
    let mut pages = old.list_objects_v2().bucket(bucket).into_paginator().send();

    while let Some(page) = pages.next().await {
        if token.is_cancelled() {
            store.batch_upsert_scanned(&buf)?;
            bail!("cancelled during scan");
        }
        let page = page.map_err(|e| anyhow!("list page: {e}"))?;
        // Refuse keys from a different bucket if the server mis-routes us.
        if let Some(name) = page.name() {
            if name != bucket {
                bail!("list page bucket mismatch: requested {bucket:?}, server returned {name:?}");
            }
        }
        for obj in page.contents() {
            buf.push(Object {
                key: obj.key().unwrap_or_default().to_string(),
                size: obj.size().unwrap_or(0),
                last_modified: obj.last_modified.map(|t| t.secs()).unwrap_or(0),
                etag: obj.e_tag().unwrap_or_default().to_string(),
            });
        }
        total += page.contents().len() as u64;
        bar.set_position(total);
        if buf.len() >= SCAN_BATCH {
            store.batch_upsert_scanned(&buf)?;
            buf.clear();
        }
    }
    store.batch_upsert_scanned(&buf)?;
    bar.finish_and_clear();
    let _ = cfg; // spool dir and workers only matter during copy
    Ok(total)
}

/// Feed pending/failed objects through the worker pool, keyset-pagination
/// style so memory stays bounded regardless of inventory size.
#[allow(clippy::too_many_arguments)]
async fn copy_all(
    cfg: &Arc<Config>,
    old: &Client,
    new: &Client,
    bucket: &str,
    store: &Arc<Store>,
    conflict: &ConflictLog,
    counters: &Arc<Counters>,
    token: &CancellationToken,
    mp: &MultiProgress,
    bar: &ProgressBar,
) -> Result<()> {
    let mut after = String::new();
    loop {
        if token.is_cancelled() {
            return Err(anyhow!("cancelled"));
        }
        let batch = store.batch_pending_after(&after, WORK_BATCH)?;
        if batch.is_empty() {
            return Ok(());
        }
        after = batch.last().unwrap().key.clone();

        let stream = futures::stream::iter(batch.into_iter().map(|obj| {
            let mp = mp.clone();
            async move {
                handle(cfg, old, new, bucket, store, conflict, counters, token, &mp, bar, obj).await
            }
        }));
        let work = stream
            .buffer_unordered(cfg.workers as usize)
            .try_collect::<Vec<()>>()
            .await;
        work?; // first fatal error (persistence failure) stops the run
    }
}

/// Process a single object: HEAD-new precheck, copy, verify, persist status.
/// Per-object S3 errors are recorded as `failed`; only persistence failures
/// bubble up as fatal errors.
#[allow(clippy::too_many_arguments)]
async fn handle(
    cfg: &Arc<Config>,
    old: &Client,
    new: &Client,
    bucket: &str,
    store: &Arc<Store>,
    conflict: &ConflictLog,
    counters: &Arc<Counters>,
    token: &CancellationToken,
    mp: &MultiProgress,
    bar: &ProgressBar,
    obj: Object,
) -> Result<()> {
    if token.is_cancelled() {
        return Ok(());
    }
    counters.processed.fetch_add(1, Ordering::Relaxed);

    // 1. If the key already exists on New we MUST NOT overwrite it.
    match new.head_object().bucket(bucket).key(&obj.key).send().await {
        Ok(existing) => {
            let old_lm = rfc3339_secs(obj.last_modified);
            let new_lm = existing
                .last_modified
                .map(|t| rfc3339_secs(t.secs()))
                .unwrap_or_default();
            conflict
                .write(&obj.key, &old_lm, &new_lm)
                .context("append conflict log")?;
            counters.conflicts.fetch_add(1, Ordering::Relaxed);
            store.mark_status(&obj.key, STATUS_CONFLICT, "exists on new")?;
            tick(bar, counters);
            return Ok(());
        }
        Err(err) if is_not_found(&err) => {} // expected — proceed to copy
        Err(err) => {
            return mark_failed(mp, store, counters, bar, &obj, &format!("head new: {err}"));
        }
    }

    // 2. Copy.
    let result = if obj.size >= cfg.multipart_threshold as i64 {
        copy_multipart(cfg, old, new, bucket, &obj, token).await
    } else {
        copy_single(cfg, old, new, bucket, &obj).await
    };
    match result {
        Ok(()) => {
            counters.copied.fetch_add(1, Ordering::Relaxed);
            counters.bytes.fetch_add(obj.size.max(0) as u64, Ordering::Relaxed);
            store.mark_status(&obj.key, STATUS_DONE, "")?;
        }
        Err(err) => {
            if is_source_missing(&err) {
                // Deleted on the source mid-migration — skip, not an error.
                counters.skipped.fetch_add(1, Ordering::Relaxed);
                store.mark_status(&obj.key, STATUS_SKIPPED, "source 404")?;
            } else {
                return mark_failed(mp, store, counters, bar, &obj, &format!("{err:#}"));
            }
        }
    }
    tick(bar, counters);
    Ok(())
}

fn mark_failed(
    mp: &MultiProgress,
    store: &Arc<Store>,
    counters: &Arc<Counters>,
    bar: &ProgressBar,
    obj: &Object,
    msg: &str,
) -> Result<()> {
    counters.failed.fetch_add(1, Ordering::Relaxed);
    log_line(mp, format!("  ✗ {}: {msg}", obj.key));
    store.mark_status(&obj.key, STATUS_FAILED, msg)?;
    tick(bar, counters);
    Ok(())
}

fn tick(bar: &ProgressBar, counters: &Arc<Counters>) {
    bar.set_position(counters.processed.load(Ordering::Relaxed));
    bar.set_message(counters.migrate_msg());
}

/// Marker error: the source returned 404 mid-copy (the application deleted
/// the object after the scan) — recorded as `skipped`, not a failure.
#[derive(Debug)]
struct SourceMissing;

impl std::fmt::Display for SourceMissing {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "source object missing (deleted after scan)")
    }
}

impl std::error::Error for SourceMissing {}

fn source_missing(err: SdkError<impl std::fmt::Debug + std::fmt::Display>) -> anyhow::Error {
    anyhow::Error::new(SourceMissing).context(format!("get old: {err}"))
}

fn is_source_missing(err: &anyhow::Error) -> bool {
    err.chain().any(|c| c.downcast_ref::<SourceMissing>().is_some())
}

/// Small-object path: GET-old -> spool to a temp file -> PUT-new -> verify.
/// Spooling keeps memory flat for objects just below the multipart
/// threshold, matching the Go tool's behaviour.
async fn copy_single(
    cfg: &Arc<Config>,
    old: &Client,
    new: &Client,
    bucket: &str,
    obj: &Object,
) -> Result<()> {
    let got = match old
        .get_object()
        .bucket(bucket)
        .key(&obj.key)
        .send()
        .await
    {
        Ok(g) => g,
        Err(e) if is_not_found(&e) => return Err(source_missing(e)),
        Err(e) => return Err(anyhow!("get old: {e}")),
    };

    let content_length = got.content_length().unwrap_or(obj.size);
    let src_etag = got.e_tag().map(|s| s.to_string());
    let spool_dir = cfg.spool_dir.clone();
    let tmp = tokio::task::spawn_blocking(move || {
        std::fs::create_dir_all(&spool_dir)?;
        tempfile::NamedTempFile::new_in(&spool_dir)
    })
    .await
    .map_err(|e| anyhow!("join spool task: {e}"))?
    .map_err(|e| anyhow!("create temp file in {}: {e}", cfg.spool_dir.display()))?;

    let mut file = tokio::fs::File::from_std(tmp.reopen()?);
    let mut reader = got.body.into_async_read();
    let written = tokio::io::copy(&mut reader, &mut file)
        .await
        .map_err(|e| anyhow!("spool old body: {e}"))?;
    file.flush().await?;
    drop(reader);
    drop(file);
    if content_length > 0 && written != content_length as u64 {
        bail!("short read: got={written} want={content_length}");
    }
    let body = ByteStream::from_path(tmp.path())
        .await
        .map_err(|e| anyhow!("spool file unreadable: {e}"))?;
    let put = new
        .put_object()
        .bucket(bucket)
        .key(&obj.key)
        .body(body)
        .content_length(written as i64)
        .set_cache_control(got.cache_control.clone())
        .set_content_disposition(got.content_disposition.clone())
        .set_content_encoding(got.content_encoding.clone())
        .set_content_language(got.content_language.clone())
        .set_content_type(got.content_type.clone())
        .set_expires(parse_expires(&got.expires_string))
        .set_metadata(got.metadata.clone())
        .send()
        .await
        .map_err(|e| anyhow!("put new: {e}"))?;
    tmp.close().map_err(|e| anyhow!("remove temp file: {e}"))?;

    // Verify size; compare ETags only when both sides expose a simple
    // (non-multipart) ETag.
    let head = new
        .head_object()
        .bucket(bucket)
        .key(&obj.key)
        .send()
        .await
        .map_err(|e| anyhow!("verify head new: {e}"))?;
    let got_size = head.content_length().unwrap_or(0);
    if got_size != obj.size {
        bail!("size mismatch after put: got={got_size} want={}", obj.size);
    }
    let (src_etag, src_mp) = etag_kind(src_etag.as_deref().unwrap_or_default());
    let (dst_etag, dst_mp) = etag_kind(put.e_tag().unwrap_or_default());
    if !src_etag.is_empty() && !dst_etag.is_empty() && !src_mp && !dst_mp && src_etag != dst_etag {
        bail!("etag mismatch: src={src_etag} dst={dst_etag}");
    }
    Ok(())
}

/// Large-object path: CreateMultipartUpload -> ranged GET + UploadPart with
/// bounded concurrency -> Complete -> verify size. Any failure aborts the
/// upload session (never deletes committed data).
async fn copy_multipart(
    cfg: &Arc<Config>,
    old: &Client,
    new: &Client,
    bucket: &str,
    obj: &Object,
    token: &CancellationToken,
) -> Result<()> {
    let attrs = match old
        .head_object()
        .bucket(bucket)
        .key(&obj.key)
        .send()
        .await
    {
        Ok(a) => a,
        Err(e) if is_not_found(&e) => return Err(source_missing(e)),
        Err(e) => return Err(anyhow!("head old: {e}")),
    };

    let mut part_size = cfg.multipart_part_size as i64;
    let mut num_parts = (obj.size + part_size - 1) / part_size;
    if num_parts > 10000 {
        // S3 caps parts at 10k — grow part size to fit.
        part_size = (obj.size + 9999) / 10000;
        num_parts = (obj.size + part_size - 1) / part_size;
    }

    let create = new
        .create_multipart_upload()
        .bucket(bucket)
        .key(&obj.key)
        .set_cache_control(attrs.cache_control.clone())
        .set_content_disposition(attrs.content_disposition.clone())
        .set_content_encoding(attrs.content_encoding.clone())
        .set_content_language(attrs.content_language.clone())
        .set_content_type(attrs.content_type.clone())
        .set_expires(parse_expires(&attrs.expires_string))
        .set_metadata(attrs.metadata.clone())
        .send()
        .await
        .map_err(|e| anyhow!("create multipart: {e}"))?;
    let upload_id = create
        .upload_id()
        .ok_or_else(|| anyhow!("create multipart: server returned no UploadId"))?
        .to_string();

    // Abort on error/cancellation with a fresh timeout so cleanup runs even
    // while the main token is cancelled.
    async fn abort_upload(new: &Client, bucket: &str, key: &str, upload_id: &str) {
        match tokio::time::timeout(
            Duration::from_secs(30),
            new.abort_multipart_upload()
                .bucket(bucket)
                .key(key)
                .upload_id(upload_id)
                .send(),
        )
        .await
        {
            Ok(Ok(_)) => {}
            other => eprintln!("WARN abort multipart {key}: {other:?}"),
        }
    }

    let parts: Arc<Mutex<HashMap<i32, CompletedPart>>> = Arc::new(Mutex::new(HashMap::new()));
    let sem = Arc::new(Semaphore::new(cfg.multipart_concurrency as usize));

    let result: Result<()> = futures::stream::iter(0..num_parts)
        .map(|idx| {
            let old = old.clone();
            let new = new.clone();
            let sem = sem.clone();
            let parts = parts.clone();
            let upload_id = upload_id.clone();
            let bucket = bucket.to_string();
            let key = obj.key.clone();
            async move {
                let _permit = sem.acquire_owned().await;
                let start = idx * part_size;
                let end = (start + part_size).min(obj.size) - 1;
                let range = format!("bytes={start}-{end}");

                let got = match old
                    .get_object()
                    .bucket(&bucket)
                    .key(&key)
                    .range(&range)
                    .send()
                    .await
                {
                    Ok(g) => g,
                    Err(e) if is_not_found(&e) => return Err(source_missing(e)),
                    Err(e) => return Err(anyhow!("get part {}: {e}", idx + 1)),
                };
                // Bounded by part_size, same as the Go tool.
                let data = got
                    .body
                    .collect()
                    .await
                    .map_err(|e| anyhow!("read part {}: {e}", idx + 1))?
                    .into_bytes();

                let up = new
                    .upload_part()
                    .bucket(&bucket)
                    .key(&key)
                    .upload_id(&upload_id)
                    .part_number((idx + 1) as i32)
                    .content_length(data.len() as i64)
                    .body(ByteStream::from(data))
                    .send()
                    .await
                    .map_err(|e| anyhow!("upload part {}: {e}", idx + 1))?;

                let part = CompletedPart::builder()
                    .part_number((idx + 1) as i32)
                    .set_e_tag(up.e_tag().map(|s| s.to_string()))
                    .build();
                parts.lock().unwrap().insert((idx + 1) as i32, part);
                Ok::<(), anyhow::Error>(())
            }
        })
        .buffer_unordered(cfg.multipart_concurrency as usize)
        .try_collect::<Vec<()>>()
        .await
        .map(|_| ());

    let first_err = match result {
        Ok(()) => {
            if token.is_cancelled() {
                Err(anyhow!("cancelled during multipart"))
            } else {
                Ok(())
            }
        }
        err @ Err(_) => err,
    };
    if let Err(reason) = first_err {
        abort_upload(new, bucket, &obj.key, &upload_id).await;
        return Err(reason.context("multipart failed"));
    }

    let mut part_list: Vec<CompletedPart> =
        parts.lock().unwrap().drain().map(|(_, p)| p).collect();
    part_list.sort_by_key(|p| p.part_number().unwrap_or(0));
    new.complete_multipart_upload()
        .bucket(bucket)
        .key(&obj.key)
        .upload_id(&upload_id)
        .multipart_upload(CompletedMultipartUpload::builder().set_parts(Some(part_list)).build())
        .send()
        .await
        .map_err(|e| anyhow!("complete multipart: {e}"))?;

    // Multipart ETags depend on part boundaries, so verify size only.
    let head = new
        .head_object()
        .bucket(bucket)
        .key(&obj.key)
        .send()
        .await
        .map_err(|e| anyhow!("verify head new: {e}"))?;
    let got_size = head.content_length().unwrap_or(0);
    if got_size != obj.size {
        bail!("size mismatch after multipart: got={got_size} want={}", obj.size);
    }
    Ok(())
}

/// Append-only TSV conflict log: `key \t old.LastModified \t new.LastModified`.
pub struct ConflictLog {
    file: Mutex<std::fs::File>,
}

impl ConflictLog {
    pub fn open(path: &std::path::Path) -> Result<Self> {
        if let Some(dir) = path.parent() {
            if !dir.as_os_str().is_empty() {
                std::fs::create_dir_all(dir)
                    .with_context(|| format!("create dir {}", dir.display()))?;
            }
        }
        let file = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(path)
            .with_context(|| format!("open conflict log {}", path.display()))?;
        Ok(ConflictLog { file: Mutex::new(file) })
    }

    pub fn write(&self, key: &str, old_lm: &str, new_lm: &str) -> Result<()> {
        let mut f = self.file.lock().unwrap();
        writeln!(f, "{key}\t{old_lm}\t{new_lm}")?;
        f.flush()?;
        Ok(())
    }
}

/// The upload builders take `Expires` as a typed DateTime while HEAD/GET
/// expose the raw HTTP-date string; parse it back (None on malformed input —
/// the attribute is advisory only).
fn parse_expires(raw: &Option<String>) -> Option<DateTime> {
    raw.as_ref()
        .and_then(|s| httpdate::parse_http_date(s).ok())
        .map(|t| match t.duration_since(std::time::UNIX_EPOCH) {
            Ok(d) => DateTime::from_secs(d.as_secs() as i64),
            Err(_) => DateTime::from_secs(0),
        })
}

fn rfc3339_secs(unix_secs: i64) -> String {
    chrono::DateTime::from_timestamp(unix_secs, 0)
        .unwrap_or_else(Utc::now)
        .to_rfc3339_opts(SecondsFormat::Secs, true)
}

/// Human-friendly byte count for progress display.
pub fn fmt_bytes(n: u64) -> String {
    const UNITS: [&str; 6] = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
    let mut value = n as f64;
    let mut unit = 0;
    while value >= 1024.0 && unit < UNITS.len() - 1 {
        value /= 1024.0;
        unit += 1;
    }
    if unit == 0 {
        format!("{n} B")
    } else {
        format!("{value:.1} {}", UNITS[unit])
    }
}
