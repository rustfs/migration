//go:build real_s3

package migrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/rustfs/migration/internal/cluster"
	"github.com/rustfs/migration/internal/config"
	"github.com/rustfs/migration/internal/differ"
	"github.com/rustfs/migration/internal/s3client"
	"github.com/rustfs/migration/internal/scanner"
	"github.com/rustfs/migration/internal/store"
)

// The proxy forwards real UploadPart bodies and waits only after RustFS has
// accepted a part. Cancellation therefore exercises a real, incomplete remote
// multipart upload, rather than failing before any data reached the server.
// The same proxy endpoint remains bound to the checkpoint during both runs.
func TestRealS3MultipartCancellationAndCheckpointRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	endpoint := func(name string) string {
		raw := os.Getenv(name)
		if raw == "" {
			t.Skip("run via scripts/audit-local-rustfs.py with isolated local RustFS nodes")
		}
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.RawQuery != "" || (u.Path != "" && u.Path != "/") {
			t.Fatalf("%s must identify an isolated HTTP loopback server", name)
		}
		return raw
	}
	newClient := func(raw string) *s3.Client {
		cli, err := s3client.New(ctx, config.Endpoint{Endpoint: raw, Region: "us-east-1", AccessKey: "audit-local", SecretKey: "audit-local-secret"})
		if err != nil {
			t.Fatal(err)
		}
		return cli
	}
	sourceURL, targetURL := endpoint("AUDIT_SOURCE"), endpoint("AUDIT_TARGET")
	if sourceURL == targetURL {
		t.Fatal("recovery test requires separate source and target nodes")
	}
	src, directTarget := newClient(sourceURL), newClient(targetURL)
	upstream, err := url.Parse(targetURL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	proxy.Transport = transport
	defer transport.CloseIdleConnections()
	var injectFailure atomic.Bool
	injectFailure.Store(true)
	partAccepted := make(chan string, 1)
	var signalPart sync.Once
	var successfulAborts atomic.Int32
	proxy.ModifyResponse = func(response *http.Response) error {
		request := response.Request
		query := request.URL.Query()
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			if request.Method == http.MethodDelete && query.Get("uploadId") != "" {
				successfulAborts.Add(1)
			}
			if request.Method == http.MethodPut && query.Get("partNumber") != "" && query.Get("uploadId") != "" && injectFailure.Load() {
				signalPart.Do(func() { partAccepted <- query.Get("uploadId") })
				<-request.Context().Done()
				return request.Context().Err()
			}
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// Expected cancellation should not generate misleading proxy diagnostics.
		if r.Context().Err() == nil {
			http.Error(w, "local recovery proxy: "+err.Error(), http.StatusBadGateway)
		}
	}
	server := httptest.NewServer(proxy)
	defer server.Close()
	dst := newClient(server.URL)
	bucket := fmt.Sprintf("migration-recovery-%d", time.Now().UnixNano())
	for _, cli := range []*s3.Client{src, directTarget} {
		if err := cluster.EnsureBucket(ctx, cli, bucket); err != nil {
			t.Fatal(err)
		}
	}
	const partSize = 5 * 1024 * 1024
	const key = "recovery/multipart.bin"
	payload := bytes.Repeat([]byte{0x7c}, 2*partSize+237)
	// Different part boundaries carry different bytes, so order and truncation
	// mistakes cannot accidentally retain the expected final digest.
	for offset := 0; offset < len(payload); offset += 4093 {
		payload[offset] = byte(offset / 4093)
	}
	wantDigest := sha256.Sum256(payload)
	if _, err := src.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(payload),
		ContentLength: aws.Int64(int64(len(payload))), ContentType: aws.String("application/x-recovery-test"),
		CacheControl: aws.String("max-age=60"), Metadata: map[string]string{"recovery": "checkpoint"},
	}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := &config.Config{
		SourceFrozen: true, Bucket: bucket, DBPath: filepath.Join(dir, "checkpoint.db"),
		ConflictLog: filepath.Join(dir, "conflict.log"), SpoolDir: dir,
		Workers: 1, MultipartThreshold: partSize, MultipartPartSize: partSize, MultipartConcurrency: 2,
	}
	st, err := store.Open(cfg.DBPath, bucket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if st != nil {
			_ = st.Close()
		}
	}()
	if _, err := scanner.Run(ctx, src, bucket, st); err != nil {
		t.Fatal(err)
	}
	active, err := New(cfg, src, dst, st)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if active != nil {
			_ = active.Close()
		}
	}()
	first := active
	runCtx, stopRun := context.WithCancel(ctx)
	finished := make(chan struct{})
	runResult := make(chan error, 1)
	go func() {
		defer close(finished)
		runResult <- first.Run(runCtx)
	}()
	defer func() {
		stopRun()
		select {
		case <-finished:
		case <-time.After(40 * time.Second):
			t.Error("canceled migration did not finish during test cleanup")
		}
	}()
	var interruptedUploadID string
	select {
	case interruptedUploadID = <-partAccepted:
	case err := <-runResult:
		t.Fatalf("migration ended before RustFS accepted a multipart part: %v", err)
	case <-time.After(time.Minute):
		t.Fatal("RustFS did not accept an UploadPart within one minute")
	}
	// Removing the fault does not change the endpoint bound in SQLite. The
	// already held response still waits for cancellation, while abort/retry
	// requests are forwarded normally.
	injectFailure.Store(false)
	stopRun()
	select {
	case err := <-runResult:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("interrupted migration must return cancellation, got %v", err)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("migration did not stop and abort after cancellation")
	}
	counts, err := st.Counts()
	if err != nil || counts.Total != 1 || counts.Failed != 1 || counts.Done != 0 || counts.Skipped != 0 || counts.Conflict != 0 {
		t.Fatalf("cancellation lost retryable checkpoint: counts=%+v err=%v", counts, err)
	}
	if successfulAborts.Load() == 0 {
		t.Fatal("cancellation did not receive a successful real AbortMultipartUpload response")
	}
	if _, err := directTarget.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}); !s3client.IsNotFound(err) {
		t.Fatalf("cancellation unexpectedly committed a target object: %v", err)
	}
	assertNoMultipartUploads := func(stage string) {
		t.Helper()
		listing, err := directTarget.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)})
		if err != nil {
			t.Fatalf("%s: cannot verify multipart cleanup: %v", stage, err)
		}
		if aws.ToBool(listing.IsTruncated) || len(listing.Uploads) != 0 {
			t.Fatalf("%s: incomplete uploads remain after abort/complete: %+v", stage, listing.Uploads)
		}
	}
	assertNoMultipartUploads("canceled run")
	t.Logf("canceled after RustFS accepted a part; upload=%q aborts=%d checkpoint failed=%d", interruptedUploadID, successfulAborts.Load(), counts.Failed)
	if err := active.Close(); err != nil {
		t.Fatal(err)
	}
	active = nil
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st = nil

	// Reopen the same durable inventory and resume directly: no rescan, no
	// manual state changes, and no replacement of the target proxy endpoint.
	st, err = store.Open(cfg.DBPath, bucket)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := st.BatchPendingAfter("", 10)
	if err != nil || len(pending) != 1 || pending[0].Key != key || pending[0].Status != store.StatusFailed {
		t.Fatalf("reopened checkpoint is not retryable: objects=%+v err=%v", pending, err)
	}
	active, err = New(cfg, src, dst, st)
	if err != nil {
		t.Fatal(err)
	}
	if err := active.Run(ctx); err != nil {
		t.Fatalf("resume after reopening checkpoint: %v", err)
	}
	counts, err = st.Counts()
	if err != nil || counts.Total != 1 || counts.Done != 1 || counts.Pending != 0 || counts.Failed != 0 || counts.Conflict != 0 || counts.Skipped != 0 {
		t.Fatalf("recovered checkpoint not complete: counts=%+v err=%v", counts, err)
	}
	for name, cli := range map[string]*s3.Client{"source": src, "target": directTarget} {
		object, err := cli.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.New()
		n, readErr := io.Copy(hash, object.Body)
		closeErr := object.Body.Close()
		if readErr != nil || closeErr != nil || n != int64(len(payload)) || !bytes.Equal(hash.Sum(nil), wantDigest[:]) {
			t.Fatalf("%s SHA256/length mismatch after recovery: bytes=%d read=%v close=%v", name, n, readErr, closeErr)
		}
	}
	result, err := differ.Run(ctx, src, dst, bucket, differ.Options{Workers: 2, ReportExtra: true, Writer: io.Discard})
	if err != nil || result.HasGaps() || result.OK != 1 || result.Extra != 0 {
		t.Fatalf("recovery verification failed: result=%+v err=%v", result, err)
	}
	assertNoMultipartUploads("recovered run")
	t.Logf("resumed from reopened SQLite via unchanged endpoint; bytes=%d sha256=%x diff_ok=%d", len(payload), wantDigest, result.OK)
}
