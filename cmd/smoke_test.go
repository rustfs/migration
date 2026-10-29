package cmd

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rustfs/migration/internal/fakes3"
)

// TestSmokeBinaryCLI builds the real migration binary and drives list, scan,
// run, status, diff, and migrate against the in-process S3 stand-in.
// Workers and --rate-limit are passed on the command line.
func TestSmokeBinaryCLI(t *testing.T) {
	bin := buildSmokeBinary(t)
	t.Run("help", func(t *testing.T) { smokeHelp(t, bin) })
	t.Run("lifecycle", func(t *testing.T) { smokeLifecycle(t, bin) })
	t.Run("nooverwrite", func(t *testing.T) { smokeNoOverwrite(t, bin) })
}

func smokeHelp(t *testing.T, bin string) {
	t.Helper()
	for _, args := range [][]string{
		{"--help"},
		{"run", "--help"},
		{"diff", "--help"},
		{"list", "--help"},
		{"scan", "--help"},
		{"migrate", "--help"},
		{"status", "--help"},
		{"--workers", "2", "--rate-limit", "20M", "--help"},
	} {
		stdout, stderr, code := runBin(t, bin, args...)
		if code != 0 {
			t.Fatalf("%v exit %d\nstdout:\n%s\nstderr:\n%s", args, code, stdout, stderr)
		}
	}
	stdout, _, code := runBin(t, bin, "--help")
	if code != 0 {
		t.Fatalf("help exit %d", code)
	}
	for _, want := range []string{"--workers", "--rate-limit", "20M", "20MiB", "永不覆盖", "Never overwrites"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("help missing %q\n%s", want, stdout)
		}
	}
}

func smokeLifecycle(t *testing.T, bin string) {
	t.Helper()
	lm := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	oldS3 := fakes3.NewWithObjects(t, "photos", map[string][]byte{
		"a.txt": []byte("alpha"),
		"b.txt": []byte("bravo"),
	}, lm)
	defer oldS3.Close()
	newS3 := fakes3.New(t)
	defer newS3.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeClusterConfig(t, cfgPath, oldS3.URL, newS3.URL, filepath.Join(dir, "migration.db"), filepath.Join(dir, "conflict.log"), dir, `
bucket: photos
workers: 1
multipart_concurrency: 1
multipart_threshold: 1048576
rate_limit: 0
`)

	stdout, stderr, code := runBin(t, bin, "list", "-c", cfgPath)
	if code != 0 || !strings.Contains(stdout, "photos") {
		t.Fatalf("list exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	// A pinned bucket makes status open (and create) that db before any scan.
	stdout, stderr, code = runBin(t, bin, "status", "-c", cfgPath)
	if code != 0 {
		t.Fatalf("status before scan exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	fields := statusRow(t, stdout)
	if fields[1] != "0" || fields[3] != "0" {
		t.Fatalf("status before scan: %q", fields)
	}

	if _, stderr, code = runBin(t, bin, "scan", "-c", cfgPath); code != 0 {
		t.Fatalf("scan exit %d\n%s", code, stderr)
	}
	stdout, stderr, code = runBin(t, bin, "status", "-c", cfgPath)
	if code != 0 {
		t.Fatalf("status after scan exit %d\n%s\n%s", code, stdout, stderr)
	}
	fields = statusRow(t, stdout)
	if fields[1] != "2" || fields[2] != "2" || fields[3] != "0" {
		t.Fatalf("status after scan: %q", fields)
	}

	stdout, stderr, code = runBin(t, bin, "run", "-c", cfgPath, "--workers", "2", "--rate-limit", "20M")
	if code != 0 {
		t.Fatalf("run exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "workers=2") || !strings.Contains(stderr, "rate_limit=20MB/s") {
		t.Fatalf("run did not apply workers and rate limit:\n%s", stderr)
	}
	for key, want := range map[string]string{"a.txt": "alpha", "b.txt": "bravo"} {
		body, ok := newS3.Object("photos", key)
		if !ok || string(body) != want {
			t.Fatalf("%s: ok=%v body=%q", key, ok, body)
		}
		if calls := newS3.PutObjectCalls(key); calls != 1 {
			t.Fatalf("%s put calls=%d", key, calls)
		}
	}
	if oldS3.DeleteObjectCalls() != 0 || newS3.DeleteObjectCalls() != 0 || oldS3.CopyObjectCalls() != 0 || newS3.CopyObjectCalls() != 0 {
		t.Fatalf("unsafe calls during run: oldDelete=%d newDelete=%d oldCopy=%d newCopy=%d",
			oldS3.DeleteObjectCalls(), newS3.DeleteObjectCalls(), oldS3.CopyObjectCalls(), newS3.CopyObjectCalls())
	}

	stdout, stderr, code = runBin(t, bin, "status", "-c", cfgPath)
	if code != 0 {
		t.Fatalf("status after run exit %d\n%s", code, stderr)
	}
	fields = statusRow(t, stdout)
	if fields[1] != "2" || fields[2] != "0" || fields[3] != "2" || fields[6] != "0" {
		t.Fatalf("status after run: %q", fields)
	}

	stdout, stderr, code = runBin(t, bin, "diff", "-c", cfgPath, "--workers", "2", "--rate-limit", "20M")
	if code != 0 || !strings.Contains(stdout, "ok=2") || !strings.Contains(stdout, "missing=0") {
		t.Fatalf("diff exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	if _, stderr, code = runBin(t, bin, "migrate", "-c", cfgPath, "--workers", "2", "--rate-limit", "20M"); code != 0 {
		t.Fatalf("second migrate exit %d\n%s", code, stderr)
	}
	if calls := newS3.PutObjectCalls("a.txt"); calls != 1 {
		t.Fatalf("second migrate recopied a.txt, put calls=%d", calls)
	}

	_, stderr, code = runBin(t, bin, "status", "-c", cfgPath, "--workers", "0")
	if code == 0 || !strings.Contains(stderr, "workers must be >= 1") {
		t.Fatalf("workers 0 exit %d\n%s", code, stderr)
	}
	_, stderr, code = runBin(t, bin, "status", "-c", cfgPath, "--rate-limit", "nope")
	if code == 0 || !strings.Contains(stderr, "rate limit") {
		t.Fatalf("bad rate-limit exit %d\n%s", code, stderr)
	}
}

func smokeNoOverwrite(t *testing.T, bin string) {
	t.Helper()
	lm := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	oldS3 := fakes3.NewWithObjects(t, "photos", map[string][]byte{
		"a.txt":        []byte("alpha"),
		"conflict.txt": []byte("source-body"),
	}, lm)
	defer oldS3.Close()
	newS3 := fakes3.NewWithObjects(t, "photos", map[string][]byte{
		"conflict.txt": []byte("target-must-stay"),
	}, lm.Add(time.Hour))
	defer newS3.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeClusterConfig(t, cfgPath, oldS3.URL, newS3.URL, filepath.Join(dir, "migration.db"), filepath.Join(dir, "conflict.log"), dir, `
bucket: photos
workers: 2
rate_limit: 20M
`)

	stdout, stderr, code := runBin(t, bin, "run", "-c", cfgPath, "--workers", "2", "--rate-limit", "20M")
	if code != 0 {
		t.Fatalf("run exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	kept, ok := newS3.Object("photos", "conflict.txt")
	if !ok || string(kept) != "target-must-stay" {
		t.Fatalf("conflict overwritten: ok=%v body=%q", ok, kept)
	}
	if calls := newS3.PutObjectCalls("conflict.txt"); calls != 0 {
		t.Fatalf("conflict put calls=%d", calls)
	}
	body, ok := newS3.Object("photos", "a.txt")
	if !ok || string(body) != "alpha" {
		t.Fatalf("a.txt: ok=%v body=%q", ok, body)
	}
	if oldS3.DeleteObjectCalls() != 0 || newS3.DeleteObjectCalls() != 0 || newS3.CopyObjectCalls() != 0 {
		t.Fatalf("unsafe calls: oldDelete=%d newDelete=%d newCopy=%d",
			oldS3.DeleteObjectCalls(), newS3.DeleteObjectCalls(), newS3.CopyObjectCalls())
	}

	stdout, stderr, code = runBin(t, bin, "diff", "-c", cfgPath, "--workers", "2")
	if code == 0 || !strings.Contains(stderr, "diff found gaps") {
		t.Fatalf("diff should report the conflict, exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "conflict.txt") {
		t.Fatalf("diff report:\n%s", stdout)
	}
}

func buildSmokeBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "migration")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = moduleRoot(t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build smoke binary: %v\n%s", err, out)
	}
	return bin
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func runBin(t *testing.T, bin string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stdout.String(), stderr.String(), exitErr.ExitCode()
	}
	t.Fatalf("run %s %v: %v\n%s", bin, args, err, stderr.String())
	return "", "", -1
}

func statusRow(t *testing.T, out string) []string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 7 && fields[0] == "photos" {
			return fields
		}
	}
	t.Fatalf("no photos status row:\n%s", out)
	return nil
}
