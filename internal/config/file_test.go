package config

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestApplyFileSetsDefaults(t *testing.T) {
	fs, o := NewFlagSet("test", io.Discard)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	p := writeConfig(t, `
# standing defaults
cluster-name = hbase-prod
grep = ERROR|WARN
i = true
progress = 2s
max-total-matches = 20
`)
	provided, _, err := ApplyFile(fs, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if o.ClusterName != "hbase-prod" || o.GrepPattern != "ERROR|WARN" || !o.IgnoreCase {
		t.Fatalf("values not applied: %+v", o)
	}
	if o.Progress.String() != "2s" || o.MaxTotalMatches != 20 {
		t.Fatalf("typed values not applied: progress=%s cap=%d", o.Progress, o.MaxTotalMatches)
	}
	for _, k := range []string{"cluster-name", "grep", "i", "progress", "max-total-matches"} {
		if !provided[k] {
			t.Errorf("provided missing %q", k)
		}
	}
}

// CLI-set flags are skipped: the file must not overwrite them, but
// they still count as provided.
func TestApplyFileCLIPriority(t *testing.T) {
	fs, o := NewFlagSet("test", io.Discard)
	if err := fs.Parse([]string{"-grep", "FROM_CLI"}); err != nil {
		t.Fatal(err)
	}
	p := writeConfig(t, "grep = FROM_FILE\ncluster-name = hbase-prod\n")
	provided, _, err := ApplyFile(fs, p, func(k string) bool { return k == "grep" })
	if err != nil {
		t.Fatal(err)
	}
	if o.GrepPattern != "FROM_CLI" {
		t.Fatalf("CLI value overwritten: %q", o.GrepPattern)
	}
	if o.ClusterName != "hbase-prod" {
		t.Fatalf("non-CLI value not applied: %q", o.ClusterName)
	}
	if !provided["grep"] {
		t.Fatal("skipped keys must still be reported as provided")
	}
}

func TestApplyFileQuotedValues(t *testing.T) {
	fs, o := NewFlagSet("test", io.Discard)
	fs.Parse(nil)
	p := writeConfig(t, `grep = " ERROR with spaces "`+"\n")
	if _, _, err := ApplyFile(fs, p, nil); err != nil {
		t.Fatal(err)
	}
	if o.GrepPattern != " ERROR with spaces " {
		t.Fatalf("quoted value: %q", o.GrepPattern)
	}
}

func TestApplyFileDashTolerated(t *testing.T) {
	fs, o := NewFlagSet("test", io.Discard)
	fs.Parse(nil)
	p := writeConfig(t, "-cluster-name = hbase\n")
	if _, _, err := ApplyFile(fs, p, nil); err != nil {
		t.Fatal(err)
	}
	if o.ClusterName != "hbase" {
		t.Fatalf("dash-prefixed key not applied: %q", o.ClusterName)
	}
}

func TestApplyFileErrors(t *testing.T) {
	cases := []struct {
		name, content, want string
	}{
		{"unknown option", "no-such-flag = 1\n", "unknown option"},
		{"missing equals", "just a line\n", "expected 'flag = value'"},
		{"bad value", "workers = many\n", "workers"},
		{"reserved config", "config = /elsewhere\n", "cannot be set"},
		{"reserved version", "version = true\n", "cannot be set"},
		{"bad quotes", `grep = "unterminated` + "\n", "bad quoted value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs, _ := NewFlagSet("test", io.Discard)
			fs.Parse(nil)
			p := writeConfig(t, tc.content)
			_, _, err := ApplyFile(fs, p, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want mention of %q", err, tc.want)
			}
			if err != nil && !strings.Contains(err.Error(), ":1:") {
				t.Fatalf("error must carry file:line: %v", err)
			}
		})
	}
}

func TestApplyFileMissing(t *testing.T) {
	fs, _ := NewFlagSet("test", io.Discard)
	fs.Parse(nil)
	if _, _, err := ApplyFile(fs, filepath.Join(t.TempDir(), "absent"), nil); err == nil {
		t.Fatal("missing explicit config file must error")
	}
}

// pattern.<name> lines are collected as named categories, not flags;
// repeated names accumulate in order.
func TestApplyFilePatterns(t *testing.T) {
	fs, o := NewFlagSet("test", io.Discard)
	fs.Parse(nil)
	p := writeConfig(t, `
cluster-name = hbase-prod
pattern.spark = ERROR|Exception
pattern.spark = Caused by
pattern.oom = OutOfMemoryError
`)
	_, patterns, err := ApplyFile(fs, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if o.ClusterName != "hbase-prod" {
		t.Fatalf("flag alongside patterns not applied: %+v", o)
	}
	if len(patterns) != 2 || len(patterns["spark"]) != 2 || patterns["spark"][1] != "Caused by" || patterns["oom"][0] != "OutOfMemoryError" {
		t.Fatalf("patterns = %v", patterns)
	}
}

func TestApplyFilePatternErrors(t *testing.T) {
	cases := []struct{ name, content, want string }{
		{"empty name", "pattern. = x\n", "needs a name"},
		{"empty value", "pattern.spark = \n", "empty pattern"},
		{"bad regex", "pattern.spark = (\n", "pattern.spark"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs, _ := NewFlagSet("test", io.Discard)
			fs.Parse(nil)
			p := writeConfig(t, tc.content)
			_, _, err := ApplyFile(fs, p, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want mention of %q", err, tc.want)
			}
		})
	}
}

func TestResolveCategory(t *testing.T) {
	patterns := map[string][]string{
		"spark": {"ERROR|Exception", "Caused by"},
		"oom":   {"OutOfMemoryError"},
	}
	if got, err := ResolveCategory("oom", patterns); err != nil || got != "OutOfMemoryError" {
		t.Fatalf("single: %q %v", got, err)
	}
	if got, err := ResolveCategory("spark", patterns); err != nil || got != "(?:ERROR|Exception)|(?:Caused by)" {
		t.Fatalf("multi: %q %v", got, err)
	}
	_, err := ResolveCategory("sprk", patterns)
	if err == nil || !strings.Contains(err.Error(), "available: oom, spark") {
		t.Fatalf("unknown category must list what exists: %v", err)
	}
	_, err = ResolveCategory("spark", nil)
	if err == nil || !strings.Contains(err.Error(), "no pattern.<name> entries") {
		t.Fatalf("no patterns at all: %v", err)
	}
}
