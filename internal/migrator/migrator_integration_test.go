package migrator

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/rustfs/migration/internal/cluster"
	appconfig "github.com/rustfs/migration/internal/config"
	"github.com/rustfs/migration/internal/fakes3"
	"github.com/rustfs/migration/internal/s3client"
	"github.com/rustfs/migration/internal/scanner"
	"github.com/rustfs/migration/internal/store"
)

func TestScanAndMigrateEndToEnd(t *testing.T) {
	ctx := context.Background()
	lastModified := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)

	oldS3 := fakes3.NewWithObjects(t, "bucket", map[string][]byte{
		"small.txt":      []byte("hello"),
		"large.bin":      []byte("0123456789abcdef"),
		"conflict.txt":   []byte("old-conflict"),
		"gone.txt":       []byte("gone"),
		"gone-large.bin": []byte("this object disappears before multipart copy"),
	}, lastModified)
	defer oldS3.Close()

	newS3 := fakes3.NewWithObjects(t, "bucket", map[string][]byte{
		"conflict.txt": []byte("new-conflict"),
	}, lastModified.Add(time.Hour))
	defer newS3.Close()

	oldClient := newTestClient(t, ctx, oldS3.URL)
	newClient := newTestClient(t, ctx, newS3.URL)

	dbPath := path.Join(t.TempDir(), "migration.db")
	conflictPath := path.Join(t.TempDir(), "conflict.log")
	st, err := store.Open(dbPath, "bucket")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	if _, err := scanner.Run(ctx, oldClient, "bucket", st); err != nil {
		t.Fatalf("scan: %v", err)
	}
	oldS3.DeleteKey("bucket", "gone.txt")
	oldS3.DeleteKey("bucket", "gone-large.bin")

	cfg := testCfg(dbPath, conflictPath, t.TempDir())
	m, err := New(cfg, oldClient, newClient, st)
	if err != nil {
		t.Fatalf("new migrator: %v", err)
	}
	defer m.Close()

	if err := m.Run(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	assertObject(t, newS3, "small.txt", []byte("hello"))
	assertObject(t, newS3, "large.bin", []byte("0123456789abcdef"))
	assertMetadata(t, newS3, "small.txt", "application/test", map[string]string{"origin": "old"})
	assertMetadata(t, newS3, "large.bin", "application/test", map[string]string{"origin": "old"})
	assertObject(t, newS3, "conflict.txt", []byte("new-conflict"))
	assertMissing(t, newS3, "gone.txt")
	assertMissing(t, newS3, "gone-large.bin")

	counts, err := st.Counts()
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Total != 5 || counts.Done != 2 || counts.Conflict != 1 || counts.Skipped != 2 || counts.Failed != 0 {
		t.Fatalf("unexpected counts: %+v", counts)
	}

	conflicts, err := os.ReadFile(conflictPath)
	if err != nil {
		t.Fatalf("read conflict log: %v", err)
	}
	if !strings.Contains(string(conflicts), "conflict.txt\t2026-05-24T10:00:00Z\t2026-05-24T11:00:00Z") {
		t.Fatalf("conflict log missing expected row: %q", conflicts)
	}

	if newS3.CopyObjectCalls() != 0 || oldS3.DeleteObjectCalls() != 0 || newS3.DeleteObjectCalls() != 0 {
		t.Fatalf("unexpected unsafe API usage: oldDelete=%d newDelete=%d newCopy=%d",
			oldS3.DeleteObjectCalls(), newS3.DeleteObjectCalls(), newS3.CopyObjectCalls())
	}
}

func TestResumeAfterTransientFailure(t *testing.T) {
	ctx := context.Background()
	lastModified := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)

	oldS3 := fakes3.NewWithObjects(t, "bucket", map[string][]byte{
		"small.txt": []byte("hi"),
		"large.bin": []byte("0123456789abcdef0123456789abcdef"),
	}, lastModified)
	defer oldS3.Close()

	newS3 := fakes3.NewWithObjects(t, "bucket", map[string][]byte{}, lastModified)
	defer newS3.Close()

	oldClient := newTestClient(t, ctx, oldS3.URL)
	newClient := newTestClient(t, ctx, newS3.URL)

	tmpDir := t.TempDir()
	dbPath := path.Join(tmpDir, "migration.db")
	conflictPath := path.Join(tmpDir, "conflict.log")
	st, err := store.Open(dbPath, "bucket")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	if _, err := scanner.Run(ctx, oldClient, "bucket", st); err != nil {
		t.Fatalf("scan: %v", err)
	}

	cfg := testCfg(dbPath, conflictPath, tmpDir)

	newS3.SetFailPartUploads(1000)

	m1, err := New(cfg, oldClient, newClient, st)
	if err != nil {
		t.Fatalf("new migrator: %v", err)
	}
	if err := m1.Run(ctx); err != nil {
		t.Fatalf("migrate run1: %v", err)
	}
	_ = m1.Close()

	counts, err := st.Counts()
	if err != nil {
		t.Fatalf("counts after run1: %v", err)
	}
	if counts.Done != 1 || counts.Failed != 1 || counts.Total != 2 {
		t.Fatalf("unexpected counts after run1: %+v", counts)
	}
	assertObject(t, newS3, "small.txt", []byte("hi"))
	if _, ok := newS3.Object("bucket", "large.bin"); ok {
		t.Fatalf("large.bin should not exist after failed run")
	}

	putsBefore := newS3.PutObjectCalls("small.txt")
	if putsBefore != 1 {
		t.Fatalf("expected exactly one PUT for small.txt after run1, got %d", putsBefore)
	}

	newS3.SetFailPartUploads(0)

	m2, err := New(cfg, oldClient, newClient, st)
	if err != nil {
		t.Fatalf("new migrator: %v", err)
	}
	if err := m2.Run(ctx); err != nil {
		t.Fatalf("migrate run2: %v", err)
	}
	_ = m2.Close()

	counts, err = st.Counts()
	if err != nil {
		t.Fatalf("counts after run2: %v", err)
	}
	if counts.Done != 2 || counts.Failed != 0 || counts.Total != 2 {
		t.Fatalf("unexpected counts after run2: %+v", counts)
	}
	assertObject(t, newS3, "large.bin", []byte("0123456789abcdef0123456789abcdef"))

	if got := newS3.PutObjectCalls("small.txt"); got != putsBefore {
		t.Fatalf("small.txt was re-uploaded on resume: before=%d after=%d", putsBefore, got)
	}
	if got := newS3.CompleteMultipartCalls("large.bin"); got != 1 {
		t.Fatalf("expected exactly one successful multipart completion for large.bin, got %d", got)
	}
	if newS3.CopyObjectCalls() != 0 || newS3.DeleteObjectCalls() != 0 || oldS3.DeleteObjectCalls() != 0 {
		t.Fatalf("unexpected unsafe API usage during resume: oldDelete=%d newDelete=%d newCopy=%d",
			oldS3.DeleteObjectCalls(), newS3.DeleteObjectCalls(), newS3.CopyObjectCalls())
	}
}

func TestSkippedObjectRescannedAndCopied(t *testing.T) {
	ctx := context.Background()
	lm := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	oldS3 := fakes3.NewWithObjects(t, "bucket", map[string][]byte{"reappear.txt": []byte("v1")}, lm)
	defer oldS3.Close()
	newS3 := fakes3.NewWithObjects(t, "bucket", map[string][]byte{}, lm)
	defer newS3.Close()

	oldClient := newTestClient(t, ctx, oldS3.URL)
	newClient := newTestClient(t, ctx, newS3.URL)
	tmp := t.TempDir()
	st, err := store.Open(path.Join(tmp, "m.db"), "bucket")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := scanner.Run(ctx, oldClient, "bucket", st); err != nil {
		t.Fatal(err)
	}
	oldS3.DeleteKey("bucket", "reappear.txt")
	cfg := testCfg(path.Join(tmp, "m.db"), path.Join(tmp, "c.log"), tmp)
	m, err := New(cfg, oldClient, newClient, st)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Run(ctx); err != nil {
		t.Fatal(err)
	}
	_ = m.Close()
	c, _ := st.Counts()
	if c.Skipped != 1 {
		t.Fatalf("want skipped=1 got %+v", c)
	}

	oldS3.Put("bucket", "reappear.txt", []byte("v2"), lm, fakes3.Metadata{ContentType: "text/plain", Metadata: map[string]string{}})
	if _, err := scanner.Run(ctx, oldClient, "bucket", st); err != nil {
		t.Fatal(err)
	}
	m2, err := New(cfg, oldClient, newClient, st)
	if err != nil {
		t.Fatal(err)
	}
	if err := m2.Run(ctx); err != nil {
		t.Fatal(err)
	}
	_ = m2.Close()
	c, _ = st.Counts()
	if c.Done != 1 || c.Skipped != 0 {
		t.Fatalf("want done after resurrection: %+v", c)
	}
	assertObject(t, newS3, "reappear.txt", []byte("v2"))
}

func TestIdenticalTargetIsDoneDifferentContentConflicts(t *testing.T) {
	ctx := context.Background()
	lm := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	identical := bytes.Repeat([]byte("a"), 1024)
	sourceClash := bytes.Repeat([]byte("b"), 1024)
	destClash := bytes.Repeat([]byte("c"), 1024)
	sourceSmall := []byte("short")
	destLarge := []byte("much-longer-than-source")

	oldS3 := fakes3.NewWithObjects(t, "bucket", map[string][]byte{
		"resume/same.dat":  identical,
		"resume/clash.dat": sourceClash,
		"resume/size.dat":  sourceSmall,
	}, lm)
	defer oldS3.Close()
	newS3 := fakes3.NewWithObjects(t, "bucket", map[string][]byte{
		"resume/same.dat":  identical,
		"resume/clash.dat": destClash,
		"resume/size.dat":  destLarge,
	}, lm.Add(time.Hour))
	defer newS3.Close()

	oldClient := newTestClient(t, ctx, oldS3.URL)
	newClient := newTestClient(t, ctx, newS3.URL)
	tmp := t.TempDir()
	dbPath := path.Join(tmp, "migration.db")
	conflictPath := path.Join(tmp, "conflict.log")
	st, err := store.Open(dbPath, "bucket")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := scanner.Run(ctx, oldClient, "bucket", st); err != nil {
		t.Fatal(err)
	}

	cfg := testCfg(dbPath, conflictPath, tmp)
	m, err := New(cfg, oldClient, newClient, st)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Run(ctx); err != nil {
		t.Fatal(err)
	}

	counts, err := st.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if counts.Total != 3 || counts.Done != 1 || counts.Conflict != 2 || counts.Failed != 0 {
		t.Fatalf("unexpected counts: %+v", counts)
	}
	if m.copied.Load() != 1 || m.conflicts.Load() != 2 || m.bytes.Load() != int64(len(identical)) {
		t.Fatalf("counters copied=%d conflicts=%d bytes=%d", m.copied.Load(), m.conflicts.Load(), m.bytes.Load())
	}

	status, lastErr, _, _ := objectRow(t, dbPath, "resume/same.dat")
	if status != store.StatusDone || lastErr != "" {
		t.Fatalf("identical target status=%s last_error=%q", status, lastErr)
	}
	for _, key := range []string{"resume/clash.dat", "resume/size.dat"} {
		status, lastErr, _, _ := objectRow(t, dbPath, key)
		if status != store.StatusConflict || lastErr != "exists on new" {
			t.Fatalf("%s status=%s last_error=%q", key, status, lastErr)
		}
	}

	assertObject(t, newS3, "resume/same.dat", identical)
	assertObject(t, newS3, "resume/clash.dat", destClash)
	assertObject(t, newS3, "resume/size.dat", destLarge)
	if newS3.PutObjectCalls("resume/same.dat") != 0 || newS3.PutObjectCalls("resume/clash.dat") != 0 || newS3.PutObjectCalls("resume/size.dat") != 0 {
		t.Fatalf("existing targets were written: same=%d clash=%d size=%d",
			newS3.PutObjectCalls("resume/same.dat"), newS3.PutObjectCalls("resume/clash.dat"), newS3.PutObjectCalls("resume/size.dat"))
	}
	if newS3.CopyObjectCalls() != 0 || newS3.DeleteObjectCalls() != 0 {
		t.Fatalf("unsafe API usage copy=%d delete=%d", newS3.CopyObjectCalls(), newS3.DeleteObjectCalls())
	}

	conflicts, err := os.ReadFile(conflictPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(conflicts)
	if strings.Contains(logText, "resume/same.dat") {
		t.Fatalf("identical object recorded as conflict: %q", logText)
	}
	if !strings.Contains(logText, "resume/clash.dat") || !strings.Contains(logText, "resume/size.dat") {
		t.Fatalf("conflict log missing mismatched keys: %q", logText)
	}
}

func TestResumeAfterPutBeforeMarkDone(t *testing.T) {
	ctx := context.Background()
	lm := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	body := bytes.Repeat([]byte{0xab}, 1024)

	oldS3 := fakes3.NewWithObjects(t, "bucket", map[string][]byte{
		"resume/a.dat": body,
	}, lm)
	defer oldS3.Close()
	newS3 := fakes3.NewWithObjects(t, "bucket", map[string][]byte{}, lm)
	defer newS3.Close()

	oldClient := newTestClient(t, ctx, oldS3.URL)
	newClient := newTestClient(t, ctx, newS3.URL)
	tmp := t.TempDir()
	dbPath := path.Join(tmp, "migration.db")
	conflictPath := path.Join(tmp, "conflict.log")
	st, err := store.Open(dbPath, "bucket")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := scanner.Run(ctx, oldClient, "bucket", st); err != nil {
		t.Fatal(err)
	}

	// SIGINT after PutObject committed and before MarkStatus(done): the
	// target has the object, SQLite still says failed.
	newS3.Put("bucket", "resume/a.dat", body, lm, fakes3.Metadata{
		ContentType: "application/test",
		Metadata:    map[string]string{"origin": "old"},
	})
	if err := st.MarkStatus("resume/a.dat", store.StatusFailed, "context canceled"); err != nil {
		t.Fatal(err)
	}

	cfg := testCfg(dbPath, conflictPath, tmp)
	m, err := New(cfg, oldClient, newClient, st)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Run(ctx); err != nil {
		t.Fatal(err)
	}

	counts, err := st.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if counts.Done != 1 || counts.Conflict != 0 || counts.Failed != 0 || counts.Total != 1 {
		t.Fatalf("unexpected counts: %+v", counts)
	}
	status, lastErr, _, size := objectRow(t, dbPath, "resume/a.dat")
	if status != store.StatusDone || lastErr != "" || size != 1024 {
		t.Fatalf("status=%s last_error=%q size=%d", status, lastErr, size)
	}
	assertObject(t, newS3, "resume/a.dat", body)
	if got := newS3.PutObjectCalls("resume/a.dat"); got != 0 {
		t.Fatalf("resume re-uploaded resume/a.dat: puts=%d", got)
	}
	if m.copied.Load() != 1 || m.conflicts.Load() != 0 || m.bytes.Load() != 1024 {
		t.Fatalf("counters copied=%d conflicts=%d bytes=%d", m.copied.Load(), m.conflicts.Load(), m.bytes.Load())
	}
	conflicts, err := os.ReadFile(conflictPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(conflicts)) != "" {
		t.Fatalf("conflict log not empty: %q", conflicts)
	}
}

func TestCreateMissingTargetBucket(t *testing.T) {
	ctx := context.Background()
	lm := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	oldS3 := fakes3.NewWithObjects(t, "photos", map[string][]byte{"a.txt": []byte("hi")}, lm)
	defer oldS3.Close()
	newS3 := fakes3.New(t)
	defer newS3.Close()

	oldClient := newTestClient(t, ctx, oldS3.URL)
	newClient := newTestClient(t, ctx, newS3.URL)

	if err := cluster.EnsureBucket(ctx, newClient, "photos"); err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	st, err := store.Open(path.Join(tmp, "m.db"), "photos")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := scanner.Run(ctx, oldClient, "photos", st); err != nil {
		t.Fatal(err)
	}
	cfg := testCfg(path.Join(tmp, "m.db"), path.Join(tmp, "c.log"), tmp)
	cfg.Bucket = "photos"
	m, err := New(cfg, oldClient, newClient, st)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Run(ctx); err != nil {
		t.Fatal(err)
	}
	got, ok := newS3.Object("photos", "a.txt")
	if !ok || !bytes.Equal(got, []byte("hi")) {
		t.Fatalf("target missing copied object ok=%v got=%q", ok, got)
	}
}

func testCfg(db, conflict, spool string) *appconfig.Config {
	return &appconfig.Config{
		Bucket:               "bucket",
		DBPath:               db,
		ConflictLog:          conflict,
		SpoolDir:             spool,
		Workers:              3,
		MultipartThreshold:   8,
		MultipartPartSize:    4,
		MultipartConcurrency: 2,
	}
}

func newTestClient(t *testing.T, ctx context.Context, endpoint string) *s3.Client {
	t.Helper()
	client, err := s3client.New(ctx, appconfig.Endpoint{
		Endpoint:  endpoint,
		AccessKey: "test-ak",
		SecretKey: "test-sk",
		Region:    "us-east-1",
	})
	if err != nil {
		t.Fatalf("new s3 client: %v", err)
	}
	return client
}

func assertObject(t *testing.T, f *fakes3.Server, key string, want []byte) {
	t.Helper()
	got, ok := f.Object("bucket", key)
	if !ok {
		t.Fatalf("object %s missing", key)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("object %s mismatch: got=%q want=%q", key, got, want)
	}
}

func assertMissing(t *testing.T, f *fakes3.Server, key string) {
	t.Helper()
	if got, ok := f.Object("bucket", key); ok {
		t.Fatalf("object %s unexpectedly exists: %q", key, got)
	}
}

func objectRow(t *testing.T, dbPath, key string) (status, lastErr, etag string, size int64) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = db.QueryRow(`SELECT status, COALESCE(last_error,''), COALESCE(etag,''), size FROM objects WHERE key = ?`, key).
		Scan(&status, &lastErr, &etag, &size)
	if err != nil {
		t.Fatalf("row %s: %v", key, err)
	}
	return status, lastErr, etag, size
}

func assertMetadata(t *testing.T, f *fakes3.Server, key, contentType string, metadata map[string]string) {
	t.Helper()
	got, ok := f.ObjectMetadata("bucket", key)
	if !ok {
		t.Fatalf("metadata for %s missing", key)
	}
	if got.ContentType != contentType {
		t.Fatalf("content-type for %s mismatch: got=%q want=%q", key, got.ContentType, contentType)
	}
	for wantKey, wantValue := range metadata {
		if got.Metadata[wantKey] != wantValue {
			t.Fatalf("metadata %s[%s] mismatch: got=%q want=%q", key, wantKey, got.Metadata[wantKey], wantValue)
		}
	}
}
