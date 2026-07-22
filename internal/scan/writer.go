package scan

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// Result is one unit of output: either a matched line or a bare object
// key (names-only and list-only modes).
type Result struct {
	Bucket   string
	Key      string
	ZipEntry string // empty unless the match came from inside a ZIP
	LineNo   int64  // 0 for key-only results
	Line     []byte // nil for key-only results
	KeyOnly  bool
}

// Writer is the sole owner of stdout (§7.1, H-02). Workers send
// Results through a bounded channel; a single goroutine serializes,
// sanitizes, and writes them. On any write error — EPIPE from
// "| head", a closed redirect — the writer cancels the shared context,
// keeps draining its channel so no worker blocks, and records the
// failure so the run reports interruption instead of success.
type Writer struct {
	ch       chan Result
	out      *bufio.Writer
	cancel   context.CancelFunc
	sanitize bool
	color    bool
	grep     *Matcher // for highlight spans; nil in list-only mode

	done     chan struct{}
	mu       sync.Mutex
	writeErr error
}

// NewWriter starts the writer goroutine. cancel is invoked on the
// first write failure. Queue depth bounds queued output (§9). With
// color enabled, keys, line numbers, and separators are tinted and
// grep matches within the line are highlighted (grep is nil when
// there is no content pattern). Color is applied after sanitization,
// so scanned content can never inject sequences that look like ours.
func NewWriter(out io.Writer, queueDepth int, sanitize, color bool, grep *Matcher, cancel context.CancelFunc) *Writer {
	w := &Writer{
		ch:       make(chan Result, queueDepth),
		out:      bufio.NewWriterSize(out, 64*1024),
		cancel:   cancel,
		sanitize: sanitize,
		color:    color,
		grep:     grep,
		done:     make(chan struct{}),
	}
	go w.run()
	return w
}

// Emit queues r for output. It returns false if the run is being
// cancelled and the result was not accepted.
func (w *Writer) Emit(ctx context.Context, r Result) bool {
	select {
	case w.ch <- r:
		return true
	case <-ctx.Done():
		return false
	}
}

// Close stops accepting results, waits for the goroutine to finish
// writing, flushes, and returns the first write error, if any.
func (w *Writer) Close() error {
	close(w.ch)
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writeErr == nil {
		if err := w.out.Flush(); err != nil {
			w.writeErr = err
		}
	}
	return w.writeErr
}

func (w *Writer) run() {
	defer close(w.done)
	failed := false
	fail := func(err error) {
		w.mu.Lock()
		w.writeErr = err
		w.mu.Unlock()
		w.cancel()
		failed = true
	}
	for r := range w.ch {
		if failed {
			continue // drain so no worker blocks on a dead pipe
		}
		if err := w.write(r); err != nil {
			fail(err)
			continue
		}
		// Prompt output: flush whenever no further result is queued,
		// so matches appear as they are found instead of sitting in
		// the 64 KiB buffer until the run ends. Under a burst the
		// queue stays non-empty and writes still batch.
		if len(w.ch) == 0 {
			if err := w.out.Flush(); err != nil {
				fail(err)
			}
		}
	}
}

func (w *Writer) write(r Result) error {
	key := r.Key
	entry := r.ZipEntry
	line := r.Line
	if w.sanitize {
		key = SanitizeString(key)
		entry = SanitizeString(entry)
		line = Sanitize(line)
	}
	if !w.color {
		if r.KeyOnly {
			_, err := fmt.Fprintf(w.out, "s3://%s/%s\n", r.Bucket, key)
			return err
		}
		// Grep-style: s3://bucket/key[!zipEntry]:lineNo: text (§7.2)
		if entry != "" {
			if _, err := fmt.Fprintf(w.out, "s3://%s/%s!%s:%d: ", r.Bucket, key, entry, r.LineNo); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprintf(w.out, "s3://%s/%s:%d: ", r.Bucket, key, r.LineNo); err != nil {
				return err
			}
		}
		if _, err := w.out.Write(line); err != nil {
			return err
		}
		return w.out.WriteByte('\n')
	}

	if r.KeyOnly {
		_, err := fmt.Fprintf(w.out, "%ss3://%s/%s%s\n", ansiKey, r.Bucket, key, ansiReset)
		return err
	}
	var sb strings.Builder
	sb.WriteString(ansiKey + "s3://" + r.Bucket + "/" + key + ansiReset)
	if entry != "" {
		sb.WriteString(ansiSep + "!" + ansiReset + ansiZip + entry + ansiReset)
	}
	sb.WriteString(ansiSep + ":" + ansiReset)
	sb.WriteString(ansiLineNo + strconv.FormatInt(r.LineNo, 10) + ansiReset)
	sb.WriteString(ansiSep + ":" + ansiReset + " ")
	if _, err := w.out.WriteString(sb.String()); err != nil {
		return err
	}
	if err := w.writeHighlighted(line); err != nil {
		return err
	}
	return w.out.WriteByte('\n')
}

// writeHighlighted prints line with every pattern occurrence wrapped
// in the match color. Spans are computed on the sanitized text — the
// bytes actually printed.
func (w *Writer) writeHighlighted(line []byte) error {
	if w.grep == nil {
		_, err := w.out.Write(line)
		return err
	}
	last := 0
	for _, sp := range w.grep.Spans(line) {
		if sp[1] <= sp[0] {
			continue
		}
		if _, err := w.out.Write(line[last:sp[0]]); err != nil {
			return err
		}
		if _, err := w.out.WriteString(ansiMatch); err != nil {
			return err
		}
		if _, err := w.out.Write(line[sp[0]:sp[1]]); err != nil {
			return err
		}
		if _, err := w.out.WriteString(ansiReset); err != nil {
			return err
		}
		last = sp[1]
	}
	_, err := w.out.Write(line[last:])
	return err
}
