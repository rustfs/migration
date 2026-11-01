package s3client

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestMetadataEqualPreservedFields(t *testing.T) {
	expires := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	source := &s3.HeadObjectOutput{
		CacheControl: aws.String("max-age=30"), ContentType: aws.String("application/json"),
		ContentEncoding: aws.String("gzip"), ContentDisposition: aws.String("inline"),
		ContentLanguage: aws.String("en"), Expires: &expires,
		Metadata: map[string]string{"origin": "source"}, ETag: aws.String("source-token"),
	}
	mutations := map[string]func(*s3.HeadObjectOutput){
		"cache-control":    func(h *s3.HeadObjectOutput) { h.CacheControl = aws.String("no-store") },
		"content-type":     func(h *s3.HeadObjectOutput) { h.ContentType = aws.String("text/html") },
		"encoding":         func(h *s3.HeadObjectOutput) { h.ContentEncoding = nil },
		"disposition":      func(h *s3.HeadObjectOutput) { h.ContentDisposition = aws.String("attachment") },
		"language":         func(h *s3.HeadObjectOutput) { h.ContentLanguage = aws.String("zh") },
		"expires-missing":  func(h *s3.HeadObjectOutput) { h.Expires = nil },
		"expires-changed":  func(h *s3.HeadObjectOutput) { h.Expires = aws.Time(expires.Add(time.Second)) },
		"metadata-value":   func(h *s3.HeadObjectOutput) { h.Metadata = map[string]string{"origin": "different"} },
		"metadata-key":     func(h *s3.HeadObjectOutput) { h.Metadata = map[string]string{"other": "source"} },
		"metadata-missing": func(h *s3.HeadObjectOutput) { h.Metadata = nil },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			target := *source
			mutate(&target)
			if MetadataEqual(source, &target) || MetadataEqual(&target, source) {
				t.Fatal("different preserved metadata accepted")
			}
		})
	}
	target := *source
	target.ETag = aws.String("different-target-token")
	target.LastModified = aws.Time(expires.Add(time.Hour))
	if !MetadataEqual(source, &target) {
		t.Fatal("ETag and LastModified are not preserved metadata")
	}
	if MetadataEqual(nil, source) || MetadataEqual(source, nil) {
		t.Fatal("missing HEAD response cannot match metadata")
	}
}
