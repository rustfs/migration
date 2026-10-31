package scanner

import (
	"context"
	"fmt"
	"log"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/rustfs/migration/internal/cluster"
	"github.com/rustfs/migration/internal/store"
)

const batchSize = 500

// Run lists every object in bucket on the source cluster and persists
// metadata into the store. Full ListObjectsV2 (S3 has no LastModified
// server filter); re-runs only refresh pending/failed/skipped rows.
//
// Listing stays single-threaded on purpose: each page needs the previous
// continuation token, and the rows land in one SQLite file. Object
// concurrency (workers) applies to copy and diff, not to scan.
// rate_limit applies to copy body reads only.
func Run(ctx context.Context, cli *s3.Client, bucket string, st *store.Store) (int64, error) {
	if err := st.SetScanComplete(false); err != nil {
		return 0, err
	}

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

	err := cluster.WalkObjects(ctx, cli, bucket, func(objects []types.Object) error {
		for _, obj := range objects {
			o := store.Object{
				Key:  awsv2.ToString(obj.Key),
				Size: *obj.Size,
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
				log.Printf("scan %s: %d objects (elapsed %s)", bucket, total, time.Since(start).Truncate(time.Second))
			}
		}
		return nil
	})
	if err != nil {
		return total, fmt.Errorf("scan %s: %w", bucket, err)
	}
	if err := flush(); err != nil {
		return total, err
	}
	if err := st.SetScanComplete(true); err != nil {
		return total, err
	}
	log.Printf("scan %s complete: %d objects in %s", bucket, total, time.Since(start).Truncate(time.Second))
	return total, nil
}
