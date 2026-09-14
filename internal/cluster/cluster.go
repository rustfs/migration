package cluster

import (
	"context"
	"fmt"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/rustfs/migration/internal/s3client"
)

type Bucket struct {
	Name    string
	Created time.Time
}

func ListBuckets(ctx context.Context, cli *s3.Client) ([]Bucket, error) {
	page, err := cli.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return nil, fmt.Errorf("list buckets: %w", err)
	}
	out := make([]Bucket, 0, len(page.Buckets))
	for _, b := range page.Buckets {
		item := Bucket{Name: awsv2.ToString(b.Name)}
		if b.CreationDate != nil {
			item.Created = *b.CreationDate
		}
		out = append(out, item)
	}
	return out, nil
}

func HeadBucket(ctx context.Context, cli *s3.Client, name string) error {
	_, err := cli.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: awsv2.String(name)})
	if err != nil {
		return fmt.Errorf("head bucket %q: %w", name, err)
	}
	return nil
}

// EnsureBucket creates the bucket on the target if it does not exist yet.
// An already-existing bucket is left untouched.
func EnsureBucket(ctx context.Context, cli *s3.Client, name string) error {
	_, err := cli.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: awsv2.String(name)})
	if err == nil {
		return nil
	}
	if !s3client.IsNotFound(err) {
		return fmt.Errorf("head bucket %q on target: %w", name, err)
	}
	_, err = cli.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: awsv2.String(name)})
	if err != nil && !s3client.IsAlreadyExists(err) {
		return fmt.Errorf("create bucket %q on target (need s3:CreateBucket): %w", name, err)
	}
	return nil
}
