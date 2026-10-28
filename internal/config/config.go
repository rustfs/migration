package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
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
	c.applyEnv()
	c.applyDefaults()
	c.normalize()
	if err := c.ValidateNames(); err != nil {
		return nil, err
	}
	if err := c.validateMultipartPartSize(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) applyEnv() {
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
	if c.Workers <= 0 {
		c.Workers = 16
	}
	if c.MultipartThreshold <= 0 {
		c.MultipartThreshold = 50 * 1024 * 1024
	}
	if c.MultipartPartSize <= 0 {
		c.MultipartPartSize = 16 * 1024 * 1024
	}
	if c.MultipartConcurrency <= 0 {
		c.MultipartConcurrency = 4
	}
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
