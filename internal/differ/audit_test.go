package differ

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rustfs/migration/internal/fakes3"
)

// A terminal HEAD failure must stop the producer as well as the worker.
// Otherwise a real CLI invocation, whose context has no deadline, hangs forever.
func TestAuditDiffReturnsOnHeadFailure(t *testing.T) {
	objects := map[string][]byte{}
	for i := 0; i < 10; i++ {
		objects[fmt.Sprintf("key-%02d", i)] = []byte("body")
	}
	source := fakes3.NewWithObjects(t, "bucket", objects, time.Now())
	defer source.Close()
	headStarted := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bucket" || r.URL.Path == "/bucket/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		select {
		case headStarted <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer target.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	oldCli := testClient(t, ctx, source.URL)
	newCli := testClient(t, ctx, target.URL)
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, oldCli, newCli, "bucket", Options{Workers: 1})
		done <- err
	}()
	select {
	case <-headStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("fixture did not reach target HEAD")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("HEAD 403 must be returned as an error")
		}
	case <-time.After(time.Second):
		cancel() // Release the blocked producer; do not leak the audit goroutine.
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("diff failed to stop even after cancellation")
		}
		t.Fatal("diff remained blocked after terminal HEAD 403; only external cancellation released it")
	}
}

// A multipart ETag is not comparable to a simple MD5. It also is not evidence
// that two equal-length objects match. The verifier must report unverified or
// compare full-object checksums instead of counting this object as OK.
func TestAuditDiffDoesNotCertifySameSizeCorruption(t *testing.T) {
	const partSize = 5 * 1024 * 1024
	sourceBody := bytes.Repeat([]byte("a"), partSize+8)
	corruptBody := append([]byte(nil), sourceBody...)
	corruptBody[0] = 'b'
	first, last := md5.Sum(corruptBody[:partSize]), md5.Sum(corruptBody[partSize:])
	combined := append(first[:], last[:]...)
	corruptETag := fmt.Sprintf(`"%x-2"`, md5.Sum(combined))
	source := fakes3.NewWithObjects(t, "bucket", map[string][]byte{"key": sourceBody}, time.Now())
	defer source.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(corruptBody)))
		w.Header().Set("ETag", corruptETag)
		w.Header().Set("Content-Type", "application/test")
		w.Header().Set("X-Amz-Meta-Origin", "old")
		if r.Method != http.MethodHead {
			_, _ = w.Write(corruptBody)
		}
	}))
	defer target.Close()
	ctx := context.Background()
	res, err := Run(ctx, testClient(t, ctx, source.URL), testClient(t, ctx, target.URL), "bucket", Options{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK != 0 || res.ContentMismatch != 1 || !res.HasGaps() {
		t.Fatalf("expected SHA256 content mismatch: %+v", res)
	}
}

// Bucket existence is part of successful migration even if there are no keys.
func TestAuditDiffDetectsMissingEmptyBucket(t *testing.T) {
	source := fakes3.New(t)
	defer source.Close()
	source.Put("empty-bucket", "seed", []byte("seed"), time.Now(), fakes3.Metadata{})
	source.DeleteKey("empty-bucket", "seed")
	target := fakes3.New(t)
	defer target.Close()
	ctx := context.Background()
	res, err := Run(ctx, testClient(t, ctx, source.URL), testClient(t, ctx, target.URL), "empty-bucket", Options{Workers: 1})
	if err == nil && !res.HasGaps() {
		t.Fatalf("missing target bucket accepted as clean: %+v", res)
	}
}

func TestDiffAcceptsSameContentWithDifferentOpaqueETags(t *testing.T) {
	body := []byte("identical bytes despite different encryption or part layout")
	source := fakes3.NewWithObjects(t, "bucket", map[string][]byte{"key": body}, time.Now())
	defer source.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && r.URL.Path == "/bucket" {
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Header().Set("ETag", `"opaque-encrypted-token"`)
		w.Header().Set("Content-Type", "application/test")
		w.Header().Set("X-Amz-Meta-Origin", "old")
		if r.Method == http.MethodGet {
			if r.Header.Get("If-Match") != `"opaque-encrypted-token"` {
				http.Error(w, "missing representation condition", http.StatusPreconditionFailed)
				return
			}
			_, _ = w.Write(body)
		}
	}))
	defer target.Close()
	ctx := context.Background()
	res, err := Run(ctx, testClient(t, ctx, source.URL), testClient(t, ctx, target.URL), "bucket", Options{Workers: 1})
	if err != nil || res.OK != 1 || res.HasGaps() {
		t.Fatalf("opaque ETags must not cause a false conflict: result=%+v err=%v", res, err)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write(p []byte) (int, error) { return 0, w.err }

func TestDiffPropagatesReportWriteFailure(t *testing.T) {
	source := fakes3.NewWithObjects(t, "bucket", map[string][]byte{"key": []byte("body")}, time.Now())
	defer source.Close()
	target := fakes3.NewWithObjects(t, "bucket", map[string][]byte{"key": []byte("body")}, time.Now())
	defer target.Close()
	ctx := context.Background()
	want := errors.New("report storage full")
	_, err := Run(ctx, testClient(t, ctx, source.URL), testClient(t, ctx, target.URL), "bucket", Options{Workers: 1, Writer: failingWriter{want}})
	if !errors.Is(err, want) {
		t.Fatalf("report error lost: %v", err)
	}
}

func TestConcurrentDiffUsesSeparateInventoryFiles(t *testing.T) {
	objects := map[string][]byte{"key": []byte("body")}
	source := fakes3.NewWithObjects(t, "bucket", objects, time.Now())
	t.Cleanup(source.Close)
	target := fakes3.NewWithObjects(t, "bucket", objects, time.Now())
	t.Cleanup(target.Close)
	for i := 0; i < 3; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			res, err := Run(ctx, testClient(t, ctx, source.URL), testClient(t, ctx, target.URL), "bucket", Options{Workers: 1, ReportExtra: true})
			if err != nil || res.OK != 1 || res.HasGaps() {
				t.Fatalf("concurrent diff result=%+v err=%v", res, err)
			}
		})
	}
}

func TestDiffDecodesURLKeysWithoutTurningPlusIntoSpace(t *testing.T) {
	key := "dir/plus+ percent% 中文\x01"
	body := []byte("body")
	makeEndpoint := func(extra bool) *httptest.Server {
		objects := map[string][]byte{key: body}
		if extra {
			objects["extra+%\x02"] = body
		}
		backend := fakes3.NewWithObjects(t, "bucket", objects, time.Now())
		t.Cleanup(backend.Close)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
				if r.URL.Query().Get("encoding-type") != "url" {
					http.Error(w, "listing must request URL encoding", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/xml")
				fmt.Fprint(w, `<ListBucketResult><Name>bucket</Name><EncodingType>url</EncodingType><IsTruncated>false</IsTruncated>`)
				for k := range objects {
					fmt.Fprintf(w, `<Contents><Key>%s</Key><Size>%d</Size><ETag>"%x"</ETag></Contents>`, url.PathEscape(k), len(body), md5.Sum(body))
				}
				fmt.Fprint(w, `</ListBucketResult>`)
				return
			}
			backend.Config.Handler.ServeHTTP(w, r)
		}))
		t.Cleanup(server.Close)
		return server
	}
	source, target := makeEndpoint(false), makeEndpoint(true)
	ctx := context.Background()
	res, err := Run(ctx, testClient(t, ctx, source.URL), testClient(t, ctx, target.URL), "bucket", Options{Workers: 1, ReportExtra: true})
	if err != nil || res.OK != 1 || res.Extra != 1 || res.HasGaps() {
		t.Fatalf("encoded keys result=%+v err=%v", res, err)
	}
	if len(res.Issues) != 1 || res.Issues[0].Key != "extra+%\x02" {
		t.Fatalf("extra key was not decoded faithfully: %+v", res.Issues)
	}
}

func TestDiffRejectsIncompletePagination(t *testing.T) {
	for _, token := range []string{"", "repeated"} {
		t.Run("token="+token, func(t *testing.T) {
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					return
				}
				w.Header().Set("Content-Type", "application/xml")
				fmt.Fprintf(w, `<ListBucketResult><Name>bucket</Name><IsTruncated>true</IsTruncated><NextContinuationToken>%s</NextContinuationToken></ListBucketResult>`, token)
			}))
			defer source.Close()
			target := fakes3.NewWithObjects(t, "bucket", map[string][]byte{"seed": []byte("body")}, time.Now())
			defer target.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := Run(ctx, testClient(t, ctx, source.URL), testClient(t, ctx, target.URL), "bucket", Options{Workers: 1})
			if err == nil || !strings.Contains(err.Error(), "continuation token") {
				t.Fatalf("invalid pagination was not rejected: %v", err)
			}
		})
	}
}

func TestDiffRejectsMissingCompletionFlag(t *testing.T) {
	for _, extra := range []bool{false, true} {
		t.Run(fmt.Sprintf("target-extra=%t", extra), func(t *testing.T) {
			broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					return
				}
				w.Header().Set("Content-Type", "application/xml")
				fmt.Fprint(w, `<ListBucketResult><Name>bucket</Name></ListBucketResult>`)
			}))
			defer broken.Close()
			good := fakes3.NewWithObjects(t, "bucket", map[string][]byte{"seed": []byte("body")}, time.Now())
			defer good.Close()
			good.DeleteKey("bucket", "seed")
			source, target := broken.URL, good.URL
			if extra {
				source, target = good.URL, broken.URL
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := Run(ctx, testClient(t, ctx, source), testClient(t, ctx, target), "bucket", Options{Workers: 1, ReportExtra: extra})
			if err == nil || !strings.Contains(err.Error(), "IsTruncated") {
				t.Fatalf("missing completion flag accepted: %v", err)
			}
		})
	}
}

func TestDiffReportsPreservedMetadataMismatch(t *testing.T) {
	body := []byte("same body")
	source := fakes3.NewWithObjects(t, "bucket", map[string][]byte{"key": body}, time.Now())
	defer source.Close()
	target := fakes3.New(t)
	defer target.Close()
	target.Put("bucket", "key", body, time.Now(), fakes3.Metadata{ContentType: "application/test", Metadata: map[string]string{"origin": "different"}})
	ctx := context.Background()
	res, err := Run(ctx, testClient(t, ctx, source.URL), testClient(t, ctx, target.URL), "bucket", Options{Workers: 1})
	if err != nil || res.MetadataMismatch != 1 || !res.HasGaps() || res.OK != 0 {
		t.Fatalf("metadata mismatch missed: result=%+v err=%v", res, err)
	}
}

func TestDiffRejectsSourceChangedSinceListing(t *testing.T) {
	body := []byte("body")
	source := fakes3.NewWithObjects(t, "bucket", map[string][]byte{"key": body}, time.Now())
	defer source.Close()
	sourceProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && r.URL.Path == "/bucket/key" {
			if r.Header.Get("If-Match") != fmt.Sprintf(`"%x"`, md5.Sum(body)) {
				http.Error(w, "HEAD must bind the listing ETag", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			w.Header().Set("ETag", `"changed-since-list"`)
			return
		}
		source.Config.Handler.ServeHTTP(w, r)
	}))
	defer sourceProxy.Close()
	target := fakes3.NewWithObjects(t, "bucket", map[string][]byte{"key": body}, time.Now())
	defer target.Close()
	ctx := context.Background()
	_, err := Run(ctx, testClient(t, ctx, sourceProxy.URL), testClient(t, ctx, target.URL), "bucket", Options{Workers: 1})
	if err == nil || !strings.Contains(err.Error(), "changed after listing") {
		t.Fatalf("changed source must fail verification: %v", err)
	}
}

func TestDiffRejectsAlternatingContinuationTokens(t *testing.T) {
	for _, targetExtra := range []bool{false, true} {
		t.Run(fmt.Sprintf("target-extra=%t", targetExtra), func(t *testing.T) {
			var requests atomic.Int32
			broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			defer broken.Close()
			good := fakes3.NewWithObjects(t, "bucket", map[string][]byte{"seed": []byte("body")}, time.Now())
			defer good.Close()
			good.DeleteKey("bucket", "seed")
			source, target := broken.URL, good.URL
			if targetExtra {
				source, target = good.URL, broken.URL
			}
			ctx := context.Background()
			_, err := Run(ctx, testClient(t, ctx, source), testClient(t, ctx, target), "bucket", Options{Workers: 1, ReportExtra: targetExtra})
			if err == nil || !strings.Contains(err.Error(), "continuation token") || requests.Load() != 3 {
				t.Fatalf("requests=%d err=%v", requests.Load(), err)
			}
		})
	}
}

func TestDiffStreamsAllIssuesAndPropagatesWriterErrors(t *testing.T) {
	objects := map[string][]byte{}
	for i := 0; i < 75; i++ {
		objects[fmt.Sprintf("key-%03d", i)] = []byte("data")
	}
	objects["special\n雪"] = []byte("data")
	src := fakes3.NewWithObjects(t, "bucket", objects, time.Now())
	defer src.Close()
	dst := fakes3.NewWithObjects(t, "bucket", map[string][]byte{"extra": []byte("target")}, time.Now())
	defer dst.Close()
	ctx := context.Background()
	oldCli, newCli := testClient(t, ctx, src.URL), testClient(t, ctx, dst.URL)
	var jsonl, summary bytes.Buffer
	result, err := Run(ctx, oldCli, newCli, "bucket", Options{Workers: 8, ReportExtra: true, Writer: &summary, IssuesJSONL: &jsonl})
	if err != nil {
		t.Fatal(err)
	}
	if result.Missing != 76 || result.Extra != 1 || len(result.Issues) != 50 {
		t.Fatalf("result: %+v", result)
	}
	decoder := json.NewDecoder(&jsonl)
	entries := map[string]string{}
	for decoder.More() {
		var entry struct{ Bucket, Key, Kind, Detail string }
		if err := decoder.Decode(&entry); err != nil {
			t.Fatal(err)
		}
		if entry.Bucket != "bucket" || entry.Detail == "" {
			t.Fatalf("entry: %+v", entry)
		}
		if _, ok := entries[entry.Key]; ok {
			t.Fatalf("duplicate key %q", entry.Key)
		}
		entries[entry.Key] = entry.Kind
	}
	if len(entries) != 77 || entries["special\n雪"] != "missing" || entries["extra"] != "extra" || !strings.Contains(summary.String(), "27 more issues") {
		t.Fatalf("entries=%d summary=%s", len(entries), summary.String())
	}
	_, err = Run(ctx, oldCli, newCli, "bucket", Options{Workers: 8, IssuesJSONL: errorWriter{}})
	if err == nil || !strings.Contains(err.Error(), "write diff JSONL") {
		t.Fatalf("writer error lost: %v", err)
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("injected disk failure") }
