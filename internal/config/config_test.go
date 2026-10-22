package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLoadAppliesEnvironmentOverrides(t *testing.T) {
	t.Setenv("OLD_ENDPOINT", "http://old-env:9000")
	t.Setenv("OLD_AK", "old-env-ak")
	t.Setenv("OLD_SK", "old-env-sk")
	t.Setenv("NEW_ENDPOINT", "http://new-env:9000")
	t.Setenv("NEW_AK", "new-env-ak")
	t.Setenv("NEW_SK", "new-env-sk")
	t.Setenv("REGION", "env-region")
	t.Setenv("BUCKET", "env-bucket")
	t.Setenv("DB_PATH", "env.db")
	t.Setenv("CONFLICT_LOG", "env-conflict.log")

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
old:
  endpoint: "http://old-file:9000"
  access_key: "old-file-ak"
  secret_key: "old-file-sk"
new:
  endpoint: "http://new-file:9000"
  access_key: "new-file-ak"
  secret_key: "new-file-sk"
bucket: "file-bucket"
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.Old.Endpoint != "http://old-env:9000" || cfg.Old.AccessKey != "old-env-ak" || cfg.Old.SecretKey != "old-env-sk" {
		t.Fatalf("old env override failed: %+v", cfg.Old)
	}
	if cfg.New.Endpoint != "http://new-env:9000" || cfg.New.AccessKey != "new-env-ak" || cfg.New.SecretKey != "new-env-sk" {
		t.Fatalf("new env override failed: %+v", cfg.New)
	}
	if cfg.Old.Region != "env-region" || cfg.New.Region != "env-region" {
		t.Fatalf("shared region override failed: old=%q new=%q", cfg.Old.Region, cfg.New.Region)
	}
	if cfg.Bucket != "env-bucket" || cfg.DBPath != "env.db" || cfg.ConflictLog != "env-conflict.log" {
		t.Fatalf("top-level env override failed: bucket=%q db=%q conflict=%q", cfg.Bucket, cfg.DBPath, cfg.ConflictLog)
	}
}

func TestOptionalBucketMeansAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
old:
  endpoint: "http://old:9000/"
  access_key: a
  secret_key: b
new:
  endpoint: "http://new:9000"
  access_key: c
  secret_key: d
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AllBuckets() {
		t.Fatalf("expected all-buckets mode, got %q", cfg.Bucket)
	}
	if cfg.Old.Endpoint != "http://old:9000" {
		t.Fatalf("trailing slash not stripped: %q", cfg.Old.Endpoint)
	}
	if cfg.Workers != 16 || cfg.MultipartThreshold != 50*1024*1024 {
		t.Fatalf("defaults: workers=%d threshold=%d", cfg.Workers, cfg.MultipartThreshold)
	}
	if cfg.DBPathFor("photos") != "migration-photos.db" {
		t.Fatalf("derived db path: %s", cfg.DBPathFor("photos"))
	}
	if cfg.ConflictLogFor("photos") != "conflict-photos.log" {
		t.Fatalf("derived conflict path: %s", cfg.ConflictLogFor("photos"))
	}
}

func TestSingleBucketUsesConfiguredPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
old: {endpoint: "http://a:1", access_key: a, secret_key: b}
new: {endpoint: "http://b:2", access_key: c, secret_key: d}
bucket: photos
db_path: /state/migration.db
conflict_log: /state/conflict.log
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBPathFor("photos") != "/state/migration.db" {
		t.Fatalf("got %s", cfg.DBPathFor("photos"))
	}
	if cfg.ConflictLogFor("photos") != "/state/conflict.log" {
		t.Fatalf("got %s", cfg.ConflictLogFor("photos"))
	}
}

func TestRejectsBadBucketName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
old: {endpoint: "http://a:1", access_key: a, secret_key: b}
bucket: a/b
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error")
	}
}

func TestValidateSourceVsPair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
old: {endpoint: "http://a:1", access_key: a, secret_key: b}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateSource(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidatePair(); err == nil {
		t.Fatal("expected pair validation to fail")
	}
}

func TestMultipartPartSizeMinimum(t *testing.T) {
	const base = `
old:
  endpoint: "http://old:9000"
  access_key: a
  secret_key: b
new:
  endpoint: "http://new:9000"
  access_key: c
  secret_key: d
`

	t.Run("default stays 16MiB", func(t *testing.T) {
		for _, body := range []string{base, base + "multipart_part_size: 0\n"} {
			cfg := loadPartSizeYAML(t, body)
			if cfg.MultipartPartSize != 16*1024*1024 {
				t.Fatalf("default part size = %d, want %d", cfg.MultipartPartSize, 16*1024*1024)
			}
		}
	})

	t.Run("explicit value at or above minimum is kept", func(t *testing.T) {
		cfg := loadPartSizeYAML(t, base+"multipart_part_size: 5242880\n")
		if cfg.MultipartPartSize != MinMultipartPartSize {
			t.Fatalf("part size = %d, want %d", cfg.MultipartPartSize, MinMultipartPartSize)
		}
		cfg = loadPartSizeYAML(t, base+"multipart_part_size: 16777216\n")
		if cfg.MultipartPartSize != 16*1024*1024 {
			t.Fatalf("part size = %d, want 16MiB", cfg.MultipartPartSize)
		}
	})

	t.Run("below 5MiB fails load", func(t *testing.T) {
		for _, size := range []int64{1, 1024 * 1024, MinMultipartPartSize - 1} {
			path := filepath.Join(t.TempDir(), "config.yaml")
			body := base + "multipart_part_size: " + strconv.FormatInt(size, 10) + "\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil {
				t.Fatalf("part size %d: expected error", size)
			}
			msg := err.Error()
			if !strings.Contains(msg, "5242880") || !strings.Contains(msg, "EntityTooSmall") {
				t.Fatalf("part size %d: error %q missing minimum or EntityTooSmall", size, msg)
			}
		}
	})
}

func loadPartSizeYAML(t *testing.T, body string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestApplyBucketFlag(t *testing.T) {
	c := &Config{}
	c.ApplyBucketFlag(" photos ")
	if c.Bucket != "photos" {
		t.Fatalf("got %q", c.Bucket)
	}
}

func TestConcurrencyYAMLAndDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
old: {endpoint: "http://a:1", access_key: a, secret_key: b}
new: {endpoint: "http://b:2", access_key: c, secret_key: d}
workers: 8
multipart_concurrency: 2
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workers != 8 || cfg.MultipartConcurrency != 2 {
		t.Fatalf("yaml concurrency lost: workers=%d parts=%d", cfg.Workers, cfg.MultipartConcurrency)
	}
	if cfg.MaxInFlight() != 16 {
		t.Fatalf("max in flight: %d", cfg.MaxInFlight())
	}
}

func TestConcurrencyEnvOverridesYAML(t *testing.T) {
	t.Setenv("WORKERS", "12")
	t.Setenv("MULTIPART_CONCURRENCY", "6")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
old: {endpoint: "http://a:1", access_key: a, secret_key: b}
workers: 3
multipart_concurrency: 1
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workers != 12 || cfg.MultipartConcurrency != 6 {
		t.Fatalf("env override failed: workers=%d parts=%d", cfg.Workers, cfg.MultipartConcurrency)
	}
	if err := cfg.ApplyConcurrencyOverrides(9, 3, true, true); err != nil {
		t.Fatal(err)
	}
	if cfg.Workers != 9 || cfg.MultipartConcurrency != 3 {
		t.Fatalf("cli should win: workers=%d parts=%d", cfg.Workers, cfg.MultipartConcurrency)
	}
}

func TestRejectsNonPositiveConcurrency(t *testing.T) {
	cases := []string{
		"workers: -1\n",
		"multipart_concurrency: -2\n",
		"multipart_threshold: -1\n",
		"multipart_part_size: -5\n",
	}
	for _, extra := range cases {
		path := filepath.Join(t.TempDir(), "config.yaml")
		body := "old: {endpoint: \"http://a:1\", access_key: a, secret_key: b}\n" + extra
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("expected error for %q", extra)
		}
	}

	t.Setenv("WORKERS", "0")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
old: {endpoint: "http://a:1", access_key: a, secret_key: b}
workers: 4
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected WORKERS=0 to fail")
	}
}

func TestRejectsBadConcurrencyEnv(t *testing.T) {
	t.Setenv("MULTIPART_CONCURRENCY", "nope")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
old: {endpoint: "http://a:1", access_key: a, secret_key: b}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected non-integer env to fail")
	}
}

func TestApplyConcurrencyOverridesRejectsZero(t *testing.T) {
	c := &Config{Workers: 4, MultipartConcurrency: 2, MultipartThreshold: 1, MultipartPartSize: 1}
	if err := c.ApplyConcurrencyOverrides(0, 2, true, false); err == nil {
		t.Fatal("expected workers=0 to fail")
	}
	if c.Workers != 4 {
		t.Fatalf("failed override mutated workers to %d", c.Workers)
	}
	if err := c.ApplyConcurrencyOverrides(4, 0, false, true); err == nil {
		t.Fatal("expected multipart 0 to fail")
	}
}

func TestJobBufferCaps(t *testing.T) {
	if JobBuffer(1) != 2 {
		t.Fatalf("got %d", JobBuffer(1))
	}
	if JobBuffer(100000) != 2048 {
		t.Fatalf("got %d", JobBuffer(100000))
	}
}
