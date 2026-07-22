package scan

import "testing"

const (
	containerKey = "j-ABC/containers/application_1700000000000_0007/container_01/stderr.gz"
	stepKey      = "j-ABC/steps/s-XYZ/stderr.gz" // no application ID in the key
)

func TestExtractAppID(t *testing.T) {
	if got := ExtractAppIDString(containerKey); got != "application_1700000000000_0007" {
		t.Fatalf("key extraction: %q", got)
	}
	if got := ExtractAppIDString(stepKey); got != "" {
		t.Fatalf("step key must have no ID, got %q", got)
	}
}

// §8 source 1: the key carries the ID; line scanning is disabled.
func TestTrackerKeySource(t *testing.T) {
	tr := NewAppIDTracker(containerKey)
	tr.Observe([]byte("mentions application_9999999999999_0001 in text"))
	if got := tr.Current(); got != "application_1700000000000_0007" {
		t.Fatalf("key ID must win: %q", got)
	}
}

// §8 source 2: the matching line itself.
func TestTrackerMatchingLineSource(t *testing.T) {
	tr := NewAppIDTracker(stepKey)
	tr.Observe([]byte("ERROR in application_1700000000000_0031: table not found"))
	if got := tr.Current(); got != "application_1700000000000_0031" {
		t.Fatalf("line ID: %q", got)
	}
}

// §8 source 3: preceding context — the most recent ID seen earlier.
func TestTrackerPrecedingContext(t *testing.T) {
	tr := NewAppIDTracker(stepKey)
	tr.Observe([]byte("submitting application_1700000000000_0005"))
	tr.Observe([]byte("now running application_1700000000000_0006"))
	tr.Observe([]byte("some failure line with no id"))
	if got := tr.Current(); got != "application_1700000000000_0006" {
		t.Fatalf("most recent preceding ID: %q", got)
	}
}

func TestTrackerNoID(t *testing.T) {
	tr := NewAppIDTracker(stepKey)
	tr.Observe([]byte("nothing here"))
	if got := tr.Current(); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestAppIDSetDedup(t *testing.T) {
	s := NewAppIDSet()
	s.Add("application_1_2")
	s.Add("application_1_2")
	s.Add("application_1_1")
	s.Add("") // ignored
	ids := s.Sorted()
	if len(ids) != 2 || ids[0] != "application_1_1" || ids[1] != "application_1_2" {
		t.Fatalf("sorted dedup: %v", ids)
	}
}
