package cmd

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rustfs/migration/internal/fakes3"
	"github.com/rustfs/migration/internal/store"
)

// TestCLIRunMigrateDiffWorkers drives the real cobra commands against the
// in-process S3 stand-in. YAML asks for one worker; the flags raise it.
// That is the regression for "the flag is what the copy and diff pools use",
// plus the never-overwrite / never-delete invariants.
func TestCLIRunMigrateDiffWorkers(t *testing.T) {
	lm := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	oldS3 := fakes3.NewWithObjects(t, "photos", map[string][]byte{
		"a.txt":        []byte("alpha"),
		"b.txt":        []byte("bravo"),
		"c.txt":        []byte("charlie"),
		"d.txt":        []byte("delta"),
		"e.txt":        []byte("echo"),
		"f.txt":        []byte("foxtrot"),
		"conflict.txt": []byte("source-body"),
	}, lm)
	defer oldS3.Close()
	oldS3.SetGetDelay(40 * time.Millisecond)

	newS3 := fakes3.NewWithObjects(t, "photos", map[string][]byte{
		"conflict.txt": []byte("target-must-stay"),
	}, lm.Add(time.Hour))
	defer newS3.Close()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "migration.db")
	conflictPath := filepath.Join(dir, "conflict.log")
	cfgPath := filepath.Join(dir, "config.yaml")
	writeClusterConfig(t, cfgPath, oldS3.URL, newS3.URL, dbPath, conflictPath, dir, `
bucket: photos
workers: 1
multipart_concurrency: 1
multipart_threshold: 1048576
`)

	out, err := execRoot(t, "run", "-c", cfgPath, "--workers", "4")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	got := oldS3.MaxGetInFlight()
	if got < 2 || got > 4 {
		t.Fatalf("--workers 4 should cap and overlap GETs, max in flight=%d", got)
	}
	for _, key := range []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt", "f.txt"} {
		body, ok := newS3.Object("photos", key)
		if !ok {
			t.Fatalf("missing copied object %s", key)
		}
		src, _ := oldS3.Object("photos", key)
		if !bytes.Equal(body, src) {
			t.Fatalf("%s mismatch: got %q", key, body)
		}
	}
	kept, ok := newS3.Object("photos", "conflict.txt")
	if !ok || !bytes.Equal(kept, []byte("target-must-stay")) {
		t.Fatalf("conflict object was overwritten: ok=%v body=%q", ok, kept)
	}
	assertNoUnsafeCalls(t, oldS3, newS3)
	conflictLog, err := os.ReadFile(conflictPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(conflictLog), "conflict.txt") {
		t.Fatalf("conflict log: %q", conflictLog)
	}
	if calls := newS3.PutObjectCalls("a.txt"); calls != 1 {
		t.Fatalf("a.txt put calls after run=%d", calls)
	}
	if calls := newS3.PutObjectCalls("conflict.txt"); calls != 0 {
		t.Fatalf("conflict.txt was written %d times", calls)
	}

	// forEachBucket wraps a per-bucket diff failure as "failed buckets".
	// The report itself is written to stdout before that return.
	diffOut, diffErr := execRootStdout(t, "diff", "-c", cfgPath, "--workers", "4")
	if diffErr == nil || !strings.Contains(diffErr.Error(), "photos") {
		t.Fatalf("diff should fail the bucket, err=%v\n%s", diffErr, diffOut)
	}
	if !strings.Contains(diffOut, "size_mismatch=1") || !strings.Contains(diffOut, "conflict.txt") {
		t.Fatalf("diff report should show the un-overwritten object:\n%s", diffOut)
	}

	oldS3.Put("photos", "g.txt", []byte("golf"), lm, fakes3.Metadata{})
	if out, err := execRoot(t, "scan", "-c", cfgPath); err != nil {
		t.Fatalf("scan: %v\n%s", err, out)
	}
	if out, err := execRoot(t, "migrate", "-c", cfgPath, "--workers", "3"); err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	body, ok := newS3.Object("photos", "g.txt")
	if !ok || !bytes.Equal(body, []byte("golf")) {
		t.Fatalf("migrate missed new object: ok=%v body=%q", ok, body)
	}
	kept, _ = newS3.Object("photos", "conflict.txt")
	if !bytes.Equal(kept, []byte("target-must-stay")) {
		t.Fatalf("migrate overwrote conflict: %q", kept)
	}
	if calls := newS3.PutObjectCalls("a.txt"); calls != 1 {
		t.Fatalf("migrate recopied a done object, put calls=%d", calls)
	}
	assertNoUnsafeCalls(t, oldS3, newS3)

	st, err := store.Open(dbPath, "photos")
	if err != nil {
		t.Fatal(err)
	}
	counts, err := st.Counts()
	_ = st.Close()
	if err != nil {
		t.Fatal(err)
	}
	if counts.Total != 8 || counts.Done != 7 || counts.Conflict != 1 || counts.Failed != 0 {
		t.Fatalf("counts after migrate: %+v", counts)
	}

	// status prints to the process stdout, so only the exit status is asserted here.
	// The counters above are the durable check that concurrent writes stayed consistent.
	if _, err := execRoot(t, "status", "-c", cfgPath); err != nil {
		t.Fatalf("status: %v", err)
	}
}

func TestCLIMigrateMultipartAndCleanDiff(t *testing.T) {
	lm := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	// Part size must stay at the 5 MiB floor. Two parts (5 MiB + a short tail)
	// still overlap under --multipart-concurrency 2.
	const partSize = 5 * 1024 * 1024
	payload := bytes.Repeat([]byte("z"), partSize+32)
	oldS3 := fakes3.NewWithObjects(t, "photos", map[string][]byte{
		"large.bin": payload,
		"small.txt": []byte("ok"),
	}, lm)
	defer oldS3.Close()
	newS3 := fakes3.New(t)
	defer newS3.Close()
	newS3.SetPartDelay(40 * time.Millisecond)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeClusterConfig(t, cfgPath, oldS3.URL, newS3.URL, filepath.Join(dir, "migration.db"), filepath.Join(dir, "conflict.log"), dir, `
bucket: photos
workers: 1
multipart_concurrency: 1
multipart_threshold: 5242880
multipart_part_size: 5242880
`)

	if out, err := execRoot(t, "scan", "-c", cfgPath); err != nil {
		t.Fatalf("scan: %v\n%s", err, out)
	}
	out, err := execRoot(t, "migrate", "-c", cfgPath, "--workers", "2", "--multipart-concurrency", "2")
	if err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	got := newS3.MaxPartInFlight()
	if got != 2 {
		t.Fatalf("--multipart-concurrency 2 max in-flight parts=%d", got)
	}
	body, ok := newS3.Object("photos", "large.bin")
	if !ok || !bytes.Equal(body, payload) {
		t.Fatalf("large object: ok=%v len=%d", ok, len(body))
	}
	small, ok := newS3.Object("photos", "small.txt")
	if !ok || !bytes.Equal(small, []byte("ok")) {
		t.Fatalf("small object: ok=%v body=%q", ok, small)
	}
	assertNoUnsafeCalls(t, oldS3, newS3)

	diffOut, err := execRoot(t, "diff", "-c", cfgPath, "--workers", "2")
	if err != nil {
		t.Fatalf("diff: %v\n%s", err, diffOut)
	}
	assertNoUnsafeCalls(t, oldS3, newS3)
}

func writeClusterConfig(t *testing.T, path, oldURL, newURL, dbPath, conflictPath, spool, extra string) {
	t.Helper()
	body := "old: {endpoint: " + quote(oldURL) + ", access_key: a, secret_key: b}\n" +
		"new: {endpoint: " + quote(newURL) + ", access_key: c, secret_key: d}\n" +
		"db_path: " + quote(dbPath) + "\n" +
		"conflict_log: " + quote(conflictPath) + "\n" +
		"spool_dir: " + quote(spool) + "\n" +
		extra
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func quote(s string) string {
	return `"` + s + `"`
}

func execRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := NewRoot()
	buf := new(bytes.Buffer)
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}

// execRootStdout captures process stdout. diff writes its report there,
// not through cobra's command output.
func execRootStdout(t *testing.T, args ...string) (string, error) {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	_, cmdErr := execRoot(t, args...)
	_ = w.Close()
	os.Stdout = orig
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	return buf.String(), cmdErr
}

func assertNoUnsafeCalls(t *testing.T, oldS3, newS3 *fakes3.Server) {
	t.Helper()
	if oldS3.DeleteObjectCalls() != 0 || newS3.DeleteObjectCalls() != 0 || oldS3.CopyObjectCalls() != 0 || newS3.CopyObjectCalls() != 0 {
		t.Fatalf("unsafe API calls: oldDelete=%d newDelete=%d oldCopy=%d newCopy=%d",
			oldS3.DeleteObjectCalls(), newS3.DeleteObjectCalls(), oldS3.CopyObjectCalls(), newS3.CopyObjectCalls())
	}
}

func TestMigrateRequiresCompletedScanAndRejectsNewTarget(t *testing.T) {
	source := fakes3.New(t)
	defer source.Close()
	target := fakes3.New(t)
	defer target.Close()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "inventory.db")
	configure := func(dst string) {
		writeClusterConfig(t, cfg, source.URL, dst, dbPath, filepath.Join(dir, "conflict.log"), dir, "bucket: photos\n")
	}
	configure(target.URL)
	if _, err := execRoot(t, "migrate", "-c", cfg); err == nil {
		t.Fatal("migrate succeeded without a scan")
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("missing inventory was created: stat=%v", err)
	}
	if _, err := execRoot(t, "scan", "-c", cfg); err == nil {
		t.Fatal("scan of nonexistent source bucket succeeded")
	}
	source.Put("photos", "k", []byte("data"), time.Now(), fakes3.Metadata{})
	if _, err := execRoot(t, "migrate", "-c", cfg); err == nil {
		t.Fatal("migrate accepted an incomplete scan")
	}
	if _, err := execRoot(t, "run", "-c", cfg); err != nil {
		t.Fatalf("run after failed scan: %v", err)
	}

	replacement := fakes3.New(t)
	defer replacement.Close()
	configure(replacement.URL)
	if _, err := execRoot(t, "migrate", "-c", cfg); err == nil {
		t.Fatal("migrate silently reused terminal rows for another target")
	}
	resp, err := http.Head(replacement.URL + "/photos")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("target bucket created before state validation: %s", resp.Status)
	}
}

func TestStatusReportsInvalidStateFile(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	path := filepath.Join(dir, "migration-photos.db")
	if err := os.WriteFile(path, []byte("unrelated file"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeClusterConfig(t, cfg, "", "", filepath.Join(dir, "migration.db"), filepath.Join(dir, "conflict.log"), dir, "")
	if _, err := execRoot(t, "status", "-c", cfg); err == nil {
		t.Fatal("status silently accepted corrupt inventory")
	}
}

func TestFailedRescanBlocksCopyAndBadTargetCanBeCorrected(t *testing.T) {
	src := fakes3.NewWithObjects(t, "photos", map[string][]byte{"data": []byte("ok")}, time.Now())
	defer src.Close()
	dst := fakes3.New(t)
	defer dst.Close()
	var denyList atomic.Bool
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if denyList.Load() && r.URL.Query().Has("list-type") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		src.Config.Handler.ServeHTTP(w, r)
	}))
	defer source.Close()
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer denied.Close()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	configure := func(target string) {
		writeClusterConfig(t, cfg, source.URL, target, filepath.Join(dir, "state.db"), filepath.Join(dir, "conflict.log"), dir, "bucket: photos\n")
	}
	configure(denied.URL)
	if _, err := execRoot(t, "scan", "-c", cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := execRoot(t, "migrate", "-c", cfg); err == nil {
		t.Fatal("inaccessible target reported success")
	}
	configure(dst.URL)
	denyList.Store(true)
	if _, err := execRoot(t, "scan", "-c", cfg); err == nil {
		t.Fatal("injected listing failure reported success")
	}
	if _, err := execRoot(t, "migrate", "-c", cfg); err == nil {
		t.Fatal("failed rescan left inventory usable")
	}
	denyList.Store(false)
	if _, err := execRoot(t, "run", "-c", cfg); err != nil {
		t.Fatal(err)
	}
	if data, _ := dst.Object("photos", "data"); string(data) != "ok" {
		t.Fatalf("corrected migration copied %q", data)
	}
}
