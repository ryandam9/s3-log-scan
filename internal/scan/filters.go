package scan

import (
	"strings"
	"time"
)

// FilterConfig holds the client-side metadata filters (§5.1). They cut
// GetObject calls and downloaded bytes, not LIST work — the prefix is
// the only server-side filter.
type FilterConfig struct {
	MaxSize    int64 // compressed bytes; 0 = unlimited
	After      time.Time
	Before     time.Time
	HasAfter   bool
	HasBefore  bool
	Extensions []string // lower-cased, with leading dot
	KeyMatcher *Matcher // nil = no key filter
}

// FilterVerdict says what happened to a descriptor in the chain.
type FilterVerdict int

const (
	VerdictAccept FilterVerdict = iota
	VerdictFolderMarker
	VerdictArchived
	VerdictOversize
	VerdictOutsideTimeWindow
	VerdictExtension
	VerdictKeyPattern
)

// archivedClasses cannot be read without a restore. GLACIER_IR
// (Instant Retrieval) is readable in real time and is deliberately not
// listed here.
var archivedClasses = map[string]bool{
	"GLACIER":      true,
	"DEEP_ARCHIVE": true,
}

// Apply runs the filter chain in cheapest-first order (M-01):
// folder marker → storage class → size → time window → extension →
// key regex. It updates counters and returns the verdict.
func (f *FilterConfig) Apply(d *ObjectDescriptor, c *Counters) FilterVerdict {
	// Folder markers: "/"-suffixed AND zero bytes (H-07). Non-empty
	// "/"-suffixed objects are real data and are scanned.
	if strings.HasSuffix(d.Key, "/") && d.Size == 0 {
		c.FoldersSkipped.Add(1)
		return VerdictFolderMarker
	}

	if archivedClasses[d.StorageClass] {
		c.ArchivedSkipped.Add(1)
		return VerdictArchived
	}

	if f.MaxSize > 0 && d.Size > f.MaxSize {
		c.OversizeSkipped.Add(1)
		return VerdictOversize
	}

	// Time window vs S3 LastModified (never timestamps inside log
	// text): -after inclusive, -before exclusive (M-02).
	if f.HasAfter && d.LastModified.Before(f.After) {
		c.TimeFiltered.Add(1)
		return VerdictOutsideTimeWindow
	}
	if f.HasBefore && !d.LastModified.Before(f.Before) {
		c.TimeFiltered.Add(1)
		return VerdictOutsideTimeWindow
	}

	if len(f.Extensions) > 0 && !hasAllowedExtension(d.Key, f.Extensions) {
		c.ExtFiltered.Add(1)
		return VerdictExtension
	}

	// Key pattern last: regex evaluation is the costliest check.
	if f.KeyMatcher != nil && !f.KeyMatcher.MatchString(d.Key) {
		c.KeyFiltered.Add(1)
		return VerdictKeyPattern
	}

	return VerdictAccept
}

func hasAllowedExtension(key string, exts []string) bool {
	lower := strings.ToLower(key)
	for _, e := range exts {
		if strings.HasSuffix(lower, e) {
			return true
		}
	}
	return false
}
