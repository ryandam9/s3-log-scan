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
	// Cluster scoping: resolve -cluster-name to the active cluster's
	// ID, derive the bucket/prefix from the cluster's S3 log
	// destination when they were not given, and pin the prefix to
	// .../<cluster-id>/ so listing covers only that cluster.
	if opts.ClusterName != "" || opts.ClusterID != "" {
		emrClient := emr.NewFromConfig(awsCfg)
		clusterID := opts.ClusterID
		if opts.ClusterName != "" {
			chosen, others, err := resolveClusterID(ctx, emrClient, opts.ClusterName)
			if err != nil {
				fmt.Fprintf(stderr, "s3logscan: %v\n", err)
				return 2
			}
			clusterID = chosen.ID
			if len(others) > 0 {
				fmt.Fprintf(stderr, "s3logscan: %d running/waiting clusters named %q; using the newest, %s\n",
					len(others)+1, opts.ClusterName, chosen)
				for _, m := range others {
					fmt.Fprintf(stderr, "s3logscan:   also matched: %s (target it with -cluster-id)\n", m)
				}
			}
		}
		bucket, prefix := opts.Bucket, opts.Prefix
		if bucket == "" {
			var err error
			bucket, prefix, err = clusterLogDestination(ctx, emrClient, clusterID)
			if err != nil {
				fmt.Fprintf(stderr, "s3logscan: %v\n", err)
				return 2
			}
		}
		cfg.Bucket = bucket
		cfg.Prefix = joinClusterPrefix(prefix, clusterID)
		fmt.Fprintf(stderr, "s3logscan: scanning s3://%s/%s\n", cfg.Bucket, cfg.Prefix)
	}

	// Unless -region was given explicitly, resolve the bucket's actual
	// region so cross-region buckets work without configuration
	// (IllegalLocationConstraintException / PermanentRedirect
	// otherwise). Detection failures fall back to the configured
	// region and let the real operation report its error.
	if opts.Region == "" {
		if region, ok := resolveBucketRegion(ctx, awsCfg, cfg.Bucket); ok {
			awsCfg.Region = region
		}
	}
	client := newS3Client(awsCfg)

	engine := scan.NewEngine(cfg, client, stderr)
	result := engine.Run(ctx, stdout)

	engine.Warner().Flush()
	if result.ListingErr != nil {
		fmt.Fprintf(stderr, "s3logscan: %v\n", result.ListingErr)
	}
	scan.PrintSummary(stderr, result, cfg.ListOnly, resolveColor(opts.Color, stderr))
	return scan.ExitCode(result)
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
