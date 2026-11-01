package migrator

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/rustfs/migration/internal/config"
	"github.com/rustfs/migration/internal/ratelimit"
	"github.com/rustfs/migration/internal/safeoutput"
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
	lim         *ratelimit.Limiter
	spoolBudget *spoolBudget
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
	return NewForBucket(cfg, cfg.Bucket, oldCli, newCli, st)
}

func NewForBucket(cfg *config.Config, bucket string, oldCli, newCli *s3.Client, st *store.Store) (*Migrator, error) {
	if bucket == "" {
		return nil, fmt.Errorf("migrator: bucket must not be empty")
	}
	boundBucket, err := st.BoundBucket()
	if err != nil {
		return nil, err
	}
	if boundBucket != bucket {
		return nil, fmt.Errorf("state bucket %q does not match migration bucket %q", boundBucket, bucket)
	}
	if err := st.BindMigration(awsv2.ToString(oldCli.Options().BaseEndpoint), awsv2.ToString(newCli.Options().BaseEndpoint)); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.SpoolDir, 0o755); err != nil {
		return nil, fmt.Errorf("create spool_dir %s: %w", cfg.SpoolDir, err)
	}
	cw, err := newConflictWriter(cfg.ConflictLogFor(bucket))
	if err != nil {
		return nil, err
	}
	return &Migrator{
		Cfg:         cfg,
		Bucket:      bucket,
		OldCli:      oldCli,
		NewCli:      newCli,
		Store:       st,
		conflict:    cw,
		lim:         ratelimit.New(int64(cfg.RateLimit)),
		spoolBudget: newSpoolBudget(cfg.SpoolMaxBytes),
	}, nil
}

func (m *Migrator) Close() error { return m.conflict.Close() }

func (m *Migrator) Run(ctx context.Context) error {
	if err := m.Cfg.ValidateConcurrency(); err != nil {
		return err
	}
	if !m.Cfg.SourceFrozen {
		return fmt.Errorf("source_frozen must be true: freeze source writes and deletes before migration")
	}
	if err := m.Store.RequireCompleteScan(); err != nil {
		return err
	}
	failedBefore := m.failed.Load()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan store.Object, config.JobBuffer(m.Cfg.Workers))
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
			log.Printf("FATAL %s; cancelling remaining work", safeoutput.Escape(err.Error()))
			cancel()
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < m.Cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for obj := range jobs {
				if ctx.Err() != nil {
					return
				}
				recordFatal(m.handle(ctx, obj))
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
	if firstFatal != nil {
		return firstFatal
	}
	if feedErr != nil {
		return feedErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if failed := m.failed.Load() - failedBefore; failed > 0 {
		return fmt.Errorf("migration failed for %d objects; inspect status and retry", failed)
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

// sourceReadError preserves which side failed. Destination 404s must never
// be interpreted as a deleted source object.
type sourceReadError struct{ err error }

func (e *sourceReadError) Error() string { return "source: " + e.err.Error() }
func (e *sourceReadError) Unwrap() error { return e.err }

// Failure to durably record local outcomes is fatal to the whole worker pool.
type persistenceError struct{ err error }

func (e *persistenceError) Error() string { return e.err.Error() }
func (e *persistenceError) Unwrap() error { return e.err }

type sourceSnapshot struct {
	attrs     objectAttrs
	etag      string
	versionID *string
	size      int64
}

func isMissingSource(err error) bool {
	var source *sourceReadError
	if !errors.As(err, &source) || s3client.IsCanceled(err) {
		return false
	}
	var api smithy.APIError
	return errors.As(source.err, &api) && api.ErrorCode() == "NoSuchKey"
}

func (m *Migrator) handle(ctx context.Context, obj store.Object) error {
	m.processed.Add(1)
	err := m.migrateObject(ctx, obj)
	if err == nil {
		return nil
	}
	var persist *persistenceError
	if errors.As(err, &persist) {
		return err
	}
	if ctx.Err() == nil && isMissingSource(err) {
		m.skipped.Add(1)
		if e := m.Store.MarkStatus(obj.Key, store.StatusSkipped, "source NoSuchKey"); e != nil {
			return fmt.Errorf("mark skipped key=%q: %w", obj.Key, e)
		}
		return nil
	}
	return m.markFailed(obj.Key, err)
}

func (m *Migrator) migrateObject(ctx context.Context, obj store.Object) error {
	head, err := m.NewCli.HeadObject(ctx, &s3.HeadObjectInput{Bucket: awsv2.String(m.Bucket), Key: awsv2.String(obj.Key)})
	if err != nil && (ctx.Err() != nil || !s3client.IsNotFound(err)) {
		return fmt.Errorf("head target: %w", err)
	}
	snapshot, err := m.sourceSnapshot(ctx, obj)
	if err != nil {
		return err
	}
	if head != nil {
		return m.reconcileExistingTarget(ctx, obj, snapshot, head)
	}
	if obj.Size >= m.Cfg.MultipartThreshold && obj.Size > 0 {
		err = m.copyMultipart(ctx, obj, snapshot)
	} else {
		err = m.copySingle(ctx, obj, snapshot)
	}
	if err != nil {
		var api smithy.APIError
		if errors.As(err, &api) && api.ErrorCode() == "PreconditionFailed" {
			// A concurrent writer won the atomic create. Only classify a
			// destination conditional write, never a source If-Match failure.
			var source *sourceReadError
			if !errors.As(err, &source) {
				target, herr := m.NewCli.HeadObject(ctx, &s3.HeadObjectInput{Bucket: awsv2.String(m.Bucket), Key: awsv2.String(obj.Key)})
				if herr == nil {
					if checkErr := m.checkSourceBeforeCommit(ctx, obj, snapshot); checkErr != nil {
						return checkErr
					}
					// A successful commit may have lost its response. The SDK
					// retries If-None-Match and receives 412 for our own bytes.
					return m.reconcileExistingTarget(ctx, obj, snapshot, target)
				}
			}
		}
		return err
	}
	return m.markDone(obj)
}

// reconcileExistingTarget distinguishes an independent business write from
// our own committed upload whose successful response was lost in transit.
func (m *Migrator) reconcileExistingTarget(ctx context.Context, obj store.Object, snapshot sourceSnapshot, head *s3.HeadObjectOutput) error {
	// ETags only pin a representation; all equality decisions use SHA256.
	if head != nil && head.ContentLength != nil && *head.ContentLength == obj.Size && attributesEqual(snapshot.attrs, head) {
		sourceDigest, err := m.sourceDigest(ctx, obj.Key, snapshot)
		if err != nil {
			return err
		}
		targetDigest, err := s3client.Digest(ctx, m.NewCli, m.Bucket, obj.Key, obj.Size, awsv2.ToString(head.ETag))
		if err != nil {
			return fmt.Errorf("verify existing target: %w", err)
		}
		if sourceDigest == targetDigest {
			if err := m.checkSourceBeforeCommit(ctx, obj, snapshot); err != nil {
				return err
			}
			return m.markDone(obj)
		}
	}
	return m.markConflict(obj, head)
}

func (m *Migrator) markConflict(obj store.Object, head *s3.HeadObjectOutput) error {
	var modified time.Time
	if head != nil && head.LastModified != nil {
		modified = *head.LastModified
	}
	if err := m.conflict.Write(obj.Key, obj.LastModified, modified); err != nil {
		return &persistenceError{fmt.Errorf("persist conflict key=%q: %w", obj.Key, err)}
	}
	if err := m.Store.MarkStatus(obj.Key, store.StatusConflict, "exists on new"); err != nil {
		return &persistenceError{fmt.Errorf("mark conflict key=%q: %w", obj.Key, err)}
	}
	m.conflicts.Add(1)
	return nil
}

func (m *Migrator) markDone(obj store.Object) error {
	if err := m.Store.MarkStatus(obj.Key, store.StatusDone, ""); err != nil {
		return &persistenceError{fmt.Errorf("mark done key=%q: %w", obj.Key, err)}
	}
	m.copied.Add(1)
	if obj.Size > 0 {
		m.bytes.Add(obj.Size)
	}
	return nil
}

func (m *Migrator) markFailed(key string, err error) error {
	m.failed.Add(1)
	log.Printf("ERROR key=%q: %q", key, err.Error())
	if markErr := m.Store.MarkStatus(key, store.StatusFailed, err.Error()); markErr != nil {
		return fmt.Errorf("mark failed key=%q: %w (original error: %q)", key, markErr, err.Error())
	}
	return nil
}

func (m *Migrator) sourceSnapshot(ctx context.Context, obj store.Object) (sourceSnapshot, error) {
	out, err := m.OldCli.HeadObject(ctx, &s3.HeadObjectInput{Bucket: awsv2.String(m.Bucket), Key: awsv2.String(obj.Key)})
	if err != nil {
		if s3client.IsNotFound(err) && ctx.Err() == nil {
			// HEAD 404 cannot distinguish NoSuchBucket from NoSuchKey. GET
			// supplies the typed object error before we permanently skip it.
			get, getErr := m.OldCli.GetObject(ctx, &s3.GetObjectInput{Bucket: awsv2.String(m.Bucket), Key: awsv2.String(obj.Key)})
			if getErr != nil {
				return sourceSnapshot{}, &sourceReadError{getErr}
			}
			get.Body.Close()
		}
		return sourceSnapshot{}, &sourceReadError{err}
	}
	etag := awsv2.ToString(out.ETag)
	if obj.Size < 0 || obj.Size == math.MaxInt64 || out.ContentLength == nil || *out.ContentLength != obj.Size ||
		normalizeETag(etag) == "" || normalizeETag(obj.ETag) == "" || normalizeETag(etag) != normalizeETag(obj.ETag) {
		return sourceSnapshot{}, fmt.Errorf("source changed since scan or lacks a stable identity: key=%q", obj.Key)
	}
	if !obj.LastModified.IsZero() && out.LastModified != nil && !obj.LastModified.Truncate(time.Second).Equal(out.LastModified.Truncate(time.Second)) {
		return sourceSnapshot{}, fmt.Errorf("source last-modified changed since scan: key=%q", obj.Key)
	}
	return sourceSnapshot{etag: etag, versionID: out.VersionId, size: obj.Size, attrs: objectAttrs{
		CacheControl: out.CacheControl, ContentDisposition: out.ContentDisposition, ContentEncoding: out.ContentEncoding,
		ContentLanguage: out.ContentLanguage, ContentType: out.ContentType, Expires: out.Expires, Metadata: copyMetadata(out.Metadata),
	}}, nil
}

func (m *Migrator) sourceDigest(ctx context.Context, key string, snap sourceSnapshot) ([32]byte, error) {
	digest, err := s3client.DigestWithReader(ctx, m.OldCli, m.Bucket, key, snap.size, snap.etag, func(r io.Reader) io.Reader { return m.throttle(ctx, r) })
	if err != nil {
		return digest, &sourceReadError{err}
	}
	return digest, nil
}

// checkSourceBeforeCommit detects completed deletes/updates, but cannot make
// a cross-cluster transaction. SourceFrozen is the required operational fence.
func (m *Migrator) checkSourceBeforeCommit(ctx context.Context, obj store.Object, snap sourceSnapshot) error {
	latest, err := m.sourceSnapshot(ctx, obj)
	if err != nil {
		return err
	}
	if latest.etag != snap.etag || awsv2.ToString(latest.versionID) != awsv2.ToString(snap.versionID) {
		return fmt.Errorf("source representation changed before target commit")
	}
	return nil
}

func (m *Migrator) spool(ctx context.Context, reader io.Reader, expected int64) (*os.File, [32]byte, error) {
	var digest [32]byte
	tmp, err := os.CreateTemp(m.Cfg.SpoolDir, "migration-*")
	if err != nil {
		return nil, digest, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(m.throttle(ctx, reader), expected+1))
	if err == nil && n != expected {
		err = fmt.Errorf("source body length mismatch: got=%d want=%d", n, expected)
	}
	if err == nil {
		_, err = tmp.Seek(0, io.SeekStart)
	}
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, digest, err
	}
	copy(digest[:], h.Sum(nil))
	return tmp, digest, nil
}

func (m *Migrator) copySingle(ctx context.Context, obj store.Object, snap sourceSnapshot) error {
	release, err := m.spoolBudget.acquire(ctx, obj.Size+1)
	if err != nil {
		return err
	}
	defer release()
	out, err := m.OldCli.GetObject(ctx, &s3.GetObjectInput{Bucket: awsv2.String(m.Bucket), Key: awsv2.String(obj.Key), IfMatch: awsv2.String(snap.etag), VersionId: snap.versionID})
	if err != nil {
		return &sourceReadError{err}
	}
	defer out.Body.Close()
	if out.ContentLength == nil || *out.ContentLength != obj.Size || normalizeETag(awsv2.ToString(out.ETag)) != normalizeETag(snap.etag) {
		return fmt.Errorf("source response does not match snapshot")
	}
	tmp, digest, err := m.spool(ctx, out.Body, obj.Size)
	if err != nil {
		return &sourceReadError{err}
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err = m.checkSourceBeforeCommit(ctx, obj, snap); err != nil {
		return err
	}
	attrs := snap.attrs
	_, err = m.NewCli.PutObject(ctx, &s3.PutObjectInput{
		Bucket: awsv2.String(m.Bucket), Key: awsv2.String(obj.Key), Body: tmp, ContentLength: awsv2.Int64(obj.Size), IfNoneMatch: awsv2.String("*"),
		CacheControl: attrs.CacheControl, ContentDisposition: attrs.ContentDisposition, ContentEncoding: attrs.ContentEncoding,
		ContentLanguage: attrs.ContentLanguage, ContentType: attrs.ContentType, Expires: attrs.Expires, Metadata: attrs.Metadata,
	})
	if err != nil {
		return fmt.Errorf("put target: %w", err)
	}
	return m.verifyTarget(ctx, obj, digest, snap.attrs)
}

func attributesEqual(attrs objectAttrs, head *s3.HeadObjectOutput) bool {
	if head == nil {
		return false
	}
	for _, pair := range [][2]*string{
		{attrs.CacheControl, head.CacheControl}, {attrs.ContentDisposition, head.ContentDisposition},
		{attrs.ContentEncoding, head.ContentEncoding}, {attrs.ContentLanguage, head.ContentLanguage},
		{attrs.ContentType, head.ContentType},
	} {
		if awsv2.ToString(pair[0]) != awsv2.ToString(pair[1]) {
			return false
		}
	}
	if (attrs.Expires == nil) != (head.Expires == nil) {
		return false
	}
	if attrs.Expires != nil && !attrs.Expires.Equal(*head.Expires) {
		return false
	}
	return maps.Equal(attrs.Metadata, head.Metadata)
}

func (m *Migrator) verifyTarget(ctx context.Context, obj store.Object, sourceDigest [32]byte, attrs objectAttrs) error {
	head, err := m.NewCli.HeadObject(ctx, &s3.HeadObjectInput{Bucket: awsv2.String(m.Bucket), Key: awsv2.String(obj.Key)})
	if err != nil {
		return fmt.Errorf("verify head target: %w", err)
	}
	if head.ContentLength == nil || *head.ContentLength != obj.Size {
		return fmt.Errorf("target size mismatch: want=%d", obj.Size)
	}
	if !attributesEqual(attrs, head) {
		return fmt.Errorf("target metadata mismatch after commit")
	}
	digest, err := s3client.Digest(ctx, m.NewCli, m.Bucket, obj.Key, obj.Size, awsv2.ToString(head.ETag))
	if err != nil {
		return fmt.Errorf("verify target digest: %w", err)
	}
	if digest != sourceDigest {
		return fmt.Errorf("SHA256 mismatch after target commit")
	}
	return nil
}

func (m *Migrator) copyMultipart(ctx context.Context, obj store.Object, snap sourceSnapshot) error {
	partSize := m.Cfg.MultipartPartSize
	if partSize <= 0 || m.Cfg.MultipartConcurrency < 1 {
		return fmt.Errorf("invalid multipart configuration")
	}
	numParts := (obj.Size-1)/partSize + 1
	if numParts > 10000 {
		partSize = (obj.Size-1)/10000 + 1
		numParts = (obj.Size-1)/partSize + 1
	}
	if partSize > 5<<30 {
		return fmt.Errorf("multipart part size exceeds S3 maximum of 5 GiB")
	}
	attrs := snap.attrs
	create, err := m.NewCli.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: awsv2.String(m.Bucket), Key: awsv2.String(obj.Key), CacheControl: attrs.CacheControl, ContentDisposition: attrs.ContentDisposition,
		ContentEncoding: attrs.ContentEncoding, ContentLanguage: attrs.ContentLanguage, ContentType: attrs.ContentType, Expires: attrs.Expires, Metadata: attrs.Metadata,
	})
	if err != nil {
		return fmt.Errorf("create multipart target: %w", err)
	}
	uploadID := awsv2.ToString(create.UploadId)
	if uploadID == "" {
		return fmt.Errorf("target returned empty multipart upload ID")
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := m.NewCli.AbortMultipartUpload(cleanup, &s3.AbortMultipartUploadInput{Bucket: awsv2.String(m.Bucket), Key: awsv2.String(obj.Key), UploadId: awsv2.String(uploadID)}); err != nil {
			log.Printf("WARN abort multipart key=%q upload=%q: %q", obj.Key, uploadID, err.Error())
		}
	}()
	partCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	parts := make([]s3types.CompletedPart, int(numParts))
	sem := make(chan struct{}, m.Cfg.MultipartConcurrency)
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	record := func(err error) {
		if err != nil {
			errOnce.Do(func() { firstErr = err; cancel() })
		}
	}
partLoop:
	for i := int64(0); i < numParts; i++ {
		select {
		case <-partCtx.Done():
			break partLoop
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(idx int64) {
			defer wg.Done()
			defer func() { <-sem }()
			start := idx * partSize
			length := partSize
			if length > obj.Size-start {
				length = obj.Size - start
			}
			end := start + length - 1
			release, err := m.spoolBudget.acquire(partCtx, length+1)
			if err != nil {
				record(err)
				return
			}
			defer release()
			get, err := m.OldCli.GetObject(partCtx, &s3.GetObjectInput{Bucket: awsv2.String(m.Bucket), Key: awsv2.String(obj.Key), Range: awsv2.String(fmt.Sprintf("bytes=%d-%d", start, end)), IfMatch: awsv2.String(snap.etag), VersionId: snap.versionID})
			if err != nil {
				record(&sourceReadError{err})
				return
			}
			defer get.Body.Close()
			expectedRange := fmt.Sprintf("bytes %d-%d/%d", start, end, obj.Size)
			if awsv2.ToString(get.ContentRange) != expectedRange || get.ContentLength == nil || *get.ContentLength != length || normalizeETag(awsv2.ToString(get.ETag)) != normalizeETag(snap.etag) {
				record(fmt.Errorf("invalid source range response: got=%q expected=%q", awsv2.ToString(get.ContentRange), expectedRange))
				return
			}
			tmp, _, err := m.spool(partCtx, get.Body, length)
			if err != nil {
				record(&sourceReadError{err})
				return
			}
			defer os.Remove(tmp.Name())
			defer tmp.Close()
			up, err := m.NewCli.UploadPart(partCtx, &s3.UploadPartInput{Bucket: awsv2.String(m.Bucket), Key: awsv2.String(obj.Key), UploadId: awsv2.String(uploadID), PartNumber: awsv2.Int32(int32(idx + 1)), Body: tmp, ContentLength: awsv2.Int64(length)})
			if err != nil {
				record(fmt.Errorf("upload target part %d: %w", idx+1, err))
				return
			}
			if awsv2.ToString(up.ETag) == "" {
				record(fmt.Errorf("target part %d has no ETag", idx+1))
				return
			}
			parts[idx] = s3types.CompletedPart{ETag: up.ETag, PartNumber: awsv2.Int32(int32(idx + 1))}
		}(i)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Re-read the pinned source sequentially. This keeps memory bounded even
	// for very large objects and independently checks the assembled target.
	digest, err := m.sourceDigest(ctx, obj.Key, snap)
	if err != nil {
		return err
	}
	if err = m.checkSourceBeforeCommit(ctx, obj, snap); err != nil {
		return err
	}
	_, err = m.NewCli.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: awsv2.String(m.Bucket), Key: awsv2.String(obj.Key), UploadId: awsv2.String(uploadID), MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts}, IfNoneMatch: awsv2.String("*")})
	if err != nil {
		return fmt.Errorf("complete multipart target: %w", err)
	}
	committed = true
	return m.verifyTarget(ctx, obj, digest, snap.attrs)
}

// throttle charges source body bytes to the shared limiter. Metadata HEAD
// requests are not charged. A nil limiter returns the reader unchanged.
func (m *Migrator) throttle(ctx context.Context, r io.Reader) io.Reader {
	return ratelimit.Wrap(ctx, r, m.lim)
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
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open conflict log: %w", err)
	}
	return &conflictWriter{f: f}, nil
}

func (c *conflictWriter) Write(key string, oldLM, newLM time.Time) error {
	// Quote escapes all control characters, including terminal escape codes.
	quoted := strconv.Quote(key)
	key = quoted[1 : len(quoted)-1]
	line := fmt.Sprintf("%s\t%s\t%s\n",
		key,
		oldLM.UTC().Format(time.RFC3339),
		newLM.UTC().Format(time.RFC3339),
	)
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.f.WriteString(line)
	if err != nil {
		return err
	}
	// Persist the audit record before the durable conflict status is committed.
	return c.f.Sync()
}

func (c *conflictWriter) Close() error { return c.f.Close() }
