package cluster

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/rustfs/migration/internal/config"
	"github.com/rustfs/migration/internal/s3client"
)

func TestListBucketsFollowsContinuation(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Query().Get("continuation-token") == "page-two" {
			_, _ = io.WriteString(w, `<ListAllMyBucketsResult><Buckets><Bucket><Name>second</Name></Bucket></Buckets></ListAllMyBucketsResult>`)
			return
		}
		_, _ = io.WriteString(w, `<ListAllMyBucketsResult><Buckets><Bucket><Name>first</Name></Bucket></Buckets><ContinuationToken>page-two</ContinuationToken></ListAllMyBucketsResult>`)
	}))
	defer srv.Close()
	buckets, err := ListBuckets(context.Background(), auditClient(t, srv.URL, "us-east-1"))
	if err != nil || len(buckets) != 2 || buckets[0].Name != "first" || buckets[1].Name != "second" || requests.Load() != 2 {
		t.Fatalf("bucket pagination: buckets=%+v requests=%d err=%v", buckets, requests.Load(), err)
	}
}

func auditClient(t *testing.T, endpoint, region string) *s3.Client {
	t.Helper()
	cli, err := s3client.New(context.Background(), config.Endpoint{
		Endpoint: endpoint, Region: region, AccessKey: "test", SecretKey: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return cli
}

func TestEnsureBucketRechecksAlreadyOwned(t *testing.T) {
	for _, accessible := range []bool{true, false} {
		t.Run(fmt.Sprint(accessible), func(t *testing.T) {
			var heads atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					if heads.Add(1) == 1 {
						w.WriteHeader(http.StatusNotFound)
					} else if accessible {
						w.WriteHeader(http.StatusOK)
					} else {
						w.WriteHeader(http.StatusForbidden)
					}
					return
				}
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `<Error><Code>BucketAlreadyOwnedByYou</Code></Error>`)
			}))
			defer srv.Close()
			err := EnsureBucket(context.Background(), auditClient(t, srv.URL, "us-east-1"), "bucket")
			if (err == nil) != accessible || heads.Load() != 2 {
				t.Fatalf("access=%v heads=%d err=%v", accessible, heads.Load(), err)
			}
		})
	}
}

// BucketAlreadyExists means the name belongs to somebody else, unlike
// BucketAlreadyOwnedByYou. A failed create cannot prove a usable target.
func TestAuditEnsureBucketRejectsForeignOwner(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPut:
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `<Error><Code>BucketAlreadyExists</Code><Message>name owned by another account</Message></Error>`)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()
	if err := EnsureBucket(context.Background(), auditClient(t, srv.URL, "us-east-1"), "bucket"); err == nil {
		t.Fatal("EnsureBucket accepted BucketAlreadyExists; target ownership/access was never established")
	}
}

// A regional client only changes endpoint/signing; CreateBucket still needs
// LocationConstraint in its XML body outside us-east-1.
func TestAuditEnsureBucketUsesConfiguredRegion(t *testing.T) {
	const region = "eu-west-1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var input struct {
			LocationConstraint string `xml:"LocationConstraint"`
		}
		if err := xml.NewDecoder(r.Body).Decode(&input); err != nil || input.LocationConstraint != region {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `<Error><Code>IllegalLocationConstraintException</Code><Message>missing regional LocationConstraint</Message></Error>`)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := EnsureBucket(context.Background(), auditClient(t, srv.URL, region), "bucket"); err != nil {
		t.Fatalf("creating a bucket in configured region %s failed: %v", region, err)
	}
}

func TestListBucketsRejectsAlternatingContinuationTokens(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		if n > 3 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		next := "a"
		if r.URL.Query().Get("continuation-token") == "a" {
			next = "b"
		}
		fmt.Fprintf(w, `<ListAllMyBucketsResult><Buckets></Buckets><ContinuationToken>%s</ContinuationToken></ListAllMyBucketsResult>`, next)
	}))
	defer srv.Close()
	_, err := ListBuckets(context.Background(), auditClient(t, srv.URL, "us-east-1"))
	if err == nil || !strings.Contains(err.Error(), "repeated") || requests.Load() != 3 {
		t.Fatalf("requests=%d err=%v", requests.Load(), err)
	}
}
