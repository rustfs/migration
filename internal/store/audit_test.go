package store

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrationBindingRejectsChangedClusters(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.db"), "bucket")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.BindMigration("https://SOURCE.example/", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginScan(); err != nil {
		t.Fatal(err)
	}
	if err := st.BatchUpsertScanned([]Object{{Key: "key", Size: 4}}); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteScan(); err != nil {
		t.Fatal(err)
	}
	if err := st.BindMigration("https://source.example", "https://target.example/"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkStatus("key", StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.BindMigration("https://source.example", "https://target.example"); err != nil {
		t.Fatalf("safe resume rejected: %v", err)
	}
	if err := st.BindMigration("https://other-source.example", "https://target.example"); err == nil {
		t.Fatal("different source accepted")
	}
	if err := st.BindMigration("https://source.example", "https://other-target.example"); err == nil {
		t.Fatal("different target accepted")
	}
}

func TestLegacyUnboundInventoryIsRejected(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.db"), "bucket")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.BatchUpsertScanned([]Object{{Key: "key", Size: 4}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`DELETE FROM meta WHERE key='state_format'`); err != nil {
		t.Fatal(err)
	}
	if err := st.BindMigration("https://source.example", "https://target.example"); err == nil {
		t.Fatal("legacy unbound rows adopted without knowing their source")
	}
}

func TestInterruptedRescanInvalidatesPreviousInventory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(path, "bucket")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RequireCompleteScan(); err == nil {
		t.Fatal("new DB treated as scanned")
	}
	if err := st.CompleteScan(); err == nil {
		t.Fatal("completion before scan accepted")
	}
	if err := st.BeginScan(); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteScan(); err != nil {
		t.Fatal(err)
	}
	if err := st.RequireCompleteScan(); err != nil {
		t.Fatalf("empty completed bucket rejected: %v", err)
	}
	if err := st.BeginScan(); err != nil {
		t.Fatal(err)
	}
	if err := st.BatchUpsertScanned([]Object{{Key: "partial", Size: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path, "bucket")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.RequireCompleteScan(); err == nil {
		t.Fatal("interrupted second generation inherited prior completion")
	}
}

func TestWriterLockAllowsReadOnlyStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	writer, err := Open(path, "bucket")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if another, err := Open(path, "bucket"); err == nil {
		another.Close()
		t.Fatal("second concurrent migration writer accepted")
	}
	alias := filepath.Join(filepath.Dir(path), "alias.db")
	if err := os.Symlink(path, alias); err == nil {
		if another, err := Open(alias, "bucket"); err == nil {
			another.Close()
			t.Fatal("symlink alias bypassed writer lock")
		}
	}
	reader, err := OpenExisting(path)
	if err != nil {
		t.Fatalf("status cannot read active migration: %v", err)
	}
	defer reader.Close()
	if _, err := reader.Counts(); err != nil {
		t.Fatal(err)
	}
	if err := reader.BeginScan(); err == nil {
		t.Fatal("status reader unexpectedly permits mutation")
	}
}

// The helper owns the SQLite pager lock in another process. Killing it must
// release the lock without a stale PID file or a manual unlock operation.
func TestWriterLockRecoversAfterProcessCrash(t *testing.T) {
	if os.Getenv("MIGRATION_TEST_LOCK_HELPER") == "1" {
		st, err := Open(os.Getenv("MIGRATION_TEST_LOCK_PATH"), "bucket")
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		fmt.Println("ready")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		return
	}
	path := filepath.Join(t.TempDir(), "state.db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestWriterLockRecoversAfterProcessCrash$")
	cmd.Env = append(os.Environ(), "MIGRATION_TEST_LOCK_HELPER=1", "MIGRATION_TEST_LOCK_PATH="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("helper did not acquire lock: %q %v", line, err)
	}
	if st, err := Open(path, "bucket"); err == nil {
		st.Close()
		t.Fatal("cross-process writer lock was bypassed")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	st, err := Open(path, "bucket")
	if err != nil {
		t.Fatalf("lock remained after process death: %v", err)
	}
	defer st.Close()
}

// Keeping an object terminal must not conceal that the source now contains
// different bytes under the same key. This does not request an overwrite:
// invalidating verification or surfacing a conflict is sufficient.
func TestAuditChangedSourceInvalidatesDone(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.db"), "bucket")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	original := Object{Key: "key", Size: 4, ETag: "old", LastModified: time.Unix(1, 0)}
	if err := st.BatchUpsertScanned([]Object{original}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkStatus(original.Key, StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	changed := Object{Key: "key", Size: 8, ETag: "new", LastModified: time.Unix(2, 0)}
	if err := st.BatchUpsertScanned([]Object{changed}); err != nil {
		return // Explicitly rejecting a changed completed object is also safe.
	}
	c, err := st.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if c.Done != 0 {
		t.Fatalf("changed source is still reported done with stale inventory: %+v", c)
	}
}

func TestAuditPendingAndFailedSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(path, "bucket")
	if err != nil {
		t.Fatal(err)
	}
	objects := []Object{
		{Key: "a", Size: 10, ETag: "first", LastModified: time.Unix(1, 0)},
		{Key: "b", Size: 20, ETag: "second", LastModified: time.Unix(2, 0)},
	}
	if err := st.BatchUpsertScanned(objects); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkStatus("b", StatusFailed, "interrupted upload"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path, "bucket")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	batch, err := st.BatchPendingAfter("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 2 || batch[0].Key != "a" || batch[1].Key != "b" || batch[1].Status != StatusFailed || batch[1].Size != 20 || batch[1].ETag != "second" {
		t.Fatalf("reopened work differs from committed work: %+v", batch)
	}
}
