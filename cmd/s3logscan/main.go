// Command s3logscan is a resource-budgeted concurrent scanner for
// EMR/YARN logs stored in S3.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/emr"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/ryandam9/s3-log-scan/internal/config"
	"github.com/ryandam9/s3-log-scan/internal/scan"
)

// Injected at build time via -ldflags (see Makefile).
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main without process-global state, so exit codes and the
// -h/-version paths are testable (M-04).
func run(args []string, stdout, stderr io.Writer) int {
	fs, opts := config.NewFlagSet("s3logscan", stderr)
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0 // help is an informational success, not a usage error
		}
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "s3logscan %s (commit %s, built %s)\n", version, commit, date)
		return 0
	}
	// Config file: standing defaults (cluster name, pattern, -i, ...)
	// read from -config or ~/.config/s3logscan/config, applied only to
	// flags NOT given on the command line — CLI always takes priority.
	cliSet := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { cliSet[f.Name] = true })
	fileSet := map[string]bool{}
	cfgFile := opts.ConfigFile
	if cfgFile == "" {
		cfgFile = config.DefaultConfigPath() // "" when absent: optional
	}
	if cfgFile != "" {
		var err error
		fileSet, err = config.ApplyFile(fs, cfgFile, func(k string) bool { return cliSet[k] })
		if err != nil {
			fmt.Fprintf(stderr, "s3logscan: %v\n", err)
			return 2
		}
	}

	// "md = true" in the config file is a standing default, not a
	// demand: it applies when the run is app-scoped with a pattern
	// (-app-id and -grep present) and is silently ignored otherwise, so
	// the same config file still serves list-only and cluster-wide
	// runs. An explicit -md on the command line keeps strict validation.
	if opts.MDReport && !cliSet["md"] && fileSet["md"] && (opts.AppID == "" || opts.GrepPattern == "") {
		opts.MDReport = false
	}

	// -group defaults on for humans: when neither the command line nor
	// the config file chose, group whenever stdout is a terminal (and
	// grouping is applicable). Piped/redirected output keeps the
	// stable single-line format.
	groupSet := cliSet["group"] || fileSet["group"]
	opts.Group = resolveGroup(groupSet, opts.Group, opts.GrepPattern != "", opts.NamesOnly, isTerminal(stdout))

	cfg, err := opts.Build()
	if err != nil {
		fmt.Fprintf(stderr, "s3logscan: %v\n", err)
		return 2
	}
	cfg.Scan.TempDir = os.TempDir()
	cfg.ColorOutput = resolveColor(opts.Color, stdout)

	// Prompt reaction to SIGINT/SIGTERM: cancel everything, still
	// print the summary, exit 130 (§10). Only signal cancellation
	// lives on this context; -overall-timeout is layered inside the
	// engine so the two remain distinguishable (H-02).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var loadOpts []func(*awsconfig.LoadOptions) error
	if opts.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(opts.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		fmt.Fprintf(stderr, "s3logscan: loading AWS configuration: %v\n", err)
		return 2
	}
	// Cluster scoping: -cluster-name resolves EVERY running/waiting
	// cluster with that name — same-name clusters are siblings, and
	// the application under investigation may live on any of them —
	// and each cluster contributes one scan scope with its own log
	// destination. -cluster-id contributes exactly one.
	scopes := []scanScope{{Bucket: cfg.Bucket, Prefix: cfg.Prefix}}
	scopeFailures := false
	if opts.ClusterName != "" || opts.ClusterID != "" {
		emrClient := emr.NewFromConfig(awsCfg)
		clusters := []clusterMatch{{ID: opts.ClusterID}}
		if opts.ClusterName != "" {
			var err error
			clusters, err = resolveClusters(ctx, emrClient, opts.ClusterName)
			if err != nil {
				fmt.Fprintf(stderr, "s3logscan: %v\n", err)
				return 2
			}
			if len(clusters) > 1 {
				fmt.Fprintf(stderr, "s3logscan: %d running/waiting clusters named %q; scanning all of them:\n",
					len(clusters), opts.ClusterName)
				for _, m := range clusters {
					fmt.Fprintf(stderr, "s3logscan:   %s\n", m)
				}
			}
		}
		scopes, scopeFailures = clusterScopes(ctx, emrClient, clusters, opts.Bucket, opts.Prefix, stderr)
		if len(scopes) == 0 {
			return 2
		}
	}
	// -app-id narrows every scope to the application's container-log
	// directory. A scope belonging to a different same-name cluster
	// simply lists zero keys — one cheap LIST, no downloads.
	if opts.AppID != "" {
		for i := range scopes {
			scopes[i].Prefix = joinAppPrefix(scopes[i].Prefix, opts.AppID)
		}
	}

	// -md: tee the bytes shown on screen into a buffer and collect the
	// matched keys, then render ~/logscan/<app-id>.md when the run ends
	// (interrupted runs included — found matches are never lost).
	var mdBuf bytes.Buffer
	var mdScopes, mdMatched []string
	engineOut := io.Writer(stdout)
	if opts.MDReport {
		engineOut = io.MultiWriter(stdout, &mdBuf)
	}
	writeReport := func() (failed bool) {
		if !opts.MDReport {
			return false
		}
		path, err := reportPath(opts.AppID)
		if err == nil {
			err = writeMDReport(path, opts.AppID, opts.GrepPattern, mdScopes, mdMatched, mdBuf.String(), time.Now())
		}
		if err != nil {
			fmt.Fprintf(stderr, "s3logscan: %v\n", err)
			return true
		}
		fmt.Fprintf(stderr, "s3logscan: report written to %s\n", path)
		return false
	}

	// One engine run per scope, sequentially. -max-total-matches is a
	// budget across ALL scopes: each run gets what the previous runs
	// left over. Exit codes combine by severity (2 > 3 > 0 > 1);
	// interruption stops everything immediately.
	worst := -1
	if scopeFailures {
		worst = 2
	}
	remaining := cfg.MaxTotalMatches // 0 = unlimited
	for _, sc := range scopes {
		if cfg.MaxTotalMatches > 0 && remaining <= 0 {
			break // global match budget exhausted
		}
		runCfg := *cfg
		runCfg.Bucket = sc.Bucket
		runCfg.Prefix = sc.Prefix
		runCfg.MaxTotalMatches = remaining

		// Unless -region was given explicitly, resolve each bucket's
		// actual region (log destinations can differ per cluster) so
		// cross-region buckets work without configuration. Detection
		// failures fall back to the configured region.
		scopeAWS := awsCfg.Copy()
		if opts.Region == "" {
			if region, ok := resolveBucketRegion(ctx, awsCfg, runCfg.Bucket); ok {
				scopeAWS.Region = region
			}
		}
		// Echo the exact scope every LIST and GET will be confined to,
		// so a run always shows where it is looking — including the
		// containers/<app-id>/ narrowing when -app-id is in play.
		fmt.Fprintf(stderr, "s3logscan: scanning s3://%s/%s\n", runCfg.Bucket, runCfg.Prefix)
		if opts.MDReport {
			scope := fmt.Sprintf("s3://%s/%s", runCfg.Bucket, runCfg.Prefix)
			mdScopes = append(mdScopes, scope)
			fmt.Fprintf(&mdBuf, "s3logscan: scanning %s\n", scope)
		}

		engine := scan.NewEngine(&runCfg, newS3Client(scopeAWS), stderr)
		result := engine.Run(ctx, engineOut)

		engine.Warner().Flush()
		if result.ListingErr != nil {
			fmt.Fprintf(stderr, "s3logscan: %v\n", result.ListingErr)
		}
		scan.PrintSummary(stderr, result, runCfg.ListOnly, resolveColor(opts.Color, stderr))
		if opts.MDReport {
			mdMatched = append(mdMatched, result.MatchedKeys...)
			scan.PrintSummary(&mdBuf, result, runCfg.ListOnly, false)
		}

		code := scan.ExitCode(result)
		if code == 130 {
			writeReport()
			return 130
		}
		worst = combineExit(worst, code)
		if cfg.MaxTotalMatches > 0 {
			remaining -= result.Counters.MatchedLines.Load()
		}
	}
	// The report was asked for; failing to write it is a run failure
	// even when the scan itself succeeded.
	if writeReport() {
		worst = combineExit(worst, 2)
	}
	return worst
}

// combineExit merges per-scope exit codes into the run's overall code
// by severity: fatal (2) > partial/object errors (3) > matched (0) >
// no matches (1). -1 means "no code yet".
func combineExit(a, b int) int {
	severity := map[int]int{2: 3, 3: 2, 0: 1, 1: 0}
	if a < 0 {
		return b
	}
	if severity[b] > severity[a] {
		return b
	}
	return a
}

// resolveGroup decides the effective -group value. An explicit flag
// always wins; otherwise grouping defaults on exactly when it is
// applicable (a content grep, not names-only) and stdout is a
// terminal — pipes keep the stable single-line format.
func resolveGroup(explicit, flagValue, grepSet, namesOnly, tty bool) bool {
	if explicit {
		return flagValue
	}
	return grepSet && !namesOnly && tty
}

// resolveColor decides whether a stream gets ANSI colors. "always"
// and "never" are absolute; "auto" colors only when the stream is a
// terminal, honoring the NO_COLOR convention and TERM=dumb.
func resolveColor(mode string, w io.Writer) bool {
	switch mode {
	case "always":
		return true
	case "never":
		return false
	}
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	return isTerminal(w)
}

// isTerminal reports whether w is an interactive terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// newS3Client builds the S3 client with response-checksum validation
// set to WhenRequired. The SDK's default (WhenSupported) logs a
// "Response has no supported checksum" WARN line for every GetObject
// of an object uploaded without a modern checksum — one useless stderr
// line per object on older buckets. Integrity is still covered by TLS,
// the gzip/ZIP CRCs of compressed content, and the If-Match ETag
// condition on every GET.
func newS3Client(cfg aws.Config) *s3.Client {
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
}

// resolveBucketRegion discovers which region a bucket lives in.
// HeadBucket reports it in the response — via the BucketRegion field
// on success, and via the x-amz-bucket-region header even on the
// 301/403 responses a wrong-region or access-restricted probe gets.
// The probe uses us-east-1 when no region is configured at all.
func resolveBucketRegion(ctx context.Context, cfg aws.Config, bucket string) (string, bool) {
	probe := cfg.Copy()
	if probe.Region == "" {
		probe.Region = "us-east-1"
	}
	client := newS3Client(probe)
	out, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	if err == nil {
		if out.BucketRegion != nil && *out.BucketRegion != "" {
			return *out.BucketRegion, true
		}
		return probe.Region, true
	}
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) && respErr.Response != nil {
		if region := respErr.Response.Header.Get("x-amz-bucket-region"); region != "" {
			return region, true
		}
	}
	return "", false
}
