package cmd

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rustfs/migration/internal/config"
	"github.com/rustfs/migration/internal/fakes3"
	"github.com/rustfs/migration/internal/s3client"
	"github.com/rustfs/migration/internal/store"
)

// A permanently denied source read leaves the destination incomplete. The
// real CLI must return an error so a shell migration gate cannot approve it.
func TestAuditRunReportsObjectFailure(t *testing.T) {
	source := fakes3.NewWithObjects(t, "photos", map[string][]byte{"key": []byte("body")}, time.Now())
	defer source.Close()
	deniedSource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/photos/key" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		source.Config.Handler.ServeHTTP(w, r)
	}))
	defer deniedSource.Close()
	target := fakes3.New(t)
	defer target.Close()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "migration.db")
	configPath := filepath.Join(dir, "config.yaml")
	writeClusterConfig(t, configPath, deniedSource.URL, target.URL, dbPath, filepath.Join(dir, "conflict.log"), dir, "bucket: photos\nworkers: 1\n")
	_, runErr := execRoot(t, "run", "-c", configPath)
	st, err := store.Open(dbPath, "photos")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	counts, err := st.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if counts.Failed != 1 || counts.Done != 0 {
		t.Fatalf("fixture must leave one failed object: %+v", counts)
	}
	if _, exists := target.Object("photos", "key"); exists {
		t.Fatal("fixture unexpectedly copied the denied object")
	}
	if runErr == nil {
		t.Fatalf("run returned success with a missing target object and failed=%d", counts.Failed)
	}
}

func TestAuditStatusReportsCorruptState(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "migration.db")
	if err := os.WriteFile(dbPath, []byte("this is not a SQLite database"), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	writeClusterConfig(t, configPath, "http://unused-source", "http://unused-target", dbPath, filepath.Join(dir, "conflict.log"), dir, "bucket: photos\n")
	if _, err := execRoot(t, "status", "-c", configPath); err == nil {
		t.Fatal("status returned success although its only state DB could not be read")
	}
}

func TestAuditMigrateRequiresScannedInventory(t *testing.T) {
	source := fakes3.NewWithObjects(t, "photos", map[string][]byte{"key": []byte("body")}, time.Now())
	defer source.Close()
	target := fakes3.New(t)
	defer target.Close()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	writeClusterConfig(t, configPath, source.URL, target.URL, filepath.Join(dir, "never-scanned.db"), filepath.Join(dir, "conflict.log"), dir, "bucket: photos\nworkers: 1\n")
	_, err := execRoot(t, "migrate", "-c", configPath)
	if _, exists := target.Object("photos", "key"); exists {
		t.Fatal("fixture unexpectedly migrated unscanned object")
	}
	if err == nil {
		t.Fatal("migrate silently created a fresh empty inventory and returned success without copying any source keys")
	}
}

func TestAuditMigrateReturnsCancellation(t *testing.T) {
	source := fakes3.NewWithObjects(t, "photos", map[string][]byte{"key": []byte("body")}, time.Now())
	defer source.Close()
	started := make(chan struct{})
	source.BlockGetsUntilCancel(started)
	target := fakes3.New(t)
	defer target.Close()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	writeClusterConfig(t, configPath, source.URL, target.URL, filepath.Join(dir, "migration.db"), filepath.Join(dir, "conflict.log"), dir, "bucket: photos\nworkers: 1\n")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	oldCli, err := s3client.New(ctx, cfg.Old)
	if err != nil {
		t.Fatal(err)
	}
	newCli, err := s3client.New(ctx, cfg.New)
	if err != nil {
		t.Fatal(err)
	}
	if err := scanOne(ctx, cfg, oldCli, "photos"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- migrateOne(ctx, cfg, oldCli, newCli, "photos") }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("copy did not reach blocked GET")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("migration returned success after cancellation with an incomplete object")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("migration did not return after cancellation")
	}
}

// Unlike the existing 40 ms sleep assertion, this barrier waits for the second
// part to reach UploadPart. It checks real parallelism without assuming a
// 5 MiB download and race-instrumented hashing complete in less than 40 ms.
func TestAuditMultipartConcurrencyWithBarrier(t *testing.T) {
	const partSize = 5 * 1024 * 1024
	body := bytes.Repeat([]byte("z"), partSize+32)
	source := fakes3.NewWithObjects(t, "photos", map[string][]byte{"large.bin": body}, time.Now())
	defer source.Close()
	target := fakes3.New(t)
	defer target.Close()
	var started atomic.Int32
	both := make(chan struct{})
	barrierTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Query().Get("partNumber") != "" {
			if started.Add(1) == 2 {
				close(both)
			}
			select {
			case <-both:
			case <-r.Context().Done():
				return
			}
		}
		target.Config.Handler.ServeHTTP(w, r)
	}))
	defer barrierTarget.Close()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	writeClusterConfig(t, configPath, source.URL, barrierTarget.URL, filepath.Join(dir, "migration.db"), filepath.Join(dir, "conflict.log"), dir, "bucket: photos\nworkers: 1\nmultipart_concurrency: 2\n")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MultipartThreshold = partSize
	cfg.MultipartPartSize = partSize
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	oldCli, err := s3client.New(ctx, cfg.Old)
	if err != nil {
		t.Fatal(err)
	}
	newCli, err := s3client.New(ctx, cfg.New)
	if err != nil {
		t.Fatal(err)
	}
	if err := scanOne(ctx, cfg, oldCli, "photos"); err != nil {
		t.Fatal(err)
	}
	if err := migrateOne(ctx, cfg, oldCli, newCli, "photos"); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil || started.Load() != 2 {
		t.Fatalf("expected two simultaneous part uploads: started=%d ctx=%v", started.Load(), ctx.Err())
	}
	copied, ok := target.Object("photos", "large.bin")
	if !ok || !bytes.Equal(copied, body) {
		t.Fatal("multipart fixture was not copied intact")
	}
}

func TestSourceFrozenCLIOverride(t *testing.T) {
	path := writeTestConfig(t)
	for _, value := range []string{"true", "false"} {
		cfg, err := withStatusConfig(t, []string{"status", "-c", path, "--source-frozen=" + value})
		if err != nil || cfg.SourceFrozen != (value == "true") {
			t.Fatalf("--source-frozen=%s: cfg=%+v err=%v", value, cfg, err)
		}
	}
}

func TestWritesRequireFrozenSourceBeforeAnyRemoteRequest(t *testing.T) {
	var requests atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "no remote requests expected", http.StatusForbidden)
	})
	source, target := httptest.NewServer(handler), httptest.NewServer(handler)
	defer source.Close()
	defer target.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeClusterConfig(t, path, source.URL, target.URL, filepath.Join(dir, "migration.db"), filepath.Join(dir, "conflict.log"), dir, "bucket: photos\n")
	for _, command := range []string{"run", "migrate"} {
		_, err := execRoot(t, command, "-c", path, "--source-frozen=false")
		if err == nil || !strings.Contains(err.Error(), "source_frozen") {
			t.Fatalf("unfrozen %s should fail explicitly: %v", command, err)
		}
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("unfrozen migration issued %d remote requests", got)
	}
}

func TestStatusDoesNotCreateMissingInventory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "missing.db")
	writeClusterConfig(t, path, "http://unused-source", "http://unused-target", dbPath, filepath.Join(dir, "conflict.log"), dir, "bucket: photos\n")
	if _, err := execRoot(t, "status", "-c", path); err == nil {
		t.Fatal("status succeeded without inventory")
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("status created a state database: %v", err)
	}
}

func TestStatusTargetsUsesConfiguredBasename(t *testing.T) {
	cfg := &config.Config{DBPath: filepath.Join(t.TempDir(), "custom.db")}
	path := cfg.DBPathFor("photos")
	st, err := store.Open(path, "photos")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	targets, err := statusTargets(cfg)
	if err != nil || len(targets) != 1 || targets[0].path != path || targets[0].bucket != "photos" {
		t.Fatalf("custom state files were not found: targets=%+v err=%v", targets, err)
	}
}

func TestAllBucketStatusRejectsMissingInventory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeClusterConfig(t, path, "http://unused-source", "http://unused-target", filepath.Join(dir, "migration.db"), filepath.Join(dir, "conflict.log"), dir, "")
	if _, err := execRoot(t, "status", "-c", path); err == nil || !strings.Contains(err.Error(), "no state DBs") {
		t.Fatalf("empty all-bucket status must report missing inventory: %v", err)
	}
}
