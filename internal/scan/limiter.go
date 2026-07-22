package scan

import "sync/atomic"

// matchLimiter enforces -max-total-matches, the run-wide match cap.
//
// It deliberately does NOT cancel the run context when the cap is
// reached: cancellation would race with results already queued to the
// writer and lose found matches. Instead, workers atomically Reserve a
// slot before emitting each result — so at most max results are ever
// queued — and the satisfied flag stops new work everywhere it is
// polled (the lister, idle workers, and every scan loop via done()).
// Queued output then drains normally and exactly the reserved results
// print.
type matchLimiter struct {
	max       int64
	count     atomic.Int64
	satisfied atomic.Bool
}

// newMatchLimiter returns nil when max is 0 (unlimited); a nil limiter
// is valid and never limits.
func newMatchLimiter(max int64) *matchLimiter {
	if max <= 0 {
		return nil
	}
	return &matchLimiter{max: max}
}

// Reserve claims one output slot. It returns false when the cap is
// already exhausted; the caller must not emit. Reaching the cap sets
// the satisfied flag, but the reservation that reached it is still
// granted and its result still prints.
func (l *matchLimiter) Reserve() bool {
	if l == nil {
		return true
	}
	n := l.count.Add(1)
	if n > l.max {
		l.count.Add(-1)
		l.satisfied.Store(true)
		return false
	}
	if n == l.max {
		l.satisfied.Store(true)
	}
	return true
}

// Release returns a reserved slot after a failed emit (run teardown),
// keeping the reservation count equal to the results actually queued.
func (l *matchLimiter) Release() {
	if l != nil {
		l.count.Add(-1)
	}
}

// Satisfied reports that the cap has been reached and no new work
// should start.
func (l *matchLimiter) Satisfied() bool {
	return l != nil && l.satisfied.Load()
}
