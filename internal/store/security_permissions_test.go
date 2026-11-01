package store

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNewStateFilesArePrivateAndExistingModePreserved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file access is governed by ACLs, not Unix mode bits")
	}
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(path, "bucket")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{path, path + ".lock", path + "-wal", path + "-shm"} {
		info, err := os.Stat(name)
		if os.IsNotExist(err) && (name == path+"-wal" || name == path+"-shm") {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s has non-private mode %o", name, info.Mode().Perm())
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path, "bucket")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("existing database permissions changed to %o", info.Mode().Perm())
	}
}
