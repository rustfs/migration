package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/rustfs/migration/internal/config"
)

func writeTestConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := []byte(`
old:
  endpoint: "http://127.0.0.1:9"
  access_key: a
  secret_key: b
new:
  endpoint: "http://127.0.0.1:8"
  access_key: c
  secret_key: d
workers: 4
multipart_concurrency: 2
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func withStatusConfig(t *testing.T, args []string) (*config.Config, error) {
	t.Helper()
	root := NewRoot()
	var got *config.Config
	status, _, err := root.Find([]string{"status"})
	if err != nil {
		t.Fatal(err)
	}
	status.RunE = func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig(cmd)
		if err != nil {
			return err
		}
		got = cfg
		return nil
	}
	buf := new(bytes.Buffer)
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs(args)
	err = root.Execute()
	return got, err
}

func TestExplicitEmptyBucketSelectsAll(t *testing.T) {
	t.Setenv("BUCKET", "photos")
	path := writeTestConfig(t)
	got, err := withStatusConfig(t, []string{"status", "-c", path, "--bucket", ""})
	if err != nil || got == nil || !got.AllBuckets() {
		t.Fatalf("empty bucket flag must override BUCKET: config=%+v err=%v", got, err)
	}
}

func TestCLIOverridesConcurrency(t *testing.T) {
	path := writeTestConfig(t)
	got, err := withStatusConfig(t, []string{"status", "-c", path, "--workers", "9", "--multipart-concurrency", "5"})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Workers != 9 || got.MultipartConcurrency != 5 {
		t.Fatalf("cli override: %+v", got)
	}
}

func TestCLIKeepsYAMLConcurrencyWhenUnset(t *testing.T) {
	path := writeTestConfig(t)
	got, err := withStatusConfig(t, []string{"status", "-c", path})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Workers != 4 || got.MultipartConcurrency != 2 {
		t.Fatalf("yaml concurrency: %+v", got)
	}
}

func TestCLIRejectsNonPositiveWorkers(t *testing.T) {
	path := writeTestConfig(t)
	root := NewRoot()
	buf := new(bytes.Buffer)
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs([]string{"status", "-c", path, "--workers", "0"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "workers must be >= 1") {
		t.Fatalf("got %v", err)
	}
}

func TestCLIRejectsNonPositiveMultipart(t *testing.T) {
	path := writeTestConfig(t)
	root := NewRoot()
	buf := new(bytes.Buffer)
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs([]string{"status", "-c", path, "--multipart-concurrency", "-3"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "multipart_concurrency must be >= 1") {
		t.Fatalf("got %v", err)
	}
}

func TestConcurrencyResolutionPriority(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		envW    string
		envP    string
		extra   []string
		wantW   int
		wantP   int
		wantErr string
	}{
		{name: "omit uses defaults", wantW: 16, wantP: 4},
		{name: "yaml zero uses defaults", yaml: "workers: 0\nmultipart_concurrency: 0\n", wantW: 16, wantP: 4},
		{name: "yaml kept when flags unset", yaml: "workers: 5\nmultipart_concurrency: 3\n", wantW: 5, wantP: 3},
		{name: "env overrides yaml", yaml: "workers: 5\nmultipart_concurrency: 3\n", envW: "8", envP: "6", wantW: 8, wantP: 6},
		{name: "cli overrides env and yaml", yaml: "workers: 5\nmultipart_concurrency: 3\n", envW: "8", envP: "6", extra: []string{"--workers", "2", "--multipart-concurrency", "1"}, wantW: 2, wantP: 1},
		{name: "cli workers only keeps env multipart", yaml: "workers: 5\nmultipart_concurrency: 3\n", envW: "8", envP: "6", extra: []string{"--workers", "2"}, wantW: 2, wantP: 6},
		{name: "cli zero is an error", yaml: "workers: 5\n", extra: []string{"--workers", "0"}, wantErr: "workers must be >= 1"},
		{name: "cli negative multipart is an error", extra: []string{"--multipart-concurrency", "-2"}, wantErr: "multipart_concurrency must be >= 1"},
		{name: "env zero is an error", yaml: "workers: 5\n", envW: "0", wantErr: "WORKERS must be >= 1"},
		{name: "env multipart zero is an error", envP: "0", wantErr: "MULTIPART_CONCURRENCY must be >= 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envW != "" {
				t.Setenv("WORKERS", tc.envW)
			}
			if tc.envP != "" {
				t.Setenv("MULTIPART_CONCURRENCY", tc.envP)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			body := "old: {endpoint: \"http://127.0.0.1:9\", access_key: a, secret_key: b}\n" + tc.yaml
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			args := append([]string{"status", "-c", path}, tc.extra...)
			got, err := withStatusConfig(t, args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Workers != tc.wantW || got.MultipartConcurrency != tc.wantP {
				t.Fatalf("workers=%d multipart=%d, want %d and %d", got.Workers, got.MultipartConcurrency, tc.wantW, tc.wantP)
			}
		})
	}
}

func TestEnvWorkersThenCLIWins(t *testing.T) {
	t.Setenv("WORKERS", "6")
	t.Setenv("MULTIPART_CONCURRENCY", "7")
	path := writeTestConfig(t)
	got, err := withStatusConfig(t, []string{"status", "-c", path, "--workers", "11"})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Workers != 11 || got.MultipartConcurrency != 7 {
		t.Fatalf("priority: %+v", got)
	}
}
