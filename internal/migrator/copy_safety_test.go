package migrator

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rustfs/migration/internal/fakes3"
	"github.com/rustfs/migration/internal/scanner"
	"github.com/rustfs/migration/internal/store"
)

// Scan first, then use the normal worker/SQLite path for each injected race.
func scannedMigrator(t *testing.T, src, dst string) *Migrator {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	cfg := testCfg(filepath.Join(tmp, "state.db"), filepath.Join(tmp, "conflicts.log"), tmp)
	cfg.Workers, cfg.MultipartConcurrency = 1, 1
	st, err := store.Open(cfg.DBPath, cfg.Bucket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	source := newTestClient(t, ctx, src)
	if _, err := scanner.Run(ctx, source, cfg.Bucket, st); err != nil {
		t.Fatal(err)
	}
	m, err := New(cfg, source, newTestClient(t, ctx, dst), st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func TestCreateOnlyWritePreservesConcurrentTarget(t *testing.T) {
	for _, tc := range []struct {
		name, source, winner string
		wantStatus           string
	}{
		{"single-conflict", "old", "new", store.StatusConflict},
		{"multipart-conflict", "old-abcdefgh", "new-abcdefgh", store.StatusConflict},
		{"single-identical", "old", "old", store.StatusDone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := fakes3.NewWithObjects(t, "bucket", map[string][]byte{"key": []byte(tc.source)}, time.Now())
			defer src.Close()
			dst := fakes3.New(t)
			defer dst.Close()
			var aborts atomic.Int32
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if (r.Method == http.MethodPut && !q.Has("partNumber")) || (r.Method == http.MethodPost && q.Has("uploadId")) {
					dst.Put("bucket", "key", []byte(tc.winner), time.Now(), fakes3.Metadata{ContentType: "application/test", Metadata: map[string]string{"origin": "old"}})
				}
				if r.Method == http.MethodDelete && q.Has("uploadId") {
					aborts.Add(1)
				}
				dst.Config.Handler.ServeHTTP(w, r)
			}))
			defer endpoint.Close()

			m := scannedMigrator(t, src.URL, endpoint.URL)
			if err := m.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			status, _, _, _ := objectRow(t, m.Cfg.DBPath, "key")
			if status != tc.wantStatus || dst.PutObjectCalls("key") != 0 || dst.CompleteMultipartCalls("key") != 0 {
				t.Fatalf("status=%s, puts=%d, completes=%d", status, dst.PutObjectCalls("key"), dst.CompleteMultipartCalls("key"))
			}
			assertObject(t, dst, "key", []byte(tc.winner))
			if int64(len(tc.source)) >= m.Cfg.MultipartThreshold && aborts.Load() != 1 {
				t.Fatalf("expected losing multipart upload to be aborted, got %d aborts", aborts.Load())
			}
		})
	}
}

func TestMultipartRejectsChangingOrInvalidRanges(t *testing.T) {
	for _, fault := range []string{"updated-source", "wrong-range", "short-body"} {
		t.Run(fault, func(t *testing.T) {
			src := fakes3.NewWithObjects(t, "bucket", map[string][]byte{"key": []byte("abcdefgh")}, time.Now())
			defer src.Close()
			dst := fakes3.New(t)
			defer dst.Close()
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") == "bytes=4-7" {
					switch fault {
					case "updated-source":
						if r.Header.Get("If-Match") == "" {
							t.Error("ranged source GET missing If-Match")
						}
						src.Put("bucket", "key", []byte("ABCDEFGH"), time.Now(), fakes3.Metadata{})
					case "wrong-range":
						r.Header.Set("Range", "bytes=0-3")
					case "short-body":
						w.Header().Set("Content-Range", "bytes 4-7/8")
						w.Header().Set("ETag", r.Header.Get("If-Match"))
						w.WriteHeader(http.StatusPartialContent)
						fmt.Fprint(w, "efg")
						return
					}
				}
				src.Config.Handler.ServeHTTP(w, r)
			}))
			defer endpoint.Close()

			m := scannedMigrator(t, endpoint.URL, dst.URL)
			if err := m.Run(context.Background()); err == nil {
				t.Fatal("run reported success")
			}
			status, _, _, _ := objectRow(t, m.Cfg.DBPath, "key")
			if status != store.StatusFailed {
				t.Fatalf("status=%s, want failed", status)
			}
			assertMissing(t, dst, "key")
		})
	}
}

func TestChangedSingleSourceFailsBeforeWrite(t *testing.T) {
	src := fakes3.NewWithObjects(t, "bucket", map[string][]byte{"key": []byte("old")}, time.Now())
	defer src.Close()
	dst := fakes3.New(t)
	defer dst.Close()
	m := scannedMigrator(t, src.URL, dst.URL)
	src.Put("bucket", "key", []byte("new"), time.Now(), fakes3.Metadata{})
	if err := m.Run(context.Background()); err == nil {
		t.Fatal("changed source reported success")
	}
	status, _, _, _ := objectRow(t, m.Cfg.DBPath, "key")
	if status != store.StatusFailed {
		t.Fatalf("status=%s, want failed", status)
	}
	assertMissing(t, dst, "key")
}

func TestTargetNotFoundIsRetryable(t *testing.T) {
	src := fakes3.NewWithObjects(t, "bucket", map[string][]byte{"key": []byte("ab")}, time.Now())
	defer src.Close()
	dst := fakes3.New(t)
	defer dst.Close()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, "<Error><Code>NoSuchBucket</Code></Error>")
			return
		}
		dst.Config.Handler.ServeHTTP(w, r)
	}))
	defer endpoint.Close()
	m := scannedMigrator(t, src.URL, endpoint.URL)
	if err := m.Run(context.Background()); err == nil {
		t.Fatal("failed target write reported success")
	}
	status, _, _, _ := objectRow(t, m.Cfg.DBPath, "key")
	if status != store.StatusFailed {
		t.Fatalf("status=%s, want failed", status)
	}
}

func TestGzipBytesAndEncodingSurviveUpload(t *testing.T) {
	var data bytes.Buffer
	zipper := gzip.NewWriter(&data)
	if _, err := zipper.Write([]byte("stored compressed")); err != nil {
		t.Fatal(err)
	}
	if err := zipper.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		threshold int64
	}{{"single", int64(data.Len() + 1)}, {"multipart", 8}} {
		t.Run(tc.name, func(t *testing.T) {
			src := fakes3.New(t)
			defer src.Close()
			src.Put("bucket", "key", data.Bytes(), time.Now(), fakes3.Metadata{ContentEncoding: "gzip"})
			dst := fakes3.New(t)
			defer dst.Close()
			m := scannedMigrator(t, src.URL, dst.URL)
			m.Cfg.MultipartThreshold = tc.threshold
			if err := m.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertObject(t, dst, "key", data.Bytes())
			if meta, ok := dst.ObjectMetadata("bucket", "key"); !ok || meta.ContentEncoding != "gzip" {
				t.Fatalf("gzip metadata missing: %+v", meta)
			}
		})
	}
}

func TestConflictLogKeepsOneRecordPerKey(t *testing.T) {
	file := filepath.Join(t.TempDir(), "conflicts.log")
	writer, err := newConflictWriter(file)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{"plain", "with\tseparator\nline", `"already quoted"`}
	for _, key := range keys {
		if err := writer.Write(key, time.Time{}, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(log), "\n"), "\n")
	if len(lines) != len(keys) {
		t.Fatalf("wrong number of conflict records: %q", lines)
	}
	for i, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			t.Fatalf("wrong number of columns: %q", fields)
		}
		got := fields[0]
		if strings.HasPrefix(got, `"`) {
			got, err = strconv.Unquote(got)
		}
		if err != nil || got != keys[i] {
			t.Fatalf("decoded key=%q err=%v, want %q", got, err, keys[i])
		}
	}
}
