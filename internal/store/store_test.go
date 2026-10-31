package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenRejectsBucketSwap(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "migration.db")

	st, err := Open(dbPath, "bucket-A")
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st, err = Open(dbPath, "bucket-A")
	if err != nil {
		t.Fatalf("reopen same bucket: %v", err)
	}
	_ = st.Close()

	if _, err := Open(dbPath, "bucket-B"); err == nil {
		t.Fatalf("expected error when reusing db_path with a different bucket")
	} else if !strings.Contains(err.Error(), "bucket-A") || !strings.Contains(err.Error(), "bucket-B") {
		t.Fatalf("error %q should mention both bucket names", err)
	}
}

func TestOpenRejectsEmptyBucket(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "x.db"), ""); err == nil {
		t.Fatalf("expected error for empty bucket")
	}
}

func TestUpsertNeverResurrectsDone(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"), "photos")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	obj := Object{Key: "k", Size: 1, LastModified: time.Unix(1, 0).UTC(), ETag: "a"}
	if err := st.BatchUpsertScanned([]Object{obj}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkStatus("k", StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	obj.Size = 2
	if err := st.BatchUpsertScanned([]Object{obj}); err != nil {
		t.Fatal(err)
	}
	c, err := st.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if c.Total != 1 || c.Done != 1 || c.Pending != 0 {
		t.Fatalf("done row resurrected: %+v", c)
	}
}

func TestSkippedResurrectedOnRescan(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"), "photos")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	obj := Object{Key: "k", Size: 1, LastModified: time.Unix(1, 0).UTC(), ETag: "a"}
	if err := st.BatchUpsertScanned([]Object{obj}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkStatus("k", StatusSkipped, "source 404"); err != nil {
		t.Fatal(err)
	}
	obj.Size = 9
	obj.ETag = "b"
	if err := st.BatchUpsertScanned([]Object{obj}); err != nil {
		t.Fatal(err)
	}
	c, err := st.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if c.Pending != 1 || c.Skipped != 0 {
		t.Fatalf("skipped should return to pending: %+v", c)
	}
}

func TestKeysetPaginationWalksAllPending(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"), "photos")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var objs []Object
	for i := 0; i < 7; i++ {
		objs = append(objs, Object{Key: "key-" + itoa2(i), Size: int64(i), LastModified: time.Unix(0, 0).UTC()})
	}
	if err := st.BatchUpsertScanned(objs); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkStatus("key-00", StatusDone, ""); err != nil {
		t.Fatal(err)
	}

	var seen []string
	after := ""
	for {
		batch, err := st.BatchPendingAfter(after, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch) == 0 {
			break
		}
		after = batch[len(batch)-1].Key
		for _, o := range batch {
			seen = append(seen, o.Key)
		}
	}
	want := []string{"key-01", "key-02", "key-03", "key-04", "key-05", "key-06"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("seen=%v want=%v", seen, want)
	}
}

func TestOpenExistingReadsBoundBucket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	st, err := Open(path, "photos")
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	ex, err := OpenExisting(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()
	name, err := ex.BoundBucket()
	if err != nil || name != "photos" {
		t.Fatalf("bound=%q err=%v", name, err)
	}
}

func itoa2(i int) string {
	return string([]byte{'0' + byte(i/10), '0' + byte(i%10)})
}

func TestLegacyInventoryBindsEndpointsWithoutLosingProgress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state#1.db")
	st, err := Open(path, "photos")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`DELETE FROM meta WHERE key != 'bucket'`); err != nil {
		t.Fatal(err)
	}
	if err := st.BatchUpsertScanned([]Object{{Key: "old"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkStatus("old", StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = OpenInventory(path, "photos")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.BindEndpoints("http://source", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.BindEndpoints("http://source", "http://target"); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"http://other", "http://target"}, {"http://source", "http://other"}} {
		if err := st.BindEndpoints(pair[0], pair[1]); err == nil {
			t.Fatalf("accepted changed endpoint in %v", pair)
		}
	}
	counts, err := st.Counts()
	if err != nil || counts.Done != 1 {
		t.Fatalf("lost existing state: %+v err=%v", counts, err)
	}
}

func TestReadOnlyStatusCannotCreateOrModifyInventory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	if _, err := OpenExisting(path); err == nil {
		t.Fatal("opened missing database")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("created missing database: %v", err)
	}
	db, err := Open(path, "photos")
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = OpenExisting(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SetScanComplete(true); err == nil {
		t.Fatal("status handle allowed a database write")
	}
}
