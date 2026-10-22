package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// DefaultWorkers is the object-level pool size used by run/migrate/diff
	// when YAML omits workers (or sets 0).
	DefaultWorkers = 16
	// DefaultMultipartConcurrency is the per-object part parallelism when
	// multipart_concurrency is omitted or 0.
	DefaultMultipartConcurrency = 4
)

type Endpoint struct {
	Endpoint  string `yaml:"endpoint"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	Region    string `yaml:"region"`
}

type Config struct {
	Old Endpoint `yaml:"old"`
	New Endpoint `yaml:"new"`

	// Bucket is optional. Empty means "every bucket on the source".
	Bucket      string `yaml:"bucket"`
	DBPath      string `yaml:"db_path"`
	ConflictLog string `yaml:"conflict_log"`
	SpoolDir    string `yaml:"spool_dir"`

	Workers              int   `yaml:"workers"`
	MultipartThreshold   int64 `yaml:"multipart_threshold"`
	MultipartPartSize    int64 `yaml:"multipart_part_size"`
	MultipartConcurrency int   `yaml:"multipart_concurrency"`

	// RateLimit is the aggregate source-read cap in bytes per second, shared
	// by every copy worker (including multipart parts). 0 means unlimited.
	// YAML 0 and a missing field both stay unlimited; this is not rewritten
	// to a default the way workers is.
	RateLimit ByteRate `yaml:"rate_limit"`
}

// MinMultipartPartSize is the S3 minimum for every part except the last
// (5 MiB). RustFS and other S3-compatible targets reject a smaller non-final
// part with EntityTooSmall.
//
// An undersized value fails at config load instead of being clamped. Other
// knobs are filled in only when unset; none of them are rewritten. The
// per-object adjustment that keeps an upload under 10 000 parts only grows
// an already-valid part size, so it is not a reason to silently raise a
// value the operator chose.
const MinMultipartPartSize int64 = 5 * 1024 * 1024

// Load parses YAML, applies env overrides and defaults. It does not require
// every field: each command validates what it needs.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: copy config.example.yaml first: %w", path, err)
	}
	c := &Config{}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.applyEnv(); err != nil {
		return nil, err
	}
	c.applyDefaults()
	c.normalize()
	if err := c.ValidateNames(); err != nil {
		return nil, err
	}
	if err := c.ValidateConcurrency(); err != nil {
		return nil, err
	}
	if err := c.validateMultipartPartSize(); err != nil {
		return nil, err
	}
	if err := c.ValidateRateLimit(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) applyEnv() error {
	c.Old.Endpoint = envOr(c.Old.Endpoint, "OLD_ENDPOINT")
	c.Old.AccessKey = envOr(c.Old.AccessKey, "OLD_AK")
	c.Old.SecretKey = envOr(c.Old.SecretKey, "OLD_SK")
	c.Old.Region = envOr(c.Old.Region, "OLD_REGION")
	c.New.Endpoint = envOr(c.New.Endpoint, "NEW_ENDPOINT")
	c.New.AccessKey = envOr(c.New.AccessKey, "NEW_AK")
	c.New.SecretKey = envOr(c.New.SecretKey, "NEW_SK")
	c.New.Region = envOr(c.New.Region, "NEW_REGION")
	sharedRegion := os.Getenv("REGION")
	if sharedRegion != "" {
		if c.Old.Region == "" {
			c.Old.Region = sharedRegion
		}
		if c.New.Region == "" {
			c.New.Region = sharedRegion
		}
	}
	c.Bucket = envOr(c.Bucket, "BUCKET")
	c.DBPath = envOr(c.DBPath, "DB_PATH")
	c.ConflictLog = envOr(c.ConflictLog, "CONFLICT_LOG")
	c.SpoolDir = envOr(c.SpoolDir, "SPOOL_DIR")
	if err := applyPositiveIntEnv(&c.Workers, "WORKERS"); err != nil {
		return err
	}
	if err := applyPositiveIntEnv(&c.MultipartConcurrency, "MULTIPART_CONCURRENCY"); err != nil {
		return err
	}
	if err := applyByteRateEnv(&c.RateLimit, "RATE_LIMIT"); err != nil {
		return err
	}
	return nil
}

// applyPositiveIntEnv overrides dst when key is set. Empty means "leave YAML".
// Zero and negative values are rejected so an explicit env typo cannot fall
// through to the default.
func applyPositiveIntEnv(dst *int, key string) error {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("env %s=%q is not an integer", key, raw)
	}
	if n < 1 {
		return fmt.Errorf("env %s must be >= 1, got %d", key, n)
	}
	*dst = n
	return nil
}

// applyByteRateEnv overrides dst when key is set. Empty means "leave YAML".
// 0 is a real value (unlimited) and replaces a YAML cap. Negative values and
// unknown suffixes are rejected.
func applyByteRateEnv(dst *ByteRate, key string) error {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	n, err := ParseByteRate(raw)
	if err != nil {
		return fmt.Errorf("env %s: %w", key, err)
	}
	*dst = ByteRate(n)
	return nil
}

func envOr(current, key string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return current
}

func (c *Config) applyDefaults() {
	if c.DBPath == "" {
		c.DBPath = "./migration.db"
	}
	if c.ConflictLog == "" {
		c.ConflictLog = "./conflict.log"
	}
	// 0 (omitted) means "use the default". Negatives are left in place so
	// ValidateConcurrency can reject them instead of silently rewriting a typo.
	if c.Workers == 0 {
		c.Workers = DefaultWorkers
	}
	if c.MultipartThreshold == 0 {
		c.MultipartThreshold = 50 * 1024 * 1024
	}
	if c.MultipartPartSize == 0 {
		c.MultipartPartSize = 16 * 1024 * 1024
	}
	if c.MultipartConcurrency == 0 {
		c.MultipartConcurrency = DefaultMultipartConcurrency
	}
	// RateLimit 0 (omitted) stays 0: unlimited. Do not invent a default cap.
	if c.SpoolDir == "" {
		c.SpoolDir = filepath.Dir(c.DBPath)
	}
	if c.Old.Region == "" {
		c.Old.Region = "us-east-1"
	}
	if c.New.Region == "" {
		c.New.Region = "us-east-1"
	}
}

func (c *Config) normalize() {
	c.Bucket = strings.TrimSpace(c.Bucket)
	c.Old.Endpoint = strings.TrimRight(strings.TrimSpace(c.Old.Endpoint), "/")
	c.New.Endpoint = strings.TrimRight(strings.TrimSpace(c.New.Endpoint), "/")
	c.Old.AccessKey = strings.TrimSpace(c.Old.AccessKey)
	c.Old.SecretKey = strings.TrimSpace(c.Old.SecretKey)
	c.New.AccessKey = strings.TrimSpace(c.New.AccessKey)
	c.New.SecretKey = strings.TrimSpace(c.New.SecretKey)
	c.Old.Region = strings.TrimSpace(c.Old.Region)
	c.New.Region = strings.TrimSpace(c.New.Region)
}

func (c *Config) validateMultipartPartSize() error {
	if c.MultipartPartSize < MinMultipartPartSize {
		return fmt.Errorf("multipart_part_size must be >= %d (5 MiB): non-final S3 parts smaller than that are rejected with EntityTooSmall (got %d)", MinMultipartPartSize, c.MultipartPartSize)
	}
	return nil
}

func (c *Config) ValidateNames() error {
	if c.Bucket != "" && strings.ContainsAny(c.Bucket, " \t/") {
		return fmt.Errorf("bucket %q must not contain whitespace or '/'", c.Bucket)
	}
	return nil
}

// AllBuckets reports whether the operator pinned no bucket (whole instance).
func (c *Config) AllBuckets() bool {
	return c.Bucket == ""
}

// ValidateSource is required by list / scan.
func (c *Config) ValidateSource() error {
	if c.Old.Endpoint == "" {
		return fmt.Errorf("old.endpoint is required")
	}
	if c.Old.AccessKey == "" || c.Old.SecretKey == "" {
		return fmt.Errorf("old.access_key and old.secret_key are required")
	}
	return nil
}

// ValidatePair is required by run / migrate / diff.
func (c *Config) ValidatePair() error {
	if err := c.ValidateSource(); err != nil {
		return err
	}
	if c.New.Endpoint == "" {
		return fmt.Errorf("new.endpoint is required")
	}
	if c.New.AccessKey == "" || c.New.SecretKey == "" {
		return fmt.Errorf("new.access_key and new.secret_key are required")
	}
	return nil
}

// ValidateConcurrency checks the knobs that size the worker pool and the
// per-object part semaphore. Call it after defaults and again after CLI overrides.
func (c *Config) ValidateConcurrency() error {
	if c.Workers < 1 {
		return fmt.Errorf("workers must be >= 1, got %d", c.Workers)
	}
	if c.MultipartConcurrency < 1 {
		return fmt.Errorf("multipart_concurrency must be >= 1, got %d", c.MultipartConcurrency)
	}
	if c.MultipartThreshold < 1 {
		return fmt.Errorf("multipart_threshold must be >= 1, got %d", c.MultipartThreshold)
	}
	if c.MultipartPartSize < 1 {
		return fmt.Errorf("multipart_part_size must be >= 1, got %d", c.MultipartPartSize)
	}
	return nil
}

// ApplyConcurrencyOverrides replaces workers and/or multipart concurrency when
// the matching CLI flag was explicitly set. Priority is CLI > env > YAML > default.
func (c *Config) ApplyConcurrencyOverrides(workers, multipart int, workersSet, multipartSet bool) error {
	if workersSet {
		if workers < 1 {
			return fmt.Errorf("workers must be >= 1, got %d", workers)
		}
		c.Workers = workers
	}
	if multipartSet {
		if multipart < 1 {
			return fmt.Errorf("multipart_concurrency must be >= 1, got %d", multipart)
		}
		c.MultipartConcurrency = multipart
	}
	return c.ValidateConcurrency()
}

// ValidateRateLimit rejects a negative cap. Zero is unlimited.
func (c *Config) ValidateRateLimit() error {
	if c.RateLimit < 0 {
		return fmt.Errorf("rate_limit must be >= 0, got %d", int64(c.RateLimit))
	}
	return nil
}

// ApplyRateLimitOverride replaces the aggregate byte cap when --rate-limit
// was explicitly set. Priority is CLI > env > YAML > default (unlimited).
// 0 means unlimited, including on the CLI.
func (c *Config) ApplyRateLimitOverride(raw string, set bool) error {
	if set {
		n, err := ParseByteRate(raw)
		if err != nil {
			return fmt.Errorf("rate_limit: %w", err)
		}
		c.RateLimit = ByteRate(n)
	}
	return c.ValidateRateLimit()
}

// MaxInFlight estimates per-host request fan-out: each worker may have
// MultipartConcurrency parts in flight. It sizes HTTP idle pools and is not a hard cap.
func (c *Config) MaxInFlight() int {
	w := c.Workers
	if w < 1 {
		w = 1
	}
	p := c.MultipartConcurrency
	if p < 1 {
		p = 1
	}
	if w > 100000 || p > 10000 {
		return 1024
	}
	n := w * p
	if n < 1 || n > 1024 {
		return 1024
	}
	return n
}

// JobBuffer bounds the in-memory queue in front of a worker pool.
func JobBuffer(workers int) int {
	if workers < 1 {
		workers = 1
	}
	n := workers * 2
	if n > 2048 {
		return 2048
	}
	return n
}

// ApplyBucketFlag overrides YAML/env when --bucket is passed.
func (c *Config) ApplyBucketFlag(flag string) {
	flag = strings.TrimSpace(flag)
	if flag != "" {
		c.Bucket = flag
	}
}

// DBPathFor returns the state DB for one bucket. Single-bucket mode uses
// db_path verbatim; all-buckets mode derives migration-<bucket>.db next to it.
func (c *Config) DBPathFor(bucket string) string {
	if !c.AllBuckets() {
		return c.DBPath
	}
	return sibling(c.DBPath, "migration-"+bucket+".db")
}

// ConflictLogFor follows the same rule as DBPathFor.
func (c *Config) ConflictLogFor(bucket string) string {
	if !c.AllBuckets() {
		return c.ConflictLog
	}
	return sibling(c.ConflictLog, "conflict-"+bucket+".log")
}

func sibling(base, name string) string {
	dir := filepath.Dir(base)
	if dir == "" || dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}
