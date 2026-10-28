package migrator

import (
	"testing"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/rustfs/migration/internal/store"
)

func TestTargetAlreadyMigrated(t *testing.T) {
	const etag = "aadd3e6a955c4a99024ca0bf126144a9"
	cases := []struct {
		name string
		obj  store.Object
		head *s3.HeadObjectOutput
		want bool
	}{
		{
			name: "identical quoted etag",
			obj:  store.Object{Size: 1024, ETag: `"` + etag + `"`},
			head: headOf(1024, `"`+etag+`"`),
			want: true,
		},
		{
			name: "quotes only on inventory",
			obj:  store.Object{Size: 1024, ETag: `"` + etag + `"`},
			head: headOf(1024, etag),
			want: true,
		},
		{
			name: "same size different etag",
			obj:  store.Object{Size: 1024, ETag: etag},
			head: headOf(1024, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
			want: false,
		},
		{
			name: "different size same etag",
			obj:  store.Object{Size: 1024, ETag: etag},
			head: headOf(1025, etag),
			want: false,
		},
		{
			name: "multipart dest etag is not comparable",
			obj:  store.Object{Size: 1024, ETag: etag},
			head: headOf(1024, etag+"-3"),
			want: true,
		},
		{
			name: "both multipart etags differ",
			obj:  store.Object{Size: 4096, ETag: etag + "-2"},
			head: headOf(4096, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-4"),
			want: true,
		},
		{
			name: "missing etags match on size",
			obj:  store.Object{Size: 8},
			head: headOf(8, ""),
			want: true,
		},
		{
			name: "one missing etag matches on size",
			obj:  store.Object{Size: 8, ETag: etag},
			head: headOf(8, ""),
			want: true,
		},
		{
			name: "zero byte identical",
			obj:  store.Object{Size: 0, ETag: "d41d8cd98f00b204e9800998ecf8427e"},
			head: headOf(0, `"d41d8cd98f00b204e9800998ecf8427e"`),
			want: true,
		},
		{
			name: "nil head",
			obj:  store.Object{Size: 1, ETag: "a"},
			head: nil,
			want: false,
		},
		{
			name: "missing content length",
			obj:  store.Object{Size: 4, ETag: "abcd"},
			head: &s3.HeadObjectOutput{ETag: awsv2.String("abcd")},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := targetAlreadyMigrated(tc.obj, tc.head); got != tc.want {
				t.Fatalf("targetAlreadyMigrated() = %v, want %v", got, tc.want)
			}
		})
	}
}

func headOf(size int64, etag string) *s3.HeadObjectOutput {
	return &s3.HeadObjectOutput{
		ContentLength: awsv2.Int64(size),
		ETag:          awsv2.String(etag),
	}
}
