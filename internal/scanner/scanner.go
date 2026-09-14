package scanner

import (
	"context"
	"fmt"
	"log"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/rustfs/migration/internal/cluster"
	"github.com/rustfs/migration/internal/store"
)

const batchSize = 500

// Run lists every object in bucket on the source cluster and persists
// metadata into the store. Full ListObjectsV2 (S3 has no LastModified
// server filter); re-runs only refresh pending/failed/skipped rows.
func Run(ctx context.Context, cli *s3.Client, bucket string, st *store.Store) (int64, error) {
	if err := cluster.HeadBucket(ctx, cli, bucket); err != nil {
		return 0, err
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
			return total, err
		}
		page, err := p.NextPage(ctx)
		if err != nil {
			return total, fmt.Errorf("list page: %w", err)
		}
		if name := awsv2.ToString(page.Name); name != "" && name != bucket {
			return total, fmt.Errorf("list page bucket mismatch: requested %q, server returned %q", bucket, name)
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
					return total, err
				}
				log.Printf("scan %s: %d objects (elapsed %s)", bucket, total, time.Since(start).Truncate(time.Second))
			}
		}
	}
	if err := flush(); err != nil {
		return total, err
	}
	log.Printf("scan %s complete: %d objects in %s", bucket, total, time.Since(start).Truncate(time.Second))
	return total, nil
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
