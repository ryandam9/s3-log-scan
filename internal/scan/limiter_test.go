package scan

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestMatchLimiterNilIsUnlimited(t *testing.T) {
	var l *matchLimiter
	for i := 0; i < 1000; i++ {
		if !l.Reserve() {
			t.Fatal("nil limiter must never limit")
		}
	}
	if l.Satisfied() {
		t.Fatal("nil limiter must never be satisfied")
	}
}

func TestMatchLimiterExactCap(t *testing.T) {
	l := newMatchLimiter(5)
	granted := 0
	for i := 0; i < 20; i++ {
		if l.Reserve() {
			granted++
		}
	}
	if granted != 5 {
		t.Fatalf("granted %d want 5", granted)
	}
	if !l.Satisfied() {
		t.Fatal("cap reached must satisfy")
	}
}

// Satisfaction triggers when the cap is REACHED (not only exceeded),
// so a run whose matches exactly equal the cap stops promptly, and the
// reservation that reached the cap is still granted.
func TestMatchLimiterSatisfiedAtExactly(t *testing.T) {
	l := newMatchLimiter(3)
	for i := 0; i < 3; i++ {
		if !l.Reserve() {
			t.Fatalf("reservation %d must be granted", i)
		}
	}
	if !l.Satisfied() {
		t.Fatal("nth reservation must satisfy the limiter")
	}
}

func TestMatchLimiterConcurrent(t *testing.T) {
	l := newMatchLimiter(100)
	var wg sync.WaitGroup
	var counter [16]int
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				if l.Reserve() {
					counter[g]++
				}
			}
		}(g)
	}
	wg.Wait()
	total := 0
	for _, n := range counter {
		total += n
	}
	if total != 100 {
		t.Fatalf("concurrent grants %d want exactly 100", total)
	}
}

// Engine end-to-end: exactly N lines print, the run stops early, the
// stop is a success (exit 0), and skipped work is never partial.
func TestEngineMaxTotalMatches(t *testing.T) {
	f := newFakeS3(1000)
	for i := 0; i < 10; i++ {
		f.put(fmt.Sprintf("logs/o%02d.log", i), strings.Repeat("ERROR hit\n", 5))
	}
	cfg := testConfig(t, "ERROR")
	cfg.MaxTotalMatches = 12
	res, out, stderr := runEngine(t, cfg, f)

	if got := strings.Count(out, "\n"); got != 12 {
		t.Fatalf("printed lines: %d want exactly 12\n%s", got, out)
	}
	if !res.MatchLimitHit {
		t.Fatal("MatchLimitHit must be set")
	}
	if res.Interrupted || res.TimedOut {
		t.Fatalf("cap stop misclassified: %+v", res)
	}
	if res.Counters.ScannedPartially.Load() != 0 {
		t.Fatalf("cap-stopped objects must not be partial: %d", res.Counters.ScannedPartially.Load())
	}
	if code := ExitCode(res); code != 0 {
		t.Fatalf("exit %d want 0 (requested early stop with matches)", code)
	}
	var summary strings.Builder
	PrintSummary(&summary, res, false, false)
	if !strings.Contains(summary.String(), "completed: -max-total-matches reached") {
		t.Fatalf("summary status missing:\n%s", summary.String())
	}
	_ = stderr
	if res.Counters.MatchedLines.Load() != 12 {
		t.Fatalf("matchedLines counter %d must equal printed 12", res.Counters.MatchedLines.Load())
	}
}

// A cap higher than the total match count changes nothing.
func TestEngineMaxTotalMatchesNotReached(t *testing.T) {
	f := newFakeS3(1000)
	f.put("logs/a.log", "ERROR one\nERROR two\n")
	cfg := testConfig(t, "ERROR")
	cfg.MaxTotalMatches = 100
	res, out, _ := runEngine(t, cfg, f)
	if strings.Count(out, "\n") != 2 || res.MatchLimitHit {
		t.Fatalf("unreached cap must not alter the run: lines=%d hit=%v", strings.Count(out, "\n"), res.MatchLimitHit)
	}
	if code := ExitCode(res); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// With -l, the cap counts matching objects (one reported name each).
func TestEngineMaxTotalMatchesNamesOnly(t *testing.T) {
	f := newFakeS3(1000)
	for i := 0; i < 8; i++ {
		f.put(fmt.Sprintf("logs/o%d.log", i), "ERROR hit\n")
	}
	cfg := testConfig(t, "ERROR")
	cfg.MaxTotalMatches = 3
	cfg.Scan.NamesOnly = true
	res, out, _ := runEngine(t, cfg, f)
	if got := strings.Count(out, "\n"); got != 3 {
		t.Fatalf("reported objects: %d want 3\n%s", got, out)
	}
	if !res.MatchLimitHit {
		t.Fatal("MatchLimitHit must be set")
	}
}

// The lister must stop paginating once the cap is hit: with tiny pages
// and matches early in the keyspace, far fewer keys are listed than
// exist.
func TestEngineMaxTotalMatchesStopsListing(t *testing.T) {
	f := newFakeS3(5)
	for i := 0; i < 500; i++ {
		f.put(fmt.Sprintf("logs/o%03d.log", i), "ERROR hit\n")
	}
	cfg := testConfig(t, "ERROR")
	cfg.MaxTotalMatches = 5
	cfg.Workers = 2
	res, _, _ := runEngine(t, cfg, f)
	if listed := res.Counters.Listed.Load(); listed >= 500 {
		t.Fatalf("listing did not stop early: listed %d of 500", listed)
	}
}
