package scan

// Fuzz targets for hostile log input (review L-07). `go test` always
// exercises the seed corpus; run `go test -fuzz=FuzzX` for exploration.

import (
	"bytes"
	"testing"
)

func FuzzLineIterator(f *testing.F) {
	f.Add([]byte("normal\nlines\n"), 64)
	f.Add([]byte("no trailing newline"), 8)
	f.Add([]byte("\r\n\r\n"), 4)
	f.Add(bytes.Repeat([]byte{0}, 300), 16)
	f.Add([]byte{0xff, 0xfe, '\n', 0x9b}, 4)
	f.Fuzz(func(t *testing.T, data []byte, maxLine int) {
		if maxLine < 1 || maxLine > 1<<16 {
			maxLine = 64
		}
		it := NewLineIterator(bytes.NewReader(data), maxLine)
		var lines, bytesSeen int
		for {
			line, _, ok := it.Next()
			if !ok {
				break
			}
			lines++
			bytesSeen += len(line)
			if len(line) > maxLine {
				t.Fatalf("line %d exceeds maxLine: %d > %d", lines, len(line), maxLine)
			}
			if lines > len(data)+1 {
				t.Fatalf("more lines than input bytes: %d", lines)
			}
		}
		if it.Err() != nil {
			t.Fatalf("in-memory reader must never error: %v", it.Err())
		}
		if bytesSeen > len(data) {
			t.Fatalf("returned more content than input: %d > %d", bytesSeen, len(data))
		}
	})
}

func FuzzSanitize(f *testing.F) {
	f.Add([]byte("plain"))
	f.Add([]byte{0x1b, '[', '3', '1', 'm'})
	f.Add([]byte{0x9b, 0xc2, 0x9b, 0xff})
	f.Add([]byte("bidi \u202e attack"))
	f.Fuzz(func(t *testing.T, data []byte) {
		out := Sanitize(data)
		if len(out) > len(data) {
			t.Fatalf("sanitized output grew: %d > %d", len(out), len(data))
		}
		if !clean(out) {
			t.Fatalf("Sanitize output still dangerous: %q", out)
		}
	})
}

func FuzzExtractAppIDs(f *testing.F) {
	f.Add([]byte("application_123_456"))
	f.Add([]byte("application_ application_1_ application_1_2"))
	f.Add([]byte{0xff, 0x00})
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = ExtractAllAppIDs(data) // must not panic
		_ = ExtractAppID(data)
	})
}
