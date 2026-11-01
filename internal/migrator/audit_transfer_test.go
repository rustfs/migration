package migrator

// Regression tests for the 2026-10-29 audit safety findings.
// All servers are loopback-only; all non-final audit parts are at least 5 MiB.

import (
	"bytes"
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/rustfs/migration/internal/fakes3"
	"github.com/rustfs/migration/internal/scanner"
	"github.com/rustfs/migration/internal/store"
)

const auditPartSize = 5 << 20 // legal S3 non-final multipart part size

var auditModified = time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)

func auditServer(t *testing.T, objects map[string][]byte) *fakes3.Server {
	t.Helper()
	s := fakes3.NewWithObjects(t, "bucket", objects, auditModified)
	t.Cleanup(s.Close)
	return s
}

func auditWrap(t *testing.T, s *fakes3.Server, hook func(http.ResponseWriter, *http.Request, http.Handler)) string {
	t.Helper()
	wrapped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hook(w, r, s.Config.Handler)
	}))
	t.Cleanup(wrapped.Close)
	return wrapped.URL
}

func auditMigration(t *testing.T, sourceURL, targetURL string) (*Migrator, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "migration.db")
	st, err := store.Open(dbPath, "bucket")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	source := newTestClient(t, context.Background(), sourceURL)
	target := newTestClient(t, context.Background(), targetURL)
	if _, err := scanner.Run(context.Background(), source, "bucket", st); err != nil {
		t.Fatal(err)
	}
	cfg := testCfg(dbPath, filepath.Join(dir, "conflict.log"), dir)
	cfg.Workers = 1
	cfg.MultipartThreshold = auditPartSize
	cfg.MultipartPartSize = auditPartSize
	cfg.MultipartConcurrency = 1
	m, err := New(cfg, source, target, st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, st
}

func auditRun(t *testing.T, m *Migrator) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Per-object errors may legitimately be returned by a future fix.
	_ = m.Run(ctx)
	if ctx.Err() != nil {
		t.Fatalf("migration exceeded test deadline: %v", ctx.Err())
	}
}

func auditError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>injected audit response</Message></Error>", code)
}

func TestAuditConcurrentTargetWriteMustNotBeOverwritten(t *testing.T) {
	for _, multipart := range []bool{false, true} {
		t.Run(fmt.Sprintf("multipart=%t", multipart), func(t *testing.T) {
			body := []byte("source snapshot")
			if multipart {
				body = bytes.Repeat([]byte("S"), auditPartSize+1)
			}
			source := auditServer(t, map[string][]byte{"key": body})
			target := auditServer(t, nil)
			businessWrite := []byte("business data committed after migration HEAD")
			var once sync.Once
			url := auditWrap(t, target, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
				q := r.URL.Query()
				isCommit := r.Method == http.MethodPut && q.Get("uploadId") == "" ||
					r.Method == http.MethodPost && q.Get("uploadId") != ""
				if isCommit {
					once.Do(func() {
						target.Put("bucket", "key", businessWrite, auditModified, fakes3.Metadata{})
					})
					// Model S3's server-side conditional write support. The original
					// fixture ignores this header, so it cannot test atomic creation.
					if r.Header.Get("If-None-Match") == "*" {
						auditError(w, http.StatusPreconditionFailed, "PreconditionFailed")
						return
					}
				}
				next.ServeHTTP(w, r)
			})
			m, st := auditMigration(t, source.URL, url)
			auditRun(t, m)
			got, exists := target.Object("bucket", "key")
			counts, err := st.Counts()
			if err != nil {
				t.Fatal(err)
			}
			if !exists || !bytes.Equal(got, businessWrite) {
				t.Fatalf("concurrent target write overwritten: stored_bytes=%d source_bytes=%d counts=%+v", len(got), len(body), counts)
			}
		})
	}
}

func TestAuditMultipartSourceMutationMustNotCommitMixedVersions(t *testing.T) {
	before := bytes.Repeat([]byte("A"), 2*auditPartSize)
	after := bytes.Repeat([]byte("B"), 2*auditPartSize)
	source := auditServer(t, map[string][]byte{"key": before})
	target := auditServer(t, nil)
	var mutate sync.Once
	url := auditWrap(t, source, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if r.Method == http.MethodGet && r.Header.Get("Range") != "" {
			body, _ := source.Object("bucket", "key")
			etag := fmt.Sprintf("\"%x\"", md5.Sum(body))
			if match := r.Header.Get("If-Match"); match != "" && match != etag {
				auditError(w, http.StatusPreconditionFailed, "PreconditionFailed")
				return
			}
			// Set ETag before the fixture writes status 206, matching real S3.
			w.Header().Set("ETag", etag)
			next.ServeHTTP(w, r)
			mutate.Do(func() {
				source.Put("bucket", "key", after, auditModified.Add(time.Second), fakes3.Metadata{})
			})
			return
		}
		next.ServeHTTP(w, r)
	})
	m, st := auditMigration(t, url, target.URL)
	auditRun(t, m)
	got, exists := target.Object("bucket", "key")
	counts, err := st.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if exists && !bytes.Equal(got, before) && !bytes.Equal(got, after) {
		t.Fatalf("committed object combines two source versions: bytes=%d prefix=%q suffix=%q counts=%+v", len(got), got[:8], got[len(got)-8:], counts)
	}
}

func TestAuditConcurrentDeleteMustNotResurrect(t *testing.T) {
	body := []byte("live application deletes this historical object")
	source := auditServer(t, map[string][]byte{"key": body})
	target := auditServer(t, nil)
	deleted := make(chan struct{})
	var once sync.Once
	sourceURL := auditWrap(t, source, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		next.ServeHTTP(w, r)
		if r.Method == http.MethodGet && r.URL.Path == "/bucket/key" {
			// The body has been captured by the migration GET. Model the
			// application's dual-delete from DESIGN.md, before target commit.
			once.Do(func() {
				source.DeleteKey("bucket", "key")
				target.DeleteKey("bucket", "key")
				close(deleted)
			})
		}
	})
	targetURL := auditWrap(t, target, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if r.Method == http.MethodPut {
			select {
			case <-deleted:
			case <-r.Context().Done():
				return
			}
		}
		next.ServeHTTP(w, r)
	})
	m, st := auditMigration(t, sourceURL, targetURL)
	auditRun(t, m)
	select {
	case <-deleted:
	default:
		t.Fatal("test did not reach the concurrent application delete")
	}
	if _, exists := source.Object("bucket", "key"); exists {
		t.Fatal("application delete did not remove source")
	}
	counts, err := st.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if got, exists := target.Object("bucket", "key"); exists {
		t.Fatalf("application-deleted object resurrected on target: body=%q counts=%+v", got, counts)
	}
}

func TestAuditUnverifiableTargetMustNotBeConsideredMigrated(t *testing.T) {
	for _, etag := range []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-2", "opaque-encrypted", ""} {
		t.Run("etag="+etag, func(t *testing.T) {
			source := auditServer(t, map[string][]byte{"key": []byte("AAAA")})
			target := auditServer(t, map[string][]byte{"key": []byte("BBBB")})
			m, st := auditMigration(t, source.URL, auditETagEndpoint(t, target, etag))
			auditRun(t, m)
			counts, err := st.Counts()
			if err != nil {
				t.Fatal(err)
			}
			if counts.Done != 0 || counts.Conflict+counts.Failed != 1 {
				t.Fatalf("same-sized unverified content accepted: %+v", counts)
			}
			got, _ := target.Object("bucket", "key")
			if !bytes.Equal(got, []byte("BBBB")) {
				t.Fatal("existing target was modified")
			}
		})
	}
}

func TestAuditTarget404MustRemainRetryable(t *testing.T) {
	for _, phase := range []string{"put", "create-multipart", "complete-multipart", "verify-head"} {
		t.Run(phase, func(t *testing.T) {
			body := []byte("source still exists")
			if strings.Contains(phase, "multipart") {
				body = bytes.Repeat([]byte("M"), auditPartSize+1)
			}
			source := auditServer(t, map[string][]byte{"key": body})
			target := auditServer(t, nil)
			url := auditWrap(t, target, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
				q := r.URL.Query()
				_, exists := target.Object("bucket", "key")
				fail := phase == "put" && r.Method == http.MethodPut && q.Get("uploadId") == "" ||
					phase == "create-multipart" && r.Method == http.MethodPost && q.Has("uploads") ||
					phase == "complete-multipart" && r.Method == http.MethodPost && q.Get("uploadId") != "" ||
					phase == "verify-head" && r.Method == http.MethodHead && exists
				if fail {
					auditError(w, http.StatusNotFound, "NoSuchBucket")
					return
				}
				next.ServeHTTP(w, r)
			})
			m, st := auditMigration(t, source.URL, url)
			auditRun(t, m)
			counts, err := st.Counts()
			if err != nil {
				t.Fatal(err)
			}
			if counts.Skipped != 0 || counts.Failed != 1 {
				t.Fatalf("destination %s 404 misclassified as missing source: %+v", phase, counts)
			}
		})
	}
}

func TestAuditControlStableSourceCopiesSingleAndMultipart(t *testing.T) {
	for _, size := range []int{15, auditPartSize + 1} {
		t.Run(fmt.Sprintf("size=%d", size), func(t *testing.T) {
			body := bytes.Repeat([]byte("C"), size)
			source := auditServer(t, map[string][]byte{"key": body})
			target := auditServer(t, nil)
			m, st := auditMigration(t, source.URL, target.URL)
			auditRun(t, m)
			counts, err := st.Counts()
			if err != nil {
				t.Fatal(err)
			}
			got, exists := target.Object("bucket", "key")
			if !exists || !bytes.Equal(got, body) || counts.Done != 1 {
				t.Fatalf("control copy failed: exists=%t bytes=%d counts=%+v", exists, len(got), counts)
			}
		})
	}
}

func TestMigrationRequiresFrozenSource(t *testing.T) {
	source := auditServer(t, map[string][]byte{"key": []byte("source")})
	target := auditServer(t, nil)
	m, st := auditMigration(t, source.URL, target.URL)
	m.Cfg.SourceFrozen = false
	if err := m.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "source_frozen") {
		t.Fatalf("unfrozen migration must fail before copying: %v", err)
	}
	counts, err := st.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if counts.Pending != 1 || target.PutObjectCalls("key") != 0 {
		t.Fatalf("unfrozen migration performed work: %+v", counts)
	}
}

func TestSameETagDoesNotProveContentEquality(t *testing.T) {
	sourceBytes := []byte("AAAA")
	source := auditServer(t, map[string][]byte{"key": sourceBytes})
	target := auditServer(t, map[string][]byte{"key": []byte("BBBB")})
	etag := fmt.Sprintf("\"%x\"", md5.Sum(sourceBytes))
	url := auditWrap(t, target, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		// Model opaque ETags that happen to be equal across two endpoints.
		if r.Method == http.MethodHead || r.Method == http.MethodGet {
			r.Header.Del("If-Match")
			recorder := httptest.NewRecorder()
			next.ServeHTTP(recorder, r)
			for key, values := range recorder.Header() {
				w.Header()[key] = values
			}
			w.Header().Set("ETag", etag)
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
			return
		}
		next.ServeHTTP(w, r)
	})
	m, st := auditMigration(t, source.URL, url)
	auditRun(t, m)
	counts, err := st.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if counts.Done != 0 || counts.Conflict != 1 {
		t.Fatalf("equal ETags hid different content: %+v", counts)
	}
	got, _ := target.Object("bucket", "key")
	if !bytes.Equal(got, []byte("BBBB")) {
		t.Fatal("existing target was modified")
	}
}

func TestMultipartRejectsInvalidRangeBeforeCommit(t *testing.T) {
	source := auditServer(t, map[string][]byte{"key": bytes.Repeat([]byte("R"), auditPartSize+1)})
	target := auditServer(t, nil)
	url := auditWrap(t, source, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if r.Method == http.MethodGet && r.Header.Get("Range") != "" {
			// A proxy incorrectly strips Range, so S3 returns the full body.
			r.Header.Del("Range")
		}
		next.ServeHTTP(w, r)
	})
	m, st := auditMigration(t, url, target.URL)
	if err := m.Run(context.Background()); err == nil {
		t.Fatal("invalid range response must fail")
	}
	counts, err := st.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if counts.Failed != 1 || target.CompleteMultipartCalls("key") != 0 {
		t.Fatalf("invalid range was committed: %+v", counts)
	}
}

func TestSourceChangedSinceScanFailsBeforeWrite(t *testing.T) {
	source := auditServer(t, map[string][]byte{"key": []byte("old-data")})
	target := auditServer(t, nil)
	m, st := auditMigration(t, source.URL, target.URL)
	source.Put("bucket", "key", []byte("new-data"), auditModified.Add(time.Second), fakes3.Metadata{})
	if err := m.Run(context.Background()); err == nil {
		t.Fatal("changed source must fail")
	}
	counts, err := st.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if counts.Failed != 1 || target.PutObjectCalls("key") != 0 {
		t.Fatalf("changed source was copied: %+v", counts)
	}
}

func TestCommittedTargetCorruptionIsNotMarkedDone(t *testing.T) {
	source := auditServer(t, map[string][]byte{"key": []byte("AAAA")})
	target := auditServer(t, nil)
	url := auditWrap(t, target, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		next.ServeHTTP(w, r)
		if r.Method == http.MethodPut {
			target.Put("bucket", "key", []byte("BBBB"), auditModified, fakes3.Metadata{ContentType: "application/test", Metadata: map[string]string{"origin": "old"}})
		}
	})
	m, st := auditMigration(t, source.URL, url)
	if err := m.Run(context.Background()); err == nil {
		t.Fatal("read-back corruption must fail")
	}
	counts, err := st.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if counts.Done != 0 || counts.Failed != 1 {
		t.Fatalf("corrupt target accepted: %+v", counts)
	}
}

func TestEqualContentDifferentMetadataConflicts(t *testing.T) {
	body := []byte("same content")
	source := auditServer(t, map[string][]byte{"key": body})
	target := auditServer(t, nil)
	target.Put("bucket", "key", body, auditModified, fakes3.Metadata{ContentType: "text/plain", Metadata: map[string]string{"origin": "business"}})
	m, st := auditMigration(t, source.URL, target.URL)
	auditRun(t, m)
	counts, err := st.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if counts.Done != 0 || counts.Conflict != 1 {
		t.Fatalf("metadata mismatch was accepted as done: %+v", counts)
	}
	meta, _ := target.ObjectMetadata("bucket", "key")
	if meta.ContentType != "text/plain" || meta.Metadata["origin"] != "business" {
		t.Fatal("existing target metadata changed")
	}
}

func TestConflictPersistenceFailureStopsWorkerPool(t *testing.T) {
	source := auditServer(t, map[string][]byte{"a": []byte("source"), "b": []byte("source")})
	target := auditServer(t, map[string][]byte{"a": []byte("target"), "b": []byte("target")})
	m, st := auditMigration(t, source.URL, target.URL)
	if err := m.conflict.f.Close(); err != nil {
		t.Fatal(err)
	}
	err := m.Run(context.Background())
	var fatal *persistenceError
	if !errors.As(err, &fatal) {
		t.Fatalf("want fatal persistence error, got %v", err)
	}
	counts, err := st.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if counts.Pending != 2 || counts.Failed != 0 || m.processed.Load() != 1 {
		t.Fatalf("fatal persistence error was downgraded or pool continued: %+v processed=%d", counts, m.processed.Load())
	}
}

func TestLostCommitResponseReconcilesRetryPrecondition(t *testing.T) {
	for _, tc := range []struct {
		name                                                       string
		multipart, businessOverwrite, sourceChanged, retryConflict bool
	}{
		{name: "single-own-commit"},
		{name: "multipart-own-commit", multipart: true},
		{name: "single-business-replacement", businessOverwrite: true},
		{name: "multipart-business-replacement", multipart: true, businessOverwrite: true},
		{name: "source-changed-after-commit", sourceChanged: true},
		{name: "retry-409-remains-failed", retryConflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte("historical object")
			if tc.multipart {
				body = bytes.Repeat([]byte("S"), auditPartSize+1)
			}
			source := auditServer(t, map[string][]byte{"key": body})
			target := auditServer(t, nil)
			business := bytes.Repeat([]byte("B"), len(body))
			var commits atomic.Int32
			url := auditWrap(t, target, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
				q := r.URL.Query()
				commit := r.Method == http.MethodPut && q.Get("uploadId") == "" ||
					r.Method == http.MethodPost && q.Get("uploadId") != ""
				if !commit {
					next.ServeHTTP(w, r)
					return
				}
				if r.Header.Get("If-None-Match") != "*" {
					t.Error("commit lost its atomic create condition")
				}
				if commits.Add(1) > 1 {
					if tc.retryConflict {
						auditError(w, http.StatusConflict, "ConditionalRequestConflict")
						return
					}
					// Some servers check the existing key before the completed
					// upload ID. Model that legitimate 412 retry response for MPU.
					auditError(w, http.StatusPreconditionFailed, "PreconditionFailed")
					return
				}
				recorded := httptest.NewRecorder()
				next.ServeHTTP(recorded, r) // Commit bytes, but discard the response.
				if recorded.Code != http.StatusOK {
					t.Errorf("injected commit did not succeed: %d %s", recorded.Code, recorded.Body.String())
				}
				attrs := fakes3.Metadata{ContentType: "application/test", Metadata: map[string]string{"origin": "old"}}
				if tc.businessOverwrite {
					target.Put("bucket", "key", business, auditModified, attrs)
				}
				if tc.sourceChanged {
					source.Put("bucket", "key", business, auditModified.Add(time.Second), attrs)
				}
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("drop committed response: %v", err)
					return
				}
				_ = connection.Close() // SDK retries the real request after EOF.
			})
			m, st := auditMigration(t, source.URL, url)
			// Keep fault-injection deterministic and independent of ambient
			// AWS retry settings; the seekable spool supports a full retry.
			m.NewCli = s3.New(m.NewCli.Options(), func(options *s3.Options) {
				options.Retryer = retry.NewStandard(func(opts *retry.StandardOptions) { opts.MaxAttempts = 2; opts.MaxBackoff = time.Millisecond })
			})
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			err := m.Run(ctx)
			if ctx.Err() != nil {
				t.Fatalf("lost-response test timed out: %v", ctx.Err())
			}
			if (err != nil) != (tc.sourceChanged || tc.retryConflict) {
				t.Fatalf("run error=%v sourceChanged=%t retryConflict=%t", err, tc.sourceChanged, tc.retryConflict)
			}
			if commits.Load() != 2 {
				t.Fatalf("did not exercise lost-response retry: commits=%d", commits.Load())
			}
			counts, err := st.Counts()
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.sourceChanged || tc.retryConflict:
				if counts.Failed != 1 || counts.Done != 0 || counts.Conflict != 0 {
					t.Fatalf("changed source accepted during reconciliation: %+v", counts)
				}
			case tc.businessOverwrite:
				if counts.Conflict != 1 || counts.Done != 0 {
					t.Fatalf("business replacement was mistaken for own commit: %+v", counts)
				}
			default:
				if counts.Done != 1 || counts.Conflict != 0 || counts.Failed != 0 {
					t.Fatalf("own successful commit was not recovered: %+v", counts)
				}
			}
			want := body
			if tc.businessOverwrite {
				want = business
			}
			got, exists := target.Object("bucket", "key")
			if !exists || !bytes.Equal(got, want) {
				t.Fatal("reconciliation changed committed target bytes")
			}
		})
	}
}
