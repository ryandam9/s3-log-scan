package scan

import (
	"testing"
	"time"
)

func mustMatcher(t *testing.T, pattern string, fixed, ci bool) *Matcher {
	t.Helper()
	m, err := NewMatcher(pattern, fixed, ci)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestFilterFolderMarkers(t *testing.T) {
	f := &FilterConfig{}
	var c Counters

	// H-07: skip only "/"-suffixed AND zero-byte keys.
	if v := f.Apply(&ObjectDescriptor{Key: "logs/", Size: 0}, &c); v != VerdictFolderMarker {
		t.Fatalf("zero-byte marker: got %v", v)
	}
	if v := f.Apply(&ObjectDescriptor{Key: "logs/", Size: 10}, &c); v != VerdictAccept {
		t.Fatalf("non-empty /-suffixed object must be scanned: got %v", v)
	}
	if v := f.Apply(&ObjectDescriptor{Key: "logs/empty.gz", Size: 0}, &c); v != VerdictAccept {
		t.Fatalf("zero-byte regular object is not a folder marker: got %v", v)
	}
	if c.FoldersSkipped.Load() != 1 {
		t.Fatalf("foldersSkipped: %d", c.FoldersSkipped.Load())
	}
}

func TestFilterStorageClass(t *testing.T) {
	f := &FilterConfig{}
	var c Counters
	for _, sc := range []string{"GLACIER", "DEEP_ARCHIVE"} {
		if v := f.Apply(&ObjectDescriptor{Key: "k", Size: 1, StorageClass: sc}, &c); v != VerdictArchived {
			t.Errorf("%s: got %v", sc, v)
		}
	}
	// Readable classes pass, including Glacier Instant Retrieval.
	for _, sc := range []string{"", "STANDARD", "STANDARD_IA", "GLACIER_IR", "INTELLIGENT_TIERING"} {
		if v := f.Apply(&ObjectDescriptor{Key: "k", Size: 1, StorageClass: sc}, &c); v != VerdictAccept {
			t.Errorf("%s: got %v", sc, v)
		}
	}
	if c.ArchivedSkipped.Load() != 2 {
		t.Fatalf("archivedSkipped: %d", c.ArchivedSkipped.Load())
	}
}

func TestFilterSizeCap(t *testing.T) {
	f := &FilterConfig{MaxSize: 100}
	var c Counters
	if v := f.Apply(&ObjectDescriptor{Key: "k", Size: 100}, &c); v != VerdictAccept {
		t.Fatalf("size == cap must pass: %v", v)
	}
	if v := f.Apply(&ObjectDescriptor{Key: "k", Size: 101}, &c); v != VerdictOversize {
		t.Fatalf("size > cap must be skipped: %v", v)
	}
	f.MaxSize = 0 // unlimited
	if v := f.Apply(&ObjectDescriptor{Key: "k", Size: 1 << 40}, &c); v != VerdictAccept {
		t.Fatalf("0 must mean unlimited: %v", v)
	}
}

// M-02: -after inclusive, -before exclusive, vs LastModified.
func TestFilterTimeWindow(t *testing.T) {
	after := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	before := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	f := &FilterConfig{After: after, HasAfter: true, Before: before, HasBefore: true}
	var c Counters

	cases := []struct {
		mtime time.Time
		want  FilterVerdict
	}{
		{after.Add(-time.Second), VerdictOutsideTimeWindow},
		{after, VerdictAccept}, // inclusive lower bound
		{after.Add(time.Hour), VerdictAccept},
		{before, VerdictOutsideTimeWindow}, // exclusive upper bound
		{before.Add(time.Second), VerdictOutsideTimeWindow},
	}
	for i, tc := range cases {
		if v := f.Apply(&ObjectDescriptor{Key: "k", Size: 1, LastModified: tc.mtime}, &c); v != tc.want {
			t.Errorf("case %d (%s): got %v want %v", i, tc.mtime, v, tc.want)
		}
	}
}

func TestFilterExtension(t *testing.T) {
	f := &FilterConfig{Extensions: []string{".gz", ".log"}}
	var c Counters
	if v := f.Apply(&ObjectDescriptor{Key: "a/b/syslog.GZ", Size: 1}, &c); v != VerdictAccept {
		t.Fatalf("extension match must be case-insensitive: %v", v)
	}
	if v := f.Apply(&ObjectDescriptor{Key: "a/b/data.parquet", Size: 1}, &c); v != VerdictExtension {
		t.Fatalf("disallowed extension: %v", v)
	}
}

func TestFilterKeyPattern(t *testing.T) {
	f := &FilterConfig{KeyMatcher: mustMatcher(t, `application_\d+_0042`, false, false)}
	var c Counters
	hit := "j-1/containers/application_1700000000000_0042/c1/stderr.gz"
	miss := "j-1/containers/application_1700000000000_0041/c1/stderr.gz"
	if v := f.Apply(&ObjectDescriptor{Key: hit, Size: 1}, &c); v != VerdictAccept {
		t.Fatalf("key pattern should match: %v", v)
	}
	if v := f.Apply(&ObjectDescriptor{Key: miss, Size: 1}, &c); v != VerdictKeyPattern {
		t.Fatalf("key pattern should reject: %v", v)
	}
}
