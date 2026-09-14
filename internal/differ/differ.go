package differ

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/rustfs/migration/internal/cluster"
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

	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("migration-diff-%s-%d.db", bucket, os.Getpid()))
	_ = os.Remove(tmp)
	defer os.Remove(tmp)
	db, err := sql.Open("sqlite", tmp+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return Result{}, err
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE src (key TEXT PRIMARY KEY, size INTEGER NOT NULL, etag TEXT)`); err != nil {
		return Result{}, err
	}

	res := Result{Bucket: bucket}
	jobs := make(chan listed, opt.Workers*2)
	var mu sync.Mutex
	var fatal atomic.Value

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
					got := int64(0)
					if head.ContentLength != nil {
						got = *head.ContentLength
					}
					dstETag := normalizeETag(awsv2.ToString(head.ETag))
					srcETag := normalizeETag(item.etag)
					switch {
					case got != item.size:
						record(&mu, &res, Issue{Key: item.key, Kind: "size", Detail: fmt.Sprintf("src=%d dst=%d", item.size, got)})
					case srcETag != "" && dstETag != "" && !isMultipart(srcETag) && !isMultipart(dstETag) && srcETag != dstETag:
						record(&mu, &res, Issue{Key: item.key, Kind: "etag", Detail: fmt.Sprintf("src=%s dst=%s", srcETag, dstETag)})
					default:
						atomic.AddInt64(&res.OK, 1)
					}
				case s3client.IsNotFound(err):
					record(&mu, &res, Issue{Key: item.key, Kind: "missing", Detail: "not on target"})
				default:
					fatal.Store(fmt.Errorf("head target %s: %w", item.key, err))
					return
				}
			}
		}()
	}

	insert, err := db.Prepare(`INSERT OR IGNORE INTO src(key, size, etag) VALUES(?,?,?)`)
	if err != nil {
		close(jobs)
		wg.Wait()
		return res, err
	}

	p := s3.NewListObjectsV2Paginator(oldCli, &s3.ListObjectsV2Input{Bucket: awsv2.String(bucket)})
	for p.HasMorePages() {
		if ctx.Err() != nil {
			break
		}
		if v := fatal.Load(); v != nil {
			break
		}
		page, err := p.NextPage(ctx)
		if err != nil {
			insert.Close()
			close(jobs)
			wg.Wait()
			return res, fmt.Errorf("list source: %w", err)
		}
		for _, obj := range page.Contents {
			item := listed{
				key:  awsv2.ToString(obj.Key),
				size: deref(obj.Size),
				etag: awsv2.ToString(obj.ETag),
			}
			if _, err := insert.Exec(item.key, item.size, item.etag); err != nil {
				insert.Close()
				close(jobs)
				wg.Wait()
				return res, err
			}
			select {
			case <-ctx.Done():
			case jobs <- item:
			}
		}
	}
	insert.Close()
	close(jobs)
	wg.Wait()
	if v := fatal.Load(); v != nil {
		return res, v.(error)
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}

	if opt.ReportExtra {
		tp := s3.NewListObjectsV2Paginator(newCli, &s3.ListObjectsV2Input{Bucket: awsv2.String(bucket)})
		for tp.HasMorePages() {
			page, err := tp.NextPage(ctx)
			if err != nil {
				// Target bucket missing: every source key is already "missing".
				if s3client.IsNotFound(err) {
					break
				}
				return res, fmt.Errorf("list target: %w", err)
			}
			for _, obj := range page.Contents {
				key := awsv2.ToString(obj.Key)
				var n int
				if err := db.QueryRow(`SELECT COUNT(*) FROM src WHERE key = ?`, key).Scan(&n); err != nil {
					return res, err
				}
				if n == 0 {
					record(&mu, &res, Issue{Key: key, Kind: "extra", Detail: "only on target"})
				}
			}
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
	fmt.Fprintf(w, "ok=%d  missing=%d  size_mismatch=%d  etag_mismatch=%d  extra_on_target=%d\n",
		res.OK, res.Missing, res.SizeMismatch, res.ETagMismatch, res.Extra)
	fmt.Fprintf(w, "一致=%d  缺失=%d  大小不一致=%d  ETag不一致=%d  仅目标有=%d\n",
		res.OK, res.Missing, res.SizeMismatch, res.ETagMismatch, res.Extra)
	for _, issue := range res.Issues {
		fmt.Fprintf(w, "  %-8s %s  %s\n", issue.Kind, issue.Key, issue.Detail)
	}
	hidden := res.Missing + res.SizeMismatch + res.ETagMismatch + res.Extra - int64(len(res.Issues))
	if hidden > 0 {
		fmt.Fprintf(w, "  ... and %d more issues / 还有 %d 条未打印\n", hidden, hidden)
	}
}

func normalizeETag(s string) string { return strings.Trim(s, `"`) }
func isMultipart(s string) bool     { return strings.Contains(s, "-") }

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
