package cmd

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/rustfs/migration/internal/fakes3"
)

// TestCLIRateLimitSharedAcrossWorkers drives cobra run with workers > 1 and
// a real --rate-limit. Timing catches a per-worker bucket; MaxGetInFlight
// catches the CLI workers flag being ignored (one worker is also ~5s).
func TestCLIRateLimitSharedAcrossWorkers(t *testing.T) {
	lm := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	payload := bytes.Repeat([]byte("z"), 200_000)
	oldS3 := fakes3.NewWithObjects(t, "photos", map[string][]byte{
		"a.bin":        payload,
		"b.bin":        append([]byte(nil), payload...),
		"conflict.txt": []byte("source-must-not-win"),
	}, lm)
	defer oldS3.Close()
	oldS3.SetGetDelay(30 * time.Millisecond)

	newS3 := fakes3.NewWithObjects(t, "photos", map[string][]byte{
		"conflict.txt": []byte("target-must-stay"),
	}, lm.Add(time.Hour))
	defer newS3.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeClusterConfig(t, cfgPath, oldS3.URL, newS3.URL, filepath.Join(dir, "migration.db"), filepath.Join(dir, "conflict.log"), dir, `
bucket: photos
workers: 1
multipart_concurrency: 1
multipart_threshold: 1048576
multipart_part_size: 16777216
rate_limit: 0
`)

	start := time.Now()
	out, err := execRoot(t, "run", "-c", cfgPath, "--workers", "2", "--rate-limit", "80KB")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if got := oldS3.MaxGetInFlight(); got < 2 {
		t.Fatalf("--workers 2 max in-flight GETs=%d", got)
	}
	// 400000 payload bytes at 80KB/s (80000). A private limiter per worker
	// finishes near 2.5s. The shared cap has to take longer.
	if elapsed < 3200*time.Millisecond {
		t.Fatalf("elapsed %s, --rate-limit was not a shared cap", elapsed)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("elapsed %s, --rate-limit stalled the copy", elapsed)
	}
	for _, key := range []string{"a.bin", "b.bin"} {
		body, ok := newS3.Object("photos", key)
		if !ok || !bytes.Equal(body, payload) {
			t.Fatalf("%s: ok=%v len=%d", key, ok, len(body))
		}
	}
	kept, ok := newS3.Object("photos", "conflict.txt")
	if !ok || !bytes.Equal(kept, []byte("target-must-stay")) {
		t.Fatalf("conflict overwritten: ok=%v body=%q", ok, kept)
	}
	if calls := newS3.PutObjectCalls("conflict.txt"); calls != 0 {
		t.Fatalf("conflict.txt put calls=%d", calls)
	}
	assertNoUnsafeCalls(t, oldS3, newS3)
}

// TestCLIRateLimitZeroOverridesYAML proves --rate-limit 0 reaches the copy
// path and means unlimited, even when YAML names a cap and workers > 1.
func TestCLIRateLimitZeroOverridesYAML(t *testing.T) {
	lm := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	payload := bytes.Repeat([]byte("q"), 40_000)
	oldS3 := fakes3.NewWithObjects(t, "photos", map[string][]byte{
		"a.bin": payload,
		"b.bin": append([]byte(nil), payload...),
	}, lm)
	defer oldS3.Close()
	oldS3.SetGetDelay(30 * time.Millisecond)
	newS3 := fakes3.New(t)
	defer newS3.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeClusterConfig(t, cfgPath, oldS3.URL, newS3.URL, filepath.Join(dir, "migration.db"), filepath.Join(dir, "conflict.log"), dir, `
bucket: photos
workers: 1
multipart_concurrency: 1
multipart_threshold: 1048576
rate_limit: 20000
`)

	start := time.Now()
	out, err := execRoot(t, "run", "-c", cfgPath, "--workers", "2", "--rate-limit", "0")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if got := oldS3.MaxGetInFlight(); got < 2 {
		t.Fatalf("--workers 2 with --rate-limit 0 max in-flight GETs=%d", got)
	}
	// 80000 bytes at the YAML 20000 B/s cap would take about 4s.
	if elapsed > 2*time.Second {
		t.Fatalf("--rate-limit 0 still throttled the copy: %s", elapsed)
	}
	for _, key := range []string{"a.bin", "b.bin"} {
		body, ok := newS3.Object("photos", key)
		if !ok || !bytes.Equal(body, payload) {
			t.Fatalf("%s: ok=%v len=%d", key, ok, len(body))
		}
	}
	assertNoUnsafeCalls(t, oldS3, newS3)
}
