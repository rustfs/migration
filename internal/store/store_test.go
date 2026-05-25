package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenRejectsBucketSwap(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "migration.db")

	st, err := Open(dbPath, "bucket-A")
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Re-opening with the same bucket must succeed (resume path).
	st, err = Open(dbPath, "bucket-A")
	if err != nil {
		t.Fatalf("reopen same bucket: %v", err)
	}
	_ = st.Close()

	// Re-opening with a different bucket must fail loudly instead of
	// silently mixing two buckets' inventories into the same SQLite file.
	if _, err := Open(dbPath, "bucket-B"); err == nil {
		t.Fatalf("expected error when reusing db_path with a different bucket")
	} else if !strings.Contains(err.Error(), "bucket-A") || !strings.Contains(err.Error(), "bucket-B") {
		t.Fatalf("error %q should mention both bucket names", err)
	}
}

func TestOpenRejectsEmptyBucket(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "x.db"), ""); err == nil {
		t.Fatalf("expected error for empty bucket")
	}
}
