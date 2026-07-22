package scan

import (
	"regexp"
	"sort"
	"sync"
)

// appIDPattern matches YARN application IDs. The leading \b prevents
// matches inside longer identifiers (e.g. "myapplication_1_2"); no
// trailing boundary is imposed because aggregated-log names legitimately
// append suffixes ("application_1_2_1.log") and the greedy \d+ already
// consumes every trailing digit.
var appIDPattern = regexp.MustCompile(`\bapplication_\d+_\d+`)

// ExtractAppID returns the first application ID in b, or "".
func ExtractAppID(b []byte) string {
	return string(appIDPattern.Find(b))
}

// ExtractAppIDString returns the first application ID in s, or "".
func ExtractAppIDString(s string) string {
	return appIDPattern.FindString(s)
}

// ExtractAllAppIDs returns every application ID in b, in order,
// without deduplication.
func ExtractAllAppIDs(b []byte) []string {
	found := appIDPattern.FindAll(b, -1)
	if len(found) == 0 {
		return nil
	}
	out := make([]string, len(found))
	for i, f := range found {
		out[i] = string(f)
	}
	return out
}

// AppIDTracker implements the discovery sources of §8 for one stream:
// the object key, the matching line itself, and preceding context (the
// most recent ID seen earlier in the same stream). Attribution is
// match-oriented: IDsForMatch resolves the IDs for one matching line at
// the moment it matches, so an unrelated ID appearing later can never
// overwrite it. For ZIPs, one tracker is created per entry (seeded only
// with the outer key ID) so context cannot leak across entries.
//
// Per-line ID scanning is enabled only when the key lacks an ID, so
// container-log scans pay nothing for it.
type AppIDTracker struct {
	keyID    string
	lastSeen string
	scanLine bool
}

// NewAppIDTracker prepares a tracker for one object or ZIP entry.
func NewAppIDTracker(key string) *AppIDTracker {
	keyID := ExtractAppIDString(key)
	return &AppIDTracker{keyID: keyID, scanLine: keyID == ""}
}

// Observe must be called for every line read (before deciding whether
// it matches). It is a no-op when the key already carries an ID.
func (t *AppIDTracker) Observe(line []byte) {
	if !t.scanLine {
		return
	}
	if id := ExtractAppID(line); id != "" {
		t.lastSeen = id
	}
}

// IDsForMatch resolves the application IDs to attribute to one
// matching line, in the design's priority order: the key ID if the key
// carries one; otherwise every ID on the matching line itself;
// otherwise the most recent preceding ID. Returns nil when no source
// yields an ID.
func (t *AppIDTracker) IDsForMatch(line []byte) []string {
	if t.keyID != "" {
		return []string{t.keyID}
	}
	if ids := ExtractAllAppIDs(line); len(ids) > 0 {
		return ids
	}
	if t.lastSeen != "" {
		return []string{t.lastSeen}
	}
	return nil
}

// Current returns the best ID known so far (key, then most recent
// sighting). Used by the -l -discover-apps read-on rule, where a
// following ID is explicitly wanted.
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

// AddAll records every ID in ids.
func (s *AppIDSet) AddAll(ids []string) {
	for _, id := range ids {
		s.Add(id)
	}
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
