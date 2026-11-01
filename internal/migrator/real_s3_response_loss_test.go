//go:build real_s3

package migrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/rustfs/migration/internal/cluster"
	"github.com/rustfs/migration/internal/config"
	"github.com/rustfs/migration/internal/differ"
	"github.com/rustfs/migration/internal/s3client"
	"github.com/rustfs/migration/internal/scanner"
	"github.com/rustfs/migration/internal/store"
)

// A genuine successful RustFS commit is hidden from the caller exactly once.
// The SDK retries the conditional write, which can now return 412/409 or
// NoSuchUpload. That response must not turn our own committed object into a
// permanent conflict. Both runs use the same proxy endpoint and SQLite file.
func TestRealS3CommittedResponseLossRecoversFromCheckpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	source := realConditionClient(t, ctx, "AUDIT_SOURCE")
	directTarget := realConditionClient(t, ctx, "AUDIT_TARGET")
	if aws.ToString(source.Options().BaseEndpoint) == aws.ToString(directTarget.Options().BaseEndpoint) {
		t.Fatal("response-loss test requires separate source and target nodes")
	}
	for _, multipart := range []bool{false, true} {
		t.Run(fmt.Sprintf("multipart=%t", multipart), func(t *testing.T) {
			const partSize = 5 * 1024 * 1024
			const key = "lost-response.bin"
			const businessKey = "business-preserved.txt"
			bucket := fmt.Sprintf("migration-response-loss-%d", time.Now().UnixNano())
			for _, client := range []*s3.Client{source, directTarget} {
				if err := cluster.EnsureBucket(ctx, client, bucket); err != nil {
					t.Fatal(err)
				}
			}
			size := 1024
			if multipart {
				size = 2 * partSize // Two legal 5 MiB parts.
			}
			payload := bytes.Repeat([]byte{0x6d}, size)
			for i := 0; i < len(payload); i += 4093 {
				payload[i] = byte(i / 4093)
			}
			if _, err := source.PutObject(ctx, &s3.PutObjectInput{
				Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(payload),
				ContentLength: aws.Int64(int64(len(payload))), ContentType: aws.String("application/x-response-loss"),
				CacheControl: aws.String("max-age=60"), Metadata: map[string]string{"recovery": "response-loss"},
			}); err != nil {
				t.Fatal(err)
			}
			business := []byte("existing business object must not be rewritten by retries")
			for _, client := range []*s3.Client{source, directTarget} {
				realConditionPut(t, ctx, client, bucket, businessKey, business)
			}
			sourceHead, err := source.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
			if err != nil {
				t.Fatal(err)
			}

			upstream, err := url.Parse(aws.ToString(directTarget.Options().BaseEndpoint))
			if err != nil {
				t.Fatal(err)
			}
			proxy := httputil.NewSingleHostReverseProxy(upstream)
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.Proxy = nil
			proxy.Transport = transport
			defer transport.CloseIdleConnections()
			isCommit := func(request *http.Request) bool {
				if request.URL.Path != "/"+bucket+"/"+key {
					return false
				}
				query := request.URL.Query()
				if multipart {
					return request.Method == http.MethodPost && query.Get("uploadId") != ""
				}
				return request.Method == http.MethodPut && query.Get("uploadId") == ""
			}
			var attempts, committed, injected, businessWrites atomic.Int32
			proxy.ModifyResponse = func(response *http.Response) error {
				if !isCommit(response.Request) || response.StatusCode < 200 || response.StatusCode >= 300 {
					return nil
				}
				committed.Add(1)
				if !injected.CompareAndSwap(0, 1) {
					return nil
				}
				// Read the real upstream acknowledgement completely before
				// replacing it. No failure is injected before RustFS commits.
				_, readErr := io.Copy(io.Discard, response.Body)
				closeErr := response.Body.Close()
				if readErr != nil || closeErr != nil {
					return fmt.Errorf("read actual committed response: read=%v close=%v", readErr, closeErr)
				}
				body := `<Error><Code>InternalError</Code><Message>local audit discarded a successful commit response</Message></Error>`
				response.StatusCode = http.StatusInternalServerError
				response.Status = "500 Internal Server Error"
				response.Header = make(http.Header)
				response.Header.Set("Content-Type", "application/xml")
				response.Header.Set("Content-Length", fmt.Sprint(len(body)))
				response.ContentLength = int64(len(body))
				response.TransferEncoding, response.Trailer = nil, nil
				response.Body = io.NopCloser(strings.NewReader(body))
				return nil
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if isCommit(r) {
					attempts.Add(1)
				}
				if r.URL.Path == "/"+bucket+"/"+businessKey && (r.Method == http.MethodPut || r.Method == http.MethodPost || r.Method == http.MethodDelete) {
					businessWrites.Add(1)
				}
				proxy.ServeHTTP(w, r)
			}))
			defer server.Close()
			target, err := s3client.New(ctx, config.Endpoint{
				Endpoint: server.URL, Region: "us-east-1", AccessKey: "audit-local", SecretKey: "audit-local-secret",
			})
			if err != nil {
				t.Fatal(err)
			}
			// Exercise an SDK retry independently of inherited AWS_MAX_ATTEMPTS.
			target = s3.New(target.Options(), func(options *s3.Options) {
				options.Retryer = retry.NewStandard(func(options *retry.StandardOptions) {
					options.MaxAttempts = 3
					options.MaxBackoff = 100 * time.Millisecond
				})
			})
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
			if _, err := scanner.Run(ctx, source, bucket, st); err != nil {
				t.Fatal(err)
			}
			migration, err := New(cfg, source, target, st)
			if err != nil {
				t.Fatal(err)
			}
			firstErr := migration.Run(ctx)
			if err := migration.Close(); err != nil {
				t.Fatal(err)
			}
			firstCounts, err := st.Counts()
			if err != nil {
				t.Fatal(err)
			}
			if injected.Load() != 1 || committed.Load() != 1 || attempts.Load() < 2 {
				t.Fatalf("did not lose exactly one successful commit and retry: injected=%d committed=%d attempts=%d run=%v", injected.Load(), committed.Load(), attempts.Load(), firstErr)
			}
			if firstCounts.Conflict != 0 || firstCounts.Skipped != 0 {
				t.Fatalf("lost success became a terminal conflict/skip: counts=%+v run=%v", firstCounts, firstErr)
			}
			// This proves the injected response followed a real object commit.
			realConditionAssertObject(t, ctx, directTarget, bucket, key, payload)
			t.Logf("real commit response replaced once; attempts=%d first_run=%v counts=%+v", attempts.Load(), firstErr, firstCounts)
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			st = nil

			// No rescan or manual status edit: resume the same checkpoint and URL.
			st, err = store.Open(cfg.DBPath, bucket)
			if err != nil {
				t.Fatal(err)
			}
			migration, err = New(cfg, source, target, st)
			if err != nil {
				t.Fatal(err)
			}
			err = migration.Run(ctx)
			closeErr := migration.Close()
			if err != nil || closeErr != nil {
				t.Fatalf("reopen recovery failed: run=%v close=%v", err, closeErr)
			}
			counts, err := st.Counts()
			if err != nil || counts.Total != 2 || counts.Done != 2 || counts.Pending != 0 || counts.Failed != 0 || counts.Conflict != 0 || counts.Skipped != 0 {
				t.Fatalf("checkpoint not fully recovered: counts=%+v err=%v", counts, err)
			}
			if committed.Load() != 1 || businessWrites.Load() != 0 {
				t.Fatalf("recovery rewrote an existing object: successful_commits=%d business_writes=%d", committed.Load(), businessWrites.Load())
			}
			realConditionAssertObject(t, ctx, directTarget, bucket, key, payload)
			realConditionAssertObject(t, ctx, directTarget, bucket, businessKey, business)
			targetHead, err := directTarget.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
			if err != nil || !s3client.MetadataEqual(sourceHead, targetHead) {
				t.Fatalf("metadata changed across response-loss recovery: err=%v", err)
			}
			result, err := differ.Run(ctx, source, target, bucket, differ.Options{Workers: 2, ReportExtra: true, Writer: io.Discard})
			if err != nil || result.HasGaps() || result.OK != 2 || result.Extra != 0 {
				t.Fatalf("recovered diff failed: result=%+v err=%v", result, err)
			}
			t.Logf("reopened SQLite recovered without rewriting; multipart=%t bytes=%d sha256=%x diff_ok=%d", multipart, len(payload), sha256.Sum256(payload), result.OK)
		})
	}
}
