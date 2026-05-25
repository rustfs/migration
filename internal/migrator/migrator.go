package migrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/rustfs/migration/internal/config"
	"github.com/rustfs/migration/internal/store"
)

type Migrator struct {
	Cfg      *config.Config
	OldCli   *s3.Client
	NewCli   *s3.Client
	Store    *store.Store
	conflict *conflictWriter

	processed atomic.Int64
	copied    atomic.Int64
	conflicts atomic.Int64
	skipped   atomic.Int64
	failed    atomic.Int64
}

type objectAttrs struct {
	CacheControl       *string
	ContentDisposition *string
	ContentEncoding    *string
	ContentLanguage    *string
	ContentType        *string
	Expires            *time.Time
	Metadata           map[string]string
}

func New(cfg *config.Config, oldCli, newCli *s3.Client, st *store.Store) (*Migrator, error) {
	cw, err := newConflictWriter(cfg.ConflictLog)
	if err != nil {
		return nil, err
	}
	return &Migrator{Cfg: cfg, OldCli: oldCli, NewCli: newCli, Store: st, conflict: cw}, nil
}

func (m *Migrator) Close() error { return m.conflict.Close() }

// Run launches the worker pool and feeds it pending records.
func (m *Migrator) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan store.Object, m.Cfg.Workers*2)
	var fatalMu sync.Mutex
	var firstFatal error
	recordFatal := func(err error) {
		if err == nil {
			return
		}
		fatalMu.Lock()
		first := firstFatal == nil
		if first {
			firstFatal = err
		}
		fatalMu.Unlock()
		if first {
			log.Printf("FATAL %v; cancelling remaining work", err)
			cancel()
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < m.Cfg.Workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for obj := range jobs {
				if ctx.Err() != nil {
					return
				}
				recordFatal(m.handle(ctx, obj))
			}
		}(i)
	}

	// progress logger
	stopProgress := make(chan struct{})
	go m.logProgress(stopProgress)

	feedErr := m.Store.IteratePending(func(o store.Object) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case jobs <- o:
			return nil
		}
	})
	close(jobs)
	wg.Wait()
	close(stopProgress)

	m.logFinal()
	if firstFatal != nil {
		return firstFatal
	}
	if feedErr != nil && !errors.Is(feedErr, context.Canceled) {
		return feedErr
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
			log.Printf("progress: processed=%d copied=%d conflicts=%d skipped=%d failed=%d",
				m.processed.Load(), m.copied.Load(), m.conflicts.Load(), m.skipped.Load(), m.failed.Load())
		}
	}
}

func (m *Migrator) logFinal() {
	log.Printf("done: processed=%d copied=%d conflicts=%d skipped=%d failed=%d",
		m.processed.Load(), m.copied.Load(), m.conflicts.Load(), m.skipped.Load(), m.failed.Load())
}

// handle processes a single object through HEAD/GET/PUT pipeline. Per-object
// S3 errors are recorded into the local store; persistence errors are returned
// so operators know resumability is no longer trustworthy.
func (m *Migrator) handle(ctx context.Context, obj store.Object) error {
	m.processed.Add(1)

	// 1. HeadObject on New cluster — if it already exists we MUST NOT overwrite.
	newHead, err := m.NewCli.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: awsv2.String(m.Cfg.Bucket),
		Key:    awsv2.String(obj.Key),
	})
	switch {
	case err == nil:
		// Exists on New -> conflict
		var newLM time.Time
		if newHead.LastModified != nil {
			newLM = *newHead.LastModified
		}
		if err := m.conflict.Write(obj.Key, obj.LastModified, newLM); err != nil {
			return fmt.Errorf("persist conflict log key=%s: %w", obj.Key, err)
		}
		m.conflicts.Add(1)
		if err := m.Store.MarkStatus(obj.Key, store.StatusConflict, "exists on new"); err != nil {
			return fmt.Errorf("mark conflict key=%s: %w", obj.Key, err)
		}
		return nil
	case isNotFound(err):
		// expected — proceed to copy
	default:
		return m.markFailed(obj.Key, fmt.Errorf("head new: %w", err))
	}

	// 2. Copy
	if err := m.copy(ctx, obj); err != nil {
		if isNotFound(err) {
			// Source 404 — application-level delete during migration. Skip.
			log.Printf("WARN source missing, skipping key=%s", obj.Key)
			m.skipped.Add(1)
			if err := m.Store.MarkStatus(obj.Key, store.StatusSkipped, "source 404"); err != nil {
				return fmt.Errorf("mark skipped key=%s: %w", obj.Key, err)
			}
			return nil
		}
		return m.markFailed(obj.Key, err)
	}

	m.copied.Add(1)
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

// copy performs GET(old) -> PUT(new), automatically switching to multipart
// for objects above MultipartThreshold.
func (m *Migrator) copy(ctx context.Context, obj store.Object) error {
	if obj.Size >= m.Cfg.MultipartThreshold {
		return m.copyMultipart(ctx, obj)
	}
	return m.copySingle(ctx, obj)
}

// --- small object path ---------------------------------------------------

func (m *Migrator) copySingle(ctx context.Context, obj store.Object) error {
	out, err := m.OldCli.GetObject(ctx, &s3.GetObjectInput{
		Bucket: awsv2.String(m.Cfg.Bucket),
		Key:    awsv2.String(obj.Key),
	})
	if err != nil {
		return fmt.Errorf("get old: %w", err)
	}
	defer out.Body.Close()

	contentLength := deref(out.ContentLength)
	if contentLength <= 0 {
		contentLength = obj.Size
	}
	tmp, err := os.CreateTemp(m.Cfg.SpoolDir, "rustfs-migration-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	written, err := io.Copy(tmp, out.Body)
	if err != nil {
		return fmt.Errorf("spool old body: %w", err)
	}
	if contentLength > 0 && written != contentLength {
		return fmt.Errorf("short read: got=%d want=%d", written, contentLength)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind temp file: %w", err)
	}

	putOut, err := m.NewCli.PutObject(ctx, &s3.PutObjectInput{
		Bucket:             awsv2.String(m.Cfg.Bucket),
		Key:                awsv2.String(obj.Key),
		Body:               tmp,
		ContentLength:      awsv2.Int64(written),
		CacheControl:       out.CacheControl,
		ContentDisposition: out.ContentDisposition,
		ContentEncoding:    out.ContentEncoding,
		ContentLanguage:    out.ContentLanguage,
		ContentType:        out.ContentType,
		Expires:            out.Expires,
		Metadata:           copyMetadata(out.Metadata),
	})
	if err != nil {
		return fmt.Errorf("put new: %w", err)
	}

	head, herr := m.NewCli.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: awsv2.String(m.Cfg.Bucket),
		Key:    awsv2.String(obj.Key),
	})
	if herr != nil {
		return fmt.Errorf("verify head new: %w", herr)
	}
	if deref(head.ContentLength) != obj.Size {
		return fmt.Errorf("size mismatch after put: got=%d want=%d", deref(head.ContentLength), obj.Size)
	}
	// Verify ETag when both sides report a simple (non-multipart) ETag.
	srcETag := normalizeETag(awsv2.ToString(out.ETag))
	dstETag := normalizeETag(awsv2.ToString(putOut.ETag))
	if srcETag != "" && dstETag != "" && !isMultipartETag(srcETag) && !isMultipartETag(dstETag) && srcETag != dstETag {
		return fmt.Errorf("etag mismatch: src=%s dst=%s", srcETag, dstETag)
	}
	return nil
}

// --- multipart path ------------------------------------------------------

func (m *Migrator) copyMultipart(ctx context.Context, obj store.Object) error {
	attrs, err := m.headSourceAttrs(ctx, obj.Key)
	if err != nil {
		return err
	}

	partSize := m.Cfg.MultipartPartSize
	numParts := int((obj.Size + partSize - 1) / partSize)
	if numParts > 10000 {
		// S3 caps parts at 10k — grow part size to fit.
		partSize = (obj.Size + 9999) / 10000
		numParts = int((obj.Size + partSize - 1) / partSize)
	}

	create, err := m.NewCli.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:             awsv2.String(m.Cfg.Bucket),
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
			Bucket:   awsv2.String(m.Cfg.Bucket),
			Key:      awsv2.String(obj.Key),
			UploadId: awsv2.String(uploadID),
		})
		if aerr != nil {
			log.Printf("WARN abort multipart key=%s: %v", obj.Key, aerr)
			return fmt.Errorf("%w; abort multipart: %v", reason, aerr)
		}
		return reason
	}

	parts := make([]s3types.CompletedPart, numParts)
	sem := make(chan struct{}, m.Cfg.MultipartConcurrency)
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	setFirstErr := func(err error) {
		errMu.Lock()
		defer errMu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
	}
	getFirstErr := func() error {
		errMu.Lock()
		defer errMu.Unlock()
		return firstErr
	}

	for i := 0; i < numParts; i++ {
		if ctx.Err() != nil {
			break
		}
		if getFirstErr() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()

			start := int64(idx) * partSize
			end := start + partSize - 1
			if end >= obj.Size {
				end = obj.Size - 1
			}
			rangeHdr := fmt.Sprintf("bytes=%d-%d", start, end)

			getOut, gerr := m.OldCli.GetObject(ctx, &s3.GetObjectInput{
				Bucket: awsv2.String(m.Cfg.Bucket),
				Key:    awsv2.String(obj.Key),
				Range:  awsv2.String(rangeHdr),
			})
			if gerr != nil {
				setFirstErr(fmt.Errorf("get part %d: %w", idx+1, gerr))
				return
			}
			body, rerr := io.ReadAll(getOut.Body)
			getOut.Body.Close()
			if rerr != nil {
				setFirstErr(fmt.Errorf("read part %d: %w", idx+1, rerr))
				return
			}

			up, uerr := m.NewCli.UploadPart(ctx, &s3.UploadPartInput{
				Bucket:        awsv2.String(m.Cfg.Bucket),
				Key:           awsv2.String(obj.Key),
				UploadId:      awsv2.String(uploadID),
				PartNumber:    awsv2.Int32(int32(idx + 1)),
				Body:          bytes.NewReader(body),
				ContentLength: awsv2.Int64(int64(len(body))),
			})
			if uerr != nil {
				setFirstErr(fmt.Errorf("upload part %d: %w", idx+1, uerr))
				return
			}
			parts[idx] = s3types.CompletedPart{
				ETag:       up.ETag,
				PartNumber: awsv2.Int32(int32(idx + 1)),
			}
		}(i)
	}
	wg.Wait()

	if err := getFirstErr(); err != nil {
		return abort(err)
	}
	if err := ctx.Err(); err != nil {
		return abort(err)
	}

	_, err = m.NewCli.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          awsv2.String(m.Cfg.Bucket),
		Key:             awsv2.String(obj.Key),
		UploadId:        awsv2.String(uploadID),
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		return abort(fmt.Errorf("complete multipart: %w", err))
	}

	// Size verification (ETag of multipart upload includes -N suffix, so we
	// rely on HEAD size comparison instead).
	head, herr := m.NewCli.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: awsv2.String(m.Cfg.Bucket),
		Key:    awsv2.String(obj.Key),
	})
	if herr != nil {
		return fmt.Errorf("verify head new: %w", herr)
	}
	if deref(head.ContentLength) != obj.Size {
		return fmt.Errorf("size mismatch after multipart: got=%d want=%d", deref(head.ContentLength), obj.Size)
	}
	return nil
}

// --- helpers -------------------------------------------------------------

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func (m *Migrator) headSourceAttrs(ctx context.Context, key string) (objectAttrs, error) {
	out, err := m.OldCli.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: awsv2.String(m.Cfg.Bucket),
		Key:    awsv2.String(key),
	})
	if err != nil {
		return objectAttrs{}, fmt.Errorf("head old: %w", err)
	}
	return objectAttrs{
		CacheControl:       out.CacheControl,
		ContentDisposition: out.ContentDisposition,
		ContentEncoding:    out.ContentEncoding,
		ContentLanguage:    out.ContentLanguage,
		ContentType:        out.ContentType,
		Expires:            out.Expires,
		Metadata:           copyMetadata(out.Metadata),
	}, nil
}

func copyMetadata(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func normalizeETag(s string) string { return strings.Trim(s, `"`) }
func isMultipartETag(s string) bool { return strings.Contains(s, "-") }

// isNotFound matches both NoSuchKey responses (GET) and 404 NotFound (HEAD)
// across the various typed/untyped shapes returned by aws-sdk-go-v2.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var nsk *s3types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nf *s3types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}

// --- conflict log --------------------------------------------------------

type conflictWriter struct {
	mu sync.Mutex
	f  *os.File
}

func newConflictWriter(path string) (*conflictWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open conflict log: %w", err)
	}
	return &conflictWriter{f: f}, nil
}

func (c *conflictWriter) Write(key string, oldLM, newLM time.Time) error {
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
