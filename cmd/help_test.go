package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestRootHelpBilingual(t *testing.T) {
	root := NewRoot()
	buf := new(bytes.Buffer)
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"migration",
		"Never overwrites",
		"永不覆盖",
		"Quick start",
		"快速开始",
		"run",
		"scan",
		"diff",
		"list",
		"status",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("help missing %q\n%s", want, out)
		}
	}
}

func TestSubcommandHelpBilingual(t *testing.T) {
	for _, sub := range []string{"run", "scan", "migrate", "diff", "list", "status"} {
		root := NewRoot()
		buf := new(bytes.Buffer)
		root.SetOut(buf)
		root.SetErr(buf)
		root.SetArgs([]string{sub, "--help"})
		if err := root.Execute(); err != nil {
			t.Fatalf("%s --help: %v", sub, err)
		}
		out := buf.String()
		if !strings.Contains(out, "/") && !containsHan(out) {
			t.Fatalf("%s help should include Chinese:\n%s", sub, out)
		}
	}
}

func containsHan(s string) bool {
	for _, r := range s {
		if r >= 0x4e00 && r <= 0x9fff {
			return true
		}
	}
	return false
}

func TestVersion(t *testing.T) {
	root := NewRoot()
	buf := new(bytes.Buffer)
	root.SetOut(buf)
	root.SetArgs([]string{"--version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), Version) {
		t.Fatalf("version output: %s", buf.String())
	}
}
