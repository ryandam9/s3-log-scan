package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFenceFor(t *testing.T) {
	cases := []struct{ content, want string }{
		{"plain text", "```"},
		{"one `tick`", "```"},
		{"has ``` inside", "````"},
		{"has ````` five", "``````"},
	}
	for _, tc := range cases {
		if got := fenceFor(tc.content); got != tc.want {
			t.Errorf("fenceFor(%q) = %q, want %q", tc.content, got, tc.want)
		}
	}
}

func TestWriteMDReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logscan", "application_1_2.md")
	matched := []string{
		"s3://b/logs/j-1/containers/application_1_2/c_01/stderr.gz",
		"s3://b/logs/j-1/containers/application_1_2/c_02/stderr.gz",
	}
	// Screen output as a colored terminal run captures it: ANSI SGR
	// sequences must not survive into the report.
	screen := "s3logscan: scanning s3://b/logs/j-1/containers/application_1_2/\n" +
		"\x1b[35ms3://b/logs/j-1/containers/application_1_2/c_01/stderr.gz\x1b[0m\n" +
		"      44: line with \x1b[1;31mERROR\x1b[0m text\n" +
		"---\ns3logscan: completed in 1.0s\n"
	// A non-UTC zone proves the timestamp renders in the local zone it
	// was produced in (main passes time.Now()), not converted to UTC.
	now := time.Date(2026, 8, 4, 20, 30, 0, 0, time.FixedZone("AEST", 10*3600))
	if err := writeMDReport(path, "application_1_2", "ERROR", []string{"s3://b/logs/j-1/containers/application_1_2/"}, matched, screen, now); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		"# s3logscan — application_1_2",
		"- **Generated**: 2026-08-04 20:30:00 AEST",
		"- **Pattern**: `ERROR`",
		"- **Scanned**: `s3://b/logs/j-1/containers/application_1_2/`",
		"- **Files with matches**: 2",
		"## Files with matches",
		"```sh\ns3://b/logs/j-1/containers/application_1_2/c_01/stderr.gz\ns3://b/logs/j-1/containers/application_1_2/c_02/stderr.gz\n```",
		"## Screen output",
		"      44: line with ERROR text",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b[") {
		t.Error("report contains unstripped ANSI escape sequences")
	}
}

// A run with no matches still writes a well-formed report.
func TestWriteMDReportNoMatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "application_9_9.md")
	err := writeMDReport(path, "application_9_9", "FATAL", []string{"s3://b/p/"}, nil,
		"s3logscan: scanning s3://b/p/\n", time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	got := string(data)
	if !strings.Contains(got, "- **Files with matches**: 0") {
		t.Errorf("missing zero count:\n%s", got)
	}
	if !strings.Contains(got, "```sh\n(none)\n```") {
		t.Errorf("missing (none) placeholder:\n%s", got)
	}
}

// Log content containing a ``` run must not close the fence early.
func TestWriteMDReportFenceEscaping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "application_3_3.md")
	screen := "s3://b/k:1: evil line with ``` fence\n"
	if err := writeMDReport(path, "application_3_3", "fence", []string{"s3://b/p/"}, []string{"s3://b/k"}, screen, time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "````sh\ns3://b/k:1: evil line with ``` fence\n````") {
		t.Errorf("fence not widened:\n%s", string(data))
	}
}

// Reports group under a local-date directory: ~/logscan/<yyyy-mm-dd>/.
func TestReportPathDateDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	now := time.Date(2026, 8, 5, 23, 30, 0, 0, time.FixedZone("AEST", 10*3600))
	got, err := reportPath("application_1_2", now)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "logscan", "2026-08-05", "application_1_2.md")
	if got != want {
		t.Fatalf("reportPath = %q, want %q", got, want)
	}
}
