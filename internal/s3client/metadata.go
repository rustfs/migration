package s3client

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// MetadataEqual compares the object headers and user metadata preserved by
// migration. ETags and LastModified are representation identifiers, not
// preserved metadata, and may legitimately differ between endpoints.
func MetadataEqual(a, b *s3.HeadObjectOutput) bool {
	if a == nil || b == nil {
		return false
	}
	if aws.ToString(a.CacheControl) != aws.ToString(b.CacheControl) ||
		aws.ToString(a.ContentType) != aws.ToString(b.ContentType) ||
		aws.ToString(a.ContentEncoding) != aws.ToString(b.ContentEncoding) ||
		aws.ToString(a.ContentDisposition) != aws.ToString(b.ContentDisposition) ||
		aws.ToString(a.ContentLanguage) != aws.ToString(b.ContentLanguage) {
		return false
	}
	if (a.Expires == nil) != (b.Expires == nil) || (a.Expires != nil && !a.Expires.Equal(*b.Expires)) {
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
