package migrator

import (
	"bytes"
	"context"
	"github.com/rustfs/migration/internal/fakes3"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ETags are concurrency tokens. A different multipart/encryption ETag is not
// itself a content conflict, and an equal ETag is never proof of equality.
func TestExistingTargetRequiresVerifiedBytes(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		source, target         []byte
		etag                   string
		done, conflict, failed int64
	}{
		{"same-content-opaque-etag", []byte("same"), []byte("same"), "opaque-encrypted-value", 1, 0, 0},
		{"same-content-multipart-etag", []byte("same"), []byte("same"), "digest-2", 1, 0, 0},
		{"same-content-quoted-etag", []byte("same"), []byte("same"), `"quoted-value"`, 1, 0, 0},
		{"different-content-same-size", []byte("AAAA"), []byte("BBBB"), "opaque", 0, 1, 0},
		{"different-size", []byte("longer"), []byte("short"), "opaque", 0, 1, 0},
		{"missing-target-etag", []byte("same"), []byte("same"), "", 0, 0, 1},
		{"zero-byte", []byte{}, []byte{}, "empty-object-etag", 1, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := auditServer(t, map[string][]byte{"key": tc.source})
			target := auditServer(t, map[string][]byte{"key": tc.target})
			url := auditETagEndpoint(t, target, tc.etag)
			m, st := auditMigration(t, source.URL, url)
			err := m.Run(context.Background())
			if (err != nil) != (tc.failed != 0) {
				t.Fatalf("unexpected run error: %v", err)
			}
			counts, err := st.Counts()
			if err != nil {
				t.Fatal(err)
			}
			if counts.Done != tc.done || counts.Conflict != tc.conflict || counts.Failed != tc.failed {
				t.Fatalf("counts=%+v", counts)
			}
			got, _ := target.Object("bucket", "key")
			if !bytes.Equal(got, tc.target) || target.PutObjectCalls("key") != 0 {
				t.Fatal("existing target was modified")
			}
		})
	}
}

// Override the fixture's MD5-shaped ETags to model an endpoint with opaque
// representation tags, while still enforcing the requested If-Match token.
func auditETagEndpoint(t *testing.T, target *fakes3.Server, etag string) string {
	t.Helper()
	return auditWrap(t, target, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		if match := r.Header.Get("If-Match"); match != "" && match != etag {
			auditError(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		r.Header.Del("If-Match")
		recorded := httptest.NewRecorder()
		next.ServeHTTP(recorded, r)
		for key, values := range recorded.Header() {
			w.Header()[key] = values
		}
		if etag == "" {
			w.Header().Del("ETag")
		} else {
			w.Header().Set("ETag", etag)
		}
		w.WriteHeader(recorded.Code)
		_, _ = w.Write(recorded.Body.Bytes())
	})
}
