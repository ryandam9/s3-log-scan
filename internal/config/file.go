package config

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

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
// an option was chosen by the user or defaulted.
//
// Format: one "flag = value" per line; blank lines and #-comments are
// ignored; a leading dash on the flag name is tolerated; values may be
// double-quoted (Go string syntax) to preserve leading/trailing
// spaces. Unknown flags and invalid values are errors, reported with
// file:line.
func ApplyFile(fs *flag.FlagSet, path string, skip func(string) bool) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	provided := make(map[string]bool)
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected 'flag = value', got %q", path, i+1, line)
		}
		key = strings.TrimPrefix(strings.TrimSpace(key), "-")
		value = strings.TrimSpace(value)
		if reservedKeys[key] {
			return nil, fmt.Errorf("%s:%d: %q cannot be set from a config file", path, i+1, key)
		}
		if fs.Lookup(key) == nil {
			return nil, fmt.Errorf("%s:%d: unknown option %q", path, i+1, key)
		}
		if strings.HasPrefix(value, `"`) {
			unquoted, err := strconv.Unquote(value)
			if err != nil {
				return nil, fmt.Errorf("%s:%d: bad quoted value for %q: %v", path, i+1, key, err)
			}
			value = unquoted
		}
		provided[key] = true
		if skip != nil && skip(key) {
			continue // the command line already set this flag; it wins
		}
		if err := fs.Set(key, value); err != nil {
			return nil, fmt.Errorf("%s:%d: %s: %v", path, i+1, key, err)
		}
	}
	return provided, nil
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
