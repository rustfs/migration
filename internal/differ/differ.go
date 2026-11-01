package differ

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"sync"

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
	Kind   string // missing | size | content | metadata | extra
	Detail string
}

type Result struct {
	Bucket           string
	OK               int64
	Missing          int64
	SizeMismatch     int64
	ETagMismatch     int64 // Retained for compatibility; ETags alone do not prove different content.
	ContentMismatch  int64
	MetadataMismatch int64
	Extra            int64
	Issues           []Issue
}

func (r Result) HasGaps() bool {
	return r.Missing > 0 || r.SizeMismatch > 0 || r.ETagMismatch > 0 || r.ContentMismatch > 0 || r.MetadataMismatch > 0
}

type Options struct {
	Workers     int
	ReportExtra bool
	Writer      io.Writer
	// IssuesJSONL receives every issue, one JSON object per line. The caller owns it.
	IssuesJSONL io.Writer
}

type listed struct {
	key  string
	size int64
	etag string
}

// Run compares all current source objects with the target. Equal sizes are
// verified by reading both bodies with If-Match and comparing SHA-256; ETags
// from different encryption schemes or multipart layouts are not comparable.
// Source and target must remain quiescent for the duration of final acceptance:
// individual conditional reads cannot provide an atomic cross-cluster snapshot.
func Run(ctx context.Context, oldCli, newCli *s3.Client, bucket string, opt Options) (Result, error) {
	if opt.Workers <= 0 {
		opt.Workers = config.DefaultWorkers
	}
	if opt.Writer == nil {
		opt.Writer = io.Discard
	}
	res := Result{Bucket: bucket}
	if err := cluster.HeadBucket(ctx, oldCli, bucket); err != nil {
		return res, err
	}
	if _, err := newCli.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: awsv2.String(bucket)}); err != nil {
		return res, fmt.Errorf("head target bucket %q: %w", bucket, err)
	}

	file, err := os.CreateTemp("", "migration-diff-*.db")
	if err != nil {
		return res, err
	}
	tmp := file.Name()
	defer func() {
		_ = os.Remove(tmp)
		_ = os.Remove(tmp + "-wal")
		_ = os.Remove(tmp + "-shm")
	}()
	if err := file.Close(); err != nil {
		return res, err
	}
	db, err := sql.Open("sqlite", tmp+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return res, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `CREATE TABLE src (key TEXT PRIMARY KEY, size INTEGER NOT NULL, etag TEXT)`); err != nil {
		return res, err
	}
	insert, err := db.PrepareContext(ctx, `INSERT OR IGNORE INTO src(key, size, etag) VALUES(?,?,?)`)
	if err != nil {
		return res, err
	}
	defer insert.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan listed, config.JobBuffer(opt.Workers))
	var mu sync.Mutex
	var firstErr error
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
		cancel()
	}
	var wg sync.WaitGroup
	for i := 0; i < opt.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				if ctx.Err() != nil {
					return
				}
				head, err := newCli.HeadObject(ctx, &s3.HeadObjectInput{Bucket: awsv2.String(bucket), Key: awsv2.String(item.key)})
				if err != nil {
					if s3client.IsNotFound(err) {
						if err := record(&mu, &res, opt.IssuesJSONL, Issue{Key: item.key, Kind: "missing", Detail: "not on target"}); err != nil {
							fail(err)
							return
						}
						continue
					}
					fail(fmt.Errorf("head target %q: %w", item.key, err))
					return
				}
				if head.ContentLength == nil || *head.ContentLength != item.size {
					if err := record(&mu, &res, opt.IssuesJSONL, Issue{Key: item.key, Kind: "size", Detail: fmt.Sprintf("src=%d dst=%d", item.size, deref(head.ContentLength))}); err != nil {
						fail(err)
						return
					}
					continue
				}
				sourceHead, err := oldCli.HeadObject(ctx, &s3.HeadObjectInput{
					Bucket: awsv2.String(bucket), Key: awsv2.String(item.key), IfMatch: awsv2.String(item.etag),
				})
				if err != nil {
					fail(fmt.Errorf("head source %q: %w", item.key, err))
					return
				}
				if sourceHead.ContentLength == nil || *sourceHead.ContentLength != item.size ||
					strings.Trim(awsv2.ToString(sourceHead.ETag), `"`) != strings.Trim(item.etag, `"`) {
					fail(fmt.Errorf("source %q changed after listing", item.key))
					return
				}
				if !s3client.MetadataEqual(sourceHead, head) {
					if err := record(&mu, &res, opt.IssuesJSONL, Issue{Key: item.key, Kind: "metadata", Detail: "preserved headers or user metadata differ"}); err != nil {
						fail(err)
						return
					}
					continue
				}
				srcDigest, err := s3client.Digest(ctx, oldCli, bucket, item.key, item.size, item.etag)
				if err != nil {
					fail(fmt.Errorf("verify source %q: %w", item.key, err))
					return
				}
				dstDigest, err := s3client.Digest(ctx, newCli, bucket, item.key, item.size, awsv2.ToString(head.ETag))
				if err != nil {
					fail(fmt.Errorf("verify target %q: %w", item.key, err))
					return
				}
				if srcDigest != dstDigest {
					if err := record(&mu, &res, opt.IssuesJSONL, Issue{Key: item.key, Kind: "content", Detail: fmt.Sprintf("sha256 src=%x dst=%x", srcDigest, dstDigest)}); err != nil {
						fail(err)
						return
					}
					continue
				}
				mu.Lock()
				res.OK++
				mu.Unlock()
			}
		}()
	}

	p := s3.NewListObjectsV2Paginator(oldCli, &s3.ListObjectsV2Input{Bucket: awsv2.String(bucket), EncodingType: types.EncodingTypeUrl})
	seenTokens := make(map[string]struct{})
feed:
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			fail(fmt.Errorf("list source: %w", err))
			break
		}
		if name := awsv2.ToString(page.Name); name != "" && name != bucket {
			fail(fmt.Errorf("list source bucket mismatch: requested %q, returned %q", bucket, name))
			break
		}
		if err := validatePageToken(page, seenTokens); err != nil {
			fail(err)
			break
		}
		for _, obj := range page.Contents {
			key, err := decodeKey(awsv2.ToString(obj.Key), page.EncodingType)
			if err != nil {
				fail(err)
				break feed
			}
			if key == "" || obj.Size == nil || *obj.Size < 0 {
				fail(fmt.Errorf("source listing has empty key or invalid size: key=%q", key))
				break feed
			}
			item := listed{key: key, size: *obj.Size, etag: awsv2.ToString(obj.ETag)}
			if _, err := insert.ExecContext(ctx, item.key, item.size, item.etag); err != nil {
				fail(fmt.Errorf("record source %q: %w", key, err))
				break feed
			}
			select {
			case <-ctx.Done():
				break feed
			case jobs <- item:
			}
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return res, firstErr
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}

	if opt.ReportExtra {
		tp := s3.NewListObjectsV2Paginator(newCli, &s3.ListObjectsV2Input{Bucket: awsv2.String(bucket), EncodingType: types.EncodingTypeUrl})
		seenTokens := make(map[string]struct{})
		for tp.HasMorePages() {
			page, err := tp.NextPage(ctx)
			if err != nil {
				return res, fmt.Errorf("list target: %w", err)
			}
			if name := awsv2.ToString(page.Name); name != "" && name != bucket {
				return res, fmt.Errorf("list target bucket mismatch: requested %q, returned %q", bucket, name)
			}
			if err := validatePageToken(page, seenTokens); err != nil {
				return res, err
			}
			for _, obj := range page.Contents {
				key, err := decodeKey(awsv2.ToString(obj.Key), page.EncodingType)
				if err != nil {
					return res, err
				}
				if key == "" {
					return res, fmt.Errorf("target listing contains empty key")
				}
				var n int
				if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM src WHERE key = ?`, key).Scan(&n); err != nil {
					return res, err
				}
				if n == 0 {
					if err := record(&mu, &res, opt.IssuesJSONL, Issue{Key: key, Kind: "extra", Detail: "only on target"}); err != nil {
						return res, err
					}
				}
			}
		}
	}
	return res, printResult(opt.Writer, res)
}

func validatePageToken(page *s3.ListObjectsV2Output, seenTokens map[string]struct{}) error {
	if page.IsTruncated == nil {
		return fmt.Errorf("incomplete object listing: missing IsTruncated")
	}
	if awsv2.ToBool(page.IsTruncated) {
		next := awsv2.ToString(page.NextContinuationToken)
		if _, exists := seenTokens[next]; next == "" || exists {
			return fmt.Errorf("incomplete object listing: truncated page has missing or repeated continuation token")
		}
		seenTokens[next] = struct{}{}
	}
	return nil
}

func decodeKey(key string, encoding types.EncodingType) (string, error) {
	if encoding != types.EncodingTypeUrl {
		return key, nil
	}
	decoded, err := url.PathUnescape(key)
	if err != nil {
		return "", fmt.Errorf("invalid URL-encoded object key %q: %w", key, err)
	}
	return decoded, nil
}

func record(mu *sync.Mutex, res *Result, output io.Writer, issue Issue) error {
	mu.Lock()
	defer mu.Unlock()
	switch issue.Kind {
	case "missing":
		res.Missing++
	case "size":
		res.SizeMismatch++
	case "content":
		res.ContentMismatch++
	case "metadata":
		res.MetadataMismatch++
	case "extra":
		res.Extra++
	}
	if len(res.Issues) < maxPrinted {
		res.Issues = append(res.Issues, issue)
	}
	if output != nil {
		entry := struct {
			Bucket string `json:"bucket"`
			Key    string `json:"key"`
			Kind   string `json:"kind"`
			Detail string `json:"detail"`
		}{res.Bucket, issue.Key, issue.Kind, issue.Detail}
		if err := json.NewEncoder(output).Encode(entry); err != nil {
			return fmt.Errorf("write diff JSONL: %w", err)
		}
	}
	return nil
}

func printResult(w io.Writer, res Result) error {
	var report bytes.Buffer
	fmt.Fprintf(&report, "=== diff %s ===\n", res.Bucket)
	fmt.Fprintf(&report, "ok=%d  missing=%d  size_mismatch=%d  etag_mismatch=%d  content_mismatch=%d  metadata_mismatch=%d  extra_on_target=%d\n",
		res.OK, res.Missing, res.SizeMismatch, res.ETagMismatch, res.ContentMismatch, res.MetadataMismatch, res.Extra)
	fmt.Fprintf(&report, "一致=%d  缺失=%d  大小不一致=%d  内容SHA256不一致=%d  元数据不一致=%d  仅目标有=%d\n",
		res.OK, res.Missing, res.SizeMismatch, res.ContentMismatch, res.MetadataMismatch, res.Extra)
	for _, issue := range res.Issues {
		fmt.Fprintf(&report, "  %-8s %q  %s\n", issue.Kind, issue.Key, issue.Detail)
	}
	hidden := res.Missing + res.SizeMismatch + res.ETagMismatch + res.ContentMismatch + res.MetadataMismatch + res.Extra - int64(len(res.Issues))
	if hidden > 0 {
		fmt.Fprintf(&report, "  ... and %d more issues / 还有 %d 条未打印\n", hidden, hidden)
	}
	_, err := report.WriteTo(w)
	if err != nil {
		return fmt.Errorf("write diff report: %w", err)
	}
	return nil
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
