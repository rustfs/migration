package cmd

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rustfs/migration/internal/fakes3"
)

func TestStrictMigrationRejectsExistingConflictOnResume(t *testing.T) {
	source := fakes3.NewWithObjects(t, "photos", map[string][]byte{"key": []byte("source")}, time.Now())
	defer source.Close()
	target := fakes3.NewWithObjects(t, "photos", map[string][]byte{"key": []byte("target")}, time.Now())
	defer target.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeClusterConfig(t, path, source.URL, target.URL, filepath.Join(dir, "state.db"), filepath.Join(dir, "conflict.log"), dir, "bucket: photos\nworkers: 1\n")
	for _, command := range []string{"run", "migrate"} {
		_, err := execRoot(t, command, "-c", path, "--strict")
		if err == nil || !strings.Contains(err.Error(), "failed buckets") {
			t.Fatalf("%s --strict should fail for conflict, got %v", command, err)
		}
	}
	got, _ := target.Object("photos", "key")
	if string(got) != "target" {
		t.Fatalf("strict mode overwrote target: %q", got)
	}
}

func TestStrictDiffRejectsTargetOnlyObjects(t *testing.T) {
	source := fakes3.NewWithObjects(t, "photos", map[string][]byte{"same": []byte("body")}, time.Now())
	defer source.Close()
	target := fakes3.NewWithObjects(t, "photos", map[string][]byte{"same": []byte("body"), "extra": []byte("extra")}, time.Now())
	defer target.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeClusterConfig(t, path, source.URL, target.URL, filepath.Join(dir, "state.db"), filepath.Join(dir, "conflict.log"), dir, "bucket: photos\nworkers: 1\n")
	if _, err := execRoot(t, "diff", "-c", path, "--strict"); err == nil {
		t.Fatal("strict diff accepted a target-only object")
	}
}
