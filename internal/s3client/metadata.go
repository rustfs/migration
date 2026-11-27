package s3client

import (
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// DefaultContentType is the standard S3 default MIME type assigned by servers
// when an object is stored without an explicit Content-Type header.
const DefaultContentType = "application/octet-stream"

// EffectiveContentType normalizes an empty or missing Content-Type to the S3
// server default so objects uploaded without a Content-Type header do not fail
// post-commit readback or diff against servers that populate the default.
func EffectiveContentType(ct *string) string {
	v := strings.TrimSpace(aws.ToString(ct))
	if v == "" {
		return DefaultContentType
	}
	return v
}

// MetadataEqual compares the object headers and user metadata preserved by
// migration. ETags and LastModified are representation identifiers, not
// preserved metadata, and may legitimately differ between endpoints.
func MetadataEqual(a, b *s3.HeadObjectOutput) bool {
	if a == nil || b == nil {
		return false
	}
	if aws.ToString(a.CacheControl) != aws.ToString(b.CacheControl) ||
		EffectiveContentType(a.ContentType) != EffectiveContentType(b.ContentType) ||
		aws.ToString(a.ContentEncoding) != aws.ToString(b.ContentEncoding) ||
		aws.ToString(a.ContentDisposition) != aws.ToString(b.ContentDisposition) ||
		aws.ToString(a.ContentLanguage) != aws.ToString(b.ContentLanguage) {
		return false
	}
	if !expiresEqual(a, b) {
		return false
	}
	if len(a.Metadata) != len(b.Metadata) {
		return false
	}
	for key, value := range a.Metadata {
		if other, ok := b.Metadata[key]; !ok || other != value {
			return false
		}
	}
	return true
}

func expiresEqual(a, b *s3.HeadObjectOutput) bool {
	aHas := a.Expires != nil || strings.TrimSpace(aws.ToString(a.ExpiresString)) != ""
	bHas := b.Expires != nil || strings.TrimSpace(aws.ToString(b.ExpiresString)) != ""
	if aHas != bHas {
		return false
	}
	if !aHas {
		return true
	}
	if a.Expires != nil && b.Expires != nil {
		return a.Expires.Equal(*b.Expires)
	}
	return strings.TrimSpace(rawExpires(a)) == strings.TrimSpace(rawExpires(b))
}

func rawExpires(h *s3.HeadObjectOutput) string {
	if s := strings.TrimSpace(aws.ToString(h.ExpiresString)); s != "" {
		return s
	}
	if h.Expires != nil {
		return h.Expires.UTC().Format(http.TimeFormat)
	}
	return ""
}
