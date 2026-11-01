package migrator

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rustfs/migration/internal/store"
)

func TestUntrustedKeyAndErrorCannotInjectLogs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conflicts")
	w, err := newConflictWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	key := "object\nforged\x1b[31m\tend"
	if err := w.Write(key, time.Time{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "\n") != 1 || bytes.Contains(data, []byte{0x1b}) {
		t.Fatalf("unsafe conflict log: %q", data)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"), "bucket")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var out bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(prev)
	m := &Migrator{Store: st}
	_ = m.markFailed(key, errors.New("server error\nforged\x1b[31m"))
	if strings.Count(out.String(), "\n") != 1 || bytes.Contains(out.Bytes(), []byte{0x1b}) {
		t.Fatalf("unsafe error log: %q", out.String())
	}
}
