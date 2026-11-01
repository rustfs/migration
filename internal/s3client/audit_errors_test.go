package s3client

import (
	"errors"
	"net/url"
	"testing"
)

// A transport failure is not an S3 response, even when the object key happens
// to contain an S3 error name. Treating it as 404 bypasses the target guard or
// makes a source object permanently skipped.
func TestAuditTransportFailureIsNotObjectNotFound(t *testing.T) {
	for _, key := range []string{"NotFound-report", "NoSuchKey-report", "NoSuchBucket-report"} {
		t.Run(key, func(t *testing.T) {
			err := &url.Error{Op: "Head", URL: "https://s3.example/bucket/" + key, Err: errors.New("connection reset by peer")}
			if IsNotFound(err) {
				t.Fatalf("transport failure misclassified as completed 404: %v", err)
			}
		})
	}
}
