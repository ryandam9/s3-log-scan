package scan

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:               "0 B",
		512:             "512 B",
		1024:            "1.0 KiB",
		1536:            "1.5 KiB",
		1 << 20:         "1.0 MiB",
		128 << 20:       "128.0 MiB",
		3 << 30:         "3.0 GiB",
		1_500_000_000_0: "14.0 GiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// -progress: periodic status lines reach stderr while the run is
// underway, and they carry the moving counters.
func TestEngineProgressLines(t *testing.T) {
	f := newFakeS3(1000)
	for i := 0; i < 6; i++ {
		o := f.put(fmt.Sprintf("logs/slow%d.log", i), strings.Repeat("some text line\n", 2000))
		o.readDelay = 2 * time.Millisecond
	}
	cfg := testConfig(t, "nomatch")
	cfg.Progress = 100 * time.Millisecond
	cfg.Workers = 2

	res, _, stderr := runEngine(t, cfg, f)
	if got := strings.Count(stderr, "progress "); got < 1 {
		t.Fatalf("expected at least one progress line:\n%s", stderr)
	}
	for _, field := range []string{"keys ", "kept ", "done ", "queue ", "match ", "dl ", "err "} {
		if !strings.Contains(stderr, field) {
			t.Fatalf("progress line missing %q:\n%s", field, stderr)
		}
	}
	// Fixed-width columns: every progress line must put each label at
	// the same byte offset, so successive lines align vertically.
	var offsets map[string]int
	for _, line := range strings.Split(stderr, "\n") {
		i := strings.Index(line, "progress ")
		if i < 0 {
			continue
		}
		line = line[i:]
		cur := map[string]int{}
		for _, label := range []string{"keys ", "kept ", "done ", "queue ", "match ", "dl ", "err "} {
			cur[label] = strings.Index(line, label)
		}
		if offsets == nil {
			offsets = cur
			continue
		}
		for label, off := range cur {
			if off != offsets[label] {
				t.Fatalf("column %q drifted between progress lines (%d vs %d):\n%s", label, off, offsets[label], stderr)
			}
		}
	}
	if code := ExitCode(res); code != 1 {
		t.Fatalf("exit %d want 1 (no matches, no errors)", code)
	}
}

// Progress must not print anything when disabled (the default).
func TestEngineNoProgressByDefault(t *testing.T) {
	f := newFakeS3(1000)
	f.put("logs/a.log", "hello\n")
	res, _, stderr := runEngine(t, testConfig(t, "nomatch"), f)
	if strings.Contains(stderr, "progress ") {
		t.Fatalf("progress lines without -progress:\n%s", stderr)
	}
	_ = res
}

// -verbose: listing pages and per-object scan starts are logged, and
// none of it lands on stdout.
func TestEngineVerbose(t *testing.T) {
	f := newFakeS3(2) // small pages to exercise the per-page log
	for i := 0; i < 5; i++ {
		f.put(fmt.Sprintf("logs/o%d.log", i), "ERROR hit\n")
	}
	cfg := testConfig(t, "ERROR")
	cfg.Verbose = true
	res, out, stderr := runEngine(t, cfg, f)

	if got := strings.Count(stderr, "listed page of"); got != 3 {
		t.Fatalf("page logs: %d want 3 (5 keys at page size 2)\n%s", got, stderr)
	}
	if got := strings.Count(stderr, "scanning s3://b/logs/"); got != 5 {
		t.Fatalf("scan-start logs: %d want 5\n%s", got, stderr)
	}
	// stdout must stay matches-only.
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if !strings.HasPrefix(line, "s3://b/logs/o") || !strings.Contains(line, "ERROR hit") {
			t.Fatalf("non-match content on stdout: %q", line)
		}
	}
	if code := ExitCode(res); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// Progress with list-only mode uses the reduced line.
func TestProgressLineListOnly(t *testing.T) {
	cfg := testConfig(t, "")
	e := NewEngine(cfg, newFakeS3(10), &strings.Builder{})
	line := e.progressLine(3 * time.Second)
	if !strings.Contains(line, "reported") || strings.Contains(line, "dl ") {
		t.Fatalf("list-only progress line wrong: %q", line)
	}
	if !strings.HasPrefix(line, "00:00:03 ") {
		t.Fatalf("elapsed must be fixed-width hh:mm:ss: %q", line)
	}
}

func TestFormatElapsed(t *testing.T) {
	cases := map[time.Duration]string{
		0:                "00:00:00",
		5 * time.Second:  "00:00:05",
		61 * time.Second: "00:01:01",
		time.Hour + 2*time.Minute + 3*time.Second: "01:02:03",
		25 * time.Hour: "25:00:00",
	}
	for in, want := range cases {
		if got := formatElapsed(in); got != want {
			t.Errorf("formatElapsed(%v) = %q, want %q", in, got, want)
		}
	}
}
