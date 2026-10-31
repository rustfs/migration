package cluster

import (
	"context"
	"fmt"
	"net/url"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// WalkObjects passes one decoded listing page at a time to visit. Request URL
// encoding so even keys containing characters forbidden in XML can be listed.
func WalkObjects(ctx context.Context, cli *s3.Client, bucket string, visit func([]types.Object) error) error {
	input := &s3.ListObjectsV2Input{Bucket: aws.String(bucket), EncodingType: types.EncodingTypeUrl}
	for {
		page, err := cli.ListObjectsV2(ctx, input)
		if err != nil {
			return err
		}
		if name := aws.ToString(page.Name); name != "" && name != bucket {
			return fmt.Errorf("list page bucket mismatch: requested %q, server returned %q", bucket, name)
		}
		for i := range page.Contents {
			obj := &page.Contents[i]
			if aws.ToString(obj.Key) == "" || obj.Size == nil || *obj.Size < 0 {
				return fmt.Errorf("invalid object in listing for bucket %q: missing key or invalid size", bucket)
			}
			if page.EncodingType == types.EncodingTypeUrl {
				// Some S3 servers encode spaces as +, others as %20.
				// A literal plus is encoded as %2B in either form.
				key, err := url.QueryUnescape(*obj.Key)
				if err != nil {
					return fmt.Errorf("decode key %q: %w", *obj.Key, err)
				}
				obj.Key = &key
			}
		}
		if err := visit(page.Contents); err != nil {
			return err
		}
		if !aws.ToBool(page.IsTruncated) {
			return nil
		}
		next := aws.ToString(page.NextContinuationToken)
		if next == "" || next == aws.ToString(input.ContinuationToken) {
			return fmt.Errorf("truncated listing for bucket %q has a missing or repeated continuation token", bucket)
		}
		input.ContinuationToken = page.NextContinuationToken
	}
}
