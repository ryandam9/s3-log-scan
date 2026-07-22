// Command s3logscan is a resource-budgeted concurrent scanner for
// EMR/YARN logs stored in S3.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
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
	cfg, err := opts.Build()
	if err != nil {
		fmt.Fprintf(stderr, "s3logscan: %v\n", err)
		return 2
	}
	cfg.Scan.TempDir = os.TempDir()

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
	client := s3.NewFromConfig(awsCfg)

	engine := scan.NewEngine(cfg, client, stderr)
	result := engine.Run(ctx, stdout)

	engine.Warner().Flush()
	if result.ListingErr != nil {
		fmt.Fprintf(stderr, "s3logscan: %v\n", result.ListingErr)
	}
	scan.PrintSummary(stderr, result, cfg.ListOnly)
	return scan.ExitCode(result)
}
