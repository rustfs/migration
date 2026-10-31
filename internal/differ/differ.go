package differ

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"unicode"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/rustfs/migration/internal/cluster"
	"github.com/rustfs/migration/internal/config"
	"github.com/rustfs/migration/internal/s3client"
	_ "modernc.org/sqlite"
)

const maxPrinted = 50

type Issue struct {
	Key    string
	Kind   string // missing | size | etag | extra
	Detail string
}

type Result struct {
	Bucket       string
	OK           int64
	SizeOnly     int64 // subset of OK for which ETags could not confirm a match
	Missing      int64
	SizeMismatch int64
	ETagMismatch int64
	Extra        int64
	Issues       []Issue
}

func (r Result) HasGaps() bool {
	return r.Missing > 0 || r.SizeMismatch > 0 || r.ETagMismatch > 0
}

type Options struct {
	Workers     int
	ReportExtra bool
	Writer      io.Writer
}

type listed struct {
	key  string
	size int64
	etag string
}

// Run lists the source bucket and HEADs each key on the target.
// Extra-on-target keys are reported when ReportExtra is true.
func Run(ctx context.Context, oldCli, newCli *s3.Client, bucket string, opt Options) (Result, error) {
	if opt.Workers <= 0 {
		opt.Workers = 16
	}
	if opt.Writer == nil {
		opt.Writer = io.Discard
	}
	if err := cluster.HeadBucket(ctx, oldCli, bucket); err != nil {
		return Result{}, err
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	// Only --extra needs a source index. SQLite gives each empty filename
	// its own disk-backed temporary database and removes it on close.
	var index *sql.Tx
	var insert *sql.Stmt
	if opt.ReportExtra {
		db, err := sql.Open("sqlite", "")
		if err != nil {
			return Result{}, err
		}
		defer db.Close()
		index, err = db.BeginTx(ctx, nil)
		if err != nil {
			return Result{}, err
		}
		defer index.Rollback()
		if _, err := index.Exec(`CREATE TABLE src (key TEXT PRIMARY KEY) WITHOUT ROWID`); err != nil {
			return Result{}, err
		}
		insert, err = index.Prepare(`INSERT OR IGNORE INTO src(key) VALUES(?)`)
		if err != nil {
			return Result{}, err
		}
		defer insert.Close()
	}

	res := Result{Bucket: bucket}
	jobs := make(chan listed, config.JobBuffer(opt.Workers))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < opt.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				if ctx.Err() != nil {
					return
				}
				head, err := newCli.HeadObject(ctx, &s3.HeadObjectInput{
					Bucket: awsv2.String(bucket),
					Key:    awsv2.String(item.key),
				})
				switch {
				case err == nil:
					if head.ContentLength == nil {
						cancel(fmt.Errorf("head target %q: missing Content-Length", item.key))
						return
					}
					got := *head.ContentLength
					dstETag := normalizeETag(awsv2.ToString(head.ETag))
					srcETag := normalizeETag(item.etag)
					switch {
					case got != item.size:
						record(&mu, &res, Issue{Key: item.key, Kind: "size", Detail: fmt.Sprintf("src=%d dst=%d", item.size, got)})
					case srcETag != "" && dstETag != "" && !isMultipart(srcETag) && !isMultipart(dstETag) && srcETag != dstETag:
						record(&mu, &res, Issue{Key: item.key, Kind: "etag", Detail: fmt.Sprintf("src=%s dst=%s", srcETag, dstETag)})
					default:
						mu.Lock()
						res.OK++
						if srcETag == "" || dstETag == "" || srcETag != dstETag {
							res.SizeOnly++
						}
						mu.Unlock()
					}
				case s3client.IsNotFound(err):
					record(&mu, &res, Issue{Key: item.key, Kind: "missing", Detail: "not on target"})
				default:
					cancel(fmt.Errorf("head target %q: %w", item.key, err))
					return
				}
			}
		}()
	}

	err := cluster.WalkObjects(ctx, oldCli, bucket, func(objects []types.Object) error {
		for _, obj := range objects {
			item := listed{key: *obj.Key, size: *obj.Size, etag: awsv2.ToString(obj.ETag)}
			if insert != nil {
				if _, err := insert.ExecContext(ctx, item.key); err != nil {
					return err
				}
			}
			select {
			case <-ctx.Done():
				return context.Cause(ctx)
			case jobs <- item:
			}
		}
		return nil
	})
	if err != nil {
		cancel(fmt.Errorf("list source: %w", err))
	}
	close(jobs)
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		return res, err
	}

	// No object HEAD ran for an empty source. Check that the bucket itself
	// was migrated, including in --extra mode.
	if res.OK == 0 && !res.HasGaps() {
		if err := cluster.HeadBucket(ctx, newCli, bucket); err != nil {
			return res, fmt.Errorf("check empty bucket on target: %w", err)
		}
	}
	if opt.ReportExtra {
		err := cluster.WalkObjects(ctx, newCli, bucket, func(objects []types.Object) error {
			for _, obj := range objects {
				var exists bool
				if err := index.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM src WHERE key = ?)`, *obj.Key).Scan(&exists); err != nil {
					return err
				}
				if !exists {
					record(&mu, &res, Issue{Key: *obj.Key, Kind: "extra", Detail: "only on target"})
				}
			}
			return nil
		})
		// A missing target bucket has no extra keys.
		if err != nil && !s3client.IsNotFound(err) {
			return res, fmt.Errorf("list target: %w", err)
		}
	}

	printResult(opt.Writer, res)
	return res, nil
}

func record(mu *sync.Mutex, res *Result, issue Issue) {
	mu.Lock()
	defer mu.Unlock()
	switch issue.Kind {
	case "missing":
		res.Missing++
	case "size":
		res.SizeMismatch++
	case "etag":
		res.ETagMismatch++
	case "extra":
		res.Extra++
	}
	if len(res.Issues) < maxPrinted {
		res.Issues = append(res.Issues, issue)
	}
}

func printResult(w io.Writer, res Result) {
	fmt.Fprintf(w, "=== diff %s ===\n", res.Bucket)
	fmt.Fprintf(w, "ok=%d  missing=%d  size_mismatch=%d  etag_mismatch=%d  extra_on_target=%d  size_only=%d\n",
		res.OK, res.Missing, res.SizeMismatch, res.ETagMismatch, res.Extra, res.SizeOnly)
	fmt.Fprintf(w, "一致=%d  缺失=%d  大小不一致=%d  ETag不一致=%d  仅目标有=%d  仅比大小=%d\n",
		res.OK, res.Missing, res.SizeMismatch, res.ETagMismatch, res.Extra, res.SizeOnly)
	for _, issue := range res.Issues {
		key := issue.Key
		if strings.IndexFunc(key, unicode.IsControl) >= 0 {
			key = strconv.Quote(key)
		}
		fmt.Fprintf(w, "  %-8s %s  %s\n", issue.Kind, key, issue.Detail)
	}
	hidden := res.Missing + res.SizeMismatch + res.ETagMismatch + res.Extra - int64(len(res.Issues))
	if hidden > 0 {
		fmt.Fprintf(w, "  ... and %d more issues / 还有 %d 条未打印\n", hidden, hidden)
	}
}

func normalizeETag(s string) string { return strings.Trim(s, `"`) }
func isMultipart(s string) bool     { return strings.Contains(s, "-") }
