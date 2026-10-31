package migrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/rustfs/migration/internal/config"
	"github.com/rustfs/migration/internal/ratelimit"
	"github.com/rustfs/migration/internal/s3client"
	"github.com/rustfs/migration/internal/store"
)

const workBatch = 1000

type Migrator struct {
	Cfg      *config.Config
	Bucket   string
	OldCli   *s3.Client
	NewCli   *s3.Client
	Store    *store.Store
	conflict *conflictWriter

	processed atomic.Int64
	copied    atomic.Int64
	conflicts atomic.Int64
	skipped   atomic.Int64
	failed    atomic.Int64
	bytes     atomic.Int64

	// lim is shared by every worker and every multipart part of this migrator.
	// Nil means unlimited. Buckets are copied one at a time, so this is the
	// process-wide source-read cap while a copy is running.
	lim *ratelimit.Limiter
}

func New(cfg *config.Config, oldCli, newCli *s3.Client, st *store.Store) (*Migrator, error) {
	return NewForBucket(cfg, cfg.Bucket, oldCli, newCli, st)
}

func NewForBucket(cfg *config.Config, bucket string, oldCli, newCli *s3.Client, st *store.Store) (*Migrator, error) {
	if bucket == "" {
		return nil, fmt.Errorf("migrator: bucket must not be empty")
	}
	if err := os.MkdirAll(cfg.SpoolDir, 0o755); err != nil {
		return nil, fmt.Errorf("create spool_dir %s: %w", cfg.SpoolDir, err)
	}
	cw, err := newConflictWriter(cfg.ConflictLogFor(bucket))
	if err != nil {
		return nil, err
	}
	return &Migrator{
		Cfg:      cfg,
		Bucket:   bucket,
		OldCli:   oldCli,
		NewCli:   newCli,
		Store:    st,
		conflict: cw,
		lim:      ratelimit.New(int64(cfg.RateLimit)),
	}, nil
}

func (m *Migrator) Close() error { return m.conflict.Close() }

func (m *Migrator) Run(ctx context.Context) error {
	if err := m.Cfg.ValidateConcurrency(); err != nil {
		return err
	}
	failedBefore := m.failed.Load()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	jobs := make(chan store.Object, config.JobBuffer(m.Cfg.Workers))
	var wg sync.WaitGroup
	for i := 0; i < m.Cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for obj := range jobs {
				if ctx.Err() != nil {
					return
				}
				if err := m.handle(ctx, obj); err != nil {
					cancel(err) // State could not be persisted.
					return
				}
			}
		}()
	}

	stopProgress := make(chan struct{})
	go m.logProgress(stopProgress)

	var feedErr error
	after := ""
	for {
		if ctx.Err() != nil {
			feedErr = ctx.Err()
			break
		}
		batch, err := m.Store.BatchPendingAfter(after, workBatch)
		if err != nil {
			feedErr = err
			break
		}
		if len(batch) == 0 {
			break
		}
		after = batch[len(batch)-1].Key
		for _, o := range batch {
			select {
			case <-ctx.Done():
				feedErr = ctx.Err()
			case jobs <- o:
			}
			if feedErr != nil {
				break
			}
		}
		if feedErr != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()
	close(stopProgress)

	m.logFinal()
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if feedErr != nil {
		return feedErr
	}
	if n := m.failed.Load() - failedBefore; n != 0 {
		return fmt.Errorf("%d object(s) failed in bucket %q; see the object errors above", n, m.Bucket)
	}
	return nil
}

func (m *Migrator) logProgress(stop <-chan struct{}) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			log.Printf("progress %s: processed=%d copied=%d conflicts=%d skipped=%d failed=%d bytes=%d",
				m.Bucket,
				m.processed.Load(), m.copied.Load(), m.conflicts.Load(), m.skipped.Load(), m.failed.Load(), m.bytes.Load())
		}
	}
}

func (m *Migrator) logFinal() {
	log.Printf("done %s: processed=%d copied=%d conflicts=%d skipped=%d failed=%d bytes=%d",
		m.Bucket,
		m.processed.Load(), m.copied.Load(), m.conflicts.Load(), m.skipped.Load(), m.failed.Load(), m.bytes.Load())
}

func (m *Migrator) handle(ctx context.Context, obj store.Object) error {
	m.processed.Add(1)

	input := &s3.HeadObjectInput{Bucket: awsv2.String(m.Bucket), Key: awsv2.String(obj.Key)}
	head, err := m.NewCli.HeadObject(ctx, input)
	if err == nil {
		return m.recordExisting(obj, head)
	}
	if ctx.Err() != nil || !s3client.IsNotFound(err) {
		return m.markFailed(obj.Key, fmt.Errorf("head new: %w", err))
	}

	err = m.copy(ctx, obj)
	switch {
	case err == nil:
		return m.markDone(obj)
	case errors.Is(err, errTargetRace):
		// A writer won the race after HEAD. Classify its object exactly as
		// we would on resume. If it vanished again, retry on the next run.
		head, err := m.NewCli.HeadObject(ctx, input)
		if err != nil {
			return m.markFailed(obj.Key, fmt.Errorf("head new after conditional write: %w", err))
		}
		return m.recordExisting(obj, head)
	case ctx.Err() == nil && errors.Is(err, errSourceMissing):
		log.Printf("WARN source missing, skipping key=%s", obj.Key)
		m.skipped.Add(1)
		if err := m.Store.MarkStatus(obj.Key, store.StatusSkipped, "source 404"); err != nil {
			return fmt.Errorf("mark skipped key=%s: %w", obj.Key, err)
		}
		return nil
	default:
		return m.markFailed(obj.Key, err)
	}
}

func (m *Migrator) recordExisting(obj store.Object, head *s3.HeadObjectOutput) error {
	if targetAlreadyMigrated(obj, head) {
		return m.markDone(obj)
	}
	if err := m.conflict.Write(obj.Key, obj.LastModified, awsv2.ToTime(head.LastModified)); err != nil {
		return fmt.Errorf("persist conflict log key=%s: %w", obj.Key, err)
	}
	m.conflicts.Add(1)
	if err := m.Store.MarkStatus(obj.Key, store.StatusConflict, "exists on new"); err != nil {
		return fmt.Errorf("mark conflict key=%s: %w", obj.Key, err)
	}
	return nil
}

func (m *Migrator) markDone(obj store.Object) error {
	m.copied.Add(1)
	if obj.Size > 0 {
		m.bytes.Add(obj.Size)
	}
	if err := m.Store.MarkStatus(obj.Key, store.StatusDone, ""); err != nil {
		return fmt.Errorf("mark done key=%s: %w", obj.Key, err)
	}
	return nil
}

func (m *Migrator) markFailed(key string, err error) error {
	m.failed.Add(1)
	log.Printf("ERROR key=%s: %v", key, err)
	if markErr := m.Store.MarkStatus(key, store.StatusFailed, err.Error()); markErr != nil {
		return fmt.Errorf("mark failed key=%s: %w (original error: %v)", key, markErr, err)
	}
	return nil
}

var (
	errSourceMissing = errors.New("source object missing")
	errTargetRace    = errors.New("conditional target write lost a race")
)

// Only a HEAD or GET of the source can make an inventory row skipped.
func sourceReadError(op string, err error) error {
	if s3client.IsNotFound(err) {
		return fmt.Errorf("%s: %w: %w", op, errSourceMissing, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// This SDK predates IfNoneMatch on the two S3 commit operations.
func createOnly(o *s3.Options) {
	o.APIOptions = append(o.APIOptions, smithyhttp.SetHeaderValue("If-None-Match", "*"))
}

func targetWriteError(op string, err error) error {
	var response *smithyhttp.ResponseError
	var api smithy.APIError
	if (errors.As(err, &response) && response.HTTPStatusCode() == http.StatusPreconditionFailed) ||
		(errors.As(err, &api) && api.ErrorCode() == "ConditionalRequestConflict") {
		return fmt.Errorf("%s: %w: %w", op, errTargetRace, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

func quotedETag(tag string) *string {
	if tag = normalizeETag(tag); tag != "" {
		return awsv2.String(`"` + tag + `"`)
	}
	return nil
}

func (m *Migrator) copy(ctx context.Context, obj store.Object) error {
	if obj.Size >= m.Cfg.MultipartThreshold || obj.Size > config.MaxMultipartPartSize {
		return m.copyMultipart(ctx, obj)
	}
	return m.copySingle(ctx, obj)
}

func (m *Migrator) copySingle(ctx context.Context, obj store.Object) error {
	out, err := m.OldCli.GetObject(ctx, &s3.GetObjectInput{
		Bucket:  awsv2.String(m.Bucket),
		Key:     awsv2.String(obj.Key),
		IfMatch: quotedETag(obj.ETag),
	})
	if err != nil {
		return sourceReadError("get old", err)
	}
	defer out.Body.Close()

	if obj.ETag != "" && normalizeETag(awsv2.ToString(out.ETag)) != normalizeETag(obj.ETag) {
		return fmt.Errorf("source ETag changed since scan; rescan bucket %q", m.Bucket)
	}
	tmp, err := os.CreateTemp(m.Cfg.SpoolDir, "migration-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	written, err := io.Copy(tmp, m.throttle(ctx, io.LimitReader(out.Body, obj.Size+1)))
	if err != nil {
		return fmt.Errorf("spool old body: %w", err)
	}
	if written != obj.Size {
		return fmt.Errorf("source size changed since scan: got=%d want=%d", written, obj.Size)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind temp file: %w", err)
	}

	putOut, err := m.NewCli.PutObject(ctx, &s3.PutObjectInput{
		Bucket:             awsv2.String(m.Bucket),
		Key:                awsv2.String(obj.Key),
		Body:               tmp,
		ContentLength:      awsv2.Int64(written),
		CacheControl:       out.CacheControl,
		ContentDisposition: out.ContentDisposition,
		ContentEncoding:    out.ContentEncoding,
		ContentLanguage:    out.ContentLanguage,
		ContentType:        out.ContentType,
		Expires:            out.Expires,
		Metadata:           out.Metadata,
	}, createOnly)
	if err != nil {
		return targetWriteError("put new", err)
	}

	srcETag := normalizeETag(awsv2.ToString(out.ETag))
	dstETag := normalizeETag(awsv2.ToString(putOut.ETag))
	if srcETag != "" && dstETag != "" && !isMultipartETag(srcETag) && !isMultipartETag(dstETag) && srcETag != dstETag {
		return fmt.Errorf("etag mismatch: src=%s dst=%s", srcETag, dstETag)
	}
	return m.verifyTarget(ctx, obj, putOut.ETag)
}

func (m *Migrator) copyMultipart(ctx context.Context, obj store.Object) error {
	attrs, err := m.OldCli.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket:  awsv2.String(m.Bucket),
		Key:     awsv2.String(obj.Key),
		IfMatch: quotedETag(obj.ETag),
	})
	if err != nil {
		return sourceReadError("head old", err)
	}
	if attrs.ContentLength == nil || *attrs.ContentLength != obj.Size ||
		(obj.ETag != "" && normalizeETag(awsv2.ToString(attrs.ETag)) != normalizeETag(obj.ETag)) {
		return fmt.Errorf("source changed since scan; rescan bucket %q", m.Bucket)
	}
	match := quotedETag(awsv2.ToString(attrs.ETag))
	if match == nil {
		return errors.New("source HEAD returned no ETag to pin multipart reads")
	}

	partConc := m.Cfg.MultipartConcurrency
	if partConc < 1 {
		return fmt.Errorf("multipart_concurrency must be >= 1, got %d", partConc)
	}

	partSize := m.Cfg.MultipartPartSize
	if obj.Size <= 0 || partSize <= 0 {
		return fmt.Errorf("invalid multipart object or part size: object=%d part=%d", obj.Size, partSize)
	}
	const maxParts = 10000
	partSize = max(partSize, 1+(obj.Size-1)/maxParts)
	if partSize > config.MaxMultipartPartSize {
		return fmt.Errorf("required part size %d exceeds S3 limit %d", partSize, config.MaxMultipartPartSize)
	}
	numParts := int(1 + (obj.Size-1)/partSize)

	create, err := m.NewCli.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:             awsv2.String(m.Bucket),
		Key:                awsv2.String(obj.Key),
		CacheControl:       attrs.CacheControl,
		ContentDisposition: attrs.ContentDisposition,
		ContentEncoding:    attrs.ContentEncoding,
		ContentLanguage:    attrs.ContentLanguage,
		ContentType:        attrs.ContentType,
		Expires:            attrs.Expires,
		Metadata:           attrs.Metadata,
	})
	if err != nil {
		return fmt.Errorf("create multipart: %w", err)
	}
	uploadID := awsv2.ToString(create.UploadId)

	abort := func(reason error) error {
		abortCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, aerr := m.NewCli.AbortMultipartUpload(abortCtx, &s3.AbortMultipartUploadInput{
			Bucket:   awsv2.String(m.Bucket),
			Key:      awsv2.String(obj.Key),
			UploadId: awsv2.String(uploadID),
		})
		var gone *s3types.NoSuchUpload
		if aerr != nil && !errors.As(aerr, &gone) {
			return fmt.Errorf("abort multipart after %v: %w", reason, aerr)
		}
		return reason
	}

	parts := make([]s3types.CompletedPart, numParts)
	sem := make(chan struct{}, partConc)
	var wg sync.WaitGroup
	partCtx, cancelParts := context.WithCancelCause(ctx)
	defer cancelParts(nil)

partLoop:
	for i := 0; i < numParts; i++ {
		if partCtx.Err() != nil {
			break
		}
		select {
		case <-partCtx.Done():
			// Do not block on a full semaphore after Ctrl-C, and do not
			// start parts that have not been scheduled yet.
			break partLoop
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			if partCtx.Err() != nil {
				return
			}

			start := int64(idx) * partSize
			end := start + partSize - 1
			if end >= obj.Size {
				end = obj.Size - 1
			}
			rangeHdr := fmt.Sprintf("bytes=%d-%d", start, end)

			getOut, gerr := m.OldCli.GetObject(partCtx, &s3.GetObjectInput{
				Bucket:  awsv2.String(m.Bucket),
				Key:     awsv2.String(obj.Key),
				Range:   awsv2.String(rangeHdr),
				IfMatch: match,
			})
			if gerr != nil {
				cancelParts(sourceReadError(fmt.Sprintf("get part %d", idx+1), gerr))
				return
			}
			defer getOut.Body.Close()
			wantRange := fmt.Sprintf("bytes %d-%d/%d", start, end, obj.Size)
			if awsv2.ToString(getOut.ContentRange) != wantRange ||
				normalizeETag(awsv2.ToString(getOut.ETag)) != normalizeETag(*match) {
				cancelParts(fmt.Errorf("unexpected source range or ETag for part %d (expected %s)", idx+1, wantRange))
				return
			}
			size := end - start + 1
			body, rerr := io.ReadAll(m.throttle(partCtx, io.LimitReader(getOut.Body, size+1)))
			if rerr != nil {
				cancelParts(fmt.Errorf("read part %d: %w", idx+1, rerr))
				return
			}

			if int64(len(body)) != size {
				cancelParts(fmt.Errorf("part %d length: got=%d want=%d", idx+1, len(body), size))
				return
			}

			up, uerr := m.NewCli.UploadPart(partCtx, &s3.UploadPartInput{
				Bucket:        awsv2.String(m.Bucket),
				Key:           awsv2.String(obj.Key),
				UploadId:      awsv2.String(uploadID),
				PartNumber:    awsv2.Int32(int32(idx + 1)),
				Body:          bytes.NewReader(body),
				ContentLength: awsv2.Int64(int64(len(body))),
			})
			if uerr != nil {
				cancelParts(fmt.Errorf("upload part %d: %w", idx+1, uerr))
				return
			}
			parts[idx] = s3types.CompletedPart{
				ETag:       up.ETag,
				PartNumber: awsv2.Int32(int32(idx + 1)),
			}
		}(i)
	}
	wg.Wait()

	if err := context.Cause(partCtx); err != nil {
		return abort(err)
	}

	committed, err := m.NewCli.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          awsv2.String(m.Bucket),
		Key:             awsv2.String(obj.Key),
		UploadId:        awsv2.String(uploadID),
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts},
	}, createOnly)
	if err != nil {
		return abort(targetWriteError("complete multipart", err))
	}

	return m.verifyTarget(ctx, obj, committed.ETag)
}

func (m *Migrator) verifyTarget(ctx context.Context, obj store.Object, committedETag *string) error {
	head, err := m.NewCli.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket:  awsv2.String(m.Bucket),
		Key:     awsv2.String(obj.Key),
		IfMatch: quotedETag(awsv2.ToString(committedETag)),
	})
	if err != nil {
		return fmt.Errorf("verify head new: %w", err)
	}
	if head.ContentLength == nil || *head.ContentLength != obj.Size {
		return fmt.Errorf("target size after upload: got=%d want=%d", awsv2.ToInt64(head.ContentLength), obj.Size)
	}
	if committedETag != nil && normalizeETag(*committedETag) != normalizeETag(awsv2.ToString(head.ETag)) {
		return errors.New("target ETag changed after upload")
	}
	return nil
}

// throttle charges source body bytes to the shared limiter. Metadata HEAD
// requests are not charged. A nil limiter returns the reader unchanged.
func (m *Migrator) throttle(ctx context.Context, r io.Reader) io.Reader {
	return ratelimit.Wrap(ctx, r, m.lim)
}

func normalizeETag(s string) string { return strings.Trim(s, `"`) }

// isMultipartETag reports S3 multipart ETags, which look like "<md5>-<parts>".
func isMultipartETag(s string) bool { return strings.Contains(s, "-") }

// Different multipart layouts may represent the same bytes, but size alone
// is insufficient evidence that an existing target belongs to this migration.
func targetAlreadyMigrated(obj store.Object, head *s3.HeadObjectOutput) bool {
	if head == nil || head.ContentLength == nil || *head.ContentLength != obj.Size {
		return false
	}
	srcETag := normalizeETag(obj.ETag)
	return srcETag != "" && srcETag == normalizeETag(awsv2.ToString(head.ETag))
}

type conflictWriter struct {
	mu sync.Mutex
	f  *os.File
}

func newConflictWriter(path string) (*conflictWriter, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create conflict log dir %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open conflict log: %w", err)
	}
	return &conflictWriter{f: f}, nil
}

func (c *conflictWriter) Write(key string, oldLM, newLM time.Time) error {
	if strings.HasPrefix(key, `"`) || strings.IndexFunc(key, unicode.IsControl) >= 0 {
		key = strconv.Quote(key)
	}
	line := fmt.Sprintf("%s\t%s\t%s\n",
		key,
		oldLM.UTC().Format(time.RFC3339),
		newLM.UTC().Format(time.RFC3339),
	)
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.f.WriteString(line)
	return err
}

func (c *conflictWriter) Close() error { return c.f.Close() }
