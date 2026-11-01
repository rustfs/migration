package s3client

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

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
			credentials.NewStaticCredentialsProvider(ep.AccessKey, ep.SecretKey, ep.SessionToken),
		),
		awsconfig.WithHTTPClient(&http.Client{Transport: idleTimeoutTransport{base: pooledTransport(idlePerHost), timeout: 90 * time.Second}}),
	)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = awsv2.String(ep.Endpoint)
		o.UsePathStyle = true
		// Avoid assuming optional checksum extensions on S3-compatible servers.
		// Migration and diff verify full SHA-256 content independently.
		o.RequestChecksumCalculation = awsv2.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = awsv2.ResponseChecksumValidationWhenRequired
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
	tr.ResponseHeaderTimeout = 30 * time.Second
	// HTTP/1 gives each upload its own connection write deadline. HTTP/2
	// stream flow-control waits cannot be bounded by a socket WriteDeadline.
	tr.ForceAttemptHTTP2 = false
	tr.Protocols = new(http.Protocols)
	tr.Protocols.SetHTTP1(true)
	dial := tr.DialContext
	if dial == nil {
		dial = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &writeTimeoutConn{Conn: conn, timeout: 90 * time.Second}, nil
	}
	if tr.MaxIdleConns < idlePerHost*2 {
		tr.MaxIdleConns = idlePerHost * 2
	}
	return tr
}
