package main

import (
	"bytes"
	"testing"
)

func TestResolveColor(t *testing.T) {
	var buf bytes.Buffer // not a terminal

	if resolveColor("always", &buf) != true {
		t.Fatal("always must force color even without a TTY")
	}
	if resolveColor("never", &buf) != false {
		t.Fatal("never must disable color")
	}
	// auto on a non-TTY writer: off.
	if resolveColor("auto", &buf) != false {
		t.Fatal("auto must disable color when stdout is not a terminal")
	}
}

func TestResolveColorHonorsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var buf bytes.Buffer
	if resolveColor("auto", &buf) {
		t.Fatal("NO_COLOR must disable auto color")
	}
	if !resolveColor("always", &buf) {
		t.Fatal("explicit always overrides NO_COLOR")
	}
}

func TestBadColorValueExitsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-bucket", "b", "-prefix", "p", "-color", "sometimes"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit %d want 2", code)
	}
}
