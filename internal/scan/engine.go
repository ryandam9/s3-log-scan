package scan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// Config is the fully validated engine configuration; construction and
// validation live in internal/config.
type Config struct {
	Bucket string
	Prefix string

	Filters FilterConfig
	Scan    ScanOptions

	ListOnly bool // no -grep: report surviving keys, no downloads

	SmallestFirst       bool
	SmallestFirstWindow int

	Workers    int
	ZipWorkers int

	ObjectTimeout  time.Duration
	OverallTimeout time.Duration

	RequestPayer        bool
	ExpectedBucketOwner string

	SanitizeOutput bool
	MaxWarnings    int
}

// RunResult is what the engine hands back to main for exit-code and
// summary decisions.
type RunResult struct {
	Counters    *Counters
	AppIDs      *AppIDSet
	ListingErr  error // fatal: exit 2
	WriteErr    error // stdout failed (EPIPE etc.)
	Interrupted bool  // context cancelled from outside (signal)
	Elapsed     time.Duration
}

// Engine wires lister → scheduler → workers → writer.
type Engine struct {
	cfg      *Config
	client   S3API
	counters Counters
	appIDs   *AppIDSet
	warner   *Warner
}

// NewEngine builds an engine around an S3 client. warnOut receives
// bounded diagnostics (stderr in production).
func NewEngine(cfg *Config, client S3API, warnOut io.Writer) *Engine {
	e := &Engine{
		cfg:    cfg,
		client: client,
		appIDs: NewAppIDSet(),
	}
	e.warner = NewWarner(warnOut, cfg.MaxWarnings, &e.counters)
	return e
}

// Warner exposes the bounded warning sink (main uses it for the
// suppression trailer).
func (e *Engine) Warner() *Warner { return e.warner }

// Run executes the scan. ctx should already carry signal cancellation;
// the overall timeout is layered here.
func (e *Engine) Run(ctx context.Context, stdout io.Writer) *RunResult {
	start := time.Now()
	if e.cfg.OverallTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.cfg.OverallTimeout)
		defer cancel()
	}
	// The writer owns this cancel: any stdout failure stops the run.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	writer := NewWriter(stdout, e.cfg.Workers*8, e.cfg.SanitizeOutput, cancel)

	work := make(chan ObjectDescriptor, e.cfg.Workers*2)
	var listErr error

	var wg sync.WaitGroup
	if !e.cfg.ListOnly {
		zipSem := make(chan struct{}, e.cfg.ZipWorkers)
		for i := 0; i < e.cfg.Workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				e.worker(ctx, work, zipSem, writer)
			}()
		}
	} else {
		// List-only mode: a single consumer prints surviving keys.
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range work {
				if !writer.Emit(ctx, Result{Bucket: e.cfg.Bucket, Key: d.Key, KeyOnly: true}) {
					return
				}
				e.counters.MatchedObjects.Add(1)
			}
		}()
	}

	listErr = e.list(ctx, work)
	close(work)
	wg.Wait()
	writeErr := writer.Close()

	res := &RunResult{
		Counters:   &e.counters,
		AppIDs:     e.appIDs,
		ListingErr: listErr,
		WriteErr:   writeErr,
		Elapsed:    time.Since(start),
	}
	if ctx.Err() != nil && writeErr == nil && listErr == nil {
		res.Interrupted = true
	}
	return res
}

// list paginates ListObjectsV2, applies the filter chain, and feeds
// survivors to the work channel — directly, or through the bounded
// smallest-first window (§5.2).
func (e *Engine) list(ctx context.Context, work chan<- ObjectDescriptor) error {
	var window *SmallestFirstWindow
	if e.cfg.SmallestFirst {
		window = NewSmallestFirstWindow(e.cfg.SmallestFirstWindow)
	}

	send := func(d ObjectDescriptor) bool {
		select {
		case work <- d:
			return true
		case <-ctx.Done():
			return false
		}
	}

	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(e.cfg.Bucket),
	}
	if e.cfg.Prefix != "" {
		input.Prefix = aws.String(e.cfg.Prefix)
	}
	if e.cfg.RequestPayer {
		input.RequestPayer = types.RequestPayerRequester
	}
	if e.cfg.ExpectedBucketOwner != "" {
		input.ExpectedBucketOwner = aws.String(e.cfg.ExpectedBucketOwner)
	}

	paginator := s3.NewListObjectsV2Paginator(e.client, input)
	for paginator.HasMorePages() {
		if ctx.Err() != nil {
			return nil
		}
		page, err := paginator.NextPage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil // cancellation, not a listing failure
			}
			return fmt.Errorf("listing s3://%s/%s: %w", e.cfg.Bucket, e.cfg.Prefix, err)
		}
		for _, obj := range page.Contents {
			e.counters.Listed.Add(1)
			d := ObjectDescriptor{
				Key:          aws.ToString(obj.Key),
				Size:         aws.ToInt64(obj.Size),
				LastModified: aws.ToTime(obj.LastModified),
				ETag:         aws.ToString(obj.ETag),
				StorageClass: string(obj.StorageClass),
			}
			if v := e.cfg.Filters.Apply(&d, &e.counters); v != VerdictAccept {
				if v == VerdictOversize {
					e.warner.Warnf("skipping s3://%s/%s: size %d exceeds -max-size", e.cfg.Bucket, d.Key, d.Size)
				}
				continue
			}
			e.counters.Survived.Add(1)
			if window != nil {
				if evicted, ok := window.Offer(d); ok {
					if !send(evicted) {
						return nil
					}
				}
			} else if !send(d) {
				return nil
			}
		}
	}
	if window != nil {
		window.Drain(send)
	}
	return nil
}

// worker consumes descriptors, GETs each with If-Match, scans, and
// accounts the outcome.
func (e *Engine) worker(ctx context.Context, work <-chan ObjectDescriptor, zipSem chan struct{}, writer *Writer) {
	for d := range work {
		if ctx.Err() != nil {
			// Keep draining so the lister never blocks after cancel.
			continue
		}
		e.scanOne(ctx, d, zipSem, writer)
	}
}

func (e *Engine) scanOne(ctx context.Context, d ObjectDescriptor, zipSem chan struct{}, writer *Writer) {
	format := DetectFormat(d.Key)

	// ZIP concurrency is gated independently of -workers (§6.3): each
	// concurrent ZIP holds up to -max-size of temp disk.
	if format == FormatZip {
		select {
		case zipSem <- struct{}{}:
			defer func() { <-zipSem }()
		case <-ctx.Done():
			return
		}
	}

	objCtx := ctx
	if e.cfg.ObjectTimeout > 0 {
		var cancel context.CancelFunc
		objCtx, cancel = context.WithTimeout(ctx, e.cfg.ObjectTimeout)
		defer cancel()
	}

	input := &s3.GetObjectInput{
		Bucket:  aws.String(e.cfg.Bucket),
		Key:     aws.String(d.Key),
		IfMatch: aws.String(d.ETag), // LIST/GET consistency (H-06)
	}
	if e.cfg.RequestPayer {
		input.RequestPayer = types.RequestPayerRequester
	}
	if e.cfg.ExpectedBucketOwner != "" {
		input.ExpectedBucketOwner = aws.String(e.cfg.ExpectedBucketOwner)
	}

	resp, err := e.client.GetObject(objCtx, input)
	if err != nil {
		class := classifyRequestError(objCtx, err)
		if ctx.Err() != nil && objCtx.Err() == nil {
			return // run-level cancellation, not an object failure
		}
		e.counters.AddError(class)
		switch class {
		case ErrClassChangedAfterListing:
			e.warner.Warnf("s3://%s/%s: object changed after listing; not scanned", e.cfg.Bucket, d.Key)
		default:
			e.warner.Warnf("s3://%s/%s: %s: %v", e.cfg.Bucket, d.Key, class, err)
		}
		return
	}
	defer resp.Body.Close()

	body := &countingReader{r: resp.Body, n: &e.counters.BytesDownloaded}
	outcome := ScanObject(objCtx, e.cfg.Bucket, &d, body, format, &e.cfg.Scan, writer, &e.counters)

	if outcome.Matches > 0 {
		e.counters.MatchedObjects.Add(1)
		e.appIDs.Add(outcome.AppID)
	}
	switch {
	case outcome.Err != nil:
		e.counters.AddError(outcome.ErrClass)
		e.warner.Warnf("s3://%s/%s: %s: %v", e.cfg.Bucket, d.Key, outcome.ErrClass, outcome.Err)
	case outcome.Partial:
		if ctx.Err() != nil {
			return // interrupted mid-scan; the run reports interruption
		}
		e.counters.ScannedPartially.Add(1)
		if outcome.ErrClass != ErrClassNone {
			e.counters.AddError(outcome.ErrClass)
		}
		e.warner.Warnf("s3://%s/%s: partially scanned: %s", e.cfg.Bucket, d.Key, outcome.PartialWhy)
	default:
		if ctx.Err() != nil {
			return
		}
		e.counters.ScannedFully.Add(1)
	}
}

// countingReader tallies compressed bytes read from S3.
type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// classifyRequestError buckets GetObject failures (§10, H-05).
func classifyRequestError(ctx context.Context, err error) ErrorClass {
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
		return ErrClassTimeout
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch code := apiErr.ErrorCode(); {
		case code == "PreconditionFailed":
			return ErrClassChangedAfterListing
		case code == "NoSuchKey" || code == "NotFound":
			return ErrClassNotFound
		case code == "AccessDenied" || strings.HasPrefix(code, "KMS."):
			// KMS denials count with access denials (§10).
			return ErrClassAccessDenied
		}
	}
	return ErrClassOther
}
