package config

import (
	"io"
	"strings"
	"testing"
	"time"
)

func parse(t *testing.T, args ...string) (*Options, error) {
	t.Helper()
	fs, o := NewFlagSet("test", io.Discard)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	return o, nil
}

func build(t *testing.T, args ...string) error {
	t.Helper()
	o, _ := parse(t, args...)
	_, err := o.Build()
	return err
}

func TestValidationFailFast(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string // substring of the error
	}{
		{"missing bucket", []string{}, "-bucket is required"},
		{"empty prefix", []string{"-bucket", "b"}, "allow-whole-bucket-scan"},
		{"workers low", []string{"-bucket", "b", "-prefix", "p", "-workers", "0"}, "-workers"},
		{"workers high", []string{"-bucket", "b", "-prefix", "p", "-workers", "257"}, "-workers"},
		{"zip workers above workers", []string{"-bucket", "b", "-prefix", "p", "-workers", "2", "-zip-workers", "3"}, "-zip-workers"},
		{"window zero", []string{"-bucket", "b", "-prefix", "p", "-smallest-first-window", "0"}, "-smallest-first-window"},
		{"discover without l", []string{"-bucket", "b", "-prefix", "p", "-grep", "x", "-discover-apps"}, "-discover-apps requires -l"},
		{"l without grep", []string{"-bucket", "b", "-prefix", "p", "-l"}, "-l requires -grep"},
		{"bad regex", []string{"-bucket", "b", "-prefix", "p", "-grep", "("}, "-grep"},
		{"bad key regex", []string{"-bucket", "b", "-prefix", "p", "-key", "["}, "-key"},
		{"bad ext no dot", []string{"-bucket", "b", "-prefix", "p", "-ext", "gz"}, "must start with a dot"},
		{"bad ext empty entry", []string{"-bucket", "b", "-prefix", "p", "-ext", ".gz,,.log"}, "empty entry"},
		{"bad after", []string{"-bucket", "b", "-prefix", "p", "-after", "yesterday"}, "-after"},
		{"after not before before", []string{"-bucket", "b", "-prefix", "p", "-after", "2026-07-02", "-before", "2026-07-01"}, "must be earlier"},
		{"bad request payer", []string{"-bucket", "b", "-prefix", "p", "-request-payer", "owner"}, "-request-payer"},
		{"line size zero", []string{"-bucket", "b", "-prefix", "p", "-max-line-size", "0"}, "-max-line-size"},
		// M-03: absurd values are rejected before MiB conversion can
		// overflow and silently disable a budget.
		{"max-size overflow", []string{"-bucket", "b", "-prefix", "p", "-max-size", "9223372036854775807"}, "-max-size"},
		{"max-size above cap", []string{"-bucket", "b", "-prefix", "p", "-max-size", "1048577"}, "-max-size"},
		{"uncompressed above cap", []string{"-bucket", "b", "-prefix", "p", "-max-uncompressed-object-size", "4194305"}, "-max-uncompressed-object-size"},
		{"line size above cap", []string{"-bucket", "b", "-prefix", "p", "-max-line-size", "257"}, "-max-line-size"},
		{"group without grep", []string{"-bucket", "b", "-prefix", "p", "-group"}, "-group requires -grep"},
		{"group with names-only", []string{"-bucket", "b", "-prefix", "p", "-grep", "x", "-l", "-group"}, "-group cannot be combined with -l"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := build(t, tc.args...)
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestValidConfigs(t *testing.T) {
	if err := build(t, "-bucket", "b", "-prefix", "logs/", "-grep", "ERROR"); err != nil {
		t.Fatalf("minimal grep config: %v", err)
	}
	if err := build(t, "-bucket", "b", "-prefix", "logs/"); err != nil {
		t.Fatalf("list-only config: %v", err)
	}
	if err := build(t, "-bucket", "b", "-allow-whole-bucket-scan", "-grep", "x"); err != nil {
		t.Fatalf("whole-bucket opt-in: %v", err)
	}
	if err := build(t, "-bucket", "b", "-prefix", "p", "-grep", "x", "-l", "-discover-apps"); err != nil {
		t.Fatalf("-l -discover-apps: %v", err)
	}
}

// M-02: date-only means UTC midnight; RFC3339 offsets are honored.
func TestTimeParsing(t *testing.T) {
	o, _ := parse(t, "-bucket", "b", "-prefix", "p", "-after", "2026-07-01", "-before", "2026-07-22T10:30:00+02:00")
	cfg, err := o.Build()
	if err != nil {
		t.Fatal(err)
	}
	wantAfter := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if !cfg.Filters.After.Equal(wantAfter) {
		t.Fatalf("after: %v want %v", cfg.Filters.After, wantAfter)
	}
	wantBefore := time.Date(2026, 7, 22, 8, 30, 0, 0, time.UTC)
	if !cfg.Filters.Before.Equal(wantBefore) {
		t.Fatalf("before: %v want %v", cfg.Filters.Before, wantBefore)
	}
}

func TestUnitConversionAndDefaults(t *testing.T) {
	o, _ := parse(t, "-bucket", "b", "-prefix", "p", "-grep", "x")
	cfg, err := o.Build()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Filters.MaxSize != 128<<20 {
		t.Fatalf("default max-size: %d", cfg.Filters.MaxSize)
	}
	if cfg.Scan.MaxLineSize != 4<<20 {
		t.Fatalf("default max-line-size: %d", cfg.Scan.MaxLineSize)
	}
	if cfg.Scan.MaxZipExpandedBytes != 512<<20 {
		t.Fatalf("default expansion budget: %d", cfg.Scan.MaxZipExpandedBytes)
	}
	if cfg.Workers != 16 || cfg.ZipWorkers != 2 || cfg.SmallestFirstWindow != 5000 {
		t.Fatalf("defaults: workers=%d zip=%d window=%d", cfg.Workers, cfg.ZipWorkers, cfg.SmallestFirstWindow)
	}
	if !cfg.SanitizeOutput {
		t.Fatal("sanitize must default on")
	}
}

func TestExtListNormalization(t *testing.T) {
	o, _ := parse(t, "-bucket", "b", "-prefix", "p", "-ext", ".GZ, .Log")
	cfg, err := o.Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Filters.Extensions) != 2 || cfg.Filters.Extensions[0] != ".gz" || cfg.Filters.Extensions[1] != ".log" {
		t.Fatalf("extensions: %v", cfg.Filters.Extensions)
	}
}

func TestZeroMeansUnlimited(t *testing.T) {
	o, _ := parse(t, "-bucket", "b", "-prefix", "p", "-grep", "x", "-max-size", "0", "-max-matches", "0")
	cfg, err := o.Build()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Filters.MaxSize != 0 || cfg.Scan.MaxMatches != 0 {
		t.Fatalf("zero must mean unlimited: %d %d", cfg.Filters.MaxSize, cfg.Scan.MaxMatches)
	}
}
