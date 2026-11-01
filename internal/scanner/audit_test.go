package scanner_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/rustfs/migration/internal/cluster"
	"github.com/rustfs/migration/internal/config"
	"github.com/rustfs/migration/internal/fakes3"
	"github.com/rustfs/migration/internal/migrator"
	"github.com/rustfs/migration/internal/s3client"
	"github.com/rustfs/migration/internal/scanner"
	"github.com/rustfs/migration/internal/store"
)

func auditClient(t *testing.T, endpoint string) *s3.Client {
	t.Helper()
	cli, err := s3client.New(context.Background(), config.Endpoint{
		Endpoint: endpoint, Region: "us-east-1", AccessKey: "test", SecretKey: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return cli
}

func TestAuditResumeRejectsDifferentDestination(t *testing.T) {
	ctx := context.Background()
	source := fakes3.NewWithObjects(t, "bucket", map[string][]byte{"key": []byte("data")}, time.Unix(1, 0))
	defer source.Close()
	first := fakes3.New(t)
	defer first.Close()
	second := fakes3.New(t)
	defer second.Close()
	oldCli := auditClient(t, source.URL)
	dir := t.TempDir()
	cfg := &config.Config{
		Bucket: "bucket", DBPath: filepath.Join(dir, "state.db"), ConflictLog: filepath.Join(dir, "conflicts.log"),
		SourceFrozen: true, SpoolDir: dir, Workers: 1, MultipartThreshold: 1024, MultipartPartSize: 5 * 1024 * 1024, MultipartConcurrency: 1,
		Old: config.Endpoint{Endpoint: source.URL}, New: config.Endpoint{Endpoint: first.URL},
	}
	run := func(target *fakes3.Server) error {
		st, err := store.Open(cfg.DBPath, cfg.Bucket)
		if err != nil {
			return err
		}
		defer st.Close()
		if _, err := scanner.Run(ctx, oldCli, cfg.Bucket, st); err != nil {
			return err
		}
		newCli := auditClient(t, target.URL)
		if err := cluster.EnsureBucket(ctx, newCli, cfg.Bucket); err != nil {
			return err
		}
		m, err := migrator.New(cfg, oldCli, newCli, st)
		if err != nil {
			return err
		}
		defer m.Close()
		return m.Run(ctx)
	}
	if err := run(first); err != nil {
		t.Fatal(err)
	}
	if body, ok := first.Object(cfg.Bucket, "key"); !ok || string(body) != "data" {
		t.Fatal("initial migration did not establish the completed checkpoint")
	}
	cfg.New.Endpoint = second.URL
	err := run(second)
	if _, ok := second.Object(cfg.Bucket, "key"); !ok && err == nil {
		t.Fatal("reusing checkpoint with another target returned success but copied no object; DB is only bound to bucket name")
	}
}

// S3 permits object keys that cannot be represented in XML 1.0. Requesting
// encoding-type=url and decoding the returned key permits a complete inventory.
func TestAuditScanXMLUnsafeObjectKey(t *testing.T) {
	const key = "before  +\x01%after"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		encodedKey, encoding := key, ""
		if r.URL.Query().Get("encoding-type") == "url" {
			encodedKey, encoding = "before+%20%2B%01%25after", "<EncodingType>url</EncodingType>"
		}
		_, _ = fmt.Fprintf(w, `<ListBucketResult><Name>bucket</Name><IsTruncated>false</IsTruncated>%s<Contents><Key>%s</Key><Size>1</Size><ETag>abc</ETag><LastModified>2026-01-01T00:00:00Z</LastModified></Contents></ListBucketResult>`, encoding, encodedKey)
	}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"), "bucket")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := scanner.Run(context.Background(), auditClient(t, srv.URL), "bucket", st); err != nil {
		t.Fatalf("legal object key cannot be inventoried: %v", err)
	}
	batch, err := st.BatchPendingAfter("", 10)
	if err != nil || len(batch) != 1 || batch[0].Key != key {
		t.Fatalf("key not preserved: rows=%+v err=%v", batch, err)
	}
}

func TestAuditScanContinuationAcrossBatchBoundary(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<ListBucketResult><Name>bucket</Name>`)
		start, end := 0, 1000
		if r.URL.Query().Get("continuation-token") == "next-page" {
			start, end = 1000, 1501
			_, _ = io.WriteString(w, `<IsTruncated>false</IsTruncated>`)
		} else {
			_, _ = io.WriteString(w, `<IsTruncated>true</IsTruncated><NextContinuationToken>next-page</NextContinuationToken>`)
		}
		for i := start; i < end; i++ {
			_, _ = fmt.Fprintf(w, `<Contents><Key>key-%04d</Key><Size>1</Size><ETag>tag</ETag><LastModified>2026-01-01T00:00:00Z</LastModified></Contents>`, i)
		}
		_, _ = io.WriteString(w, `</ListBucketResult>`)
	}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"), "bucket")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	n, err := scanner.Run(context.Background(), auditClient(t, srv.URL), "bucket", st)
	if err != nil {
		t.Fatal(err)
	}
	c, err := st.Counts()
	if err != nil || n != 1501 || c.Pending != 1501 || calls.Load() != 2 {
		t.Fatalf("pagination lost or duplicated work: total=%d counts=%+v pages=%d err=%v", n, c, calls.Load(), err)
	}
}

func TestFailedRescanRequiresCompleteRetry(t *testing.T) {
	var denySecond atomic.Bool
	denySecond.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Query().Get("continuation-token") == "second" {
			if denySecond.Load() {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code></Error>`)
				return
			}
			_, _ = io.WriteString(w, `<ListBucketResult><Name>bucket</Name><IsTruncated>false</IsTruncated><Contents><Key>z-last</Key><Size>1</Size></Contents></ListBucketResult>`)
			return
		}
		_, _ = io.WriteString(w, `<ListBucketResult><Name>bucket</Name><IsTruncated>true</IsTruncated><NextContinuationToken>second</NextContinuationToken>`)
		for i := 0; i < 500; i++ {
			_, _ = fmt.Fprintf(w, `<Contents><Key>key-%04d</Key><Size>1</Size></Contents>`, i)
		}
		_, _ = io.WriteString(w, `</ListBucketResult>`)
	}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"), "bucket")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.BeginScan(); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteScan(); err != nil {
		t.Fatal(err)
	}
	cli := auditClient(t, srv.URL)
	if _, err := scanner.Run(context.Background(), cli, "bucket", st); err == nil {
		t.Fatal("partial listing failure was hidden")
	}
	if err := st.RequireCompleteScan(); err == nil {
		t.Fatal("failed rescan authorized migration of partial inventory")
	}
	c, err := st.Counts()
	if err != nil || c.Pending != 500 {
		t.Fatalf("first committed batch should remain resumable: %+v, %v", c, err)
	}
	denySecond.Store(false)
	if n, err := scanner.Run(context.Background(), cli, "bucket", st); err != nil || n != 501 {
		t.Fatalf("retry scan: n=%d err=%v", n, err)
	}
	if err := st.RequireCompleteScan(); err != nil {
		t.Fatalf("successful retry not certified: %v", err)
	}
}

func TestScanRejectsInvalidContinuation(t *testing.T) {
	for _, token := range []string{"", "repeated", "missing-truncation-flag"} {
		t.Run("token="+token, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					w.WriteHeader(http.StatusOK)
					return
				}
				w.Header().Set("Content-Type", "application/xml")
				if token == "missing-truncation-flag" {
					_, _ = io.WriteString(w, `<ListBucketResult><Name>bucket</Name></ListBucketResult>`)
					return
				}
				_, _ = fmt.Fprintf(w, `<ListBucketResult><Name>bucket</Name><IsTruncated>true</IsTruncated><NextContinuationToken>%s</NextContinuationToken></ListBucketResult>`, token)
			}))
			defer srv.Close()
			st, err := store.Open(filepath.Join(t.TempDir(), "state.db"), "bucket")
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := scanner.Run(ctx, auditClient(t, srv.URL), "bucket", st); err == nil {
				t.Fatal("broken pagination marked complete")
			}
			if ctx.Err() != nil {
				t.Fatal("scanner relied on deadline to escape broken pagination")
			}
			if err := st.RequireCompleteScan(); err == nil {
				t.Fatal("broken pagination left completed generation")
			}
		})
	}
}

func TestScanRejectsDifferentSourceBeforeInvalidatingInventory(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"), "bucket")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.BindMigration("https://original-source.example", "https://target.example"); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginScan(); err != nil {
		t.Fatal(err)
	}
	if err := st.BatchUpsertScanned([]store.Object{{Key: "original", Size: 4, ETag: "original-etag"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteScan(); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Run(context.Background(), auditClient(t, srv.URL), "bucket", st); err == nil {
		t.Fatal("scan accepted a different source endpoint")
	}
	if requests.Load() != 0 {
		t.Fatalf("mismatched source received %d requests", requests.Load())
	}
	if err := st.RequireCompleteScan(); err != nil {
		t.Fatalf("rejected source invalidated original complete scan: %v", err)
	}
	rows, err := st.BatchPendingAfter("", 10)
	if err != nil || len(rows) != 1 || rows[0].Key != "original" || rows[0].ETag != "original-etag" {
		t.Fatalf("rejected source changed inventory: %+v, %v", rows, err)
	}
}

func TestScanRejectsAlternatingContinuationTokens(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			return
		}
		n := requests.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		if n > 3 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		next := "a"
		if r.URL.Query().Get("continuation-token") == "a" {
			next = "b"
		}
		fmt.Fprintf(w, `<ListBucketResult><Name>bucket</Name><IsTruncated>true</IsTruncated><NextContinuationToken>%s</NextContinuationToken></ListBucketResult>`, next)
	}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"), "bucket")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = scanner.Run(context.Background(), auditClient(t, srv.URL), "bucket", st)
	if err == nil || !strings.Contains(err.Error(), "continuation token") || requests.Load() != 3 {
		t.Fatalf("requests=%d err=%v", requests.Load(), err)
	}
	if err := st.RequireCompleteScan(); err == nil {
		t.Fatal("cyclic scan was marked complete")
	}
}
