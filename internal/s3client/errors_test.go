package s3client

import (
	"context"
	"errors"
	"fmt"
	"testing"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

type apiErr struct {
	code string
}

func (e apiErr) Error() string                 { return e.code }
func (e apiErr) ErrorCode() string             { return e.code }
func (e apiErr) ErrorMessage() string          { return e.code }
func (e apiErr) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(&s3types.NoSuchKey{}) {
		t.Fatal("NoSuchKey")
	}
	if !IsNotFound(&s3types.NotFound{}) {
		t.Fatal("NotFound")
	}
	if !IsNotFound(&s3types.NoSuchBucket{}) {
		t.Fatal("NoSuchBucket")
	}
	if !IsNotFound(apiErr{code: "NoSuchKey"}) {
		t.Fatal("api NoSuchKey")
	}
	if IsNotFound(errors.New("boom")) {
		t.Fatal("plain error should not match")
	}
	if IsNotFound(nil) {
		t.Fatal("nil")
	}
}

func TestCanceledRequestIsNotSource404(t *testing.T) {
	// These are the shapes a SIGINT produces. None is a typed NoSuchKey.
	// The NotFound / StatusCode: 404 fragments are what the substring
	// matcher used to treat as a finished source 404.
	cases := []error{
		fmt.Errorf("get old: %w", context.Canceled),
		fmt.Errorf("get old: %w", context.DeadlineExceeded),
		fmt.Errorf("upload part 1: %w", &awsv2.RequestCanceledError{Err: context.Canceled}),
		fmt.Errorf("get part 2: %w", &smithy.CanceledError{Err: context.Canceled}),
		fmt.Errorf("operation error S3: GetObject, https response error StatusCode: 404, RequestID: r, HostID: h, NotFound: %w", context.Canceled),
		fmt.Errorf("get part 1: %w; abort multipart: https response error StatusCode: 404, RequestID: r, HostID: h, NoSuchUpload: The specified upload does not exist.", context.DeadlineExceeded),
		fmt.Errorf("read part 3: request canceled, context canceled; NotFound"),
	}
	for _, err := range cases {
		if IsNotFound(err) {
			t.Fatalf("IsNotFound(%q) = true, want false", err)
		}
		if !IsCanceled(err) {
			t.Fatalf("IsCanceled(%q) = false, want true", err)
		}
	}

	if !IsNotFound(&s3types.NoSuchKey{}) {
		t.Fatal("typed NoSuchKey")
	}
	if !IsNotFound(fmt.Errorf("get old: %w", &s3types.NoSuchKey{})) {
		t.Fatal("wrapped NoSuchKey")
	}
	if !IsNotFound(&s3types.NotFound{}) {
		t.Fatal("typed NotFound")
	}
	if IsNotFound(errors.New("https response error StatusCode: 404, RequestID: z, NotFound")) {
		t.Fatal("text without a structured HTTP response is not evidence of 404")
	}
	upload404 := fmt.Errorf("upload part 1: https response error StatusCode: 404, RequestID: r, HostID: h, %w", &s3types.NoSuchUpload{})
	if IsNotFound(upload404) {
		t.Fatal("NoSuchUpload is not a missing source object")
	}
}

func TestIsAlreadyExists(t *testing.T) {
	if IsAlreadyExists(&s3types.BucketAlreadyExists{}) {
		t.Fatal("foreign bucket ownership must not be accepted")
	}
	if !IsAlreadyExists(&s3types.BucketAlreadyOwnedByYou{}) {
		t.Fatal("owned")
	}
	if IsAlreadyExists(errors.New("nope")) {
		t.Fatal("plain")
	}
}
