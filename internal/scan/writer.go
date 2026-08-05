package scan

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Result is one unit of output: a matched line, a bare object key
// (names-only and list-only modes), or a group-end marker (grouped
// mode: the object finished, flush its block).
type Result struct {
	Bucket   string
	Key      string
	ZipEntry string // empty unless the match came from inside a ZIP
	LineNo   int64  // 0 for key-only results
	Line     []byte // nil for key-only results
	KeyOnly  bool
	GroupEnd bool // grouped mode: no more results for Key
}

// WriterConfig bundles the writer's rendering options.
type WriterConfig struct {
	QueueDepth int
	Sanitize   bool
	Color      bool
	Group      bool     // print each object key once as a heading
	Grep       *Matcher // for highlight spans; nil = no highlighting
	// Record observes every content match exactly once, in emission
	// order, from the writer goroutine (the -md report rebuilds its
	// own per-file structure from these instead of parsing rendered
	// output). Line bytes are safe to retain: workers copy each line
	// before emitting.
	Record func(Result)
}

// groupFlushBytes caps how much of one object's output is buffered in
// grouped mode before a segment is flushed early (repeating the
// heading). Bounds writer memory at roughly workers × this value.
const groupFlushBytes = 1 << 20

type groupBuf struct {
	lines []Result
	bytes int
}

// Writer is the sole owner of stdout (§7.1, H-02). Workers send
// Results through a bounded channel; a single goroutine serializes,
// sanitizes, and writes them. On any write error — EPIPE from
// "| head", a closed redirect — the writer cancels the shared context,
// keeps draining its channel so no worker blocks, and records the
// failure so the run reports interruption instead of success.
//
// In grouped mode each object's matches are buffered and printed as
// one block when the object finishes, so a deep key prints once as a
// heading instead of repeating on every line. Blocks from concurrent
// workers never interleave.
type Writer struct {
	ch       chan Result
	out      *bufio.Writer
	cancel   context.CancelFunc
	sanitize bool
	color    bool
	group    bool
	grep     *Matcher
	record   func(Result)

	groups       map[string]*groupBuf
	groupPrinted bool // a group heading has been printed (separator state)

	done     chan struct{}
	mu       sync.Mutex
	writeErr error
}

// NewWriter starts the writer goroutine. cancel is invoked on the
// first write failure. Color is applied after sanitization, so scanned
// content can never inject sequences that look like ours.
func NewWriter(out io.Writer, cfg WriterConfig, cancel context.CancelFunc) *Writer {
	w := &Writer{
		ch:       make(chan Result, cfg.QueueDepth),
		out:      bufio.NewWriterSize(out, 64*1024),
		cancel:   cancel,
		sanitize: cfg.Sanitize,
		color:    cfg.Color,
		group:    cfg.Group,
		grep:     cfg.Grep,
		record:   cfg.Record,
		groups:   make(map[string]*groupBuf),
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
	// Grouped mode: an interrupted worker may never send its
	// group-end marker; flush whatever was buffered so found matches
	// are never lost (deterministic order for the leftovers).
	if !failed && w.group {
		keys := make([]string, 0, len(w.groups))
		for k := range w.groups {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := w.flushGroup(k); err != nil {
				fail(err)
				return
			}
		}
	}
}

func (w *Writer) write(r Result) error {
	if w.record != nil && !r.KeyOnly && !r.GroupEnd {
		w.record(r)
	}
	if w.group && !r.KeyOnly {
		if r.GroupEnd {
			return w.flushGroup(r.Key)
		}
		g := w.groups[r.Key]
		if g == nil {
			g = &groupBuf{}
			w.groups[r.Key] = g
		}
		g.lines = append(g.lines, r)
		g.bytes += len(r.Line) + len(r.ZipEntry) + 32
		if g.bytes >= groupFlushBytes {
			// Segment flush: bound memory; the heading repeats for
			// the remainder of this object.
			return w.flushGroup(r.Key)
		}
		return nil
	}
	return w.writeSingle(r)
}

// flushGroup prints one object's buffered matches as a block: the key
// once as a heading, then each match indented, blocks separated by a
// blank line.
func (w *Writer) flushGroup(key string) error {
	g := w.groups[key]
	delete(w.groups, key)
	if g == nil || len(g.lines) == 0 {
		return nil
	}
	if w.groupPrinted {
		if err := w.out.WriteByte('\n'); err != nil {
			return err
		}
	}
	w.groupPrinted = true

	heading := key
	if w.sanitize {
		heading = SanitizeString(heading)
	}
	bucket := g.lines[0].Bucket
	if w.color {
		if _, err := fmt.Fprintf(w.out, "%ss3://%s/%s%s\n", ansiKey, bucket, heading, ansiReset); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(w.out, "s3://%s/%s\n", bucket, heading); err != nil {
			return err
		}
	}

	// Line numbers print as a fixed 6-character right-aligned field
	// (wider only if a file exceeds 999,999 lines), so the matched
	// text starts in the same column within and across blocks.
	const lineNoWidth = 6
	for _, r := range g.lines {
		entry := r.ZipEntry
		line := r.Line
		if w.sanitize {
			entry = SanitizeString(entry)
			line = Sanitize(line)
		}
		lineNo := strconv.FormatInt(r.LineNo, 10)
		pad := lineNoWidth - len(lineNo)
		if pad < 0 {
			pad = 0
		}
		var sb strings.Builder
		sb.WriteString("  ")
		if entry != "" {
			if w.color {
				sb.WriteString(ansiZip + entry + ansiReset + ansiSep + ":" + ansiReset)
			} else {
				sb.WriteString(entry + ":")
			}
		}
		sb.WriteString(strings.Repeat(" ", pad))
		if w.color {
			sb.WriteString(ansiLineNo + lineNo + ansiReset + ansiSep + ":" + ansiReset + " ")
		} else {
			sb.WriteString(lineNo + ": ")
		}
		if _, err := w.out.WriteString(sb.String()); err != nil {
			return err
		}
		if err := w.writeLine(line); err != nil {
			return err
		}
		if err := w.out.WriteByte('\n'); err != nil {
			return err
		}
	}
	return nil
}

// writeSingle renders the classic one-line-per-match format.
func (w *Writer) writeSingle(r Result) error {
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
	if err := w.writeLine(line); err != nil {
		return err
	}
	return w.out.WriteByte('\n')
}

// writeLine prints line, highlighting every pattern occurrence when
// color is on. Spans are computed on the sanitized text — the bytes
// actually printed.
func (w *Writer) writeLine(line []byte) error {
	if !w.color || w.grep == nil {
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
