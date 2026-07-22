// Package config parses and validates the s3logscan command line.
// Validation is fail-fast (M-03): every violation is reported with
// exit status 2 before any AWS call is made.
package config

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ryandam9/s3-log-scan/internal/scan"
)

const mib = 1 << 20

// Options is the parsed, not-yet-validated command line.
type Options struct {
	Bucket               string
	Prefix               string
	AllowWholeBucketScan bool

	KeyPattern  string
	GrepPattern string
	FixedString bool
	IgnoreCase  bool

	ExtList   string
	AfterStr  string
	BeforeStr string

	MaxSizeMiB          int64
	MaxZipEntries       int
	MaxUncompressedMiB  int64
	MaxLineSizeMiB      int64
	MaxMatches          int64
	NamesOnly           bool
	DiscoverApps        bool
	SmallestFirst       bool
	SmallestFirstWindow int
	Workers             int
	ZipWorkers          int
	ObjectTimeout       time.Duration
	OverallTimeout      time.Duration
	RequestPayer        string
	ExpectedBucketOwner string
	SanitizeOutput      bool
	MaxWarnings         int
	Region              string
}

// NewFlagSet binds all flags (§11) onto a fresh FlagSet.
func NewFlagSet(name string, out io.Writer) (*flag.FlagSet, *Options) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(out)
	o := &Options{}

	fs.StringVar(&o.Bucket, "bucket", "", "S3 bucket name (required)")
	fs.StringVar(&o.Prefix, "prefix", "", "key prefix; required unless -allow-whole-bucket-scan")
	fs.BoolVar(&o.AllowWholeBucketScan, "allow-whole-bucket-scan", false, "explicit opt-in to empty-prefix enumeration of the whole bucket")
	fs.StringVar(&o.KeyPattern, "key", "", "object-key filter (client-side; cuts GETs, not LIST work)")
	fs.StringVar(&o.GrepPattern, "grep", "", "content filter; omit for list-only mode (no downloads)")
	fs.BoolVar(&o.FixedString, "F", false, "-key/-grep are fixed strings, not regex")
	fs.BoolVar(&o.IgnoreCase, "i", false, "case-insensitive matching")
	fs.StringVar(&o.ExtList, "ext", "", "comma-separated extension allow-list, e.g. .gz,.log (case-insensitive)")
	fs.StringVar(&o.AfterStr, "after", "", "RFC3339 or YYYY-MM-DD (UTC midnight); inclusive; vs LastModified")
	fs.StringVar(&o.BeforeStr, "before", "", "RFC3339 or YYYY-MM-DD (UTC midnight); exclusive; vs LastModified")
	fs.Int64Var(&o.MaxSizeMiB, "max-size", 128, "compressed object size cap in MiB (0 = unlimited)")
	fs.IntVar(&o.MaxZipEntries, "max-zip-entries", 10000, "maximum entries per ZIP object")
	fs.Int64Var(&o.MaxUncompressedMiB, "max-uncompressed-object-size", 512, "cumulative ZIP expansion budget in MiB")
	fs.Int64Var(&o.MaxLineSizeMiB, "max-line-size", 4, "oversized-line truncation boundary in MiB")
	fs.Int64Var(&o.MaxMatches, "max-matches", 0, "match cap per object; whole ZIP = one object (0 = unlimited)")
	fs.BoolVar(&o.NamesOnly, "l", false, "names only; first-hit exit; best-effort application IDs")
	fs.BoolVar(&o.DiscoverApps, "discover-apps", false, "with -l: read on until an application ID is found")
	fs.BoolVar(&o.SmallestFirst, "smallest-first", false, "windowed approximate smallest-first ordering")
	fs.IntVar(&o.SmallestFirstWindow, "smallest-first-window", 5000, "smallest-first window size in descriptors")
	fs.IntVar(&o.Workers, "workers", 16, "download/scan workers (1-256)")
	fs.IntVar(&o.ZipWorkers, "zip-workers", 2, "concurrent ZIP objects (1-workers); each holds up to -max-size temp disk")
	fs.DurationVar(&o.ObjectTimeout, "object-timeout", 0, "per-object timeout (0 = none)")
	fs.DurationVar(&o.OverallTimeout, "overall-timeout", 0, "whole-run timeout (0 = none)")
	fs.StringVar(&o.RequestPayer, "request-payer", "", `set to "requester" for requester-pays buckets`)
	fs.StringVar(&o.ExpectedBucketOwner, "expected-bucket-owner", "", "account ID the bucket must belong to (cross-account safety)")
	fs.BoolVar(&o.SanitizeOutput, "sanitize-output", true, "replace control characters in output")
	fs.IntVar(&o.MaxWarnings, "max-warnings", 100, "stderr warning cap (0 = unlimited)")
	fs.StringVar(&o.Region, "region", "", "AWS region override")
	return fs, o
}

// Build validates o and produces the engine configuration. All
// violations are usage errors (exit 2).
func (o *Options) Build() (*scan.Config, error) {
	if o.Bucket == "" {
		return nil, fmt.Errorf("-bucket is required")
	}
	if o.Prefix == "" && !o.AllowWholeBucketScan {
		return nil, fmt.Errorf("-prefix is required; pass -allow-whole-bucket-scan to enumerate the whole bucket (listing time is proportional to key count)")
	}
	if o.Workers < 1 || o.Workers > 256 {
		return nil, fmt.Errorf("-workers must be in 1-256, got %d", o.Workers)
	}
	if o.ZipWorkers < 1 || o.ZipWorkers > o.Workers {
		return nil, fmt.Errorf("-zip-workers must be in 1-%d (workers), got %d", o.Workers, o.ZipWorkers)
	}
	if o.SmallestFirstWindow < 1 {
		return nil, fmt.Errorf("-smallest-first-window must be >= 1, got %d", o.SmallestFirstWindow)
	}
	if o.MaxSizeMiB < 0 || o.MaxUncompressedMiB < 0 || o.MaxMatches < 0 || o.MaxWarnings < 0 {
		return nil, fmt.Errorf("size, match, and warning limits must be >= 0")
	}
	if o.MaxLineSizeMiB < 1 {
		return nil, fmt.Errorf("-max-line-size must be >= 1 MiB, got %d", o.MaxLineSizeMiB)
	}
	if o.MaxZipEntries < 1 {
		return nil, fmt.Errorf("-max-zip-entries must be >= 1, got %d", o.MaxZipEntries)
	}
	if o.DiscoverApps && !o.NamesOnly {
		return nil, fmt.Errorf("-discover-apps requires -l")
	}
	if o.RequestPayer != "" && o.RequestPayer != "requester" {
		return nil, fmt.Errorf(`-request-payer accepts only "requester", got %q`, o.RequestPayer)
	}
	if o.ObjectTimeout < 0 || o.OverallTimeout < 0 {
		return nil, fmt.Errorf("timeouts must be >= 0")
	}

	cfg := &scan.Config{
		Bucket:              o.Bucket,
		Prefix:              o.Prefix,
		ListOnly:            o.GrepPattern == "",
		SmallestFirst:       o.SmallestFirst,
		SmallestFirstWindow: o.SmallestFirstWindow,
		Workers:             o.Workers,
		ZipWorkers:          o.ZipWorkers,
		ObjectTimeout:       o.ObjectTimeout,
		OverallTimeout:      o.OverallTimeout,
		RequestPayer:        o.RequestPayer == "requester",
		ExpectedBucketOwner: o.ExpectedBucketOwner,
		SanitizeOutput:      o.SanitizeOutput,
		MaxWarnings:         o.MaxWarnings,
	}

	cfg.Filters.MaxSize = o.MaxSizeMiB * mib
	if o.KeyPattern != "" {
		m, err := scan.NewMatcher(o.KeyPattern, o.FixedString, o.IgnoreCase)
		if err != nil {
			return nil, fmt.Errorf("-key: %w", err)
		}
		cfg.Filters.KeyMatcher = m
	}
	if o.GrepPattern != "" {
		m, err := scan.NewMatcher(o.GrepPattern, o.FixedString, o.IgnoreCase)
		if err != nil {
			return nil, fmt.Errorf("-grep: %w", err)
		}
		cfg.Scan.Grep = m
	}
	if o.NamesOnly && cfg.ListOnly {
		return nil, fmt.Errorf("-l requires -grep (names of objects with content matches)")
	}
	if o.DiscoverApps && cfg.ListOnly {
		return nil, fmt.Errorf("-discover-apps requires -grep")
	}

	exts, err := parseExtList(o.ExtList)
	if err != nil {
		return nil, err
	}
	cfg.Filters.Extensions = exts

	if o.AfterStr != "" {
		t, err := parseTimeFlag(o.AfterStr)
		if err != nil {
			return nil, fmt.Errorf("-after: %w", err)
		}
		cfg.Filters.After, cfg.Filters.HasAfter = t, true
	}
	if o.BeforeStr != "" {
		t, err := parseTimeFlag(o.BeforeStr)
		if err != nil {
			return nil, fmt.Errorf("-before: %w", err)
		}
		cfg.Filters.Before, cfg.Filters.HasBefore = t, true
	}
	if cfg.Filters.HasAfter && cfg.Filters.HasBefore && !cfg.Filters.After.Before(cfg.Filters.Before) {
		return nil, fmt.Errorf("-after (%s) must be earlier than -before (%s)", cfg.Filters.After.Format(time.RFC3339), cfg.Filters.Before.Format(time.RFC3339))
	}

	cfg.Scan.MaxMatches = o.MaxMatches
	cfg.Scan.MaxLineSize = int(o.MaxLineSizeMiB * mib)
	cfg.Scan.NamesOnly = o.NamesOnly
	cfg.Scan.DiscoverApps = o.DiscoverApps
	cfg.Scan.MaxZipEntries = o.MaxZipEntries
	cfg.Scan.MaxZipExpandedBytes = o.MaxUncompressedMiB * mib
	return cfg, nil
}

// parseExtList validates a comma-separated extension allow-list;
// entries are lower-cased and must begin with a dot.
func parseExtList(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("-ext: empty entry in %q", s)
		}
		if !strings.HasPrefix(p, ".") {
			return nil, fmt.Errorf("-ext: %q must start with a dot (e.g. .gz)", p)
		}
		out = append(out, strings.ToLower(p))
	}
	return out, nil
}

// parseTimeFlag accepts RFC3339 (with offsets, for precision) or
// YYYY-MM-DD, which means UTC midnight (M-02).
func parseTimeFlag(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("%q is neither RFC3339 nor YYYY-MM-DD", s)
}
