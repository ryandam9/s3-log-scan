package scan

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a goroutine-safe writer target so the test can read
// what the writer goroutine has flushed while it is still running.
type syncBuffer struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

// A match must reach the underlying writer shortly after it is
// emitted — not only when the run ends. (The bug: results sat in the
// 64 KiB buffer until Ctrl-C flushed them all at once.)
func TestWriterFlushesWhenIdle(t *testing.T) {
	var out syncBuffer
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := NewWriter(&out, 64, true, false, nil, cancel)

	w.Emit(context.Background(), Result{Bucket: "b", Key: "k.log", LineNo: 3, Line: []byte("hit")})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "s3://b/k.log:3: hit\n") {
			w.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	w.Close()
	t.Fatalf("match not flushed while writer still running; buffered until close. got: %q", out.String())
}
