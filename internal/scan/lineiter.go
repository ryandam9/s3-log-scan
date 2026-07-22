package scan

import (
	"bufio"
	"bytes"
	"io"
)

// LineIterator replaces the fixed-limit bufio.Scanner (C-04). A line
// longer than maxLineSize never terminates the scan: the head is
// returned truncated, the tail is drained without allocation, the line
// number still increments, and iteration continues to end of stream.
type LineIterator struct {
	r           *bufio.Reader
	maxLineSize int
	buf         []byte
	lineNo      int64
	err         error
}

const lineReaderBufSize = 64 * 1024

// NewLineIterator wraps r. maxLineSize is the truncation boundary in
// bytes (must be > 0; enforced by flag validation).
func NewLineIterator(r io.Reader, maxLineSize int) *LineIterator {
	return &LineIterator{
		r:           bufio.NewReaderSize(r, lineReaderBufSize),
		maxLineSize: maxLineSize,
		buf:         make([]byte, 0, 4096),
	}
}

// LineNo returns the 1-based number of the line most recently returned
// by Next.
func (it *LineIterator) LineNo() int64 { return it.lineNo }

// Err returns the first non-EOF error encountered by the underlying
// reader. A truncated stream (e.g. a mid-download failure) surfaces
// here so the caller can mark the object partially scanned.
func (it *LineIterator) Err() error { return it.err }

// Next returns the next line with the trailing newline removed and
// CRLF normalized. truncated reports that the line exceeded maxLineSize
// and only its first maxLineSize bytes are returned; the remainder was
// drained. ok=false means end of stream (check Err for failures).
// The returned slice is only valid until the following Next call.
func (it *LineIterator) Next() (line []byte, truncated, ok bool) {
	if it.err != nil {
		return nil, false, false
	}
	it.buf = it.buf[:0]
	for {
		chunk, err := it.r.ReadSlice('\n')
		if len(chunk) > 0 {
			room := it.maxLineSize - len(it.buf)
			if room > 0 {
				if len(chunk) <= room {
					it.buf = append(it.buf, chunk...)
				} else {
					it.buf = append(it.buf, chunk[:room]...)
					truncated = true
				}
			} else {
				truncated = true
			}
		}
		switch err {
		case nil:
			if it.buf[len(it.buf)-1] == '\n' {
				// Complete line within the buffer window.
				it.lineNo++
				return trimEOL(it.buf), truncated, true
			}
			// Newline arrived but fell in the drained tail of an
			// oversized line.
			it.lineNo++
			return it.buf, truncated, true
		case bufio.ErrBufferFull:
			// Line continues; loop to accumulate or drain.
			continue
		case io.EOF:
			if len(it.buf) == 0 && !truncated {
				return nil, false, false
			}
			// Final line without a trailing newline is processed.
			it.lineNo++
			return trimEOL(it.buf), truncated, true
		default:
			it.err = err
			if len(it.buf) > 0 || truncated {
				// Hand back what was read before the failure.
				it.lineNo++
				return trimEOL(it.buf), truncated, true
			}
			return nil, false, false
		}
	}
}

// trimEOL removes a trailing "\n" and normalizes CRLF by also removing
// a trailing "\r".
func trimEOL(b []byte) []byte {
	b = bytes.TrimSuffix(b, []byte("\n"))
	b = bytes.TrimSuffix(b, []byte("\r"))
	return b
}
