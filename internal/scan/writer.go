package scan

import (
	"bufio"
	"context"
	"fmt"
	"io"
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

	done     chan struct{}
	mu       sync.Mutex
	writeErr error
}

// NewWriter starts the writer goroutine. cancel is invoked on the
// first write failure. Queue depth bounds queued output (§9).
func NewWriter(out io.Writer, queueDepth int, sanitize bool, cancel context.CancelFunc) *Writer {
	w := &Writer{
		ch:       make(chan Result, queueDepth),
		out:      bufio.NewWriterSize(out, 64*1024),
		cancel:   cancel,
		sanitize: sanitize,
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
	for r := range w.ch {
		if failed {
			continue // drain so no worker blocks on a dead pipe
		}
		if err := w.write(r); err != nil {
			w.mu.Lock()
			w.writeErr = err
			w.mu.Unlock()
			w.cancel()
			failed = true
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
