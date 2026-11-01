package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMigrationRequiresExplicitFrozenSource(t *testing.T) {
	c := &Config{Old: Endpoint{Endpoint: "https://source.example", AccessKey: "a", SecretKey: "b"}, New: Endpoint{Endpoint: "https://target.example", AccessKey: "a", SecretKey: "b"}}
	if err := c.ValidateMigration(); err == nil {
		t.Fatal("unfrozen migration accepted")
	}
	c.SourceFrozen = true
	if err := c.ValidateMigration(); err != nil {
		t.Fatal(err)
	}
	c.New.Endpoint = c.Old.Endpoint
	if err := c.ValidateMigration(); err == nil {
		t.Fatal("same cluster accepted")
	}
}

func TestConfigRejectsAdditionalDocument(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("bucket: first\n---\nbucket: second\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("additional YAML document accepted")
	}
}

func TestJobPathsPreserveDistinctBasenames(t *testing.T) {
	a, b := Config{DBPath: "/state/job-a.db"}, Config{DBPath: "/state/job-b.db"}
	if a.DBPathFor("photos") == b.DBPathFor("photos") {
		t.Fatal("distinct jobs share state")
	}
	if a.DBPathFor("photos") != "/state/job-a-photos.db" {
		t.Fatal(a.DBPathFor("photos"))
	}
}

func TestUnsafeBucketAndEndpointRejected(t *testing.T) {
	for _, name := range []string{"..", "a\\b", "a\nb", "a/b", "a\x00b"} {
		if err := ValidateBucketName(name); err == nil {
			t.Fatalf("accepted bucket %q", name)
		}
	}
	for _, endpoint := range []string{"file:///tmp/data", "https://user:password@host", "https://host?secret=key", "host:9000"} {
		if err := validateEndpoint(endpoint); err == nil {
			t.Fatalf("accepted unsafe endpoint")
		}
	}
}
