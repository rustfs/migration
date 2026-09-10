//! rf — safe one-way bucket migration between two S3-compatible (RustFS)
//! clusters. Rust rewrite of github.com/rustfs/migration.
//!
//! Subcommands:
//!   rf list     list every bucket on the source instance
//!   rf migrate  migrate buckets (all, or --bucket) with live progress
//!   rf status   show per-bucket progress counters from the local state DBs

mod config;
mod migrator;
mod s3;
mod store;

use std::io::Write as _;
use std::path::PathBuf;
use std::sync::Arc;

use anyhow::{Result, anyhow};
use aws_sdk_s3::Client;
use clap::{Parser, Subcommand};
use indicatif::{MultiProgress, ProgressBar};
use tokio_util::sync::CancellationToken;

use config::Config;
use migrator::BucketSummary;

#[derive(Parser)]
#[command(
    name = "rf",
    version,
    about = "Safe one-way bucket migration between two S3-compatible (RustFS) clusters",
    after_help = "Env overrides: OLD_ENDPOINT, OLD_AK, OLD_SK, OLD_REGION, NEW_ENDPOINT, NEW_AK, \
                  NEW_SK, NEW_REGION, REGION, BUCKET, DB_PATH, CONFLICT_LOG, SPOOL_DIR"
)]
struct Cli {
    /// Path to YAML config
    #[arg(short, long, value_name = "FILE", default_value = "config.yaml", global = true)]
    config: PathBuf,

    #[command(subcommand)]
    command: Command,
}

#[derive(Subcommand)]
enum Command {
    /// List all buckets on the source RustFS instance
    #[command(alias = "ls")]
    List,

    /// Migrate buckets from the source to the target instance (resumable,
    /// never overwrites an object that already exists on the target)
    Migrate {
        /// Migrate only this bucket (default: every bucket on the source)
        #[arg(long, value_name = "NAME")]
        bucket: Option<String>,
    },

    /// Show migration progress counters from the local state DBs
    Status {
        /// Restrict to one bucket
        #[arg(long, value_name = "NAME")]
        bucket: Option<String>,
    },
}

fn main() {
    let cli = Cli::parse();
    let token = CancellationToken::new();

    let rt = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .expect("build tokio runtime");
    let code = match rt.block_on(run(cli, token)) {
        Ok(code) => code,
        Err(err) => {
            // Anything the progress layer already printed stays above this.
            let mut stderr = std::io::stderr();
            let _ = writeln!(stderr, "error: {err:#}");
            1
        }
    };
    std::process::exit(code);
}

async fn run(cli: Cli, token: CancellationToken) -> Result<i32> {
    install_signal_handler(token.clone());
    let cfg = Arc::new(Config::load(&cli.config)?);

    match cli.command {
        Command::List => {
            cfg.validate_list()?;
            cmd_list(&cfg).await?;
            Ok(0)
        }
        Command::Migrate { bucket } => {
            let mut cfg = (*cfg).clone();
            if let Some(b) = bucket {
                cfg.bucket = Some(b);
            }
            let cfg = Arc::new(cfg);
            cfg.validate_migrate()?;
            Ok(cmd_migrate(&cfg, token).await?)
        }
        Command::Status { bucket } => {
            let mut cfg = (*cfg).clone();
            if let Some(b) = bucket {
                cfg.bucket = Some(b);
            }
            let cfg = Arc::new(cfg);
            cmd_status(&cfg).await?;
            Ok(0)
        }
    }
}

// --- rf list ----------------------------------------------------------------

async fn list_buckets(old: &Client) -> Result<Vec<(String, String)>> {
    let mut out = Vec::new();
    let mut pages = old.list_buckets().into_paginator().send();
    while let Some(page) = pages.next().await {
        let page = page.map_err(|e| anyhow!("list buckets: {e}"))?;
        for b in page.buckets() {
            let name = b.name().unwrap_or_default().to_string();
            let created = b
                .creation_date()
                .map(|t| {
                    chrono::DateTime::from_timestamp(t.secs(), 0)
                        .unwrap_or_default()
                        .to_rfc3339_opts(chrono::SecondsFormat::Secs, true)
                })
                .unwrap_or_else(|| "-".into());
            out.push((name, created));
        }
    }
    Ok(out)
}

async fn cmd_list(cfg: &Arc<Config>) -> Result<()> {
    let client = s3::new_client(&cfg.old);
    let buckets = list_buckets(&client).await?;
    if buckets.is_empty() {
        println!("(source instance has no buckets)");
        return Ok(());
    }
    println!("{:<44} CREATED (UTC)", "BUCKET");
    for (name, created) in &buckets {
        println!("{:<44} {created}", name);
    }
    println!("\n{} bucket(s) on {}", buckets.len(), cfg.old.endpoint);
    Ok(())
}

// --- rf migrate --------------------------------------------------------------

async fn cmd_migrate(cfg: &Arc<Config>, token: CancellationToken) -> Result<i32> {
    let old = s3::new_client(&cfg.old);
    let new = s3::new_client(&cfg.new);

    let buckets: Vec<String> = match &cfg.bucket {
        Some(b) => vec![b.clone()],
        None => list_buckets(&old)
            .await?
            .into_iter()
            .map(|(name, _)| name)
            .collect(),
    };
    if buckets.is_empty() {
        println!("source instance has no buckets to migrate");
        return Ok(0);
    }

    println!(
        "migrating {} bucket(s): {} → {}",
        buckets.len(),
        cfg.old.endpoint,
        cfg.new.endpoint
    );
    println!(
        "tuning: workers={} multipart_threshold={} part_size={} part_concurrency={} (resumable; re-run to retry failures)",
        cfg.workers,
        migrator::fmt_bytes(cfg.multipart_threshold),
        migrator::fmt_bytes(cfg.multipart_part_size),
        cfg.multipart_concurrency,
    );

    let mp = MultiProgress::new();
    let overall = mp.add(
        ProgressBar::new(buckets.len() as u64).with_style(
            indicatif::ProgressStyle::with_template(
                "[{elapsed_precise}] {bar:30.magenta/blue} {pos}/{len} buckets {msg}",
            )
            .unwrap(),
        ),
    );

    let mut summaries: Vec<BucketSummary> = Vec::new();
    let mut failed_buckets: Vec<String> = Vec::new();

    for bucket in &buckets {
        if token.is_cancelled() {
            break;
        }
        overall.set_message(bucket.clone());
        match migrator::migrate_bucket(cfg, &old, &new, bucket, &token, &mp, Some(&overall)).await
        {
            Ok(summary) => summaries.push(summary),
            Err(err) => {
                migrator::log_line(&mp, format!("✗ bucket {bucket}: {err:#}"));
                failed_buckets.push(bucket.clone());
                if token.is_cancelled() {
                    break;
                }
            }
        }
    }
    overall.finish_and_clear();

    // Final report.
    println!("\n=== migration summary ===");
    println!(
        "{:<30} {:>9} {:>9} {:>9} {:>9} {:>9} {:>12}",
        "BUCKET", "SCANNED", "COPIED", "CONFLICT", "SKIPPED", "FAILED", "TRANSFERRED"
    );
    let mut tot = BucketTotals::default();
    for s in &summaries {
        println!(
            "{:<30} {:>9} {:>9} {:>9} {:>9} {:>9} {:>12}",
            truncate(&s.bucket, 30),
            s.scanned,
            s.copied,
            s.conflicts,
            s.skipped,
            s.failed,
            migrator::fmt_bytes(s.bytes)
        );
        tot.add(s);
    }
    println!(
        "{:<30} {:>9} {:>9} {:>9} {:>9} {:>9} {:>12}",
        "TOTAL", tot.scanned, tot.copied, tot.conflicts, tot.skipped, tot.failed, tot.bytes_str
    );

    if token.is_cancelled() {
        println!("\ninterrupted — state is saved, re-run the same command to resume");
        return Ok(130);
    }
    if !failed_buckets.is_empty() {
        println!("\nfailed buckets: {}", failed_buckets.join(", "));
        return Ok(1);
    }
    Ok(0)
}

#[derive(Default)]
struct BucketTotals {
    scanned: u64,
    copied: u64,
    conflicts: u64,
    skipped: u64,
    failed: u64,
    bytes: u64,
    bytes_str: String,
}

impl BucketTotals {
    fn add(&mut self, s: &BucketSummary) {
        self.scanned += s.scanned;
        self.copied += s.copied;
        self.conflicts += s.conflicts;
        self.skipped += s.skipped;
        self.failed += s.failed;
        self.bytes += s.bytes;
        self.bytes_str = migrator::fmt_bytes(self.bytes);
    }
}

fn truncate(s: &str, max: usize) -> String {
    if s.chars().count() <= max {
        s.to_string()
    } else {
        let cut: String = s.chars().take(max - 1).collect();
        format!("{cut}…")
    }
}

// --- rf status ---------------------------------------------------------------

async fn cmd_status(cfg: &Arc<Config>) -> Result<()> {
    let targets: Vec<(String, PathBuf)> = match &cfg.bucket {
        Some(bucket) => vec![(bucket.clone(), cfg.db_path.clone())],
        None => {
            let dir = cfg
                .db_path
                .parent()
                .filter(|p| !p.as_os_str().is_empty())
                .unwrap_or_else(|| std::path::Path::new("."));
            let mut found = Vec::new();
            let mut entries = tokio::fs::read_dir(dir).await?;
            while let Some(entry) = entries.next_entry().await? {
                let name = entry.file_name().to_string_lossy().to_string();
                if let Some(bucket) = name
                    .strip_prefix("rf-")
                    .and_then(|rest| rest.strip_suffix(".db"))
                {
                    found.push((bucket.to_string(), entry.path()));
                }
            }
            found.sort();
            found
        }
    };

    if targets.is_empty() {
        println!("no state DBs found — run `rf migrate` first");
        return Ok(());
    }

    println!(
        "{:<30} {:>9} {:>9} {:>9} {:>9} {:>9} {:>9}",
        "BUCKET", "TOTAL", "PENDING", "DONE", "CONFLICT", "SKIPPED", "FAILED"
    );
    let mut any = false;
    for (bucket, db) in &targets {
        match store::Store::open(db, bucket) {
            Ok(st) => {
                let c = st.counts()?;
                any = true;
                println!(
                    "{:<30} {:>9} {:>9} {:>9} {:>9} {:>9} {:>9}",
                    truncate(bucket, 30),
                    c.total,
                    c.pending,
                    c.done,
                    c.conflict,
                    c.skipped,
                    c.failed
                );
            }
            Err(err) => println!("{bucket}: {err:#}"),
        }
    }
    if !any {
        println!("(all state DBs empty)");
    }
    Ok(())
}

// --- signals -----------------------------------------------------------------

/// First SIGINT/SIGTERM cancels gracefully (state persists, multipart uploads
/// are aborted); a second signal force-exits with 130.
fn install_signal_handler(token: CancellationToken) {
    tokio::spawn(async move {
        let mut term = match tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
        {
            Ok(rx) => rx,
            Err(_) => return,
        };
        tokio::select! {
            _ = tokio::signal::ctrl_c() => {}
            _ = term.recv() => {}
        }
        eprintln!("\nreceived signal, shutting down gracefully… (re-run to resume)");
        token.cancel();
        tokio::select! {
            _ = tokio::signal::ctrl_c() => {}
            _ = term.recv() => {}
        }
        eprintln!("received second signal, forcing exit");
        std::process::exit(130);
    });
}
