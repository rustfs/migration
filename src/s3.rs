//! S3 client construction for self-hosted S3-compatible endpoints
//! (RustFS / MinIO style): static credentials, forced path-style addressing.

use aws_sdk_s3::config::{BehaviorVersion, Credentials, Region};
use aws_sdk_s3::error::SdkError;
use aws_sdk_s3::Client;

use crate::config::Endpoint;

/// Build a client for one endpoint. One client per endpoint, shared across
/// all workers — the SDK's HTTP transport pools connections internally.
pub fn new_client(ep: &Endpoint) -> Client {
    let creds = Credentials::new(&ep.access_key, &ep.secret_key, None, None, "rf");
    let cfg = aws_sdk_s3::Config::builder()
        .behavior_version(BehaviorVersion::latest())
        .credentials_provider(creds)
        .region(Region::new(ep.region.clone()))
        .endpoint_url(&ep.endpoint)
        .force_path_style(true)
        .build();
    Client::from_conf(cfg)
}

/// True when the error is a 404 from either side: `NoSuchKey` (GET) or
/// `NotFound` (HEAD). Falls back to matching the message because some
/// S3-compatible servers only surface the code in text form.
pub fn is_not_found<E>(err: &SdkError<E>) -> bool
where
    E: std::fmt::Debug + std::fmt::Display,
{
    if let Some(raw) = err.raw_response() {
        return raw.status().as_u16() == 404;
    }
    let msg = err.to_string();
    msg.contains("NoSuchKey") || msg.contains("NotFound") || msg.contains("404")
}

/// Strip quotes from an ETag and report whether it belongs to a multipart
/// upload (contains a `-N` suffix), whose value is not portable across
/// clusters.
pub fn etag_kind(etag: &str) -> (String, bool) {
    let normalized = etag.trim_matches('"').to_string();
    let multipart = normalized.contains('-');
    (normalized, multipart)
}
