package scan

import (
	"bytes"
	"fmt"
	"regexp"
)

// Matcher decides whether a byte slice matches the user's pattern.
// Fixed-string mode (-F) avoids regex entirely; regex mode uses Go's
// RE2 engine (no lookaround or backreferences, documented in the CLI
// help).
type Matcher struct {
	fixed      []byte // non-nil in case-sensitive fixed-string mode
	fixedFold  []byte // non-nil in case-insensitive fixed-string mode
	re         *regexp.Regexp
	expression string
}

// NewMatcher compiles pattern. When fixedString is true the pattern is
// treated literally; ignoreCase applies to both modes.
func NewMatcher(pattern string, fixedString, ignoreCase bool) (*Matcher, error) {
	m := &Matcher{expression: pattern}
	switch {
	case fixedString && !ignoreCase:
		m.fixed = []byte(pattern)
	case fixedString && ignoreCase:
		m.fixedFold = bytes.ToLower([]byte(pattern))
	default:
		expr := pattern
		if ignoreCase {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %w (RE2 syntax; no lookaround or backreferences)", pattern, err)
		}
		m.re = re
	}
	return m, nil
}

// Match reports whether b contains the pattern.
func (m *Matcher) Match(b []byte) bool {
	switch {
	case m.fixed != nil:
		return bytes.Contains(b, m.fixed)
	case m.fixedFold != nil:
		return bytes.Contains(bytes.ToLower(b), m.fixedFold)
	default:
		return m.re.Match(b)
	}
}

// MatchString reports whether s contains the pattern.
func (m *Matcher) MatchString(s string) bool {
	return m.Match([]byte(s))
}
