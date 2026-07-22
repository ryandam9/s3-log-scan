package scan

import (
	"regexp"
	"sort"
	"sync"
)

// appIDPattern matches YARN application IDs wherever they appear.
var appIDPattern = regexp.MustCompile(`application_\d+_\d+`)

// ExtractAppID returns the first application ID in b, or "".
func ExtractAppID(b []byte) string {
	return string(appIDPattern.Find(b))
}

// ExtractAppIDString returns the first application ID in s, or "".
func ExtractAppIDString(s string) string {
	return appIDPattern.FindString(s)
}

// AppIDTracker implements the three-source discovery model (§8, C-01):
// the object key, the matching line itself, and preceding context (the
// most recent ID seen anywhere earlier in the object). Per-line ID
// scanning is enabled only when the key lacks an ID, so container-log
// scans pay nothing for it.
type AppIDTracker struct {
	keyID    string
	lastSeen string
	scanLine bool
}

// NewAppIDTracker prepares a tracker for one object.
func NewAppIDTracker(key string) *AppIDTracker {
	keyID := ExtractAppIDString(key)
	return &AppIDTracker{keyID: keyID, scanLine: keyID == ""}
}

// Observe must be called for every line read from the object (before
// deciding whether it matches). It is a no-op when the key already
// carries an ID.
func (t *AppIDTracker) Observe(line []byte) {
	if !t.scanLine {
		return
	}
	if id := ExtractAppID(line); id != "" {
		t.lastSeen = id
	}
}

// Current returns the best ID known so far, in priority order:
// key, then most recent line/context sighting.
func (t *AppIDTracker) Current() string {
	if t.keyID != "" {
		return t.keyID
	}
	return t.lastSeen
}

// AppIDSet collects the deduplicated set of discovered IDs across the
// whole run for the summary.
type AppIDSet struct {
	mu  sync.Mutex
	ids map[string]struct{}
}

// NewAppIDSet returns an empty set.
func NewAppIDSet() *AppIDSet {
	return &AppIDSet{ids: make(map[string]struct{})}
}

// Add records id; empty strings are ignored.
func (s *AppIDSet) Add(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	s.ids[id] = struct{}{}
	s.mu.Unlock()
}

// Sorted returns the collected IDs in lexical order.
func (s *AppIDSet) Sorted() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.ids))
	for id := range s.ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
