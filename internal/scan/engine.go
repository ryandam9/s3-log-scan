package scan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
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
	ColorOutput    bool // ANSI-color stdout results (resolved by main from -color + TTY)
	GroupOutput    bool // -group: print each object key once as a heading
	MaxWarnings    int

	// MaxTotalMatches stops the whole run after this many matches
	// have been reported (0 = unlimited). Objects cut short by it are
	// counted as stopped early by request, never as partial.
	MaxTotalMatches int64

	// Progress > 0 prints a one-line status to stderr every interval,
	// so long scans are distinguishable from hangs. Verbose logs each
	// listing page and each object as scanning starts. Both write to
	// stderr only; stdout stays matches-only.
	Progress time.Duration
	Verbose  bool

	// CollectMatchedKeys records the s3:// URI of every object with at
	// least one match into RunResult.MatchedKeys (the -md report needs
	// the list, not just the count). Off by default: matched keys are
	// unbounded in principle, so only report runs pay for the slice.
	CollectMatchedKeys bool

	// RecordMatch, when set, observes every content match in emission
	// order (called from the writer goroutine; safe to read after Run
	// returns). The -md report uses it to rebuild matches per file.
	RecordMatch func(Result)
}

// RunResult is what the engine hands back to main for exit-code and
// summary decisions.
type RunResult struct {
	Counters      *Counters
	AppIDs        *AppIDSet
	MatchedKeys   []string // sorted s3:// URIs; only with CollectMatchedKeys
	ListingErr    error    // fatal: exit 2
	WriteErr      error    // stdout failed (EPIPE etc.)
	TimedOut      bool     // -overall-timeout expired (exit 3, H-02)
	Interrupted   bool     // external cancellation: SIGINT/SIGTERM (exit 130)
	MatchLimitHit bool     // -max-total-matches reached; a successful early stop
	Elapsed       time.Duration
}

// Engine wires lister → scheduler → workers → writer. An Engine is
// single-use: counters and discovered IDs accumulate across its
// lifetime, so build a fresh Engine per run.
type Engine struct {
	cfg      *Config
	client   S3API
	counters Counters
	appIDs   *AppIDSet
	warner   *Warner

	listingDone atomic.Bool

	matchedMu   sync.Mutex
	matchedKeys []string // populated only with CollectMatchedKeys
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

// Run executes the scan. externalCtx should carry signal cancellation
// only; the overall timeout is layered here so the two are
// distinguishable in the result (H-02).
func (e *Engine) Run(externalCtx context.Context, stdout io.Writer) *RunResult {
	start := time.Now()
	runCtx := externalCtx
	if e.cfg.OverallTimeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(runCtx, e.cfg.OverallTimeout)
		defer cancel()
	}
	// The writer owns this cancel: any stdout failure stops the run.
	runCtx, cancel := context.WithCancel(runCtx)
	defer cancel()

	// The run-wide match cap must be wired before any worker starts.
	e.cfg.Scan.limiter = newMatchLimiter(e.cfg.MaxTotalMatches)

	// -cat's match-everything pattern would "highlight" at every byte
	// position; the writer gets no matcher so lines print untouched.
	highlight := e.cfg.Scan.Grep
	if e.cfg.Scan.CatMode {
		highlight = nil
	}
	writer := NewWriter(stdout, WriterConfig{
		QueueDepth: e.cfg.Workers * 8,
		Sanitize:   e.cfg.SanitizeOutput,
		Color:      e.cfg.ColorOutput,
		Group:      e.cfg.GroupOutput,
		Grep:       highlight,
		Record:     e.cfg.RecordMatch,
	}, cancel)

	work := make(chan ObjectDescriptor, e.cfg.Workers*2)
	var listErr error

	var wg sync.WaitGroup
	if !e.cfg.ListOnly {
		zipSem := make(chan struct{}, e.cfg.ZipWorkers)
		for i := 0; i < e.cfg.Workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				e.worker(runCtx, work, zipSem, writer)
			}()
		}
	} else {
		// List-only mode: a single consumer prints surviving keys.
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range work {
				if !writer.Emit(runCtx, Result{Bucket: e.cfg.Bucket, Key: d.Key, KeyOnly: true}) {
					return
				}
				e.counters.MatchedObjects.Add(1)
			}
		}()
	}

	// The progress reporter runs until scanning completes, printing a
	// status line every interval so the operator can see work moving.
	progressDone := make(chan struct{})
	var progressWG sync.WaitGroup
	if e.cfg.Progress > 0 {
		progressWG.Add(1)
		go func() {
			defer progressWG.Done()
			e.reportProgress(start, progressDone)
		}()
	}

	listErr = e.list(runCtx, work)
	e.listingDone.Store(true)
	close(work)
	wg.Wait()
	close(progressDone)
	progressWG.Wait()
	writeErr := writer.Close()

	sort.Strings(e.matchedKeys) // workers finish in arbitrary order
	res := &RunResult{
		Counters:      &e.counters,
		AppIDs:        e.appIDs,
		MatchedKeys:   e.matchedKeys,
		ListingErr:    listErr,
		WriteErr:      writeErr,
		MatchLimitHit: e.cfg.Scan.limiter.Satisfied(),
		Elapsed:       time.Since(start),
	}
	// Classify why the run context ended, if it did (H-02): external
	// signal, configured deadline, or writer-driven cancellation
	// (already captured in WriteErr).
	switch {
	case writeErr != nil || listErr != nil:
	case externalCtx.Err() != nil:
		res.Interrupted = true
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		res.TimedOut = true
	}
	return res
}

// list paginates ListObjectsV2, applies the filter chain, and feeds
// survivors to the work channel — directly, or through the bounded
// smallest-first window (§5.2). RestoreStatus is requested so archived
// objects with a readable restored copy are scanned, not skipped (H-04).
func (e *Engine) list(ctx context.Context, work chan<- ObjectDescriptor) error {
	var window *SmallestFirstWindow
	if e.cfg.SmallestFirst {
		window = NewSmallestFirstWindow(e.cfg.SmallestFirstWindow)
	}

	send := func(d ObjectDescriptor) bool {
		if e.cfg.Scan.limiter.Satisfied() {
			return false // run-wide match cap reached; stop feeding work
		}
		select {
		case work <- d:
			return true
		case <-ctx.Done():
			return false
		}
	}

	input := &s3.ListObjectsV2Input{
		Bucket:                   aws.String(e.cfg.Bucket),
		OptionalObjectAttributes: []types.OptionalObjectAttributes{types.OptionalObjectAttributesRestoreStatus},
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
		if ctx.Err() != nil || e.cfg.Scan.limiter.Satisfied() {
			return nil
		}
		page, err := paginator.NextPage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil // cancellation, not a listing failure
			}
			return fmt.Errorf("listing s3://%s/%s: %w%s", e.cfg.Bucket, e.cfg.Prefix, err, regionHint(err))
		}
		for _, obj := range page.Contents {
			e.counters.Listed.Add(1)
			d := ObjectDescriptor{
				Key:          aws.ToString(obj.Key),
				Size:         aws.ToInt64(obj.Size),
				LastModified: aws.ToTime(obj.LastModified),
				ETag:         aws.ToString(obj.ETag),
				StorageClass: string(obj.StorageClass),
				Restored:     restoredCopyAvailable(obj.RestoreStatus),
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
		if e.cfg.Verbose {
			e.warner.Logf("listed page of %d keys (%d so far, %d survived filters)",
				len(page.Contents), e.counters.Listed.Load(), e.counters.Survived.Load())
		}
	}
	if window != nil {
		window.Drain(send)
	}
	return nil
}

// reportProgress prints one status line per interval until scanning
// finishes. Counters are atomic, so reading them here is race-free.
// A one-time legend is printed first so the columns need no manual.
func (e *Engine) reportProgress(start time.Time, done <-chan struct{}) {
	e.warner.Logf("progress columns:%s", e.progressLegend())
	ticker := time.NewTicker(e.cfg.Progress)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			e.warner.Logf("progress %s", e.progressLine(time.Since(start)))
		}
	}
}

// progressLegend explains each field of the progress line in plain
// language, shown once before the first progress line.
func (e *Engine) progressLegend() string {
	if e.cfg.ListOnly {
		return `
    00:00:00  time since the run started (hh:mm:ss)
    listing   S3 is still enumerating keys; changes to "listed" when done
    keys      objects the S3 listing has found so far
    kept      objects that passed the filters and will be reported
    reported  object names printed so far`
	}
	return `
    00:00:00  time since the run started (hh:mm:ss)
    listing   S3 is still enumerating keys; changes to "listed" when done
    keys      objects the S3 listing has found so far
    kept      objects that passed the filters and will be downloaded + scanned
    done      objects finished (fully scanned, stopped early, or failed)
    queue     objects still waiting or in flight (kept minus done)
    match     matching objects / matching lines found so far
    dl        compressed data downloaded from S3 so far
    err       objects that failed (classified in the final summary)`
}

// progressLine renders the current state of the run in one line.
// Every field is fixed-width so successive lines align vertically and
// the whole line stays comfortably inside a normal terminal width:
//
//	progress 00:00:30 listing  keys 250       kept 250       done 201       queue 49      match 13/203        dl 22.7 MiB   err 0
//
// keys = keys enumerated by the listing so far; kept = survived the
// metadata filters (the download queue); done = finished objects
// (including partial/errored); queue = kept minus done; match =
// matching objects/lines; dl = compressed bytes downloaded.
func (e *Engine) progressLine(elapsed time.Duration) string {
	c := &e.counters
	listing := "listing"
	if e.listingDone.Load() {
		listing = "listed"
	}
	if e.cfg.ListOnly {
		return fmt.Sprintf("%s %-7s  keys %-9d kept %-9d reported %d",
			formatElapsed(elapsed), listing, c.Listed.Load(), c.Survived.Load(), c.MatchedObjects.Load())
	}
	completed := c.ScannedFully.Load() + c.StoppedEarly.Load() + c.ScannedPartially.Load() + c.ObjectErrors()
	match := fmt.Sprintf("%d/%d", c.MatchedObjects.Load(), c.MatchedLines.Load())
	return fmt.Sprintf("%s %-7s  keys %-9d kept %-9d done %-9d queue %-7d match %-13s dl %-10s err %d",
		formatElapsed(elapsed), listing, c.Listed.Load(), c.Survived.Load(),
		completed, c.Survived.Load()-completed,
		match, humanBytes(c.BytesDownloaded.Load()), c.ObjectErrors())
}

// formatElapsed renders a duration as hh:mm:ss so the elapsed column
// keeps a constant width.
func formatElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	m := int(d/time.Minute) % 60
	s := int(d/time.Second) % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

// humanBytes renders a byte count for progress lines.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// restoredCopyAvailable interprets the ListObjectsV2 RestoreStatus
// attribute: a readable restored copy exists when a restore is not in
// progress and an expiry date is present.
func restoredCopyAvailable(rs *types.RestoreStatus) bool {
	return rs != nil && !aws.ToBool(rs.IsRestoreInProgress) && rs.RestoreExpiryDate != nil
}

// worker consumes descriptors, GETs each with If-Match, scans, and
// accounts the outcome.
func (e *Engine) worker(ctx context.Context, work <-chan ObjectDescriptor, zipSem chan struct{}, writer *Writer) {
	for d := range work {
		if ctx.Err() != nil || e.cfg.Scan.limiter.Satisfied() {
			// Keep draining so the lister never blocks after cancel
			// or after the run-wide match cap is reached.
			continue
		}
		e.scanOne(ctx, d, zipSem, writer)
	}
}

func (e *Engine) scanOne(ctx context.Context, d ObjectDescriptor, zipSem chan struct{}, writer *Writer) {
	format := DetectFormat(d.Key)
	if e.cfg.Verbose {
		e.warner.Logf("scanning s3://%s/%s (%s)", e.cfg.Bucket, SanitizeString(d.Key), humanBytes(d.Size))
	}

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
		if runCancelled(ctx) {
			return // run-level teardown, not an object failure
		}
		class := classifyRequestError(objCtx, err)
		e.counters.AddError(class)
		switch class {
		case ErrClassChangedAfterListing:
			e.warner.Warnf("s3://%s/%s: object changed after listing; not scanned", e.cfg.Bucket, d.Key)
		case ErrClassArchived:
			e.warner.Warnf("s3://%s/%s: archived and no restored copy is available; not scanned", e.cfg.Bucket, d.Key)
		default:
			e.warner.Warnf("s3://%s/%s: %s: %v", e.cfg.Bucket, d.Key, class, err)
		}
		return
	}
	defer resp.Body.Close()

	body := &countingReader{r: resp.Body, n: &e.counters.BytesDownloaded}
	outcome := ScanObject(objCtx, e.cfg.Bucket, &d, body, format, &e.cfg.Scan, writer, &e.counters)

	if e.cfg.GroupOutput {
		// Tell the writer this object's block is complete so it can
		// print the group atomically. Best-effort under cancellation.
		writer.Emit(ctx, Result{Key: d.Key, GroupEnd: true})
	}

	if outcome.Matches > 0 {
		e.counters.MatchedObjects.Add(1)
		e.appIDs.AddAll(outcome.AppIDs)
		if e.cfg.CollectMatchedKeys {
			key := d.Key
			if e.cfg.SanitizeOutput {
				key = SanitizeString(key)
			}
			e.matchedMu.Lock()
			e.matchedKeys = append(e.matchedKeys, "s3://"+e.cfg.Bucket+"/"+key)
			e.matchedMu.Unlock()
		}
	}
	switch {
	case outcome.Err != nil:
		if runCancelled(ctx) {
			return
		}
		e.counters.AddError(outcome.ErrClass)
		e.warner.Warnf("s3://%s/%s: %s: %v", e.cfg.Bucket, d.Key, outcome.ErrClass, outcome.Err)
	case outcome.Partial:
		if runCancelled(ctx) {
			return // interrupted mid-scan; the run reports interruption
		}
		e.counters.ScannedPartially.Add(1)
		if outcome.ErrClass != ErrClassNone {
			e.counters.AddError(outcome.ErrClass)
		}
		e.warner.Warnf("s3://%s/%s: partially scanned: %s", e.cfg.Bucket, d.Key, outcome.PartialWhy)
	case outcome.StoppedEarly:
		// -l or -max-matches terminated the object by request: the
		// query was satisfied, but the object was not read to EOF
		// (M-06).
		e.counters.StoppedEarly.Add(1)
	default:
		if runCancelled(ctx) {
			return
		}
		e.counters.ScannedFully.Add(1)
	}
}

// runCancelled reports a plain cancellation of the run context (signal,
// writer failure). A deadline on the run context (-overall-timeout) is
// NOT a plain cancellation: objects cut off by it are still accounted
// as partial so the summary never under-reports.
func runCancelled(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled)
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

// regionHint appends actionable advice when an S3 error is the classic
// region-mismatch signature: the request was signed for (or sent to) a
// different region than the bucket's.
func regionHint(err error) string {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return ""
	}
	switch apiErr.ErrorCode() {
	case "IllegalLocationConstraintException", "PermanentRedirect", "AuthorizationHeaderMalformed":
		return " (the bucket lives in a different region than this request used; rerun with -region <bucket-region>)"
	}
	return ""
}

// classifyRequestError buckets GetObject failures (§10, H-05, M-10).
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
		case code == "InvalidObjectState":
			// Archived (or Intelligent-Tiering archive tier) with no
			// readable copy.
			return ErrClassArchived
		case code == "SlowDown" || code == "Throttling" || code == "ThrottlingException" ||
			code == "RequestLimitExceeded" || code == "TooManyRequestsException":
			return ErrClassThrottled
		case code == "AccessDenied" || strings.HasPrefix(code, "KMS."):
			// KMS denials count with access denials (§10).
			return ErrClassAccessDenied
		}
	}
	return ErrClassOther
}
