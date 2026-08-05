package config

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// patternPrefix marks config-file keys that define named grep
// categories ("pattern.<name> = <regex>") rather than flag defaults.
// Repeated lines for the same name are OR-combined at use time.
const patternPrefix = "pattern."

// reservedKeys cannot be set from a config file: -config would recurse
// and -version is a one-shot informational action, not a default.
var reservedKeys = map[string]bool{
	"config":  true,
	"version": true,
}

// ApplyFile reads a config file of "flag = value" lines and applies
// each value to fs — but only for flags not excluded by skip, which is
// how CLI-provided flags keep priority. Returns the set of keys the
// file provided (including skipped ones), so callers can tell whether
// an option was chosen by the user or defaulted, and the named grep
// categories the file defines ("pattern.<name> = <regex>" lines,
// resolved by -category).
//
// Format: one "flag = value" per line; blank lines and #-comments are
// ignored; a leading dash on the flag name is tolerated; values may be
// double-quoted (Go string syntax) to preserve leading/trailing
// spaces. Unknown flags and invalid values are errors, reported with
// file:line.
func ApplyFile(fs *flag.FlagSet, path string, skip func(string) bool) (provided map[string]bool, patterns map[string][]string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading config file: %w", err)
	}
	provided = make(map[string]bool)
	patterns = make(map[string][]string)
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, nil, fmt.Errorf("%s:%d: expected 'flag = value', got %q", path, i+1, line)
		}
		key = strings.TrimPrefix(strings.TrimSpace(key), "-")
		value = strings.TrimSpace(value)
		if reservedKeys[key] {
			return nil, nil, fmt.Errorf("%s:%d: %q cannot be set from a config file", path, i+1, key)
		}
		if strings.HasPrefix(value, `"`) {
			unquoted, err := strconv.Unquote(value)
			if err != nil {
				return nil, nil, fmt.Errorf("%s:%d: bad quoted value for %q: %v", path, i+1, key, err)
			}
			value = unquoted
		}
		if name, isPattern := strings.CutPrefix(key, patternPrefix); isPattern {
			if name == "" {
				return nil, nil, fmt.Errorf("%s:%d: pattern entry needs a name (pattern.<name> = <regex>)", path, i+1)
			}
			if value == "" {
				return nil, nil, fmt.Errorf("%s:%d: pattern.%s: empty pattern", path, i+1, name)
			}
			// Fail fast with the file position; -i is layered on at use
			// time and cannot invalidate a pattern that compiles here.
			if _, err := regexp.Compile(value); err != nil {
				return nil, nil, fmt.Errorf("%s:%d: pattern.%s: %v (RE2 syntax; no lookaround or backreferences)", path, i+1, name, err)
			}
			patterns[name] = append(patterns[name], value)
			continue
		}
		if fs.Lookup(key) == nil {
			return nil, nil, fmt.Errorf("%s:%d: unknown option %q", path, i+1, key)
		}
		provided[key] = true
		if skip != nil && skip(key) {
			continue // the command line already set this flag; it wins
		}
		if err := fs.Set(key, value); err != nil {
			return nil, nil, fmt.Errorf("%s:%d: %s: %v", path, i+1, key, err)
		}
	}
	return provided, patterns, nil
}

// ResolveCategory turns a -category name into the grep pattern it
// names. Multiple config lines for the same category OR-combine, each
// isolated in a non-capturing group. Unknown names fail fast — a typo
// silently falling back to "no pattern" would turn a search into a
// file listing — and the error says what the file actually defines.
func ResolveCategory(name string, patterns map[string][]string) (string, error) {
	pats := patterns[name]
	if len(pats) == 0 {
		if len(patterns) == 0 {
			return "", fmt.Errorf("-category %q: the config file defines no pattern.<name> entries", name)
		}
		known := make([]string, 0, len(patterns))
		for k := range patterns {
			known = append(known, k)
		}
		sort.Strings(known)
		return "", fmt.Errorf("-category %q is not defined in the config file (available: %s)", name, strings.Join(known, ", "))
	}
	if len(pats) == 1 {
		return pats[0], nil
	}
	groups := make([]string, len(pats))
	for i, p := range pats {
		groups[i] = "(?:" + p + ")"
	}
	return strings.Join(groups, "|"), nil
}

// DefaultConfigPath returns ~/.config/s3logscan/config if it exists,
// "" otherwise.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	p := home + "/.config/s3logscan/config"
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}
