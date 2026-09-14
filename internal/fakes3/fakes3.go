// Package fakes3 is an httptest S3 stand-in used by integration tests.
// It speaks enough of the path-style protocol for aws-sdk-go-v2.
package fakes3

import (
	"crypto/md5"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type Metadata struct {
	ContentType string
	Metadata    map[string]string
}

type object struct {
	body []byte
	lm   time.Time
	meta Metadata
}

type Server struct {
	*httptest.Server

	mu              sync.Mutex
	buckets         map[string]map[string]*object
	created         map[string]time.Time
	multipart       map[string]map[int][]byte
	uploadBucket    map[string]string
	uploadKey       map[string]string
	uploadMeta      map[string]Metadata
	nextUploadID    int
	deleteCalls     int
	copyCalls       int
	putCalls        map[string]int
	completeCalls   map[string]int
	failPartUploads int
}

func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		buckets:       map[string]map[string]*object{},
		created:       map[string]time.Time{},
		multipart:     map[string]map[int][]byte{},
		uploadBucket:  map[string]string{},
		uploadKey:     map[string]string{},
		uploadMeta:    map[string]Metadata{},
		putCalls:      map[string]int{},
		completeCalls: map[string]int{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

func NewWithObjects(t *testing.T, bucket string, objects map[string][]byte, lm time.Time) *Server {
	t.Helper()
	s := New(t)
	for key, value := range objects {
		s.Put(bucket, key, value, lm, Metadata{
			ContentType: "application/test",
			Metadata:    map[string]string{"origin": "old"},
		})
	}
	return s
}

func (s *Server) Put(bucket, key string, body []byte, lm time.Time, meta Metadata) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureBucketLocked(bucket)
	if meta.Metadata == nil {
		meta.Metadata = map[string]string{}
	}
	s.buckets[bucket][key] = &object{
		body: append([]byte(nil), body...),
		lm:   lm,
		meta: meta,
	}
}

func (s *Server) DeleteKey(bucket, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.buckets[bucket]; ok {
		delete(b, key)
	}
}

func (s *Server) Object(bucket, key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[bucket]
	if !ok {
		return nil, false
	}
	obj, ok := b[key]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), obj.body...), true
}

func (s *Server) ObjectMetadata(bucket, key string) (Metadata, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[bucket]
	if !ok {
		return Metadata{}, false
	}
	obj, ok := b[key]
	if !ok {
		return Metadata{}, false
	}
	meta := obj.meta
	if meta.Metadata != nil {
		meta.Metadata = copyMap(meta.Metadata)
	}
	return meta, true
}

func (s *Server) DeleteObjectCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteCalls
}

func (s *Server) CopyObjectCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.copyCalls
}

func (s *Server) PutObjectCalls(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putCalls[key]
}

func (s *Server) CompleteMultipartCalls(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.completeCalls[key]
}

func (s *Server) SetFailPartUploads(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failPartUploads = n
}

func (s *Server) ensureBucketLocked(name string) {
	if _, ok := s.buckets[name]; !ok {
		s.buckets[name] = map[string]*object{}
		s.created[name] = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	bucket, key := parsePath(r.URL.Path)
	q := r.URL.Query()

	if r.Header.Get("X-Amz-Copy-Source") != "" {
		s.mu.Lock()
		s.copyCalls++
		s.mu.Unlock()
	}
	if r.Method == http.MethodDelete && q.Get("uploadId") == "" {
		s.mu.Lock()
		s.deleteCalls++
		s.mu.Unlock()
	}

	switch {
	case r.Method == http.MethodGet && bucket == "" && key == "":
		s.listBuckets(w)
	case r.Method == http.MethodGet && q.Get("list-type") == "2":
		s.listObjects(w, bucket)
	case r.Method == http.MethodHead && key == "":
		s.headBucket(w, bucket)
	case r.Method == http.MethodPut && key == "" && q.Get("partNumber") == "":
		s.createBucket(w, bucket)
	case r.Method == http.MethodHead:
		s.headObject(w, bucket, key)
	case r.Method == http.MethodGet:
		s.getObject(w, r, bucket, key)
	case r.Method == http.MethodPut && q.Get("partNumber") != "" && q.Get("uploadId") != "":
		s.uploadPart(w, r, key, q)
	case r.Method == http.MethodPut:
		s.putObject(w, r, bucket, key)
	case r.Method == http.MethodPost && q.Has("uploads"):
		s.createMultipart(w, r, bucket, key)
	case r.Method == http.MethodPost && q.Get("uploadId") != "":
		s.completeMultipart(w, bucket, key, q.Get("uploadId"))
	case r.Method == http.MethodDelete && q.Get("uploadId") != "":
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unsupported", http.StatusBadRequest)
	}
}

func (s *Server) listBuckets(w http.ResponseWriter) {
	s.mu.Lock()
	names := make([]string, 0, len(s.buckets))
	for name := range s.buckets {
		names = append(names, name)
	}
	sort.Strings(names)
	created := make(map[string]time.Time, len(s.created))
	for k, v := range s.created {
		created[k] = v
	}
	s.mu.Unlock()

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	b.WriteString(`<ListAllMyBucketsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	b.WriteString(`<Owner><ID>fake</ID><DisplayName>fake</DisplayName></Owner><Buckets>`)
	for _, name := range names {
		cd := created[name].UTC().Format(time.RFC3339)
		b.WriteString(`<Bucket><Name>` + xmlEscape(name) + `</Name><CreationDate>` + cd + `</CreationDate></Bucket>`)
	}
	b.WriteString(`</Buckets></ListAllMyBucketsResult>`)
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(b.String()))
}

func (s *Server) headBucket(w http.ResponseWriter, bucket string) {
	s.mu.Lock()
	_, ok := s.buckets[bucket]
	s.mu.Unlock()
	if !ok {
		writeNotFound(w, "NotFound", "The specified bucket does not exist")
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) createBucket(w http.ResponseWriter, bucket string) {
	s.mu.Lock()
	s.ensureBucketLocked(bucket)
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (s *Server) listObjects(w http.ResponseWriter, bucket string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bkt, ok := s.buckets[bucket]
	if !ok {
		writeNotFound(w, "NoSuchBucket", "The specified bucket does not exist")
		return
	}
	keys := make([]string, 0, len(bkt))
	for key := range bkt {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	b.WriteString(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	b.WriteString(`<Name>` + xmlEscape(bucket) + `</Name><Prefix></Prefix><KeyCount>` + strconv.Itoa(len(keys)) + `</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>`)
	for _, key := range keys {
		obj := bkt[key]
		b.WriteString(`<Contents><Key>` + xmlEscape(key) + `</Key>`)
		b.WriteString(`<LastModified>` + obj.lm.UTC().Format(time.RFC3339) + `</LastModified>`)
		b.WriteString(`<ETag>"` + md5hex(obj.body) + `"</ETag>`)
		b.WriteString(`<Size>` + strconv.Itoa(len(obj.body)) + `</Size><StorageClass>STANDARD</StorageClass></Contents>`)
	}
	b.WriteString(`</ListBucketResult>`)
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(b.String()))
}

func (s *Server) headObject(w http.ResponseWriter, bucket, key string) {
	s.mu.Lock()
	bkt, ok := s.buckets[bucket]
	var obj *object
	if ok {
		obj, ok = bkt[key]
	}
	s.mu.Unlock()
	if !ok || obj == nil {
		writeNoSuchKey(w)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(obj.body)))
	w.Header().Set("ETag", `"`+md5hex(obj.body)+`"`)
	w.Header().Set("Last-Modified", obj.lm.UTC().Format(http.TimeFormat))
	writeMetadataHeaders(w, obj.meta)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	s.mu.Lock()
	bkt, ok := s.buckets[bucket]
	var obj *object
	if ok {
		obj, ok = bkt[key]
	}
	s.mu.Unlock()
	if !ok || obj == nil {
		writeNoSuchKey(w)
		return
	}
	value := obj.body
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
	writeMetadataHeaders(w, obj.meta)
	_, _ = w.Write(chunk)
}

func (s *Server) putObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.ensureBucketLocked(bucket)
	s.buckets[bucket][key] = &object{
		body: body,
		lm:   time.Date(2026, 5, 24, 11, 0, 0, 0, time.UTC),
		meta: readMetadataHeaders(r),
	}
	s.putCalls[key]++
	s.mu.Unlock()
	w.Header().Set("ETag", `"`+md5hex(body)+`"`)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) createMultipart(w http.ResponseWriter, r *http.Request, bucket, key string) {
	s.mu.Lock()
	s.nextUploadID++
	uploadID := fmt.Sprintf("upload-%d-%s", s.nextUploadID, key)
	s.multipart[uploadID] = map[int][]byte{}
	s.uploadMeta[uploadID] = readMetadataHeaders(r)
	s.uploadBucket[uploadID] = bucket
	s.uploadKey[uploadID] = key
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/xml")
	_, _ = fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, xmlEscape(bucket), xmlEscape(key), uploadID)
}

func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request, key string, q url.Values) {
	body, _ := io.ReadAll(r.Body)
	partNumber, _ := strconv.Atoi(q.Get("partNumber"))
	uploadID := q.Get("uploadId")
	s.mu.Lock()
	if s.failPartUploads > 0 {
		s.failPartUploads--
		s.mu.Unlock()
		http.Error(w, "injected failure", http.StatusInternalServerError)
		return
	}
	if _, ok := s.multipart[uploadID]; !ok {
		s.mu.Unlock()
		http.Error(w, "no such upload", http.StatusNotFound)
		return
	}
	s.multipart[uploadID][partNumber] = body
	s.mu.Unlock()
	w.Header().Set("ETag", `"`+md5hex(body)+`"`)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) completeMultipart(w http.ResponseWriter, bucket, key, uploadID string) {
	s.mu.Lock()
	parts := s.multipart[uploadID]
	nums := make([]int, 0, len(parts))
	for num := range parts {
		nums = append(nums, num)
	}
	sort.Ints(nums)
	var body []byte
	for _, num := range nums {
		body = append(body, parts[num]...)
	}
	s.ensureBucketLocked(bucket)
	s.buckets[bucket][key] = &object{
		body: body,
		lm:   time.Date(2026, 5, 24, 11, 0, 0, 0, time.UTC),
		meta: s.uploadMeta[uploadID],
	}
	s.completeCalls[key]++
	delete(s.multipart, uploadID)
	delete(s.uploadMeta, uploadID)
	delete(s.uploadBucket, uploadID)
	delete(s.uploadKey, uploadID)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/xml")
	_, _ = fmt.Fprintf(w, `<CompleteMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><ETag>"%s-%d"</ETag></CompleteMultipartUploadResult>`, xmlEscape(bucket), xmlEscape(key), md5hex(body), len(nums))
}

func parsePath(rawPath string) (bucket, key string) {
	clean := strings.Trim(rawPath, "/")
	if clean == "" {
		return "", ""
	}
	bucket, rest, found := strings.Cut(clean, "/")
	if !found {
		return bucket, ""
	}
	key, _ = url.PathUnescape(rest)
	return bucket, key
}

func writeNoSuchKey(w http.ResponseWriter) {
	writeNotFound(w, "NoSuchKey", "The specified key does not exist.")
}

func writeNotFound(w http.ResponseWriter, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusNotFound)
	_, _ = fmt.Fprintf(w, `<Error><Code>%s</Code><Message>%s</Message></Error>`, code, xmlEscape(msg))
}

func md5hex(value []byte) string {
	sum := md5.Sum(value)
	return fmt.Sprintf("%x", sum)
}

func writeMetadataHeaders(w http.ResponseWriter, meta Metadata) {
	if meta.ContentType != "" {
		w.Header().Set("Content-Type", meta.ContentType)
	}
	for key, value := range meta.Metadata {
		w.Header().Set("X-Amz-Meta-"+key, value)
	}
}

func readMetadataHeaders(r *http.Request) Metadata {
	meta := Metadata{
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
