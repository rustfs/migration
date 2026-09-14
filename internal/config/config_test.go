package config

import (
	"os"
	"path/filepath"
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

func TestApplyBucketFlag(t *testing.T) {
	c := &Config{}
	c.ApplyBucketFlag(" photos ")
	if c.Bucket != "photos" {
		t.Fatalf("got %q", c.Bucket)
	}
}
