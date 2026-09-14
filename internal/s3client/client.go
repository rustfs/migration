package s3client

import (
	"context"
	"fmt"
	"os"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/rustfs/migration/internal/config"
)

// New builds an S3 client for a self-hosted, S3-compatible endpoint
// (RustFS / MinIO style). Path-style addressing is forced on.
func New(ctx context.Context, ep config.Endpoint) (*s3.Client, error) {
	// Self-hosted clusters have no EC2 IMDS; disable it so LoadDefaultConfig
	// does not hang for several seconds on a missing metadata service.
	if os.Getenv("AWS_EC2_METADATA_DISABLED") == "" {
		_ = os.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(ep.Region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(ep.AccessKey, ep.SecretKey, ""),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = awsv2.String(ep.Endpoint)
		o.UsePathStyle = true
	})
	return client, nil
}
