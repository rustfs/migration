package s3client

import (
	"context"
	"fmt"
	"net/http"
	"os"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/rustfs/migration/internal/config"
)

// New builds an S3 client for a self-hosted, S3-compatible endpoint
// (RustFS / MinIO style). Path-style addressing is forced on.
// The HTTP idle pool is sized for the default worker fan-out.
func New(ctx context.Context, ep config.Endpoint) (*s3.Client, error) {
	return NewWithPool(ctx, ep, 0)
}

// NewWithPool is New with an explicit per-host idle-connection hint.
// idlePerHost <= 0 selects a default large enough for workers=16 and
// multipart_concurrency=4. This does not cap in-flight requests; the worker
// pool and the part semaphore do that. A larger idle pool just avoids
// reconnecting on every object when MaxIdleConnsPerHost would otherwise
// stay at the Go default of 2.
func NewWithPool(ctx context.Context, ep config.Endpoint, idlePerHost int) (*s3.Client, error) {
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
		awsconfig.WithHTTPClient(&http.Client{Transport: pooledTransport(idlePerHost)}),
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

func pooledTransport(idlePerHost int) *http.Transport {
	if idlePerHost <= 0 {
		idlePerHost = config.DefaultWorkers * config.DefaultMultipartConcurrency
	}
	if idlePerHost < 2 {
		idlePerHost = 2
	}
	if idlePerHost > 1024 {
		idlePerHost = 1024
	}
	var tr *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = base.Clone()
	} else {
		tr = &http.Transport{}
	}
	tr.MaxIdleConnsPerHost = idlePerHost
	if tr.MaxIdleConns < idlePerHost*2 {
		tr.MaxIdleConns = idlePerHost * 2
	}
	return tr
}
