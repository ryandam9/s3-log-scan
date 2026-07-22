package scan

import "strings"

// sanitizePlaceholder replaces control characters in printed output,
// preventing terminal-escape injection and NUL confusion (H-09).
const sanitizePlaceholder = '?'

// Sanitize replaces every control character other than tab with a
// placeholder. Applied by default to matched content, object keys, and
// ZIP entry names before printing (§7.2). It never mutates its input.
func Sanitize(b []byte) []byte {
	idx := -1
	for i, c := range b {
		if isControl(c) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return b
	}
	out := make([]byte, len(b))
	copy(out, b)
	for i := idx; i < len(out); i++ {
		if isControl(out[i]) {
			out[i] = sanitizePlaceholder
		}
	}
	return out
}

// SanitizeString is Sanitize for strings.
func SanitizeString(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		if isControl(s[i]) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if isControl(s[i]) {
			sb.WriteByte(sanitizePlaceholder)
		} else {
			sb.WriteByte(s[i])
		}
	}
	return sb.String()
}

// isControl reports whether c is a C0 control byte (other than tab) or
// DEL. Bytes >= 0x80 are left alone: they are either valid UTF-8
// continuation bytes or plain binary noise that terminals do not
// interpret as escapes.
func isControl(c byte) bool {
	return (c < 0x20 && c != '\t') || c == 0x7f
}
