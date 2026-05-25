package migrator

import (
	"bytes"
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	appconfig "github.com/rustfs/migration/internal/config"
	"github.com/rustfs/migration/internal/s3client"
	"github.com/rustfs/migration/internal/scanner"
	"github.com/rustfs/migration/internal/store"
)

func TestScanAndMigrateEndToEnd(t *testing.T) {
	ctx := context.Background()
	lastModified := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)

	oldS3 := newFakeS3(t, map[string][]byte{
		"small.txt":      []byte("hello"),
		"large.bin":      []byte("0123456789abcdef"),
		"conflict.txt":   []byte("old-conflict"),
		"gone.txt":       []byte("gone"),
		"gone-large.bin": []byte("this object disappears before multipart copy"),
	}, lastModified)
	defer oldS3.Close()

	newS3 := newFakeS3(t, map[string][]byte{
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

	if err := scanner.Run(ctx, oldClient, "bucket", st); err != nil {
		t.Fatalf("scan: %v", err)
	}
	oldS3.delete("gone.txt")
	oldS3.delete("gone-large.bin")

	cfg := &appconfig.Config{
		Bucket:               "bucket",
		DBPath:               dbPath,
		ConflictLog:          conflictPath,
		Workers:              3,
		MultipartThreshold:   8,
		MultipartPartSize:    4,
		MultipartConcurrency: 2,
	}
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

	if newS3.copyObjectCalls() != 0 || oldS3.deleteObjectCalls() != 0 || newS3.deleteObjectCalls() != 0 {
		t.Fatalf("unexpected unsafe API usage: oldDelete=%d newDelete=%d newCopy=%d",
			oldS3.deleteObjectCalls(), newS3.deleteObjectCalls(), newS3.copyObjectCalls())
	}
}

// TestResumeAfterTransientFailure verifies that a migration which fails part
// way through can be resumed without re-uploading already-completed objects
// and without losing any pending work.
func TestResumeAfterTransientFailure(t *testing.T) {
	ctx := context.Background()
	lastModified := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)

	oldS3 := newFakeS3(t, map[string][]byte{
		"small.txt": []byte("hi"),
		"large.bin": []byte("0123456789abcdef0123456789abcdef"),
	}, lastModified)
	defer oldS3.Close()

	newS3 := newFakeS3(t, map[string][]byte{}, lastModified)
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

	if err := scanner.Run(ctx, oldClient, "bucket", st); err != nil {
		t.Fatalf("scan: %v", err)
	}

	cfg := &appconfig.Config{
		Bucket:               "bucket",
		DBPath:               dbPath,
		ConflictLog:          conflictPath,
		SpoolDir:             tmpDir,
		Workers:              2,
		MultipartThreshold:   8,
		MultipartPartSize:    4,
		MultipartConcurrency: 2,
	}

	// First run: inject enough part-upload failures to fail every attempt on
	// the large object. The small object should still succeed.
	newS3.setFailPartUploads(1000)

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
	if _, ok := newS3.object("large.bin"); ok {
		t.Fatalf("large.bin should not exist after failed run")
	}

	putsBefore := newS3.putObjectCalls("small.txt")
	if putsBefore != 1 {
		t.Fatalf("expected exactly one PUT for small.txt after run1, got %d", putsBefore)
	}

	// Second run: lift the failure and resume. Only the failed large object
	// should be retried; small.txt must not be re-uploaded.
	newS3.setFailPartUploads(0)

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

	if got := newS3.putObjectCalls("small.txt"); got != putsBefore {
		t.Fatalf("small.txt was re-uploaded on resume: before=%d after=%d", putsBefore, got)
	}
	if got := newS3.completeMultipartCalls("large.bin"); got != 1 {
		t.Fatalf("expected exactly one successful multipart completion for large.bin, got %d", got)
	}
	if newS3.copyObjectCalls() != 0 || newS3.deleteObjectCalls() != 0 || oldS3.deleteObjectCalls() != 0 {
		t.Fatalf("unexpected unsafe API usage during resume: oldDelete=%d newDelete=%d newCopy=%d",
			oldS3.deleteObjectCalls(), newS3.deleteObjectCalls(), newS3.copyObjectCalls())
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

type fakeS3 struct {
	*httptest.Server

	mu              sync.Mutex
	objects         map[string][]byte
	lastModified    map[string]time.Time
	metadata        map[string]fakeMetadata
	multipart       map[string]map[int][]byte
	uploadMeta      map[string]fakeMetadata
	nextUploadID    int
	deleteCalls     int
	copyCalls       int
	putCalls        map[string]int
	completeCalls   map[string]int
	failPartUploads int
}

type fakeMetadata struct {
	ContentType string
	Metadata    map[string]string
}

func newFakeS3(t *testing.T, objects map[string][]byte, lastModified time.Time) *fakeS3 {
	t.Helper()
	f := &fakeS3{
		objects:       map[string][]byte{},
		lastModified:  map[string]time.Time{},
		metadata:      map[string]fakeMetadata{},
		multipart:     map[string]map[int][]byte{},
		uploadMeta:    map[string]fakeMetadata{},
		putCalls:      map[string]int{},
		completeCalls: map[string]int{},
	}
	for key, value := range objects {
		f.objects[key] = append([]byte(nil), value...)
		f.lastModified[key] = lastModified
		f.metadata[key] = fakeMetadata{
			ContentType: "application/test",
			Metadata:    map[string]string{"origin": "old"},
		}
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *fakeS3) handle(w http.ResponseWriter, r *http.Request) {
	key := keyFromPath(r.URL.Path)
	q := r.URL.Query()

	if r.Header.Get("X-Amz-Copy-Source") != "" {
		f.mu.Lock()
		f.copyCalls++
		f.mu.Unlock()
	}
	if r.Method == http.MethodDelete && r.URL.Query().Get("uploadId") == "" {
		f.mu.Lock()
		f.deleteCalls++
		f.mu.Unlock()
	}

	switch {
	case r.Method == http.MethodGet && q.Get("list-type") == "2":
		f.listObjects(w)
	case r.Method == http.MethodHead && key == "":
		// HeadBucket against /<bucket>/
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodHead:
		f.headObject(w, key)
	case r.Method == http.MethodGet:
		f.getObject(w, r, key)
	case r.Method == http.MethodPut && q.Get("partNumber") != "" && q.Get("uploadId") != "":
		f.uploadPart(w, r, key, q)
	case r.Method == http.MethodPut:
		f.putObject(w, r, key)
	case r.Method == http.MethodPost && q.Has("uploads"):
		f.createMultipart(w, r, key)
	case r.Method == http.MethodPost && q.Get("uploadId") != "":
		f.completeMultipart(w, key, q.Get("uploadId"))
	case r.Method == http.MethodDelete && q.Get("uploadId") != "":
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unsupported", http.StatusBadRequest)
	}
}

func (f *fakeS3) listObjects(w http.ResponseWriter) {
	f.mu.Lock()
	defer f.mu.Unlock()

	keys := make([]string, 0, len(f.objects))
	for key := range f.objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	b.WriteString(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	b.WriteString(`<Name>bucket</Name><Prefix></Prefix><KeyCount>` + strconv.Itoa(len(keys)) + `</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>`)
	for _, key := range keys {
		value := f.objects[key]
		b.WriteString(`<Contents><Key>` + xmlEscape(key) + `</Key>`)
		b.WriteString(`<LastModified>` + f.lastModified[key].UTC().Format(time.RFC3339) + `</LastModified>`)
		b.WriteString(`<ETag>"` + md5hex(value) + `"</ETag>`)
		b.WriteString(`<Size>` + strconv.Itoa(len(value)) + `</Size><StorageClass>STANDARD</StorageClass></Contents>`)
	}
	b.WriteString(`</ListBucketResult>`)
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(b.String()))
}

func (f *fakeS3) headObject(w http.ResponseWriter, key string) {
	f.mu.Lock()
	value, ok := f.objects[key]
	lm := f.lastModified[key]
	meta := f.metadata[key]
	f.mu.Unlock()
	if !ok {
		writeNoSuchKey(w)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(value)))
	w.Header().Set("ETag", `"`+md5hex(value)+`"`)
	w.Header().Set("Last-Modified", lm.UTC().Format(http.TimeFormat))
	writeMetadataHeaders(w, meta)
	w.WriteHeader(http.StatusOK)
}

func (f *fakeS3) getObject(w http.ResponseWriter, r *http.Request, key string) {
	f.mu.Lock()
	value, ok := f.objects[key]
	meta := f.metadata[key]
	f.mu.Unlock()
	if !ok {
		writeNoSuchKey(w)
		return
	}
	start, end := int64(0), int64(len(value)-1)
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		parts := strings.Split(strings.TrimPrefix(rangeHeader, "bytes="), "-")
		parsedStart, _ := strconv.ParseInt(parts[0], 10, 64)
		parsedEnd, _ := strconv.ParseInt(parts[1], 10, 64)
		start, end = parsedStart, parsedEnd
		w.WriteHeader(http.StatusPartialContent)
	}
	if start < 0 || end >= int64(len(value)) || start > end {
		http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	chunk := value[start : end+1]
	w.Header().Set("Content-Length", strconv.Itoa(len(chunk)))
	w.Header().Set("ETag", `"`+md5hex(value)+`"`)
	writeMetadataHeaders(w, meta)
	_, _ = w.Write(chunk)
}

func (f *fakeS3) putObject(w http.ResponseWriter, r *http.Request, key string) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.objects[key] = body
	f.lastModified[key] = time.Date(2026, 5, 24, 11, 0, 0, 0, time.UTC)
	f.metadata[key] = readMetadataHeaders(r)
	f.putCalls[key]++
	f.mu.Unlock()
	w.Header().Set("ETag", `"`+md5hex(body)+`"`)
	w.WriteHeader(http.StatusOK)
}

func (f *fakeS3) createMultipart(w http.ResponseWriter, r *http.Request, key string) {
	f.mu.Lock()
	f.nextUploadID++
	uploadID := fmt.Sprintf("upload-%d-%s", f.nextUploadID, key)
	f.multipart[uploadID] = map[int][]byte{}
	f.uploadMeta[uploadID] = readMetadataHeaders(r)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/xml")
	_, _ = fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, xmlEscape(key), uploadID)
}

func (f *fakeS3) uploadPart(w http.ResponseWriter, r *http.Request, key string, q url.Values) {
	body, _ := io.ReadAll(r.Body)
	partNumber, _ := strconv.Atoi(q.Get("partNumber"))
	uploadID := q.Get("uploadId")
	f.mu.Lock()
	if f.failPartUploads > 0 {
		f.failPartUploads--
		f.mu.Unlock()
		http.Error(w, "injected failure", http.StatusInternalServerError)
		return
	}
	f.multipart[uploadID][partNumber] = body
	f.mu.Unlock()
	w.Header().Set("ETag", `"`+md5hex(body)+`"`)
	w.WriteHeader(http.StatusOK)
}

func (f *fakeS3) completeMultipart(w http.ResponseWriter, key, uploadID string) {
	f.mu.Lock()
	parts := f.multipart[uploadID]
	nums := make([]int, 0, len(parts))
	for num := range parts {
		nums = append(nums, num)
	}
	sort.Ints(nums)
	var body []byte
	for _, num := range nums {
		body = append(body, parts[num]...)
	}
	f.objects[key] = body
	f.lastModified[key] = time.Date(2026, 5, 24, 11, 0, 0, 0, time.UTC)
	f.metadata[key] = f.uploadMeta[uploadID]
	f.completeCalls[key]++
	delete(f.multipart, uploadID)
	delete(f.uploadMeta, uploadID)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/xml")
	_, _ = fmt.Fprintf(w, `<CompleteMultipartUploadResult><Bucket>bucket</Bucket><Key>%s</Key><ETag>"%s-%d"</ETag></CompleteMultipartUploadResult>`, xmlEscape(key), md5hex(body), len(nums))
}

func (f *fakeS3) delete(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	delete(f.lastModified, key)
}

func (f *fakeS3) object(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.objects[key]
	return append([]byte(nil), value...), ok
}

func (f *fakeS3) objectMetadata(key string) (fakeMetadata, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	meta, ok := f.metadata[key]
	if meta.Metadata != nil {
		meta.Metadata = copyMap(meta.Metadata)
	}
	return meta, ok
}

func (f *fakeS3) deleteObjectCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deleteCalls
}

func (f *fakeS3) copyObjectCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.copyCalls
}

func (f *fakeS3) putObjectCalls(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.putCalls[key]
}

func (f *fakeS3) completeMultipartCalls(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.completeCalls[key]
}

func (f *fakeS3) setFailPartUploads(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failPartUploads = n
}

func assertObject(t *testing.T, f *fakeS3, key string, want []byte) {
	t.Helper()
	got, ok := f.object(key)
	if !ok {
		t.Fatalf("object %s missing", key)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("object %s mismatch: got=%q want=%q", key, got, want)
	}
}

func assertMissing(t *testing.T, f *fakeS3, key string) {
	t.Helper()
	if got, ok := f.object(key); ok {
		t.Fatalf("object %s unexpectedly exists: %q", key, got)
	}
}

func assertMetadata(t *testing.T, f *fakeS3, key, contentType string, metadata map[string]string) {
	t.Helper()
	got, ok := f.objectMetadata(key)
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

func keyFromPath(rawPath string) string {
	clean := strings.TrimPrefix(rawPath, "/")
	_, key, _ := strings.Cut(clean, "/")
	unescaped, _ := url.PathUnescape(key)
	return unescaped
}

func writeNoSuchKey(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>`))
}

func md5hex(value []byte) string {
	sum := md5.Sum(value)
	return fmt.Sprintf("%x", sum)
}

func writeMetadataHeaders(w http.ResponseWriter, meta fakeMetadata) {
	if meta.ContentType != "" {
		w.Header().Set("Content-Type", meta.ContentType)
	}
	for key, value := range meta.Metadata {
		w.Header().Set("X-Amz-Meta-"+key, value)
	}
}

func readMetadataHeaders(r *http.Request) fakeMetadata {
	meta := fakeMetadata{
		ContentType: r.Header.Get("Content-Type"),
		Metadata:    map[string]string{},
	}
	for key, values := range r.Header {
		if !strings.HasPrefix(strings.ToLower(key), "x-amz-meta-") || len(values) == 0 {
			continue
		}
		metaKey := strings.TrimPrefix(strings.ToLower(key), "x-amz-meta-")
		meta.Metadata[metaKey] = values[0]
	}
	return meta
}

func copyMap(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func xmlEscape(s string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;")
	return replacer.Replace(s)
}
