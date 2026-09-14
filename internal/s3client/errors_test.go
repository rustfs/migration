package s3client

import (
	"errors"
	"testing"

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

func TestIsAlreadyExists(t *testing.T) {
	if !IsAlreadyExists(&s3types.BucketAlreadyExists{}) {
		t.Fatal("exists")
	}
	if !IsAlreadyExists(&s3types.BucketAlreadyOwnedByYou{}) {
		t.Fatal("owned")
	}
	if IsAlreadyExists(errors.New("nope")) {
		t.Fatal("plain")
	}
}
