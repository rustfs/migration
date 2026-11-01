package scanner

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/rustfs/migration/internal/cluster"
	"github.com/rustfs/migration/internal/store"
)

const batchSize = 500

// Run lists every object in bucket on the source cluster and persists
// metadata into the store. Full ListObjectsV2 (S3 has no LastModified
// server filter); changes invalidate previous completion for that key.
//
// Listing stays single-threaded on purpose: each page needs the previous
// continuation token, and the rows land in one SQLite file. Object
// concurrency (workers) applies to copy and diff, not to scan.
// rate_limit applies to copy body reads only.
func Run(ctx context.Context, cli *s3.Client, bucket string, st *store.Store) (int64, error) {
	bound, err := st.BoundBucket()
	if err != nil {
		return 0, err
	}
	if bound != bucket {
		return 0, fmt.Errorf("scan bucket %q does not match state bucket %q", bucket, bound)
	}
	if err := st.BindMigration(awsv2.ToString(cli.Options().BaseEndpoint), ""); err != nil {
		return 0, err
	}
	if err := st.BeginScan(); err != nil {
		return 0, fmt.Errorf("begin scan: %w", err)
	}
	if err := cluster.HeadBucket(ctx, cli, bucket); err != nil {
		return 0, err
	}

	p := s3.NewListObjectsV2Paginator(cli, &s3.ListObjectsV2Input{
		Bucket:       awsv2.String(bucket),
		EncodingType: s3types.EncodingTypeUrl,
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

	seenTokens := make(map[string]struct{})
	for p.HasMorePages() {
		if err := ctx.Err(); err != nil {
			_ = flush()
			return total, err
		}
		page, err := p.NextPage(ctx)
		if err != nil {
			return total, fmt.Errorf("list page: %w", err)
		}
		if page.IsTruncated == nil {
			return total, fmt.Errorf("list page omitted IsTruncated; inventory completeness is unknown")
		}
		if name := awsv2.ToString(page.Name); name != "" && name != bucket {
			return total, fmt.Errorf("list page bucket mismatch: requested %q, server returned %q", bucket, name)
		}
		if awsv2.ToBool(page.IsTruncated) {
			next := awsv2.ToString(page.NextContinuationToken)
			if _, exists := seenTokens[next]; next == "" || exists {
				return total, fmt.Errorf("list page is truncated without a new continuation token")
			}
			seenTokens[next] = struct{}{}
		}
		for _, obj := range page.Contents {
			key := awsv2.ToString(obj.Key)
			if page.EncodingType == s3types.EncodingTypeUrl {
				var err error
				key, err = url.QueryUnescape(key)
				if err != nil {
					return total, fmt.Errorf("decode listed key: %w", err)
				}
			}
			if key == "" || obj.Size == nil || *obj.Size < 0 {
				return total, fmt.Errorf("invalid listed object key=%q size=%v", key, obj.Size)
			}
			o := store.Object{
				Key:  key,
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
	if err := ctx.Err(); err != nil {
		return total, err
	}
	if err := st.CompleteScan(); err != nil {
		return total, fmt.Errorf("complete scan: %w", err)
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
