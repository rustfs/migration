package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/rustfs/migration/internal/config"
	"github.com/rustfs/migration/internal/differ"
	"github.com/rustfs/migration/internal/fakes3"
	"github.com/rustfs/migration/internal/migrator"
	"github.com/rustfs/migration/internal/s3client"
	"github.com/rustfs/migration/internal/scanner"
	"github.com/rustfs/migration/internal/store"
)

func TestWhitespaceEnvVarsPreserveYAMLAndTrimSessionTokens(t *testing.T) {
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	src := fakes3.NewWithObjects(t, "production-photos", map[string][]byte{"a.jpg": []byte("photo")}, now)
	defer src.Close()
	src.Put("secret-hr-bucket", "payroll.csv", []byte("sensitive"), now, fakes3.Metadata{})

	dst := fakes3.New(t)
	defer dst.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeClusterConfig(t, cfgPath, src.URL, dst.URL, filepath.Join(dir, "migration.db"), filepath.Join(dir, "conflict.log"), dir, "bucket: production-photos\nworkers: 1\n")

	t.Setenv("BUCKET", "   ")
	t.Setenv("DB_PATH", "   ")
	t.Setenv("OLD_SESSION_TOKEN", "  sts-token-with-newline\n")
	t.Setenv("NEW_SESSION_TOKEN", "\tnew-sts-token\r\n")

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Bucket != "production-photos" || cfg.AllBuckets() {
		t.Fatalf("whitespace BUCKET env overwrote YAML bucket: got %q", cfg.Bucket)
	}
	if cfg.Old.SessionToken != "sts-token-with-newline" || cfg.New.SessionToken != "new-sts-token" {
		t.Fatalf("session tokens were not trimmed: old=%q new=%q", cfg.Old.SessionToken, cfg.New.SessionToken)
	}

	listOut, err := execRootStdout(t, "list", "-c", cfgPath)
	if err != nil {
		t.Fatalf("list failed with trimmed session token: %v", err)
	}
	if !strings.Contains(listOut, "production-photos") || !strings.Contains(listOut, "2 bucket(s)") {
		t.Fatalf("unexpected list output:\n%s", listOut)
	}

	if _, err := execRoot(t, "run", "-c", cfgPath); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if _, ok := dst.Object("production-photos", "a.jpg"); !ok {
		t.Fatal("expected production-photos/a.jpg to be migrated")
	}
	if _, leaked := dst.Object("secret-hr-bucket", "payroll.csv"); leaked {
		t.Fatal("secret-hr-bucket was migrated despite YAML specifying bucket: production-photos")
	}

	// Also exercise empty-cluster list output.
	emptySrc := fakes3.New(t)
	defer emptySrc.Close()
	emptyCfg := filepath.Join(dir, "empty.yaml")
	writeClusterConfig(t, emptyCfg, emptySrc.URL, dst.URL, filepath.Join(dir, "empty.db"), filepath.Join(dir, "empty.log"), dir, "")
	emptyListOut, err := execRootStdout(t, "list", "-c", emptyCfg)
	if err != nil || !strings.Contains(emptyListOut, "source has no buckets") {
		t.Fatalf("empty list: err=%v out=%q", err, emptyListOut)
	}
}

func TestEmptySourceContentTypeMigratesAndDiffsCleanly(t *testing.T) {
	body := []byte("raw object without content-type on source")
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && (r.URL.Path == "/photos" || r.URL.Path == "/photos/"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><Name>photos</Name><IsTruncated>false</IsTruncated><EncodingType>url</EncodingType><Contents><Key>legacy.bin</Key><LastModified>2026-11-25T00:00:00Z</LastModified><ETag>"etag1"</ETag><Size>%d</Size></Contents></ListBucketResult>`, len(body))
		case r.Method == http.MethodHead && r.URL.Path == "/photos/legacy.bin":
			w.Header()["Content-Type"] = nil
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			w.Header().Set("ETag", `"etag1"`)
			w.Header().Set("Last-Modified", "Wed, 25 Nov 2026 00:00:00 GMT")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/photos/legacy.bin":
			w.Header()["Content-Type"] = nil
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			w.Header().Set("ETag", `"etag1"`)
			w.Header().Set("Last-Modified", "Wed, 25 Nov 2026 00:00:00 GMT")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer src.Close()

	dst := fakes3.New(t)
	defer dst.Close()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "migration.db")
	cfgPath := filepath.Join(dir, "config.yaml")
	writeClusterConfig(t, cfgPath, src.URL, dst.URL, dbPath, filepath.Join(dir, "conflict.log"), dir, "bucket: photos\nworkers: 1\n")

	if _, err := execRoot(t, "run", "-c", cfgPath, "--strict"); err != nil {
		t.Fatalf("run --strict failed for empty source Content-Type: %v", err)
	}
	if _, err := execRoot(t, "migrate", "-c", cfgPath, "--strict"); err != nil {
		t.Fatalf("idempotent migrate --strict failed: %v", err)
	}
	diffOut, err := execRootStdout(t, "diff", "-c", cfgPath, "--strict")
	if err != nil || !strings.Contains(diffOut, "ok=1") || !strings.Contains(diffOut, "metadata_mismatch=0") {
		t.Fatalf("diff failed: err=%v\n%s", err, diffOut)
	}
}

func TestRawNonRFC1123ExpiresHeaderPreservedAndVerified(t *testing.T) {
	body := []byte("asset with Expires: 0")
	var targetReceivedExpires atomic.Value
	targetReceivedExpires.Store("")

	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/photos":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><Name>photos</Name><IsTruncated>false</IsTruncated><Contents><Key>app.js</Key><LastModified>2026-11-25T00:00:00Z</LastModified><ETag>"e1"</ETag><Size>%d</Size></Contents></ListBucketResult>`, len(body))
		case r.URL.Path == "/photos/app.js":
			w.Header().Set("Content-Type", "application/javascript")
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			w.Header().Set("ETag", `"e1"`)
			w.Header().Set("Expires", "0")
			w.Header().Set("Last-Modified", "Wed, 25 Nov 2026 00:00:00 GMT")
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodGet {
				_, _ = w.Write(body)
			}
		}
	}))
	defer src.Close()

	dstBackend := fakes3.New(t)
	defer dstBackend.Close()
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/photos/app.js" {
			targetReceivedExpires.Store(r.Header.Get("Expires"))
		}
		dstBackend.Config.Handler.ServeHTTP(w, r)
	}))
	defer dst.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeClusterConfig(t, cfgPath, src.URL, dst.URL, filepath.Join(dir, "migration.db"), filepath.Join(dir, "conflict.log"), dir, "bucket: photos\nworkers: 1\n")

	if _, err := execRoot(t, "run", "-c", cfgPath, "--strict"); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if got := targetReceivedExpires.Load(); got != "0" {
		t.Fatalf("target received Expires=%q, want %q", got, "0")
	}
	diffOut, err := execRootStdout(t, "diff", "-c", cfgPath, "--strict")
	if err != nil || !strings.Contains(diffOut, "ok=1") {
		t.Fatalf("diff failed: err=%v\n%s", err, diffOut)
	}
}

func TestIsCanceledDoesNotMisclassify404EchoingSensitiveKeyWords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/photos" && r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `<Error><Code>NoSuchKey</Code><Message>The key %s does not exist</Message></Error>`, r.URL.Path)
	}))
	defer srv.Close()

	ctx := context.Background()
	cli, err := s3client.New(ctx, config.Endpoint{Endpoint: srv.URL, Region: "us-east-1", AccessKey: "a", SecretKey: "b"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"recordings/interrupted-session.mp4",
		"logs/deadline exceeded/report.txt",
		"jobs/request canceled.json",
	} {
		_, getErr := cli.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String("photos"),
			Key:    aws.String(key),
		})
		if s3client.IsCanceled(getErr) {
			t.Fatalf("key %q: HTTP 404 NoSuchKey misclassified as canceled: %v", key, getErr)
		}
		if !s3client.IsNotFound(getErr) {
			t.Fatalf("key %q: HTTP 404 NoSuchKey not recognized as NotFound: %v", key, getErr)
		}
	}
}

func TestRescanPrunesDeletedSourceRowsAndUnblocksStrictMode(t *testing.T) {
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	src := fakes3.NewWithObjects(t, "photos", map[string][]byte{
		"keep.txt": []byte("keep"),
		"temp.txt": []byte("ephemeral"),
	}, now)
	defer src.Close()
	dst := fakes3.New(t)
	defer dst.Close()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "migration.db")
	cfgPath := filepath.Join(dir, "config.yaml")
	writeClusterConfig(t, cfgPath, src.URL, dst.URL, dbPath, filepath.Join(dir, "conflict.log"), dir, "bucket: photos\nworkers: 1\n")

	if _, err := execRoot(t, "scan", "-c", cfgPath); err != nil {
		t.Fatal(err)
	}
	src.DeleteKey("photos", "temp.txt")

	if _, err := execRoot(t, "run", "-c", cfgPath, "--strict"); err != nil {
		t.Fatalf("run --strict failed after rescanning post-deletion source: %v", err)
	}
	st, err := store.OpenExisting(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	counts, err := st.Counts()
	_ = st.Close()
	if err != nil {
		t.Fatal(err)
	}
	if counts.Total != 1 || counts.Done != 1 || counts.Skipped != 0 {
		t.Fatalf("unexpected counts after pruning deleted source row: %+v", counts)
	}
}

func TestStatusSucceedsOnReadOnlyDirectoryAfterCleanClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permissions test")
	}
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	src := fakes3.NewWithObjects(t, "photos", map[string][]byte{"a.txt": []byte("ok")}, now)
	defer src.Close()
	dst := fakes3.New(t)
	defer dst.Close()

	base := t.TempDir()
	stateDir := filepath.Join(base, "readonly-snapshot")
	dbPath := filepath.Join(stateDir, "migration.db")
	cfgPath := filepath.Join(base, "config.yaml")
	writeClusterConfig(t, cfgPath, src.URL, dst.URL, dbPath, filepath.Join(base, "conflict.log"), base, "bucket: photos\nworkers: 1\n")

	if _, err := execRoot(t, "scan", "-c", cfgPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(stateDir, 0o755)

	out, err := execRootStdout(t, "status", "-c", cfgPath)
	if err != nil {
		t.Fatalf("status failed on read-only directory: %v", err)
	}
	if !strings.Contains(out, "photos") {
		t.Fatalf("unexpected status output:\n%s", out)
	}
}

func TestStatusTargetsHandlesBracketDirectoriesAndDeduplicatesBuckets(t *testing.T) {
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	src := fakes3.NewWithObjects(t, "photos", map[string][]byte{"a.txt": []byte("ok")}, now)
	defer src.Close()
	dst := fakes3.New(t)
	defer dst.Close()

	base := t.TempDir()
	for _, sub := range []string{"[2026-11]", "[2026]"} {
		stateDir := filepath.Join(base, sub)
		dbPath := filepath.Join(stateDir, "migration.db")
		cfgPath := filepath.Join(base, "cfg-"+sub+".yaml")
		writeClusterConfig(t, cfgPath, src.URL, dst.URL, dbPath, filepath.Join(stateDir, "conflict.log"), base, "workers: 1\n")
		if _, err := execRoot(t, "scan", "-c", cfgPath); err != nil {
			t.Fatalf("%s scan: %v", sub, err)
		}
		out, err := execRootStdout(t, "status", "-c", cfgPath)
		if err != nil || !strings.Contains(out, "photos") {
			t.Fatalf("%s status failed: err=%v out=%q", sub, err, out)
		}
	}

	dupDir := filepath.Join(base, "dup")
	dupDB := filepath.Join(dupDir, "migration.db")
	cfgPath := filepath.Join(base, "dup.yaml")
	writeClusterConfig(t, cfgPath, src.URL, dst.URL, dupDB, filepath.Join(dupDir, "conflict.log"), base, "workers: 1\n")
	if _, err := execRoot(t, "scan", "-c", cfgPath, "--bucket", "photos"); err != nil {
		t.Fatal(err)
	}
	if _, err := execRoot(t, "scan", "-c", cfgPath); err != nil {
		t.Fatal(err)
	}
	out, err := execRootStdout(t, "status", "-c", cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out, "photos"); got != 1 {
		t.Fatalf("expected 1 photos row in status output, got %d:\n%s", got, out)
	}
}

func TestDefaultPortsNormalizedInValidatePairAndBindMigration(t *testing.T) {
	for _, pair := range [][2]string{
		{"http://s3.internal.local", "http://s3.internal.local:80"},
		{"https://S3.INTERNAL.LOCAL:443/", "https://s3.internal.local"},
		{"http://[::1]:80", "http://[::1]"},
	} {
		cfg := &config.Config{
			Old: config.Endpoint{Endpoint: pair[0], AccessKey: "a", SecretKey: "b"},
			New: config.Endpoint{Endpoint: pair[1], AccessKey: "a", SecretKey: "b"},
		}
		if err := cfg.ValidatePair(); err == nil {
			t.Fatalf("ValidatePair accepted identical endpoints %q and %q", pair[0], pair[1])
		}
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"), "photos")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.BindMigration("https://s3.internal.local:443", "http://dst.internal.local:80"); err != nil {
		t.Fatal(err)
	}
	if err := st.BindMigration("https://s3.internal.local/", "http://dst.internal.local/"); err != nil {
		t.Fatalf("BindMigration rejected equivalent default-port URLs: %v", err)
	}
}

func TestDiffIssuesJSONLNotCreatedOnInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	jsonl := filepath.Join(dir, "issues.jsonl")
	if _, err := execRoot(t, "diff", "-c", filepath.Join(dir, "missing.yaml"), "--issues-jsonl", jsonl); err == nil {
		t.Fatal("expected missing config to fail")
	}
	if _, err := os.Stat(jsonl); !os.IsNotExist(err) {
		t.Fatalf("invalid config invocation created JSONL file prematurely: %v", err)
	}
}

func TestSQLitePendingQueryPlanUsesCompositeIndexAndDiffRespectsTempDir(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "plan.db")
	st, err := store.Open(dbPath, "photos")
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer rawDB.Close()
	query := `
SELECT key, size, last_modified, etag, status FROM (
    SELECT key, size, last_modified, etag, status FROM (
        SELECT key, size, last_modified, etag, status FROM objects
         WHERE status = 'pending' AND key > ? ORDER BY key LIMIT ?
    )
    UNION ALL
    SELECT key, size, last_modified, etag, status FROM (
        SELECT key, size, last_modified, etag, status FROM objects
         WHERE status = 'failed' AND key > ? ORDER BY key LIMIT ?
    )
) ORDER BY key LIMIT ?`
	rows, err := rawDB.Query("EXPLAIN QUERY PLAN "+query, "", 1000, "", 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plans []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plans = append(plans, detail)
	}
	joined := strings.Join(plans, " | ")
	if !strings.Contains(joined, "idx_objects_status_key") {
		t.Fatalf("expected composite index idx_objects_status_key in query plan, got: %s", joined)
	}

	osTmp := filepath.Join(t.TempDir(), "os-tmp")
	spoolTmp := filepath.Join(t.TempDir(), "spool-tmp")
	_ = os.MkdirAll(osTmp, 0o700)
	_ = os.MkdirAll(spoolTmp, 0o700)
	t.Setenv("TMPDIR", osTmp)

	var sawOSTmp, sawSpoolTmp atomic.Bool
	src := fakes3.NewWithObjects(t, "photos", map[string][]byte{"a.txt": []byte("hello")}, time.Now())
	defer src.Close()
	dstBackend := fakes3.NewWithObjects(t, "photos", map[string][]byte{"a.txt": []byte("hello")}, time.Now())
	defer dstBackend.Close()
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if entries, _ := os.ReadDir(osTmp); len(entries) > 0 {
			sawOSTmp.Store(true)
		}
		if entries, _ := os.ReadDir(spoolTmp); len(entries) > 0 {
			sawSpoolTmp.Store(true)
		}
		dstBackend.Config.Handler.ServeHTTP(w, r)
	}))
	defer dst.Close()

	ctx := context.Background()
	oldCli, _ := s3client.New(ctx, config.Endpoint{Endpoint: src.URL, Region: "us-east-1", AccessKey: "a", SecretKey: "b"})
	newCli, _ := s3client.New(ctx, config.Endpoint{Endpoint: dst.URL, Region: "us-east-1", AccessKey: "a", SecretKey: "b"})

	if _, err := differ.Run(ctx, oldCli, newCli, "photos", differ.Options{Workers: 1, ReportExtra: false, TempDir: spoolTmp, Writer: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if sawOSTmp.Load() || sawSpoolTmp.Load() {
		t.Fatalf("differ.Run(ReportExtra=false) should not create a temp DB: osTmp=%v spoolTmp=%v", sawOSTmp.Load(), sawSpoolTmp.Load())
	}

	if _, err := differ.Run(ctx, oldCli, newCli, "photos", differ.Options{Workers: 1, ReportExtra: true, TempDir: spoolTmp, Writer: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if sawOSTmp.Load() || !sawSpoolTmp.Load() {
		t.Fatalf("differ.Run(ReportExtra=true) should use TempDir (spool_dir): osTmp=%v spoolTmp=%v", sawOSTmp.Load(), sawSpoolTmp.Load())
	}
}

func TestLostMultipartCommitResponseReturningNoSuchUploadReconcilesOnFirstRun(t *testing.T) {
	const partSize = 5 * 1024 * 1024
	payload := bytes.Repeat([]byte("M"), partSize+16)
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	src := fakes3.NewWithObjects(t, "photos", map[string][]byte{"big.bin": payload}, now)
	defer src.Close()
	dstBackend := fakes3.New(t)
	defer dstBackend.Close()

	var completeAttempts, abortCalls atomic.Int32
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Query().Get("uploadId") != "" {
			abortCalls.Add(1)
		}
		if r.Method == http.MethodPost && r.URL.Query().Get("uploadId") != "" {
			if completeAttempts.Add(1) == 1 {
				rec := httptest.NewRecorder()
				dstBackend.Config.Handler.ServeHTTP(rec, r)
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
					return
				}
			}
		}
		dstBackend.Config.Handler.ServeHTTP(w, r)
	}))
	defer dst.Close()

	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	st, err := store.Open(dbPath, "photos")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	oldCli, _ := s3client.New(ctx, config.Endpoint{Endpoint: src.URL, Region: "us-east-1", AccessKey: "a", SecretKey: "b"})
	newCli, _ := s3client.New(ctx, config.Endpoint{Endpoint: dst.URL, Region: "us-east-1", AccessKey: "a", SecretKey: "b"})
	newCli = s3.New(newCli.Options(), func(o *s3.Options) {
		o.Retryer = retry.NewStandard(func(ro *retry.StandardOptions) { ro.MaxAttempts = 2; ro.MaxBackoff = time.Millisecond })
	})
	if _, err := scanner.Run(ctx, oldCli, "photos", st); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		SourceFrozen: true, Bucket: "photos", DBPath: dbPath, ConflictLog: filepath.Join(dir, "conflict.log"),
		SpoolDir: dir, Workers: 1, MultipartThreshold: partSize, MultipartPartSize: partSize, MultipartConcurrency: 1,
	}
	m, err := migrator.New(cfg, oldCli, newCli, st)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	if err := m.Run(ctx); err != nil {
		t.Fatalf("expected lost-response multipart commit to reconcile on first run, got: %v", err)
	}
	counts, err := st.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if completeAttempts.Load() != 2 || abortCalls.Load() != 0 || counts.Done != 1 || counts.Failed != 0 || counts.Conflict != 0 {
		t.Fatalf("unexpected state: completeAttempts=%d aborts=%d counts=%+v", completeAttempts.Load(), abortCalls.Load(), counts)
	}
}

func TestMultipartOversizedForSpoolBudgetFailsBeforeRemoteCreate(t *testing.T) {
	const partSize = 5 * 1024 * 1024
	payload := bytes.Repeat([]byte("P"), partSize+10)
	src := fakes3.NewWithObjects(t, "photos", map[string][]byte{"large.bin": payload}, time.Now())
	defer src.Close()
	dstBackend := fakes3.New(t)
	defer dstBackend.Close()

	var createMultipartCalls, abortCalls atomic.Int32
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Query().Has("uploads") {
			createMultipartCalls.Add(1)
		}
		if r.Method == http.MethodDelete && r.URL.Query().Get("uploadId") != "" {
			abortCalls.Add(1)
		}
		dstBackend.Config.Handler.ServeHTTP(w, r)
	}))
	defer dst.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeClusterConfig(t, cfgPath, src.URL, dst.URL, filepath.Join(dir, "migration.db"), filepath.Join(dir, "conflict.log"), dir, `
bucket: photos
workers: 1
multipart_threshold: 5242880
multipart_part_size: 5242880
spool_max_bytes: 1048576
`)
	if _, err := execRoot(t, "run", "-c", cfgPath); err == nil {
		t.Fatal("expected run to fail when spool_max_bytes < multipart_part_size + 1")
	}
	if createMultipartCalls.Load() != 0 || abortCalls.Load() != 0 {
		t.Fatalf("expected 0 remote CreateMultipartUpload/Abort calls, got create=%d abort=%d", createMultipartCalls.Load(), abortCalls.Load())
	}
}
