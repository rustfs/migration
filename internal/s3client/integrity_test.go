package s3client

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rustfs/migration/internal/config"
)

func TestDigestPinsRepresentationAndChecksExactBytes(t *testing.T) {
	for _, tc := range []struct {
		name, body, responseETag string
		wantSize, declaredSize   int64
		wantError                bool
	}{
		{"valid", "abc", `"opaque"`, 3, 3, false},
		{"empty", "", `"opaque"`, 0, 0, false},
		{"truncated", "abc", `"opaque"`, 10, 10, true},
		{"oversized", "abcd", `"opaque"`, 3, 4, true},
		{"changed-etag", "abc", `"changed"`, 3, 3, true},
		{"missing-etag", "abc", "", 3, 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("If-Match") != `"opaque"` {
					t.Errorf("unconditional digest read: %v", r.Header)
				}
				w.Header().Set("Content-Length", fmt.Sprint(tc.declaredSize))
				if tc.responseETag != "" {
					w.Header().Set("ETag", tc.responseETag)
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client, err := New(context.Background(), config.Endpoint{Endpoint: server.URL, Region: "us-east-1", AccessKey: "test", SecretKey: "test"})
			if err != nil {
				t.Fatal(err)
			}
			got, err := Digest(context.Background(), client, "bucket", "key", tc.wantSize, `"opaque"`)
			if (err != nil) != tc.wantError {
				t.Fatalf("digest error=%v wantError=%t", err, tc.wantError)
			}
			if !tc.wantError && got != sha256.Sum256([]byte(tc.body)) {
				t.Fatal("incorrect SHA256")
			}
		})
	}
}

func TestDigestRejectsUnpinnedReadsBeforeNetwork(t *testing.T) {
	// Nil client deliberately proves missing identity is rejected before I/O.
	_, err := Digest(context.Background(), nil, "bucket", "key", 3, "")
	if err == nil || !strings.Contains(err.Error(), "ETag") {
		t.Fatalf("missing identity: %v", err)
	}
}
