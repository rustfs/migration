//! Configuration loading: YAML file + environment variable overrides,
//! defaults, normalization and per-command validation.
//!
//! The file format is compatible with the original Go `rustfs-migration`
//! tool (`old`/`new` endpoints plus top-level tuning knobs).

use anyhow::{Context, Result, bail};
use serde::Deserialize;
use std::path::{Path, PathBuf};

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default)]
pub struct EndpointFile {
    pub endpoint: Option<String>,
    pub access_key: Option<String>,
    pub secret_key: Option<String>,
    pub region: Option<String>,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default)]
struct FileConfig {
    old: Option<EndpointFile>,
    new: Option<EndpointFile>,
    bucket: Option<String>,
    db_path: Option<String>,
    conflict_log: Option<String>,
    spool_dir: Option<String>,
    workers: Option<u32>,
    multipart_threshold: Option<u64>,
    multipart_part_size: Option<u64>,
    multipart_concurrency: Option<u32>,
}

/// A fully resolved S3 endpoint (credentials included).
#[derive(Debug, Clone)]
pub struct Endpoint {
    pub endpoint: String,
    pub access_key: String,
    pub secret_key: String,
    pub region: String,
}

/// Validated runtime configuration.
#[derive(Debug, Clone)]
pub struct Config {
    pub old: Endpoint,
    pub new: Endpoint,
    /// `None` means "every bucket on the source instance".
    pub bucket: Option<String>,
    pub db_path: PathBuf,
    pub conflict_log: PathBuf,
    pub spool_dir: PathBuf,
    pub workers: u32,
    pub multipart_threshold: u64,
    pub multipart_part_size: u64,
    pub multipart_concurrency: u32,
}

fn env_or(current: Option<String>, key: &str) -> Option<String> {
    match std::env::var(key) {
        Ok(v) if !v.is_empty() => Some(v),
        _ => current,
    }
}

impl Config {
    pub fn load(path: &Path) -> Result<Self> {
        let raw = std::fs::read_to_string(path).with_context(|| {
            format!("read config {}: create one from config.example.yaml first", path.display())
        })?;
        let file: FileConfig =
            serde_yaml::from_str(&raw).with_context(|| format!("parse config {}", path.display()))?;
        let old = file.old.clone().unwrap_or_default();
        let new = file.new.clone().unwrap_or_default();
        Self::from_parts(old, new, file)
    }

    /// Build a config from parsed YAML sections plus env overrides.
    fn from_parts(
        old: EndpointFile,
        new: EndpointFile,
        file: FileConfig,
    ) -> Result<Self> {
        let mut old = resolve_endpoint(old, "OLD");
        let mut new = resolve_endpoint(new, "NEW");

        // Shared REGION fallback when both per-cluster regions are empty.
        if let Ok(shared) = std::env::var("REGION") {
            if !shared.is_empty() {
                if old.region.is_none() {
                    old.region = Some(shared.clone());
                }
                if new.region.is_none() {
                    new.region = Some(shared);
                }
            }
        }

        let bucket = env_or(file.bucket, "BUCKET");
        let db_path = env_or(file.db_path, "DB_PATH").unwrap_or_else(|| "./migration.db".into());
        let conflict_log =
            env_or(file.conflict_log, "CONFLICT_LOG").unwrap_or_else(|| "./conflict.log".into());
        let spool_dir = env_or(file.spool_dir, "SPOOL_DIR");

        let db_path = PathBuf::from(db_path);
        // Small objects spool through temp files; default to the directory
        // holding the state DB so tmpfs /tmp doesn't absorb the traffic.
        let spool_dir = match spool_dir {
            Some(dir) if !dir.is_empty() => PathBuf::from(dir),
            _ => db_path
                .parent()
                .filter(|p| !p.as_os_str().is_empty())
                .map(Path::to_path_buf)
                .unwrap_or_else(|| PathBuf::from(".")),
        };

        let mut cfg = Config {
            old: finish_endpoint(old),
            new: finish_endpoint(new),
            bucket: bucket.filter(|b| !b.is_empty()),
            db_path,
            conflict_log: PathBuf::from(conflict_log),
            spool_dir,
            workers: file.workers.unwrap_or(0),
            multipart_threshold: file.multipart_threshold.unwrap_or(0),
            multipart_part_size: file.multipart_part_size.unwrap_or(0),
            multipart_concurrency: file.multipart_concurrency.unwrap_or(0),
        };

        // Defaults for tuning knobs (0 / unset -> built-in default).
        if cfg.workers == 0 {
            cfg.workers = 16;
        }
        if cfg.multipart_threshold == 0 {
            cfg.multipart_threshold = 50 * 1024 * 1024;
        }
        if cfg.multipart_part_size == 0 {
            cfg.multipart_part_size = 16 * 1024 * 1024;
        }
        if cfg.multipart_concurrency == 0 {
            cfg.multipart_concurrency = 4;
        }
        cfg.normalize();
        cfg.validate_common()?;
        Ok(cfg)
    }

    fn normalize(&mut self) {
        if let Some(b) = &self.bucket {
            self.bucket = Some(b.trim().to_string());
        }
        for ep in [&mut self.old, &mut self.new] {
            ep.endpoint = ep.endpoint.trim().trim_end_matches('/').to_string();
            ep.access_key = ep.access_key.trim().to_string();
            ep.secret_key = ep.secret_key.trim().to_string();
            ep.region = ep.region.trim().to_string();
            if ep.region.is_empty() {
                ep.region = "us-east-1".into();
            }
        }
    }

    fn validate_common(&self) -> Result<()> {
        if let Some(b) = &self.bucket {
            if b.contains(' ') || b.contains('\t') || b.contains('/') {
                bail!("bucket {b:?} must not contain whitespace or '/'");
            }
        }
        Ok(())
    }


    /// Validation for `list`: only the source endpoint must be usable.
    pub fn validate_list(&self) -> Result<()> {
        if self.old.endpoint.is_empty() {
            bail!("old.endpoint is required");
        }
        if self.old.access_key.is_empty() || self.old.secret_key.is_empty() {
            bail!("old.access_key and old.secret_key are required");
        }
        Ok(())
    }

    /// Validation for `migrate`: both endpoints, credentials, spool dir.
    pub fn validate_migrate(&self) -> Result<()> {
        self.validate_list()?;
        if self.new.endpoint.is_empty() {
            bail!("new.endpoint is required");
        }
        if self.new.access_key.is_empty() || self.new.secret_key.is_empty() {
            bail!("new.access_key and new.secret_key are required");
        }
        Ok(())
    }

    /// State DB path for a bucket. Single-bucket mode uses `db_path` verbatim
    /// (Go-tool compatible); all-buckets mode derives `<dir>/rf-<bucket>.db`.
    pub fn db_path_for(&self, bucket: &str) -> PathBuf {
        match &self.bucket {
            Some(_) => self.db_path.clone(),
            None => sibling_path(&self.db_path, &format!("rf-{bucket}.db")),
        }
    }

    /// Conflict log path for a bucket (same rule as [`Config::db_path_for`]).
    pub fn conflict_log_for(&self, bucket: &str) -> PathBuf {
        match &self.bucket {
            Some(_) => self.conflict_log.clone(),
            None => sibling_path(&self.conflict_log, &format!("rf-{bucket}.conflict.log")),
        }
    }
}

/// Resolve per-cluster env overrides: OLD_ENDPOINT/OLD_AK/OLD_SK/OLD_REGION
/// or NEW_ENDPOINT/NEW_AK/NEW_SK/NEW_REGION depending on `prefix`.
fn resolve_endpoint(ep: EndpointFile, prefix: &str) -> EndpointFile {
    EndpointFile {
        endpoint: env_or(ep.endpoint, &format!("{prefix}_ENDPOINT")),
        access_key: env_or(ep.access_key, &format!("{prefix}_AK")),
        secret_key: env_or(ep.secret_key, &format!("{prefix}_SK")),
        region: env_or(ep.region, &format!("{prefix}_REGION")),
    }
}

fn finish_endpoint(ep: EndpointFile) -> Endpoint {
    Endpoint {
        endpoint: ep.endpoint.unwrap_or_default(),
        access_key: ep.access_key.unwrap_or_default(),
        secret_key: ep.secret_key.unwrap_or_default(),
        region: ep.region.unwrap_or_default(),
    }
}

/// `<dir of base>/<file_name>`, falling back to the current directory.
fn sibling_path(base: &Path, file_name: &str) -> PathBuf {
    let dir = base.parent().filter(|p| !p.as_os_str().is_empty());
    match dir {
        Some(dir) => dir.join(file_name),
        None => PathBuf::from(file_name),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn parts(yaml: &str) -> Result<Config> {
        let file: FileConfig = serde_yaml::from_str(yaml).unwrap();
        let old = file.old.clone().unwrap_or_default();
        let new = file.new.clone().unwrap_or_default();
        Config::from_parts(old, new, file)
    }

    #[test]
    fn defaults_and_paths() {
        let cfg = parts(
            r#"
old:
  endpoint: "http://10.0.0.1:9000/"
  access_key: a
  secret_key: b
new:
  endpoint: "http://10.0.0.2:9000"
  access_key: c
  secret_key: d
"#,
        )
        .unwrap();
        assert_eq!(cfg.old.endpoint, "http://10.0.0.1:9000"); // trailing slash stripped
        assert_eq!(cfg.old.region, "us-east-1");
        assert_eq!(cfg.workers, 16);
        assert_eq!(cfg.multipart_threshold, 50 * 1024 * 1024);
        assert!(cfg.bucket.is_none());
        assert_eq!(cfg.db_path, PathBuf::from("./migration.db"));
        assert_eq!(cfg.spool_dir, PathBuf::from("."));
    }

    #[test]
    fn per_bucket_state_paths() {
        let cfg = parts(
            r#"
old: {endpoint: "http://a:1", access_key: a, secret_key: b}
new: {endpoint: "http://b:2", access_key: c, secret_key: d}
db_path: "/state/migration.db"
conflict_log: "/state/conflict.log"
"#,
        )
        .unwrap();
        assert_eq!(cfg.db_path_for("photos"), PathBuf::from("/state/rf-photos.db"));
        assert_eq!(
            cfg.conflict_log_for("photos"),
            PathBuf::from("/state/rf-photos.conflict.log")
        );
    }

    #[test]
    fn single_bucket_uses_configured_paths() {
        let cfg = parts(
            r#"
old: {endpoint: "http://a:1", access_key: a, secret_key: b}
new: {endpoint: "http://b:2", access_key: c, secret_key: d}
bucket: "photos"
db_path: "/state/migration.db"
conflict_log: "/state/conflict.log"
"#,
        )
        .unwrap();
        assert_eq!(cfg.db_path_for("photos"), PathBuf::from("/state/migration.db"));
        assert_eq!(cfg.conflict_log_for("photos"), PathBuf::from("/state/conflict.log"));
    }

    #[test]
    fn rejects_bad_bucket_name() {
        let err = parts(
            r#"
old: {endpoint: "http://a:1", access_key: a, secret_key: b}
new: {endpoint: "http://b:2", access_key: c, secret_key: d}
bucket: "a/b"
"#,
        )
        .unwrap_err();
        assert!(err.to_string().contains("whitespace or '/'"));
    }

    #[test]
    fn list_validation_only_needs_old() {
        let cfg = parts(
            r#"
old: {endpoint: "http://a:1", access_key: a, secret_key: b}
"#,
        )
        .unwrap();
        cfg.validate_list().unwrap();
        assert!(cfg.validate_migrate().is_err());
    }
}
