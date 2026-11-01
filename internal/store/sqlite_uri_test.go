package store

import (
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSQLiteURIWindowsPathFormatting(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"C:/Users/user/state #1.db", "file:///C:/Users/user/state%20%231.db"},
		{"//server/share/state #1.db", "file:////server/share/state%20%231.db"},
	} {
		got := (&url.URL{Scheme: "file", Path: sqliteURIPath(tc.path, true)}).String()
		if got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestSQLiteURIPathSpecialCharactersAndWriterLock(t *testing.T) {
	name := "state #? % ü.db"
	if runtime.GOOS == "windows" {
		name = "state # % ü.db" // Windows filenames cannot contain question marks.
	}
	path := filepath.Join(t.TempDir(), name)
	writer, err := Open(path, "bucket")
	if err != nil {
		t.Fatal(err)
	}
	if other, err := Open(path, "bucket"); err == nil {
		other.Close()
		writer.Close()
		t.Fatal("second writer acquired lock")
	} else if !strings.Contains(err.Error(), "in use by another writer") {
		writer.Close()
		t.Fatalf("unexpected lock error: %v", err)
	}
	reader, err := OpenExisting(path)
	if err != nil {
		writer.Close()
		t.Fatalf("open read-only: %v", err)
	}
	reader.Close()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, "bucket")
	if err != nil {
		t.Fatalf("reopen after release: %v", err)
	}
	reopened.Close()
}
