package scan

import (
	"bytes"
	"testing"
)

func TestSanitizeControlChars(t *testing.T) {
	in := []byte("a\x1b[31mred\x00b\tc")
	out := Sanitize(in)
	want := "a?[31mred?b\tc"
	if string(out) != want {
		t.Fatalf("got %q want %q", out, want)
	}
	// Input must not be mutated.
	if !bytes.Contains(in, []byte{0x1b}) {
		t.Fatal("Sanitize mutated its input")
	}
}

func TestSanitizeCleanPassthrough(t *testing.T) {
	in := []byte("plain text with tab\tand unicode: héllo")
	if out := Sanitize(in); &out[0] != &in[0] {
		t.Fatal("clean input should be returned without copying")
	}
}

func TestSanitizeString(t *testing.T) {
	if got := SanitizeString("key\rwith\ncontrols"); got != "key?with?controls" {
		t.Fatalf("got %q", got)
	}
	if got := SanitizeString("clean-key.gz"); got != "clean-key.gz" {
		t.Fatalf("got %q", got)
	}
}

func TestSanitizeDEL(t *testing.T) {
	if got := Sanitize([]byte{'a', 0x7f, 'b'}); string(got) != "a?b" {
		t.Fatalf("DEL not sanitized: %q", got)
	}
}
