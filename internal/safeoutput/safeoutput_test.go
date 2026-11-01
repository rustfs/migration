package safeoutput

import "testing"

func TestEscape(t *testing.T) {
	got := Escape("服务 \"x\"\nforged\x1b[31m\r\t\u202e")
	want := "服务 \"x\"\\nforged\\x1b[31m\\r\\t\\u202e"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
