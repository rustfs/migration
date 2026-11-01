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
// Only structured API codes or an actual HTTP response are evidence of a 404.
// Object keys and proxy messages in error text must never authorize a write.
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
			// A HEAD response may have no body or API code.
		default:
			// Completed response for some other code, such as NoSuchUpload.
			return false
		}
	}
	var response interface{ HTTPStatusCode() int }
	return errors.As(err, &response) && response.HTTPStatusCode() == 404
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
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "BucketAlreadyOwnedByYou":
			return true
		}
	}
	return false
}

// IsAlreadyOwned is the explicit ownership-specific name.
func IsAlreadyOwned(err error) bool { return IsAlreadyExists(err) }
