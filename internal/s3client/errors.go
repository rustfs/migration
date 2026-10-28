package s3client

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"

	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// IsCanceled reports a request that did not finish. Callers must treat this
// as failed before any not-found check: the SDK text for a canceled
// GetObject or UploadPart can contain the substring "NotFound" or
// "StatusCode: 404" (a multipart abort's NoSuchUpload is an HTTP 404) even
// though the source object is still there.
func IsCanceled(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, syscall.EINTR) {
		return true
	}
	var canceled interface{ CanceledError() bool }
	if errors.As(err, &canceled) && canceled.CanceledError() {
		return true
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "context canceled"),
		strings.Contains(msg, "context cancelled"),
		strings.Contains(msg, "context deadline exceeded"),
		strings.Contains(msg, "deadline exceeded"),
		strings.Contains(msg, "request canceled"),
		strings.Contains(msg, "request cancelled"),
		strings.Contains(msg, "operation was canceled"),
		strings.Contains(msg, "operation was cancelled"),
		strings.Contains(msg, "interrupted"):
		return true
	default:
		return false
	}
}

// IsNotFound matches object 404s (GET NoSuchKey, HEAD NotFound) and bucket
// 404s (NoSuchBucket) across the typed and untyped shapes aws-sdk-go-v2
// returns against self-hosted S3 implementations.
//
// Cancellation is rejected first, on purpose. The substring checks below
// ("NotFound", "StatusCode: 404") are how a canceled request was recorded
// as a source miss. NoSuchUpload is an upload-id 404, not a missing object.
func IsNotFound(err error) bool {
	if err == nil || IsCanceled(err) {
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
	var nsb *s3types.NoSuchBucket
	if errors.As(err, &nsb) {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchBucket", "404":
			return true
		case "":
			// Untyped body; fall through to the message heuristics.
		default:
			// Completed response for some other code, such as NoSuchUpload.
			return false
		}
	}
	msg := err.Error()
	if strings.Contains(msg, "NoSuchUpload") &&
		!strings.Contains(msg, "NoSuchKey") &&
		!strings.Contains(msg, "NoSuchBucket") &&
		!strings.Contains(msg, "NotFound") {
		return false
	}
	return strings.Contains(msg, "NoSuchKey") ||
		strings.Contains(msg, "NoSuchBucket") ||
		strings.Contains(msg, "NotFound") ||
		strings.Contains(msg, "StatusCode: 404")
}

// IsAlreadyExists matches CreateBucket when the bucket is already there.
func IsAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	var owned *s3types.BucketAlreadyOwnedByYou
	if errors.As(err, &owned) {
		return true
	}
	var exists *s3types.BucketAlreadyExists
	if errors.As(err, &exists) {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
			return true
		}
	}
	return false
}
