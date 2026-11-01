package s3client

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Digest reads a specific object representation and verifies its exact length.
// ETags are opaque concurrency tokens, never treated as content digests.
func Digest(ctx context.Context, cli *s3.Client, bucket, key string, size int64, etag string) ([32]byte, error) {
	return DigestWithReader(ctx, cli, bucket, key, size, etag, nil)
}

// DigestWithReader permits a caller to apply its aggregate read-rate limiter.
func DigestWithReader(ctx context.Context, cli *s3.Client, bucket, key string, size int64, etag string, wrap func(io.Reader) io.Reader) ([32]byte, error) {
	var digest [32]byte
	if size < 0 || size == math.MaxInt64 || strings.Trim(etag, `"`) == "" {
		return digest, fmt.Errorf("cannot verify object without valid size and ETag")
	}
	out, err := cli.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), IfMatch: aws.String(etag),
	})
	if err != nil {
		return digest, err
	}
	defer out.Body.Close()
	if out.ContentLength == nil || *out.ContentLength != size {
		return digest, fmt.Errorf("digest content length mismatch: got=%v want=%d", out.ContentLength, size)
	}
	if strings.Trim(aws.ToString(out.ETag), `"`) != strings.Trim(etag, `"`) {
		return digest, fmt.Errorf("object ETag changed during digest")
	}
	var reader io.Reader = out.Body
	if wrap != nil {
		reader = wrap(reader)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(reader, size+1))
	if err != nil {
		return digest, fmt.Errorf("read digest body: %w", err)
	}
	if n != size {
		return digest, fmt.Errorf("digest body length mismatch: got=%d want=%d", n, size)
	}
	copy(digest[:], h.Sum(nil))
	return digest, nil
}
