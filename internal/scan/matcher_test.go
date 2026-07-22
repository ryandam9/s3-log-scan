package scan

import "testing"

func TestMatcherRegex(t *testing.T) {
	m := mustMatcher(t, `ERROR|FATAL`, false, false)
	if !m.Match([]byte("2026-07-22 ERROR something")) {
		t.Fatal("regex should match")
	}
	if m.Match([]byte("2026-07-22 error something")) {
		t.Fatal("case-sensitive regex must not match lowercase")
	}
}

func TestMatcherRegexIgnoreCase(t *testing.T) {
	m := mustMatcher(t, `error`, false, true)
	if !m.Match([]byte("FATAL ERROR here")) {
		t.Fatal("-i regex should match")
	}
}

// M-10: -F treats the pattern literally.
func TestMatcherFixedString(t *testing.T) {
	m := mustMatcher(t, `a.b[1]`, true, false)
	if !m.Match([]byte("path a.b[1] here")) {
		t.Fatal("fixed string should match literally")
	}
	if m.Match([]byte("aXb1")) {
		t.Fatal("fixed string must not behave like a regex")
	}
}

func TestMatcherFixedStringIgnoreCase(t *testing.T) {
	m := mustMatcher(t, `Table Not Found`, true, true)
	if !m.Match([]byte("ERROR: TABLE NOT FOUND: db.t1")) {
		t.Fatal("-F -i should match")
	}
}

func TestMatcherInvalidRegex(t *testing.T) {
	if _, err := NewMatcher(`(`, false, false); err == nil {
		t.Fatal("invalid regex must be rejected")
	}
}
