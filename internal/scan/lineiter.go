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
// CRLF normalized. truncated reports that the line's content exceeded
// maxLineSize and only its first maxLineSize bytes are returned; the
// remainder was drained. Line-ending bytes never count against the
// budget, so a newline-terminated line whose content is exactly
// maxLineSize is not flagged. ok=false means end of stream (check Err
// for failures). The returned slice is only valid until the following
// Next call.
func (it *LineIterator) Next() (line []byte, truncated, ok bool) {
	if it.err != nil {
		return nil, false, false
	}
	it.buf = it.buf[:0]
	for {
		chunk, err := it.r.ReadSlice('\n')
		content := chunk
		if err == nil {
			// The chunk completes the line: strip the EOL before
			// budgeting so it never triggers truncation.
			content = chunk[:len(chunk)-1]
			switch {
			case len(content) > 0 && content[len(content)-1] == '\r':
				content = content[:len(content)-1]
			case len(content) == 0 && !truncated && len(it.buf) > 0 && it.buf[len(it.buf)-1] == '\r':
				// CRLF split across reads: the '\r' is already buffered.
				it.buf = it.buf[:len(it.buf)-1]
			}
		}
		if len(content) > 0 {
			room := it.maxLineSize - len(it.buf)
			switch {
			case room >= len(content):
				it.buf = append(it.buf, content...)
			case room > 0:
				it.buf = append(it.buf, content[:room]...)
				truncated = true
			default:
				truncated = true
			}
		}
		switch err {
		case nil:
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
