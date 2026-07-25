package main

import (
	"bytes"
	"strings"
	"testing"
)

// M-04: help is an informational success — and it carries worked
// examples and the exit-code cheat sheet, not just the flag list.
func TestHelpExitsZero(t *testing.T) {
	for _, flag := range []string{"-h", "-help"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{flag}, &stdout, &stderr); code != 0 {
			t.Errorf("%s: exit %d want 0", flag, code)
		}
		usage := stderr.String()
		for _, want := range []string{
			"-bucket",
			"-version",
			"Examples:",
			"-cluster-name hbase-prod",
			"-discover-apps",
			"Exit codes:",
			"130 interrupted",
		} {
			if !strings.Contains(usage, want) {
				t.Errorf("%s: usage missing %q", flag, want)
			}
		}
	}
}

func TestInvalidFlagExitsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-no-such-flag"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit %d want 2", code)
	}
}

func TestValidationErrorExitsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-bucket", "b", "-prefix", "p", "-workers", "0"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit %d want 2", code)
	}
	if !strings.Contains(stderr.String(), "-workers") {
		t.Fatalf("stderr: %s", stderr.String())
	}
}

// -version needs no AWS configuration and exits 0.
func TestVersionExitsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d want 0", code)
	}
	if !strings.Contains(stdout.String(), "s3logscan") {
		t.Fatalf("stdout: %q", stdout.String())
	}
}
