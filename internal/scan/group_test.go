package scan

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

func groupedWriterOutput(t *testing.T, color bool, emit func(*Writer)) string {
	t.Helper()
	var out bytes.Buffer
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := NewWriter(&out, WriterConfig{QueueDepth: 64, Sanitize: true, Color: color, Group: true}, cancel)
	emit(w)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// The heading prints once; matches are indented beneath it.
func TestGroupedBasic(t *testing.T) {
	deep := "a/b/c/d/e/f/very/deep/app.log"
	got := groupedWriterOutput(t, false, func(w *Writer) {
		ctx := context.Background()
		w.Emit(ctx, Result{Bucket: "b", Key: deep, LineNo: 44, Line: []byte("ERROR one")})
		w.Emit(ctx, Result{Bucket: "b", Key: deep, LineNo: 812, Line: []byte("ERROR two")})
		w.Emit(ctx, Result{Key: deep, GroupEnd: true})
	})
	want := "s3://b/" + deep + "\n" +
		"  44: ERROR one\n" +
		"  812: ERROR two\n"
	if got != want {
		t.Fatalf("grouped output:\n%q\nwant:\n%q", got, want)
	}
}

// Groups are atomic even when workers interleave results, and blocks
// are separated by a blank line.
func TestGroupedInterleavedObjects(t *testing.T) {
	got := groupedWriterOutput(t, false, func(w *Writer) {
		ctx := context.Background()
		w.Emit(ctx, Result{Bucket: "b", Key: "x/one.log", LineNo: 1, Line: []byte("m1")})
		w.Emit(ctx, Result{Bucket: "b", Key: "y/two.log", LineNo: 5, Line: []byte("m2")})
		w.Emit(ctx, Result{Bucket: "b", Key: "x/one.log", LineNo: 9, Line: []byte("m3")})
		w.Emit(ctx, Result{Key: "y/two.log", GroupEnd: true})
		w.Emit(ctx, Result{Bucket: "b", Key: "x/one.log", LineNo: 12, Line: []byte("m4")})
		w.Emit(ctx, Result{Key: "x/one.log", GroupEnd: true})
	})
	want := "s3://b/y/two.log\n  5: m2\n" +
		"\n" +
		"s3://b/x/one.log\n  1: m1\n  9: m3\n  12: m4\n"
	if got != want {
		t.Fatalf("interleaved grouping:\n%q\nwant:\n%q", got, want)
	}
	// Each key appears exactly once despite interleaving.
	if strings.Count(got, "x/one.log") != 1 || strings.Count(got, "y/two.log") != 1 {
		t.Fatalf("headings repeated:\n%s", got)
	}
}

// ZIP entries keep their entry name per line under the object heading.
func TestGroupedZipEntries(t *testing.T) {
	got := groupedWriterOutput(t, false, func(w *Writer) {
		ctx := context.Background()
		w.Emit(ctx, Result{Bucket: "b", Key: "logs.zip", ZipEntry: "inner.log", LineNo: 3, Line: []byte("hit")})
		w.Emit(ctx, Result{Key: "logs.zip", GroupEnd: true})
	})
	want := "s3://b/logs.zip\n  inner.log:3: hit\n"
	if got != want {
		t.Fatalf("zip grouping:\n%q\nwant:\n%q", got, want)
	}
}

// An object with no matches produces no heading.
func TestGroupedNoMatchesNoHeading(t *testing.T) {
	got := groupedWriterOutput(t, false, func(w *Writer) {
		w.Emit(context.Background(), Result{Key: "quiet.log", GroupEnd: true})
	})
	if got != "" {
		t.Fatalf("empty group must print nothing: %q", got)
	}
}

// A missing group-end marker (interrupted worker) must not lose
// buffered matches: they flush at close.
func TestGroupedFlushOnClose(t *testing.T) {
	got := groupedWriterOutput(t, false, func(w *Writer) {
		w.Emit(context.Background(), Result{Bucket: "b", Key: "cut.log", LineNo: 2, Line: []byte("found")})
		// no GroupEnd
	})
	if !strings.Contains(got, "s3://b/cut.log\n  2: found\n") {
		t.Fatalf("buffered match lost at close:\n%q", got)
	}
}

// Colored grouped output: magenta heading, green line numbers, match
// highlighting intact.
func TestGroupedColor(t *testing.T) {
	grep := mustMatcher(t, "hit", true, false)
	var out bytes.Buffer
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := NewWriter(&out, WriterConfig{QueueDepth: 8, Sanitize: true, Color: true, Group: true, Grep: grep}, cancel)
	ctx := context.Background()
	w.Emit(ctx, Result{Bucket: "b", Key: "k.log", LineNo: 7, Line: []byte("a hit here")})
	w.Emit(ctx, Result{Key: "k.log", GroupEnd: true})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	want := ansiKey + "s3://b/k.log" + ansiReset + "\n" +
		"  " + ansiLineNo + "7" + ansiReset + ansiSep + ":" + ansiReset + " " +
		"a " + ansiMatch + "hit" + ansiReset + " here\n"
	if got != want {
		t.Fatalf("colored group:\n%q\nwant:\n%q", got, want)
	}
}

// End-to-end: -group through the engine, headings unique per object.
func TestEngineGroupedOutput(t *testing.T) {
	f := newFakeS3(1000)
	for i := 0; i < 6; i++ {
		f.put(fmt.Sprintf("deep/a/b/c/d/o%d.log", i), "ERROR one\nnope\nERROR two\n")
	}
	cfg := testConfig(t, "ERROR")
	cfg.Prefix = "deep/"
	cfg.GroupOutput = true
	res, out, _ := runEngine(t, cfg, f)

	for i := 0; i < 6; i++ {
		key := fmt.Sprintf("deep/a/b/c/d/o%d.log", i)
		if got := strings.Count(out, key); got != 1 {
			t.Fatalf("key %s appears %d times, want exactly 1 (heading only)\n%s", key, got, out)
		}
	}
	if got := strings.Count(out, "  "); got < 12 {
		t.Fatalf("expected 12 indented match lines:\n%s", out)
	}
	if res.Counters.MatchedLines.Load() != 12 {
		t.Fatalf("matchedLines: %d", res.Counters.MatchedLines.Load())
	}
	if code := ExitCode(res); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// -group with -max-total-matches still prints exactly N lines.
func TestEngineGroupedWithTotalCap(t *testing.T) {
	f := newFakeS3(1000)
	for i := 0; i < 5; i++ {
		f.put(fmt.Sprintf("logs/o%d.log", i), strings.Repeat("ERROR hit\n", 4))
	}
	cfg := testConfig(t, "ERROR")
	cfg.GroupOutput = true
	cfg.MaxTotalMatches = 7
	res, out, _ := runEngine(t, cfg, f)
	if got := strings.Count(out, ": ERROR hit"); got != 7 {
		t.Fatalf("match lines: %d want exactly 7\n%s", got, out)
	}
	if !res.MatchLimitHit {
		t.Fatal("MatchLimitHit must be set")
	}
}
