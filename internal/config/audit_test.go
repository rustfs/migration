package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Unknown configuration keys must not silently expand a requested single-bucket
// run to every bucket. This is an operator typo, not a request for all buckets.
func TestAuditRejectMisspelledBucketSetting(t *testing.T) {
	t.Setenv("BUCKET", "")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("buckte: production-photos\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err == nil {
		t.Fatalf("unknown bucket field accepted; all-buckets mode=%t", cfg.AllBuckets())
	}
}

func TestAuditJobBufferCannotOverflow(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	if n := JobBuffer(maxInt); n < 1 || n > 2048 {
		t.Fatalf("workers=%d produced invalid channel capacity=%d", maxInt, n)
	}
}
