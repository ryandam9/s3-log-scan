package scan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// fakeObject is one object in the stub S3.
type fakeObject struct {
	body          []byte
	lastModified  time.Time
	storageClass  string
	restoreStatus *types.RestoreStatus
	etag          string
	getErr        error         // forced GetObject failure
	liveETag      string        // if set and != etag, GET returns 412
	readDelay     time.Duration // per-Read delay on the response body
}

// slowReadCloser dribbles data out with a delay per read, so timeouts
// can expire mid-scan deterministically.
type slowReadCloser struct {
	data  []byte
	pos   int
	delay time.Duration
}

func (s *slowReadCloser) Read(p []byte) (int, error) {
	if s.pos >= len(s.data) {
		return 0, io.EOF
	}
	time.Sleep(s.delay)
	end := s.pos + 64
	if end > len(s.data) {
		end = len(s.data)
	}
	n := copy(p, s.data[s.pos:end])
	s.pos += n
	return n, nil
}

func (s *slowReadCloser) Close() error { return nil }

// fakeS3 implements S3API with configurable pagination.
type fakeS3 struct {
	mu       sync.Mutex
	objects  map[string]*fakeObject
	pageSize int
	listErr  error // fail listing after the first page
	getCalls int
}

func newFakeS3(pageSize int) *fakeS3 {
	return &fakeS3{objects: make(map[string]*fakeObject), pageSize: pageSize}
}

func (f *fakeS3) put(key, body string) *fakeObject {
	o := &fakeObject{body: []byte(body), lastModified: time.Now(), etag: `"` + key + `-v1"`}
	f.objects[key] = o
	return o
}

func (f *fakeS3) sortedKeys(prefix string) []string {
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func (f *fakeS3) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := aws.ToString(in.Prefix)
	keys := f.sortedKeys(prefix)

	start := 0
	if tok := aws.ToString(in.ContinuationToken); tok != "" {
		if f.listErr != nil {
			return nil, f.listErr
		}
		start, _ = strconv.Atoi(tok)
	}
	end := start + f.pageSize
	if end > len(keys) {
		end = len(keys)
	}
	out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(end < len(keys))}
	if end < len(keys) {
		out.NextContinuationToken = aws.String(strconv.Itoa(end))
	}
	for _, k := range keys[start:end] {
		o := f.objects[k]
		obj := types.Object{
			Key:          aws.String(k),
			Size:         aws.Int64(int64(len(o.body))),
			LastModified: aws.Time(o.lastModified),
			ETag:         aws.String(o.etag),
		}
		if o.storageClass != "" {
			obj.StorageClass = types.ObjectStorageClass(o.storageClass)
		}
		obj.RestoreStatus = o.restoreStatus
		out.Contents = append(out.Contents, obj)
	}
	return out, nil
}

func (f *fakeS3) GetObject(ctx context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mu.Lock()
	f.getCalls++
	o, exists := f.objects[aws.ToString(in.Key)]
	f.mu.Unlock()
	if !exists {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "not found"}
	}
	if o.getErr != nil {
		return nil, o.getErr
	}
	if o.liveETag != "" && o.liveETag != aws.ToString(in.IfMatch) {
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "at least one of the pre-conditions you specified did not hold"}
	}
	if o.readDelay > 0 {
		return &s3.GetObjectOutput{Body: &slowReadCloser{data: o.body, delay: o.readDelay}}, nil
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(o.body))}, nil
}

func testConfig(t *testing.T, grep string) *Config {
	t.Helper()
	cfg := &Config{
		Bucket:              "b",
		Prefix:              "logs/",
		Workers:             4,
		ZipWorkers:          2,
		SmallestFirstWindow: 100,
		SanitizeOutput:      true,
		MaxWarnings:         100,
	}
	cfg.Scan.MaxLineSize = 1 << 20
	cfg.Scan.MaxZipEntries = 10000
	cfg.Scan.MaxZipExpandedBytes = 512 << 20
	cfg.Scan.TempDir = t.TempDir()
	if grep == "" {
		cfg.ListOnly = true
	} else {
		m, err := NewMatcher(grep, false, false)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Scan.Grep = m
	}
	return cfg
}

func runEngine(t *testing.T, cfg *Config, client S3API) (*RunResult, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	e := NewEngine(cfg, client, &stderr)
	res := e.Run(context.Background(), &stdout)
	e.Warner().Flush()
	return res, stdout.String(), stderr.String()
}

// Pagination boundaries: page size 1000 with 999/1000/1001 keys (§15).
func TestEnginePaginationBoundaries(t *testing.T) {
	for _, n := range []int{999, 1000, 1001} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			f := newFakeS3(1000)
			for i := 0; i < n; i++ {
				f.put(fmt.Sprintf("logs/obj-%04d.log", i), "ERROR hit\n")
			}
			cfg := testConfig(t, "ERROR")
			res, out, _ := runEngine(t, cfg, f)
			if got := res.Counters.Listed.Load(); got != int64(n) {
				t.Fatalf("listed %d want %d", got, n)
			}
			if got := strings.Count(out, "\n"); got != n {
				t.Fatalf("matched lines in output: %d want %d", got, n)
			}
			if code := ExitCode(res); code != 0 {
				t.Fatalf("exit code %d", code)
			}
		})
	}
}

func TestEngineListOnlyMode(t *testing.T) {
	f := newFakeS3(1000)
	f.put("logs/a.gz", "x")
	f.put("logs/b.gz", "y")
	cfg := testConfig(t, "")
	res, out, _ := runEngine(t, cfg, f)
	if f.getCalls != 0 {
		t.Fatalf("list-only mode must not download; %d GETs", f.getCalls)
	}
	if !strings.Contains(out, "s3://b/logs/a.gz") || !strings.Contains(out, "s3://b/logs/b.gz") {
		t.Fatalf("output:\n%s", out)
	}
	if code := ExitCode(res); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestEngineNoMatchesExitOne(t *testing.T) {
	f := newFakeS3(1000)
	f.put("logs/a.log", "nothing here\n")
	res, _, _ := runEngine(t, testConfig(t, "ERROR"), f)
	if code := ExitCode(res); code != 1 {
		t.Fatalf("exit %d want 1", code)
	}
}

// H-06: object changed between LIST and GET → 412 → counted, skipped.
func TestEngineChangedAfterListing(t *testing.T) {
	f := newFakeS3(1000)
	o := f.put("logs/changed.log", "ERROR old\n")
	o.liveETag = `"changed-v2"`
	f.put("logs/stable.log", "ERROR stable\n")

	res, out, stderr := runEngine(t, testConfig(t, "ERROR"), f)
	if res.Counters.ChangedAfterListing.Load() != 1 {
		t.Fatalf("changedAfterListing: %d", res.Counters.ChangedAfterListing.Load())
	}
	if !strings.Contains(stderr, "object changed after listing; not scanned") {
		t.Fatalf("warning missing:\n%s", stderr)
	}
	if strings.Contains(out, "ERROR old") {
		t.Fatal("stale content must never be printed")
	}
	if code := ExitCode(res); code != 3 {
		t.Fatalf("exit %d want 3 (object error present)", code)
	}
}

func TestEngineAccessDeniedAndKMS(t *testing.T) {
	f := newFakeS3(1000)
	f.put("logs/denied.log", "ERROR secret\n").getErr = &smithy.GenericAPIError{Code: "AccessDenied"}
	f.put("logs/kms.log", "ERROR kms\n").getErr = &smithy.GenericAPIError{Code: "KMS.AccessDeniedException"}
	f.put("logs/fine.log", "ERROR fine\n")

	res, out, _ := runEngine(t, testConfig(t, "ERROR"), f)
	if res.Counters.AccessDenied.Load() != 2 {
		t.Fatalf("accessDenied: %d", res.Counters.AccessDenied.Load())
	}
	if !strings.Contains(out, "ERROR fine") {
		t.Fatal("one denial must not stop the run")
	}
	if code := ExitCode(res); code != 3 {
		t.Fatalf("exit %d want 3", code)
	}
}

func TestEngineDeletedBetweenListAndGet(t *testing.T) {
	f := newFakeS3(1000)
	f.put("logs/gone.log", "ERROR gone\n").getErr = &smithy.GenericAPIError{Code: "NoSuchKey"}
	res, _, _ := runEngine(t, testConfig(t, "ERROR"), f)
	if res.Counters.NotFound.Load() != 1 {
		t.Fatalf("notFound: %d", res.Counters.NotFound.Load())
	}
}

// A listing failure mid-pagination is fatal: exit 2.
func TestEngineListingFailure(t *testing.T) {
	f := newFakeS3(2)
	for i := 0; i < 5; i++ {
		f.put(fmt.Sprintf("logs/o%d.log", i), "x\n")
	}
	f.listErr = &smithy.GenericAPIError{Code: "InternalError"}
	res, _, _ := runEngine(t, testConfig(t, "ERROR"), f)
	if res.ListingErr == nil {
		t.Fatal("listing error must surface")
	}
	if code := ExitCode(res); code != 2 {
		t.Fatalf("exit %d want 2", code)
	}
}

func TestEngineArchivedSkippedAtListing(t *testing.T) {
	f := newFakeS3(1000)
	f.put("logs/cold.gz", "ERROR frozen\n").storageClass = "GLACIER"
	f.put("logs/deep.gz", "ERROR deeper\n").storageClass = "DEEP_ARCHIVE"
	f.put("logs/warm.log", "ERROR warm\n")

	res, out, _ := runEngine(t, testConfig(t, "ERROR"), f)
	if res.Counters.ArchivedSkipped.Load() != 2 {
		t.Fatalf("archivedSkipped: %d", res.Counters.ArchivedSkipped.Load())
	}
	if f.getCalls != 1 {
		t.Fatalf("archived objects must never be fetched; %d GETs", f.getCalls)
	}
	if !strings.Contains(out, "ERROR warm") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestEngineSmallestFirst(t *testing.T) {
	f := newFakeS3(1000)
	f.put("logs/big.log", strings.Repeat("filler\n", 1000)+"ERROR big\n")
	f.put("logs/small.log", "ERROR small\n")
	cfg := testConfig(t, "ERROR")
	cfg.SmallestFirst = true
	cfg.Workers = 1 // deterministic dispatch order
	res, out, _ := runEngine(t, cfg, f)
	first := strings.SplitN(out, "\n", 2)[0]
	if !strings.Contains(first, "small") {
		t.Fatalf("smallest object should be scanned first:\n%s", out)
	}
	if res.Counters.MatchedObjects.Load() != 2 {
		t.Fatalf("matchedObjects: %d", res.Counters.MatchedObjects.Load())
	}
}

// End-to-end discovery: step log without ID in key; the summary set
// must carry the ID found in content.
func TestEngineAppIDDiscovery(t *testing.T) {
	f := newFakeS3(1000)
	f.put("logs/j-1/steps/s-1/stderr.gz", string(gzipBytesT(t,
		"running application_1700000000000_0123\nERROR: step failed\n")))
	f.put("logs/j-1/containers/application_1700000000000_0456/c1/stderr.log",
		"ERROR: container oops\n")

	res, _, _ := runEngine(t, testConfig(t, "ERROR"), f)
	ids := res.AppIDs.Sorted()
	want := []string{"application_1700000000000_0123", "application_1700000000000_0456"}
	if len(ids) != 2 || ids[0] != want[0] || ids[1] != want[1] {
		t.Fatalf("discovered IDs: %v want %v", ids, want)
	}
}

func TestEngineCancellation(t *testing.T) {
	f := newFakeS3(10)
	for i := 0; i < 200; i++ {
		f.put(fmt.Sprintf("logs/o%03d.log", i), "ERROR x\n")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before start: worst case
	var stdout, stderr bytes.Buffer
	cfg := testConfig(t, "ERROR")
	e := NewEngine(cfg, f, &stderr)
	done := make(chan *RunResult, 1)
	go func() { done <- e.Run(ctx, &stdout) }()
	select {
	case res := <-done:
		if !res.Interrupted {
			t.Fatalf("cancelled run must report interruption: %+v", res)
		}
		if code := ExitCode(res); code != 130 {
			t.Fatalf("exit %d want 130", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancellation deadlocked")
	}
}

// H-02: a failing stdout (EPIPE from `| head`) cancels the run; no
// deadlock, and the result reports the write failure.
func TestEngineBrokenPipe(t *testing.T) {
	f := newFakeS3(1000)
	// Enough output to overflow the writer's 64 KiB buffer many times,
	// forcing flushes (and the failure) while workers are still busy.
	line := "ERROR " + strings.Repeat("p", 200) + "\n"
	for i := 0; i < 50; i++ {
		f.put(fmt.Sprintf("logs/o%02d.log", i), strings.Repeat(line, 100))
	}
	cfg := testConfig(t, "ERROR")
	var stderr bytes.Buffer
	e := NewEngine(cfg, f, &stderr)
	done := make(chan *RunResult, 1)
	go func() { done <- e.Run(context.Background(), &failAfterWriter{limit: 3}) }()
	select {
	case res := <-done:
		if res.WriteErr == nil {
			t.Fatal("write failure must be reported")
		}
		if code := ExitCode(res); code != 3 {
			t.Fatalf("exit %d want 3", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("broken pipe deadlocked the engine")
	}
}

type failAfterWriter struct {
	mu    sync.Mutex
	n     int
	limit int
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.n++
	if w.n > w.limit {
		return 0, errors.New("broken pipe")
	}
	return len(p), nil
}

// Warning cap: only -max-warnings messages, then a suppression count.
func TestEngineWarningCap(t *testing.T) {
	f := newFakeS3(1000)
	for i := 0; i < 10; i++ {
		f.put(fmt.Sprintf("logs/deny%02d.log", i), "x\n").getErr = &smithy.GenericAPIError{Code: "AccessDenied"}
	}
	cfg := testConfig(t, "ERROR")
	cfg.MaxWarnings = 3
	res, _, stderr := runEngine(t, cfg, f)
	if got := strings.Count(stderr, "warning:"); got != 3 {
		t.Fatalf("warnings emitted: %d want 3\n%s", got, stderr)
	}
	if !strings.Contains(stderr, "7 further warnings were suppressed") {
		t.Fatalf("suppression trailer missing:\n%s", stderr)
	}
	if res.Counters.AccessDenied.Load() != 10 {
		t.Fatalf("all errors must still be counted: %d", res.Counters.AccessDenied.Load())
	}
}

func gzipBytesT(t *testing.T, content string) []byte {
	t.Helper()
	return gzipBytes(t, content)
}

// -md report support: with CollectMatchedKeys the run returns the
// sorted s3:// URIs of every object that matched, and only those.
func TestEngineCollectMatchedKeys(t *testing.T) {
	f := newFakeS3(1000)
	f.put("logs/b.log", "ERROR two\n")
	f.put("logs/a.log", "ERROR one\nERROR again\n")
	f.put("logs/c.log", "all quiet\n")
	cfg := testConfig(t, "ERROR")
	cfg.CollectMatchedKeys = true
	res, _, _ := runEngine(t, cfg, f)
	want := []string{"s3://b/logs/a.log", "s3://b/logs/b.log"}
	if len(res.MatchedKeys) != len(want) {
		t.Fatalf("MatchedKeys = %v, want %v", res.MatchedKeys, want)
	}
	for i := range want {
		if res.MatchedKeys[i] != want[i] {
			t.Fatalf("MatchedKeys = %v, want %v", res.MatchedKeys, want)
		}
	}
}

// Without the opt-in, no keys are retained.
func TestEngineMatchedKeysOffByDefault(t *testing.T) {
	f := newFakeS3(1000)
	f.put("logs/a.log", "ERROR one\n")
	res, _, _ := runEngine(t, testConfig(t, "ERROR"), f)
	if res.MatchedKeys != nil {
		t.Fatalf("MatchedKeys = %v, want nil", res.MatchedKeys)
	}
}
