package scanner

import (
	"context"
	"fmt"
	"log"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/rustfs/migration/internal/store"
)

const batchSize = 500

// Run lists every object in bucket on the Old cluster and persists metadata
// into the store. Uses ListObjectsV2 paginator so memory stays bounded.
func Run(ctx context.Context, cli *s3.Client, bucket string, st *store.Store) error {
	// Fail loud and early if the bucket name is mis-routed (typo, wrong
	// endpoint, missing permission, etc.) instead of silently iterating
	// whatever the server happens to return.
	if _, err := cli.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: awsv2.String(bucket)}); err != nil {
		return fmt.Errorf("head bucket %q: %w", bucket, err)
	}

	p := s3.NewListObjectsV2Paginator(cli, &s3.ListObjectsV2Input{
		Bucket: awsv2.String(bucket),
	})

	var total int64
	start := time.Now()
	buf := make([]store.Object, 0, batchSize)

	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		if err := st.BatchUpsertScanned(buf); err != nil {
			return fmt.Errorf("persist batch: %w", err)
		}
		total += int64(len(buf))
		buf = buf[:0]
		return nil
	}

	for p.HasMorePages() {
		if err := ctx.Err(); err != nil {
			_ = flush()
			return err
		}
		page, err := p.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list page: %w", err)
		}
		// Defensive: refuse to record keys from a different bucket if the
		// server (mis)routes our request. RustFS / MinIO normally echo the
		// requested bucket name in <Name>.
		if name := awsv2.ToString(page.Name); name != "" && name != bucket {
			return fmt.Errorf("list page bucket mismatch: requested %q, server returned %q", bucket, name)
		}
		for _, obj := range page.Contents {
			o := store.Object{
				Key:  awsv2.ToString(obj.Key),
				Size: deref(obj.Size),
				ETag: awsv2.ToString(obj.ETag),
			}
			if obj.LastModified != nil {
				o.LastModified = *obj.LastModified
			}
			buf = append(buf, o)
			if len(buf) >= batchSize {
				if err := flush(); err != nil {
					return err
				}
				log.Printf("scan: %d objects recorded (elapsed %s)", total, time.Since(start).Truncate(time.Second))
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	log.Printf("scan complete: %d objects in %s", total, time.Since(start).Truncate(time.Second))
	return nil
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
