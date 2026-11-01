package cmd

import (
	"bytes"
	"testing"
)

func TestCobraLeavesErrorOutputToSafeBoundary(t *testing.T) {
	root := NewRoot()
	var output bytes.Buffer
	root.SetErr(&output)
	root.SetOut(&output)
	root.SetArgs([]string{"--definitely-invalid\nforged"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected flag error")
	}
	if output.Len() != 0 {
		t.Fatalf("Cobra printed unescaped error: %q", output.String())
	}
}
