package scan

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func collectLines(t *testing.T, input string, maxLine int) (lines []string, truncs []bool) {
	t.Helper()
	it := NewLineIterator(strings.NewReader(input), maxLine)
	for {
		line, trunc, ok := it.Next()
		if !ok {
			break
		}
		lines = append(lines, string(line))
		truncs = append(truncs, trunc)
	}
	if err := it.Err(); err != nil {
		t.Fatalf("unexpected iterator error: %v", err)
	}
	return lines, truncs
}

func TestLineIteratorBasic(t *testing.T) {
	lines, _ := collectLines(t, "alpha\nbeta\ngamma\n", 1024)
	want := []string{"alpha", "beta", "gamma"}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d", len(lines), len(want))
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d: got %q want %q", i, lines[i], want[i])
		}
	}
}

func TestLineIteratorNoTrailingNewline(t *testing.T) {
	lines, _ := collectLines(t, "alpha\nfinal-no-newline", 1024)
	if len(lines) != 2 || lines[1] != "final-no-newline" {
		t.Fatalf("final line without newline not processed: %#v", lines)
	}
}

func TestLineIteratorCRLF(t *testing.T) {
	lines, _ := collectLines(t, "one\r\ntwo\r\n", 1024)
	if lines[0] != "one" || lines[1] != "two" {
		t.Fatalf("CRLF not normalized: %#v", lines)
	}
}

func TestLineIteratorEmptyInput(t *testing.T) {
	lines, _ := collectLines(t, "", 1024)
	if len(lines) != 0 {
		t.Fatalf("empty input produced lines: %#v", lines)
	}
}

// C-04: an oversized line is truncated at the boundary, the tail is
// drained, the line number still increments, and scanning continues.
func TestLineIteratorOversized(t *testing.T) {
	long := strings.Repeat("x", 300)
	input := "short\n" + long + "\nafter\n"
	it := NewLineIterator(strings.NewReader(input), 100)

	line, trunc, ok := it.Next()
	if !ok || trunc || string(line) != "short" {
		t.Fatalf("first line wrong: %q trunc=%v", line, trunc)
	}
	line, trunc, ok = it.Next()
	if !ok || !trunc {
		t.Fatalf("oversized line not flagged truncated")
	}
	if len(line) != 100 || string(line) != strings.Repeat("x", 100) {
		t.Fatalf("truncated line should be first 100 bytes, got %d", len(line))
	}
	if it.LineNo() != 2 {
		t.Fatalf("line number: got %d want 2", it.LineNo())
	}
	line, trunc, ok = it.Next()
	if !ok || trunc || string(line) != "after" {
		t.Fatalf("scanning did not continue after oversized line: %q ok=%v", line, ok)
	}
	if it.LineNo() != 3 {
		t.Fatalf("line number after oversized: got %d want 3", it.LineNo())
	}
}

// An oversized line larger than the internal buffer (forces multiple
// ErrBufferFull rounds during accumulation and draining).
func TestLineIteratorOversizedBeyondInternalBuffer(t *testing.T) {
	long := strings.Repeat("y", 300*1024) // > 64 KiB internal buffer
	lines, truncs := collectLines(t, long+"\ntail\n", 128*1024)
	if len(lines) != 2 {
		t.Fatalf("got %d lines want 2", len(lines))
	}
	if !truncs[0] || len(lines[0]) != 128*1024 {
		t.Fatalf("giant line not truncated at boundary: trunc=%v len=%d", truncs[0], len(lines[0]))
	}
	if lines[1] != "tail" {
		t.Fatalf("line after giant not scanned: %q", lines[1])
	}
}

func TestLineIteratorInvalidUTF8(t *testing.T) {
	input := []byte{0xff, 0xfe, 'a', '\n', 'b', '\n'}
	it := NewLineIterator(bytes.NewReader(input), 1024)
	line, _, ok := it.Next()
	if !ok || !bytes.Equal(line, []byte{0xff, 0xfe, 'a'}) {
		t.Fatalf("invalid UTF-8 line mishandled: %v", line)
	}
	line, _, ok = it.Next()
	if !ok || string(line) != "b" {
		t.Fatalf("second line lost")
	}
}

type failingReader struct {
	data []byte
	err  error
	pos  int
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.pos >= len(f.data) {
		return 0, f.err
	}
	n := copy(p, f.data[f.pos:])
	f.pos += n
	return n, nil
}

// A mid-stream failure surfaces through Err after the buffered data is
// returned, so callers can mark the object partially scanned.
func TestLineIteratorMidStreamError(t *testing.T) {
	boom := errors.New("connection reset")
	it := NewLineIterator(&failingReader{data: []byte("good\npartial"), err: boom}, 1024)

	line, _, ok := it.Next()
	if !ok || string(line) != "good" {
		t.Fatalf("first line: %q ok=%v", line, ok)
	}
	line, _, ok = it.Next()
	if !ok || string(line) != "partial" {
		t.Fatalf("data before failure should be returned: %q ok=%v", line, ok)
	}
	_, _, ok = it.Next()
	if ok {
		t.Fatal("iteration should stop after error")
	}
	if !errors.Is(it.Err(), boom) {
		t.Fatalf("Err: got %v want %v", it.Err(), boom)
	}
}

func TestLineIteratorEOFError(t *testing.T) {
	it := NewLineIterator(io.MultiReader(strings.NewReader("x\n")), 1024)
	it.Next()
	if _, _, ok := it.Next(); ok {
		t.Fatal("expected end of stream")
	}
	if it.Err() != nil {
		t.Fatalf("clean EOF must not report an error: %v", it.Err())
	}
}
