package scan

// Tests for the fixes from the 22 Jul 2026 code review: H-01 through
// H-04, M-01, M-02, M-06, M-11, M-12, and the classification additions.

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// zipBytesOrdered builds a ZIP with entries in a deterministic order.
func zipBytesOrdered(t *testing.T, names, contents []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i, name := range names {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(contents[i])); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// --- H-01: per-object timeout accounting ---

// A deadline that expires before any content is a timeout failure,
// never a scan.
func TestObjectTimeoutBeforeFirstLine(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	opts := grepOpts(t, "ERROR")
	opts.MaxLineSize = 1 << 20
	opts.TempDir = t.TempDir()
	var out bytes.Buffer
	_, wcancel := context.WithCancel(context.Background())
	defer wcancel()
	w := NewWriter(&out, WriterConfig{QueueDepth: 8, Sanitize: true}, wcancel)
	var c Counters
	desc := &ObjectDescriptor{Key: "l.log"}
	outcome := ScanObject(ctx, "b", desc, strings.NewReader("ERROR x\n"), FormatPlain, &opts, w, &c)
	w.Close()
	if outcome.Err == nil || outcome.ErrClass != ErrClassTimeout {
		t.Fatalf("expired deadline before scanning must fail with timeout: %+v", outcome)
	}
}

// H-01 mid-scan: a per-object deadline observed between lines marks
// the object partially scanned with a timeout classification — never
// fully scanned.
func TestObjectTimeoutMidScanMarkedPartial(t *testing.T) {
	// A real short deadline plus a slow reader: the deadline expires
	// while lines are still arriving.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	data := strings.Repeat("filler line\n", 10000)
	slow := &slowLineReader{data: []byte(data), delay: 2 * time.Millisecond}

	opts := grepOpts(t, "nomatch")
	opts.MaxLineSize = 1 << 20
	opts.TempDir = t.TempDir()
	var out bytes.Buffer
	_, wcancel := context.WithCancel(context.Background())
	w := NewWriter(&out, WriterConfig{QueueDepth: 8, Sanitize: true}, wcancel)
	var c Counters
	desc := &ObjectDescriptor{Key: "l.log"}
	outcome := ScanObject(ctx, "b", desc, slow, FormatPlain, &opts, w, &c)
	w.Close()

	if !outcome.Partial {
		t.Fatalf("mid-scan object timeout must be partial: %+v", outcome)
	}
	if outcome.ErrClass != ErrClassTimeout {
		t.Fatalf("class: %v", outcome.ErrClass)
	}
	if !strings.Contains(outcome.PartialWhy, "timeout") {
		t.Fatalf("why: %q", outcome.PartialWhy)
	}
}

type slowLineReader struct {
	data  []byte
	pos   int
	delay time.Duration
}

func (s *slowLineReader) Read(p []byte) (int, error) {
	if s.pos >= len(s.data) {
		return 0, fmt.Errorf("stream exhausted before deadline; slow down the reader")
	}
	time.Sleep(s.delay)
	end := s.pos + 32
	if end > len(s.data) {
		end = len(s.data)
	}
	n := copy(p, s.data[s.pos:end])
	s.pos += n
	return n, nil
}

// Engine-level H-01: an -object-timeout expiry surfaces as a partial
// scan and a timeout error, never as ScannedFully, and forces exit 3.
func TestEngineObjectTimeoutAccounted(t *testing.T) {
	f := newFakeS3(1000)
	o := f.put("logs/slow.log", strings.Repeat("line of text here\n", 5000))
	o.readDelay = 2 * time.Millisecond

	cfg := testConfig(t, "nomatch")
	cfg.ObjectTimeout = 30 * time.Millisecond
	res, _, _ := runEngine(t, cfg, f)

	if res.Counters.ScannedFully.Load() != 0 {
		t.Fatalf("timed-out object counted as fully scanned")
	}
	if res.Counters.ScannedPartially.Load() != 1 {
		t.Fatalf("partial: %d want 1", res.Counters.ScannedPartially.Load())
	}
	if res.Counters.Timeout.Load() != 1 {
		t.Fatalf("timeout counter: %d want 1", res.Counters.Timeout.Load())
	}
	if code := ExitCode(res); code != 3 {
		t.Fatalf("exit %d want 3", code)
	}
	if res.Interrupted || res.TimedOut {
		t.Fatalf("object timeout must not mark the run interrupted/timed out: %+v", res)
	}
}

// --- H-02: overall timeout vs signal ---

func TestEngineOverallTimeoutIsNotInterruption(t *testing.T) {
	f := newFakeS3(1000)
	for i := 0; i < 4; i++ {
		o := f.put(fmt.Sprintf("logs/slow%d.log", i), strings.Repeat("text line\n", 5000))
		o.readDelay = 2 * time.Millisecond
	}
	cfg := testConfig(t, "nomatch")
	cfg.OverallTimeout = 40 * time.Millisecond

	res, _, _ := runEngine(t, cfg, f)
	if !res.TimedOut {
		t.Fatalf("overall timeout must set TimedOut: %+v", res)
	}
	if res.Interrupted {
		t.Fatal("overall timeout must not be reported as an interruption")
	}
	if code := ExitCode(res); code != 3 {
		t.Fatalf("exit %d want 3 (timeout is a partial run, not 130)", code)
	}
}

func TestEngineExternalCancelIsInterruption(t *testing.T) {
	f := newFakeS3(1000)
	f.put("logs/a.log", "x\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	cfg := testConfig(t, "ERROR")
	cfg.OverallTimeout = time.Hour // present but not the cause
	e := NewEngine(cfg, f, &stderr)
	res := e.Run(ctx, &stdout)
	if !res.Interrupted || res.TimedOut {
		t.Fatalf("external cancel must be Interrupted, not TimedOut: %+v", res)
	}
	if code := ExitCode(res); code != 130 {
		t.Fatalf("exit %d want 130", code)
	}
}

// --- H-03 / M-11: application-ID attribution ---

// One object, two applications, both with matches: both IDs reported.
func TestMultipleAppIDsInOneObject(t *testing.T) {
	content := "submitting application_100_0001\n" +
		"ERROR table missing\n" +
		"submitting application_100_0002\n" +
		"ERROR access denied\n"
	r := runObjectScan(t, "j-1/steps/s-1/stderr", []byte(content), grepOpts(t, "ERROR"))
	want := []string{"application_100_0001", "application_100_0002"}
	if len(r.outcome.AppIDs) != 2 || r.outcome.AppIDs[0] != want[0] || r.outcome.AppIDs[1] != want[1] {
		t.Fatalf("AppIDs: %v want %v", r.outcome.AppIDs, want)
	}
}

// Two IDs on the matching line itself: both attributed.
func TestTwoIDsOnOneMatchingLine(t *testing.T) {
	content := "ERROR moving data from application_100_0007 to application_100_0008\n"
	r := runObjectScan(t, "j-1/steps/s-1/stderr", []byte(content), grepOpts(t, "ERROR"))
	if len(r.outcome.AppIDs) != 2 {
		t.Fatalf("both line IDs must be attributed: %v", r.outcome.AppIDs)
	}
}

// An ID appearing after the last match is unrelated and must not be
// attributed (the old final-state model got this wrong).
func TestLaterUnrelatedIDNotAttributed(t *testing.T) {
	content := "ERROR early failure with no app context\n" +
		"much later: cleaning up application_999_0042\n"
	r := runObjectScan(t, "j-1/steps/s-1/stderr", []byte(content), grepOpts(t, "ERROR"))
	if len(r.outcome.AppIDs) != 0 {
		t.Fatalf("later unrelated ID must not be attributed to an earlier match: %v", r.outcome.AppIDs)
	}
}

// M-11: preceding context in ZIP entry A must not attribute a match in
// entry B.
func TestZipEntryContextIsolation(t *testing.T) {
	body := zipBytesOrdered(t,
		[]string{"entryA", "entryB"},
		[]string{
			"running application_555_0001\nall fine here\n",
			"ERROR with no application context\n",
		})
	r := runObjectScan(t, "logs/bundle.zip", body, grepOpts(t, "ERROR"))
	if len(r.outcome.AppIDs) != 0 {
		t.Fatalf("entry A context leaked into entry B: %v", r.outcome.AppIDs)
	}
}

// The outer key ID still applies to every entry.
func TestZipKeyIDAppliesToAllEntries(t *testing.T) {
	body := zipBytesOrdered(t,
		[]string{"e1", "e2"},
		[]string{"ERROR one\n", "ERROR two\n"})
	key := "j-1/containers/application_777_0001/c1/logs.zip"
	r := runObjectScan(t, key, body, grepOpts(t, "ERROR"))
	if len(r.outcome.AppIDs) != 1 || r.outcome.AppIDs[0] != "application_777_0001" {
		t.Fatalf("key ID must attribute matches in every entry: %v", r.outcome.AppIDs)
	}
}

// --- H-04: archive restore state ---

func restoredStatus() *types.RestoreStatus {
	exp := time.Now().Add(24 * time.Hour)
	return &types.RestoreStatus{IsRestoreInProgress: aws.Bool(false), RestoreExpiryDate: &exp}
}

func TestEngineRestoredGlacierIsScanned(t *testing.T) {
	f := newFakeS3(1000)
	restored := f.put("logs/restored.log", "ERROR from restored glacier\n")
	restored.storageClass = "GLACIER"
	restored.restoreStatus = restoredStatus()

	inProgress := f.put("logs/thawing.log", "ERROR still frozen\n")
	inProgress.storageClass = "GLACIER"
	inProgress.restoreStatus = &types.RestoreStatus{IsRestoreInProgress: aws.Bool(true)}

	cold := f.put("logs/cold.log", "ERROR frozen\n")
	cold.storageClass = "DEEP_ARCHIVE"

	res, out, _ := runEngine(t, testConfig(t, "ERROR"), f)
	if !strings.Contains(out, "ERROR from restored glacier") {
		t.Fatalf("restored object must be scanned:\n%s", out)
	}
	if strings.Contains(out, "frozen") {
		t.Fatalf("unrestored objects must not be scanned:\n%s", out)
	}
	if res.Counters.ArchivedSkipped.Load() != 2 {
		t.Fatalf("archivedSkipped: %d want 2", res.Counters.ArchivedSkipped.Load())
	}
}

// Intelligent-Tiering archive tier: listing shows a readable class, the
// GET fails with InvalidObjectState → dedicated classification.
func TestEngineInvalidObjectStateClassified(t *testing.T) {
	f := newFakeS3(1000)
	o := f.put("logs/it-archived.log", "ERROR hidden\n")
	o.storageClass = "INTELLIGENT_TIERING"
	o.getErr = &smithy.GenericAPIError{Code: "InvalidObjectState", Message: "not accessible"}

	res, _, stderr := runEngine(t, testConfig(t, "ERROR"), f)
	if res.Counters.ArchivedUnavailable.Load() != 1 {
		t.Fatalf("archivedUnavailable: %d", res.Counters.ArchivedUnavailable.Load())
	}
	if !strings.Contains(stderr, "no restored copy") {
		t.Fatalf("warning:\n%s", stderr)
	}
	if code := ExitCode(res); code != 3 {
		t.Fatalf("exit %d want 3", code)
	}
}

// --- M-10: throttling classification ---

func TestEngineThrottledClassified(t *testing.T) {
	f := newFakeS3(1000)
	f.put("logs/busy.log", "x\n").getErr = &smithy.GenericAPIError{Code: "SlowDown", Message: "reduce request rate"}
	res, _, _ := runEngine(t, testConfig(t, "ERROR"), f)
	if res.Counters.Throttled.Load() != 1 {
		t.Fatalf("throttled: %d", res.Counters.Throttled.Load())
	}
}

// --- M-01: ZIP expansion boundary ---

func TestZipExpansionExactLimitSucceeds(t *testing.T) {
	c1 := strings.Repeat("a", 499) + "\n" // 500 bytes
	c2 := strings.Repeat("b", 499) + "\n" // 500 bytes
	body := zipBytesOrdered(t, []string{"e1", "e2"}, []string{c1, c2})
	opts := grepOpts(t, "nomatch")
	opts.MaxZipExpandedBytes = 1000 // exactly the expanded total
	r := runObjectScan(t, "logs/exact.zip", body, opts)
	if r.outcome.Partial {
		t.Fatalf("expansion exactly at the limit must succeed: %+v", r.outcome)
	}
}

func TestZipExpansionOneByteOverLimit(t *testing.T) {
	body := zipBytesOrdered(t, []string{"e1"}, []string{strings.Repeat("a", 1000) + "\n"}) // 1001 bytes
	opts := grepOpts(t, "nomatch")
	opts.MaxZipExpandedBytes = 1000
	r := runObjectScan(t, "logs/over.zip", body, opts)
	if !r.outcome.Partial || !strings.Contains(r.outcome.PartialWhy, "expansion") {
		t.Fatalf("one byte over the budget must be partial: %+v", r.outcome)
	}
}

// --- M-02: exact max-line-size boundary ---

func TestLineExactlyAtLimitNotTruncated(t *testing.T) {
	line := strings.Repeat("x", 100)
	for _, eol := range []string{"\n", "\r\n"} {
		lines, truncs := collectLines(t, line+eol+"next"+eol, 100)
		if len(lines) != 2 || lines[0] != line {
			t.Fatalf("eol %q: lines %v", eol, len(lines))
		}
		if truncs[0] {
			t.Fatalf("eol %q: content exactly at the limit must not be flagged truncated", eol)
		}
	}
}

func TestLineOneByteOverLimitTruncated(t *testing.T) {
	line := strings.Repeat("x", 101)
	lines, truncs := collectLines(t, line+"\n", 100)
	if !truncs[0] || len(lines[0]) != 100 {
		t.Fatalf("101 bytes vs limit 100: trunc=%v len=%d", truncs[0], len(lines[0]))
	}
}

// --- M-06: early-exit accounting ---

func TestStoppedEarlyOutcomeAndCounter(t *testing.T) {
	opts := grepOpts(t, "ERROR")
	opts.MaxMatches = 1
	r := runObjectScan(t, "l.log", []byte("ERROR a\nERROR b\n"), opts)
	if !r.outcome.StoppedEarly {
		t.Fatalf("max-matches exit must report StoppedEarly: %+v", r.outcome)
	}
	if r.outcome.Partial {
		t.Fatal("requested early exit is not a partial scan")
	}

	f := newFakeS3(1000)
	f.put("logs/multi.log", "ERROR 1\nERROR 2\n")
	f.put("logs/single.log", "ERROR only\n")
	cfg := testConfig(t, "ERROR")
	cfg.Scan.MaxMatches = 1
	res, _, stderr := runEngine(t, cfg, f)
	if res.Counters.StoppedEarly.Load() != 2 {
		t.Fatalf("stoppedEarly: %d want 2 (both objects hit the cap at match 1)", res.Counters.StoppedEarly.Load())
	}
	if code := ExitCode(res); code != 0 {
		t.Fatalf("early exit is a success: exit %d", code)
	}
	_ = stderr
}

// A plain full read reports neither StoppedEarly nor Partial.
func TestFullScanNotStoppedEarly(t *testing.T) {
	r := runObjectScan(t, "l.log", []byte("ERROR a\nERROR b\n"), grepOpts(t, "ERROR"))
	if r.outcome.StoppedEarly || r.outcome.Partial {
		t.Fatalf("full read misclassified: %+v", r.outcome)
	}
}

// --- M-12: writer enqueue accounting ---

// When stdout dies mid-run, match counters must stop growing shortly
// after the writer cancels: matches are counted only after the writer
// accepts them, and the cancellation check stops the scan.
func TestEmitFailureStopsCounting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The underlying writer fails on the first flush (~64 KiB of
	// output), which cancels ctx.
	w := NewWriter(&failAfterWriter{limit: 0}, WriterConfig{QueueDepth: 4, Sanitize: true}, cancel)

	opts := grepOpts(t, "ERROR")
	opts.MaxLineSize = 1 << 20
	opts.TempDir = t.TempDir()
	var c Counters
	desc := &ObjectDescriptor{Key: "l.log"}
	line := "ERROR " + strings.Repeat("p", 1017) // ~1 KiB per output line
	const total = 300
	outcome := ScanObject(ctx, "b", desc, strings.NewReader(strings.Repeat(line+"\n", total)), FormatPlain, &opts, w, &c)
	if err := w.Close(); err == nil {
		t.Fatal("writer must report the flush failure")
	}
	if got := c.MatchedLines.Load(); got >= total {
		t.Fatalf("counting did not stop after output death: %d of %d", got, total)
	}
	if outcome.Matches != c.MatchedLines.Load() {
		t.Fatalf("outcome (%d) and counter (%d) disagree", outcome.Matches, c.MatchedLines.Load())
	}
}

// --- boundary regex (L-05) ---

func TestAppIDBoundary(t *testing.T) {
	if got := ExtractAppIDString("myapplication_1_2 embedded"); got != "" {
		t.Fatalf("embedded prefix must not match: %q", got)
	}
	if got := ExtractAppIDString("wrote application_1_2_1.log today"); got != "application_1_2" {
		t.Fatalf("aggregated-log suffix: %q", got)
	}
	all := ExtractAllAppIDs([]byte("application_1_1 then application_1_2"))
	if len(all) != 2 {
		t.Fatalf("FindAll: %v", all)
	}
}
