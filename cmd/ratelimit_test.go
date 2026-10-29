package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRateLimitPriority(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		env     string
		extra   []string
		want    int64
		wantErr string
	}{
		{name: "omit is unlimited", want: 0},
		{name: "yaml zero is unlimited", yaml: "rate_limit: 0\n", want: 0},
		{name: "yaml 20M", yaml: "rate_limit: 20M\n", want: 20 * 1000 * 1000},
		{name: "yaml quoted 20MiB", yaml: "rate_limit: \"20MiB\"\n", want: 20 * 1024 * 1024},
		{name: "env overrides yaml", yaml: "rate_limit: 20M\n", env: "30M", want: 30 * 1000 * 1000},
		{name: "env zero clears yaml", yaml: "rate_limit: 50M\n", env: "0", want: 0},
		{name: "cli overrides env and yaml", yaml: "rate_limit: 20M\n", env: "30M", extra: []string{"--rate-limit", "50M"}, want: 50 * 1000 * 1000},
		{name: "cli zero clears env", yaml: "rate_limit: 20M\n", env: "30M", extra: []string{"--rate-limit", "0"}, want: 0},
		{name: "cli MiB", extra: []string{"--rate-limit", "20MiB"}, want: 20 * 1024 * 1024},
		{name: "cli workers stay independent", yaml: "workers: 5\nrate_limit: 20M\n", extra: []string{"--workers", "2"}, want: 20 * 1000 * 1000},
		{name: "bad yaml suffix", yaml: "rate_limit: 20X\n", wantErr: "unknown size suffix"},
		{name: "bad env", yaml: "rate_limit: 20M\n", env: "fast", wantErr: "RATE_LIMIT"},
		{name: "bad cli", extra: []string{"--rate-limit", "-5"}, wantErr: "must be >= 0"},
		{name: "cli empty", extra: []string{"--rate-limit", ""}, wantErr: "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Empty unsets a value inherited from the process environment.
			// applyByteRateEnv treats an empty variable as "not set".
			t.Setenv("RATE_LIMIT", tc.env)
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
			if int64(got.RateLimit) != tc.want {
				t.Fatalf("rate=%d (%s), want %d", int64(got.RateLimit), got.RateLimit, tc.want)
			}
			if tc.name == "cli workers stay independent" && got.Workers != 2 {
				t.Fatalf("workers=%d", got.Workers)
			}
		})
	}
}
