package cluster

import (
	"context"
	"errors"
	"fmt"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/rustfs/migration/internal/config"
	"github.com/rustfs/migration/internal/s3client"
)

type Bucket struct {
	Name    string
	Created time.Time
}

func ListBuckets(ctx context.Context, cli *s3.Client) ([]Bucket, error) {
	p := s3.NewListBucketsPaginator(cli, &s3.ListBucketsInput{MaxBuckets: awsv2.Int32(1000)})
	var out []Bucket
	seenTokens := make(map[string]struct{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list buckets: %w", err)
		}
		if next := awsv2.ToString(page.ContinuationToken); next != "" {
			if _, exists := seenTokens[next]; exists {
				return nil, fmt.Errorf("list buckets repeated its continuation token")
			}
			seenTokens[next] = struct{}{}
		}
		for _, b := range page.Buckets {
			item := Bucket{Name: awsv2.ToString(b.Name)}
			if item.Name == "" {
				return nil, fmt.Errorf("list buckets returned an empty name")
			}
			if err := config.ValidateBucketName(item.Name); err != nil {
				return nil, fmt.Errorf("invalid listed bucket: %w", err)
			}
			if b.CreationDate != nil {
				item.Created = *b.CreationDate
			}
			out = append(out, item)
		}
	}
	return out, nil
}

func HeadBucket(ctx context.Context, cli *s3.Client, name string) error {
	if name == "" {
		return fmt.Errorf("bucket name is required")
	}
	if err := config.ValidateBucketName(name); err != nil {
		return err
	}
	_, err := cli.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: awsv2.String(name)})
	if err != nil {
		return fmt.Errorf("head bucket %q: %w", name, err)
	}
	return nil
}

// EnsureBucket creates the bucket on the target if it does not exist yet.
// An already-existing bucket is left untouched.
func EnsureBucket(ctx context.Context, cli *s3.Client, name string) error {
	if name == "" {
		return fmt.Errorf("bucket name is required")
	}
	if err := config.ValidateBucketName(name); err != nil {
		return err
	}
	_, err := cli.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: awsv2.String(name)})
	if err == nil {
		return nil
	}
	if !s3client.IsNotFound(err) {
		return fmt.Errorf("head bucket %q on target: %w", name, err)
	}
	return CreateBucket(ctx, cli, name)
}

// CreateBucket creates a bucket without a preliminary HEAD.
func CreateBucket(ctx context.Context, cli *s3.Client, name string) error {
	if err := config.ValidateBucketName(name); err != nil {
		return err
	}
	input := &s3.CreateBucketInput{Bucket: awsv2.String(name)}
	if region := cli.Options().Region; region != "" && region != "us-east-1" {
		input.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(region),
		}
	}
	_, err := cli.CreateBucket(ctx, input)
	if err != nil {
		var apiErr smithy.APIError
		if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "BucketAlreadyOwnedByYou" {
			return fmt.Errorf("create bucket %q on target (need s3:CreateBucket): %w", name, err)
		}
		// A concurrent creator in our account can legitimately win. Recheck
		// access instead of treating a globally occupied name as success.
		if err := HeadBucket(ctx, cli, name); err != nil {
			return err
		}
	}
	return nil
}
