//go:build real_s3

package migrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
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

// Only isolated loopback servers started by scripts/audit-local-rustfs.py are
// accepted. The script owns and destroys their data directories after the test.
func TestRealS3MigrationSHA256AndResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client := func(name string) *s3.Client {
		endpoint := os.Getenv(name)
		if endpoint == "" {
			t.Skip("run via scripts/audit-local-rustfs.py")
		}
		u, err := url.Parse(endpoint)
		if err != nil || u.Hostname() != "127.0.0.1" {
			t.Fatal("requires isolated loopback endpoint")
		}
		c, err := s3client.New(ctx, config.Endpoint{Endpoint: endpoint, Region: "us-east-1", AccessKey: "audit-local", SecretKey: "audit-local-secret"})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	src, dst := client("AUDIT_SOURCE"), client("AUDIT_TARGET")
	bucket := fmt.Sprintf("migration-audit-%d", time.Now().UnixNano())
	for _, c := range []*s3.Client{src, dst} {
		if err := cluster.EnsureBucket(ctx, c, bucket); err != nil {
			t.Fatal(err)
		}
	}
	objects := map[string][]byte{
		"empty": {}, "hello.txt": []byte("migration"),
		"中文/空 格+%?#.txt": []byte("unicode and escaping"),
		"folder/":        {}, "boundary.bin": bytes.Repeat([]byte{0x91}, 5*1024*1024),
		"multipart.bin": bytes.Repeat([]byte("abcdefg"), 1600000),
	}
	sourceAttrs := make(map[string]objectAttrs)
	for key, data := range objects {
		_, err := src.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data), ContentType: aws.String("application/x-audit"), CacheControl: aws.String("max-age=60"), Metadata: map[string]string{"audit": "preserve"}})
		if err != nil {
			t.Fatal(err)
		}
		baseline, err := src.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if err != nil {
			t.Fatal(err)
		}
		_, readErr := io.Copy(io.Discard, baseline.Body)
		baseline.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		sourceAttrs[key] = objectAttrs{ContentType: baseline.ContentType, CacheControl: baseline.CacheControl, Metadata: baseline.Metadata}
		t.Logf("source baseline key=%q content-type=%q cache-control=%q metadata=%v", key, aws.ToString(baseline.ContentType), aws.ToString(baseline.CacheControl), baseline.Metadata)
	}
	dir := t.TempDir()
	cfg := &config.Config{SourceFrozen: true, Bucket: bucket, DBPath: filepath.Join(dir, "state.db"), ConflictLog: filepath.Join(dir, "conflict.log"), SpoolDir: dir, Workers: 3, MultipartConcurrency: 2, MultipartThreshold: 5 * 1024 * 1024, MultipartPartSize: 5 * 1024 * 1024}
	st, err := store.Open(cfg.DBPath, bucket)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for run := 0; run < 2; run++ {
		if _, err := scanner.Run(ctx, src, bucket, st); err != nil {
			t.Fatal(err)
		}
		m, err := New(cfg, src, dst, st)
		if err != nil {
			t.Fatal(err)
		}
		err = m.Run(ctx)
		m.Close()
		if err != nil {
			t.Errorf("migration run %d: %v", run, err)
		}
		counts, err := st.Counts()
		if err != nil || counts.Done != int64(len(objects)) || counts.Failed != 0 {
			t.Errorf("run %d counts=%+v err=%v", run, counts, err)
		}
	}
	for key, want := range objects {
		out, err := dst.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if err != nil {
			t.Errorf("read target key=%q: %v", key, err)
			continue
		}
		got, err := io.ReadAll(out.Body)
		out.Body.Close()
		if err != nil || sha256.Sum256(got) != sha256.Sum256(want) {
			t.Fatalf("SHA256 mismatch key=%q err=%v", key, err)
		}
		wantAttrs := sourceAttrs[key]
		if aws.ToString(out.ContentType) != aws.ToString(wantAttrs.ContentType) || aws.ToString(out.CacheControl) != aws.ToString(wantAttrs.CacheControl) || out.Metadata["audit"] != wantAttrs.Metadata["audit"] {
			t.Errorf("metadata mismatch key=%q source=%+v target content-type=%q cache-control=%q metadata=%v", key, wantAttrs, aws.ToString(out.ContentType), aws.ToString(out.CacheControl), out.Metadata)
		}
		t.Logf("verified key=%q bytes=%d sha256=%x", key, len(got), sha256.Sum256(got))
	}
	r, err := differ.Run(ctx, src, dst, bucket, differ.Options{Workers: 3, ReportExtra: true, Writer: io.Discard})
	if err != nil || r.HasGaps() || r.OK != int64(len(objects)) || r.Extra != 0 {
		t.Fatalf("diff=%+v err=%v", r, err)
	}
}
