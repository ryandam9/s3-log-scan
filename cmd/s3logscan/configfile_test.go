package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Values from the config file reach validation exactly as if typed:
// an invalid file-provided value fails with the flag's own error.
func TestConfigFileValuesApplied(t *testing.T) {
	p := writeTempConfig(t, "bucket = b\nprefix = logs/\nmax-line-size = 999\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-config", p}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "-max-line-size") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}

// A CLI flag beats the same flag in the config file: the file's bad
// workers value is ignored because the CLI provided one; the CLI's
// bad value is what validation sees.
func TestConfigFileCLIPriority(t *testing.T) {
	p := writeTempConfig(t, "bucket = b\nprefix = logs/\nworkers = 999\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-config", p, "-workers", "0"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "got 0") {
		t.Fatalf("CLI value must win over the file: %q", stderr.String())
	}
}

func TestConfigFileUnknownKeyFails(t *testing.T) {
	p := writeTempConfig(t, "no-such = 1\n")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-config", p}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "unknown option") {
		t.Fatalf("stderr %q", stderr.String())
	}
}

func TestConfigFileExplicitMissingFails(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-config", filepath.Join(t.TempDir(), "nope")}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "reading config file") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}

// group=false in the config file counts as an explicit choice for the
// group auto-default (it must not be overridden by TTY detection; in
// tests stdout is not a TTY anyway, so here we just prove the value
// parses and the run proceeds to ordinary validation).
func TestConfigFileGroupParsed(t *testing.T) {
	p := writeTempConfig(t, "group = true\ngrep = ERROR\nbucket = b\nprefix = logs/\nworkers = 0\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-config", p}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "-workers") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}
