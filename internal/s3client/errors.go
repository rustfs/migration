package s3client

import (
	"context"
	"errors"
	"net"
	"net/http"
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
// NoSuchUpload refers to an upload ID. A URL or key in a transport error
// can contain any text, including "NoSuchKey", without being a 404.
func IsNotFound(err error) bool {
	if err == nil || IsCanceled(err) {
		return false
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchBucket", "404":
			return true
		case "":
			// Some HEAD responses have an HTTP status and no S3 error code.
		default:
			return false
		}
	}
	var response interface{ HTTPStatusCode() int }
	if errors.As(err, &response) {
		return response.HTTPStatusCode() == http.StatusNotFound
	}
	// Also recognize the old, unwrapped SDK response format.
	msg := err.Error()
	return strings.HasPrefix(msg, "https response error StatusCode: 404,") &&
		!strings.Contains(msg, "NoSuchUpload")
}

// IsAlreadyOwned recognizes a concurrent creation by the same owner.
func IsAlreadyOwned(err error) bool {
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
