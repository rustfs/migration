package cluster

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/rustfs/migration/internal/config"
	"github.com/rustfs/migration/internal/s3client"
)

func clientFor(t *testing.T, url string) *s3.Client {
	t.Helper()
	cli, err := s3client.New(context.Background(), config.Endpoint{Endpoint: url, AccessKey: "access", SecretKey: "secret", Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	return cli
}

func TestWalkObjectsDecodesKeysOnceAndPreservesOpaqueTokens(t *testing.T) {
	const token = "unrelated+/token%2F"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("encoding-type") != "url" {
			t.Error("missing URL encoding request")
		}
		page := ""
		switch query.Get("continuation-token") {
		case "":
			page = "<EncodingType>url</EncodingType><IsTruncated>true</IsTruncated><NextContinuationToken>" + token + "</NextContinuationToken><Contents><Key>dir%2F%25%32%46%2B+a%20b%09%01</Key><Size>0</Size></Contents>"
		case token:
			// Older compatible implementations can omit EncodingType and return plain keys.
			page = "<IsTruncated>false</IsTruncated><Contents><Key>literal%2F</Key><Size>1</Size></Contents>"
		default:
			t.Errorf("unexpected token %q", query.Get("continuation-token"))
		}
		fmt.Fprintf(w, "<ListBucketResult><Name>photos</Name>%s</ListBucketResult>", page)
	}))
	defer server.Close()
	var got []string
	err := WalkObjects(context.Background(), clientFor(t, server.URL), "photos", func(objects []types.Object) error {
		for _, obj := range objects {
			got = append(got, *obj.Key)
		}
		return nil
	})
	if err != nil || !slices.Equal(got, []string{"dir/%2F+ a b\t\x01", "literal%2F"}) {
		t.Fatalf("keys=%q err=%v", got, err)
	}
}

func TestWalkObjectsRejectsBrokenPagination(t *testing.T) {
	for _, token := range []string{"", "duplicate"} {
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			if requests > 2 {
				w.WriteHeader(400)
				return
			}
			fmt.Fprintf(w, "<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>%s</NextContinuationToken></ListBucketResult>", token)
		}))
		err := WalkObjects(context.Background(), clientFor(t, server.URL), "photos", func([]types.Object) error { return nil })
		server.Close()
		if err == nil || !strings.Contains(err.Error(), "continuation token") {
			t.Fatalf("token=%q err=%v", token, err)
		}
	}
}

func TestWalkObjectsRejectsAlternatingTokens(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests > 3 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		next := "a"
		if r.URL.Query().Get("continuation-token") == "a" {
			next = "b"
		}
		fmt.Fprintf(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>%s</NextContinuationToken></ListBucketResult>`, next)
	}))
	defer server.Close()
	err := WalkObjects(context.Background(), clientFor(t, server.URL), "photos", func([]types.Object) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "continuation token") || requests != 3 {
		t.Fatalf("requests=%d err=%v", requests, err)
	}
}
