package scan

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestMatcherSpans(t *testing.T) {
	re := mustMatcher(t, `ERR\w+`, false, false)
	spans := re.Spans([]byte("an ERROR and an ERRATUM"))
	if len(spans) != 2 || spans[0] != [2]int{3, 8} || spans[1] != [2]int{16, 23} {
		t.Fatalf("regex spans: %v", spans)
	}

	fixed := mustMatcher(t, "ab", true, false)
	spans = fixed.Spans([]byte("ab ab AB"))
	if len(spans) != 2 || spans[0] != [2]int{0, 2} || spans[1] != [2]int{3, 5} {
		t.Fatalf("fixed spans: %v", spans)
	}

	fold := mustMatcher(t, "ab", true, true)
	spans = fold.Spans([]byte("ab AB"))
	if len(spans) != 2 || spans[1] != [2]int{3, 5} {
		t.Fatalf("fold spans: %v", spans)
	}
}

func colorWriterOutput(t *testing.T, grep *Matcher, r Result) string {
	t.Helper()
	var out bytes.Buffer
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := NewWriter(&out, 8, true, true, grep, cancel)
	w.Emit(context.Background(), r)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestColorizedMatchLine(t *testing.T) {
	grep := mustMatcher(t, "kyneton", false, true)
	got := colorWriterOutput(t, grep, Result{
		Bucket: "b",
		Key:    "photos/list.txt",
		LineNo: 7,
		Line:   []byte("trip to Kyneton in spring"),
	})
	want := ansiKey + "s3://b/photos/list.txt" + ansiReset +
		ansiSep + ":" + ansiReset +
		ansiLineNo + "7" + ansiReset +
		ansiSep + ":" + ansiReset + " " +
		"trip to " + ansiMatch + "Kyneton" + ansiReset + " in spring\n"
	if got != want {
		t.Fatalf("colored line:\n%q\nwant:\n%q", got, want)
	}
}

func TestColorizedZipEntry(t *testing.T) {
	grep := mustMatcher(t, "x", true, false)
	got := colorWriterOutput(t, grep, Result{
		Bucket: "b", Key: "a.zip", ZipEntry: "inner.log", LineNo: 2, Line: []byte("x"),
	})
	if !strings.Contains(got, ansiZip+"inner.log"+ansiReset) {
		t.Fatalf("zip entry not tinted: %q", got)
	}
}

func TestColorizedKeyOnly(t *testing.T) {
	got := colorWriterOutput(t, nil, Result{Bucket: "b", Key: "k.log", KeyOnly: true})
	if got != ansiKey+"s3://b/k.log"+ansiReset+"\n" {
		t.Fatalf("key-only: %q", got)
	}
}

// Multiple occurrences on one line are each highlighted.
func TestColorizedMultipleMatches(t *testing.T) {
	grep := mustMatcher(t, "hit", true, false)
	got := colorWriterOutput(t, grep, Result{Bucket: "b", Key: "k", LineNo: 1, Line: []byte("hit and hit")})
	if strings.Count(got, ansiMatch+"hit"+ansiReset) != 2 {
		t.Fatalf("both occurrences must be highlighted: %q", got)
	}
}

// With color off (the default in every other test), output stays
// byte-identical to the documented format — no ANSI anywhere.
func TestNoColorByDefault(t *testing.T) {
	r := runObjectScan(t, "l.log", []byte("ERROR x\n"), grepOpts(t, "ERROR"))
	if strings.Contains(r.output, "\x1b[") {
		t.Fatalf("ANSI in uncolored output: %q", r.output)
	}
}

// Sanitization runs before coloring: hostile escape bytes in content
// are neutralized, ours are the only ANSI sequences present.
func TestColorAfterSanitize(t *testing.T) {
	grep := mustMatcher(t, "evil", true, false)
	got := colorWriterOutput(t, grep, Result{
		Bucket: "b", Key: "k", LineNo: 1, Line: []byte("\x1b[31mevil\x1b[0m"),
	})
	if !strings.Contains(got, "?[31m"+ansiMatch+"evil"+ansiReset+"?[0m") {
		t.Fatalf("content escapes must be sanitized before coloring: %q", got)
	}
}

func TestSummaryColor(t *testing.T) {
	var sb strings.Builder
	res := &RunResult{Counters: &Counters{}, AppIDs: NewAppIDSet()}
	PrintSummary(&sb, res, false, true)
	if !strings.Contains(sb.String(), "\x1b[32mcompleted"+ansiReset) {
		t.Fatalf("status not tinted:\n%q", sb.String())
	}
	sb.Reset()
	PrintSummary(&sb, res, false, false)
	if strings.Contains(sb.String(), "\x1b[") {
		t.Fatalf("uncolored summary has ANSI:\n%q", sb.String())
	}
}
