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
