//go:build real_s3

package migrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/rustfs/migration/internal/cluster"
	"github.com/rustfs/migration/internal/config"
	"github.com/rustfs/migration/internal/s3client"
)

// These tests write only to disposable loopback services supplied by the local
// audit launcher. Their buckets disappear when the launcher removes its data.
func realConditionClient(t *testing.T, ctx context.Context, env string) *s3.Client {
	t.Helper()
	endpoint := os.Getenv(env)
	if endpoint == "" {
		t.Skip("run via scripts/audit-local-rustfs.py")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Port() == "" ||
		(u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		t.Fatalf("%s must identify an isolated loopback service", env)
	}
	client, err := s3client.New(ctx, config.Endpoint{
		Endpoint: endpoint, Region: "us-east-1", AccessKey: "audit-local", SecretKey: "audit-local-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func realConditionBucket(t *testing.T, ctx context.Context, client *s3.Client) string {
	t.Helper()
	bucket := fmt.Sprintf("migration-condition-%d", time.Now().UnixNano())
	if err := cluster.EnsureBucket(ctx, client, bucket); err != nil {
		t.Fatal(err)
	}
	return bucket
}

func realConditionPut(t *testing.T, ctx context.Context, client *s3.Client, bucket, key string, body []byte) string {
	t.Helper()
	out, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))), ContentType: aws.String("application/octet-stream"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(out.ETag) == "" {
		t.Fatal("PutObject returned no ETag")
	}
	return *out.ETag
}

func realConditionAssertObject(t *testing.T, ctx context.Context, client *s3.Client, bucket, key string, want []byte) {
	t.Helper()
	out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatal(err)
	}
	defer out.Body.Close()
	got, err := io.ReadAll(io.LimitReader(out.Body, int64(len(want))+1))
	if err != nil {
		t.Fatal(err)
	}
	if out.ContentLength == nil || *out.ContentLength != int64(len(want)) || !bytes.Equal(got, want) {
		t.Fatalf("committed business data changed: returned_length=%v read=%d want=%d got_sha256=%x want_sha256=%x",
			out.ContentLength, len(got), len(want), sha256.Sum256(got), sha256.Sum256(want))
	}
	t.Logf("preserved key=%q bytes=%d sha256=%x", key, len(got), sha256.Sum256(got))
}

func realConditionHTTPStatus(err error) int {
	var status interface{ HTTPStatusCode() int }
	if errors.As(err, &status) {
		return status.HTTPStatusCode()
	}
	return 0
}

func TestRealS3PutRequiresAbsentTarget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	client := realConditionClient(t, ctx, "AUDIT_TARGET")
	bucket := realConditionBucket(t, ctx, client)
	const key = "conditional-put"
	business := []byte("business data must survive conditional migration writes")
	realConditionPut(t, ctx, client, bucket, key, business)
	migration := []byte("historical source bytes")
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(migration),
		ContentLength: aws.Int64(int64(len(migration))), IfNoneMatch: aws.String("*"),
	})
	if status := realConditionHTTPStatus(err); status != 412 {
		t.Errorf("PutObject If-None-Match:* must reject an existing key with 412; got status=%d error=%v", status, err)
	} else {
		t.Log("server rejected conditional PutObject with HTTP 412")
	}
	realConditionAssertObject(t, ctx, client, bucket, key, business)
}

func TestRealS3MultipartCompletionRequiresAbsentTarget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client := realConditionClient(t, ctx, "AUDIT_TARGET")
	bucket := realConditionBucket(t, ctx, client)
	const key = "conditional-multipart"
	upload, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key), ContentType: aws.String("application/octet-stream"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(upload.UploadId) == "" {
		t.Fatal("CreateMultipartUpload returned no upload ID")
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		_, err := client.AbortMultipartUpload(cleanup, &s3.AbortMultipartUploadInput{
			Bucket: aws.String(bucket), Key: aws.String(key), UploadId: upload.UploadId,
		})
		var api smithy.APIError
		if err != nil && !(errors.As(err, &api) && api.ErrorCode() == "NoSuchUpload") {
			t.Errorf("clean up multipart upload: %v", err)
		}
	})
	parts := make([]s3types.CompletedPart, 0, 2)
	for number := int32(1); number <= 2; number++ {
		// Both parts meet S3's 5 MiB minimum; no tiny fake-server parts.
		body := bytes.Repeat([]byte{byte('A' + number - 1)}, 5*1024*1024)
		part, err := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String(bucket), Key: aws.String(key), UploadId: upload.UploadId,
			PartNumber: aws.Int32(number), Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body))),
		})
		if err != nil {
			t.Fatal(err)
		}
		if aws.ToString(part.ETag) == "" {
			t.Fatal("UploadPart returned no ETag")
		}
		parts = append(parts, s3types.CompletedPart{PartNumber: aws.Int32(number), ETag: part.ETag})
	}
	// This write occurs after upload creation and all part uploads, immediately
	// before completion. A preliminary HEAD cannot protect this commit window.
	business := []byte("business object created while multipart migration was running")
	realConditionPut(t, ctx, client, bucket, key, business)
	_, err = client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: upload.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts}, IfNoneMatch: aws.String("*"),
	})
	if status := realConditionHTTPStatus(err); status != 412 && status != 409 {
		t.Errorf("conditional completion must reject concurrent target creation with 412/409; got status=%d error=%v", status, err)
	} else {
		t.Logf("server rejected conditional CompleteMultipartUpload with HTTP %d", status)
	}
	realConditionAssertObject(t, ctx, client, bucket, key, business)
}

func TestRealS3GetRejectsStaleIfMatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	client := realConditionClient(t, ctx, "AUDIT_SOURCE")
	bucket := realConditionBucket(t, ctx, client)
	const key = "conditional-source"
	before, after := bytes.Repeat([]byte("A"), 32), bytes.Repeat([]byte("B"), 32)
	oldETag := realConditionPut(t, ctx, client, bucket, key, before)
	newETag := realConditionPut(t, ctx, client, bucket, key, after)
	if oldETag == newETag {
		t.Fatal("source replacement failed to change the concurrency token")
	}
	for _, ranged := range []bool{false, true} {
		t.Run(fmt.Sprintf("range=%t", ranged), func(t *testing.T) {
			input := &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), IfMatch: aws.String(oldETag)}
			if ranged {
				input.Range = aws.String("bytes=0-3")
			}
			out, err := client.GetObject(ctx, input)
			if out != nil && out.Body != nil {
				out.Body.Close()
			}
			if status := realConditionHTTPStatus(err); status != 412 {
				t.Errorf("stale If-Match must reject source replacement with 412; got status=%d error=%v", status, err)
			} else {
				t.Log("server rejected stale source If-Match with HTTP 412")
			}
		})
	}
	// Positive control: the same Range is readable with the current ETag.
	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), IfMatch: aws.String(newETag), Range: aws.String("bytes=0-3"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer out.Body.Close()
	got, err := io.ReadAll(io.LimitReader(out.Body, 5))
	if err != nil || !bytes.Equal(got, after[:4]) || aws.ToString(out.ContentRange) != "bytes 0-3/32" || aws.ToInt64(out.ContentLength) != 4 {
		t.Fatalf("current ETag range control failed: bytes=%q content-range=%q length=%d error=%v", got, aws.ToString(out.ContentRange), aws.ToInt64(out.ContentLength), err)
	}
	realConditionAssertObject(t, ctx, client, bucket, key, after)
}
