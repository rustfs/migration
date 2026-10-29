package migrator

import (
	"bytes"
	"context"
	"path"
	"testing"
	"time"

	appconfig "github.com/rustfs/migration/internal/config"
	"github.com/rustfs/migration/internal/fakes3"
	"github.com/rustfs/migration/internal/scanner"
	"github.com/rustfs/migration/internal/store"
)

// Two workers copy two multipart objects through one limiter. A per-worker
// cap would finish near the time of a single object; the shared cap has to
// take about the sum.
func TestRateLimitSharedAcrossCopyWorkers(t *testing.T) {
	ctx := context.Background()
	lm := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	payload := bytes.Repeat([]byte("z"), 200_000)
	oldS3 := fakes3.NewWithObjects(t, "bucket", map[string][]byte{
		"a.bin":        payload,
		"b.bin":        append([]byte(nil), payload...),
		"conflict.txt": []byte("source-must-not-win"),
	}, lm)
	defer oldS3.Close()
	newS3 := fakes3.NewWithObjects(t, "bucket", map[string][]byte{
		"conflict.txt": []byte("target-must-stay"),
	}, lm.Add(time.Hour))
	defer newS3.Close()

	oldClient := newTestClient(t, ctx, oldS3.URL)
	newClient := newTestClient(t, ctx, newS3.URL)
	dir := t.TempDir()
	dbPath := path.Join(dir, "migration.db")
	conflictPath := path.Join(dir, "conflict.log")
	st, err := store.Open(dbPath, "bucket")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := scanner.Run(ctx, oldClient, "bucket", st); err != nil {
		t.Fatal(err)
	}

	cfg := testCfg(dbPath, conflictPath, dir)
	cfg.Workers = 2
	cfg.MultipartConcurrency = 2
	cfg.MultipartThreshold = 40_000
	cfg.MultipartPartSize = 40_000
	cfg.RateLimit = 80_000 // 80000 B/s shared across every worker and part

	m, err := New(cfg, oldClient, newClient, st)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	start := time.Now()
	if err := m.Run(ctx); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	assertObject(t, newS3, "a.bin", payload)
	assertObject(t, newS3, "b.bin", payload)
	assertObject(t, newS3, "conflict.txt", []byte("target-must-stay"))
	if oldS3.DeleteObjectCalls() != 0 || newS3.DeleteObjectCalls() != 0 || newS3.CopyObjectCalls() != 0 {
		t.Fatalf("unsafe API: oldDelete=%d newDelete=%d copy=%d",
			oldS3.DeleteObjectCalls(), newS3.DeleteObjectCalls(), newS3.CopyObjectCalls())
	}
	// 400000 payload bytes at 80000 B/s. One private limiter per worker
	// would land near 2.5s. The shared bucket has to take longer.
	if elapsed < 3200*time.Millisecond {
		t.Fatalf("elapsed %s, rate limit was not shared across workers", elapsed)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("elapsed %s, rate limit stalled the copy", elapsed)
	}
}

func TestUnlimitedCopyStaysFast(t *testing.T) {
	ctx := context.Background()
	lm := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	payload := bytes.Repeat([]byte("q"), 150_000)
	oldS3 := fakes3.NewWithObjects(t, "bucket", map[string][]byte{"a.bin": payload}, lm)
	defer oldS3.Close()
	newS3 := fakes3.NewWithObjects(t, "bucket", map[string][]byte{}, lm)
	defer newS3.Close()
	oldClient := newTestClient(t, ctx, oldS3.URL)
	newClient := newTestClient(t, ctx, newS3.URL)
	dir := t.TempDir()
	st, err := store.Open(path.Join(dir, "migration.db"), "bucket")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := scanner.Run(ctx, oldClient, "bucket", st); err != nil {
		t.Fatal(err)
	}
	cfg := testCfg(path.Join(dir, "migration.db"), path.Join(dir, "conflict.log"), dir)
	cfg.Workers = 2
	cfg.MultipartThreshold = 1024 * 1024
	cfg.MultipartPartSize = 1024 * 1024
	cfg.RateLimit = 0
	m, err := New(cfg, oldClient, newClient, st)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	start := time.Now()
	if err := m.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("unlimited copy took %s", time.Since(start))
	}
	assertObject(t, newS3, "a.bin", payload)
	if cfg.RateLimit != appconfig.ByteRate(0) {
		t.Fatalf("rate %d", cfg.RateLimit)
	}
}
