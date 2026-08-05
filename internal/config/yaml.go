package config

import (
	"flag"
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// ApplyFile reads a YAML config file and applies its values to fs —
// but only for flags not excluded by skip, which is how CLI-provided
// flags keep priority. Returns the set of keys the file provided
// (including skipped ones), so callers can tell whether an option was
// chosen by the user or defaulted, and the named grep categories the
// file defines under "patterns".
//
// Top-level scalar keys are flag defaults (same names as the CLI
// flags); the "patterns" mapping defines the categories -category
// picks from — each name a regex or a list of regexes that
// OR-combine:
//
//	cluster-name: hbase-prod
//	i: true
//	patterns:
//	  spark:
//	    - ERROR|Exception
//	    - Caused by
//	  oom: OutOfMemoryError|exit code 137
//
// Unknown keys, non-scalar flag values, and invalid regexes are
// errors, reported with file:line. Quote a regex if it starts with a
// YAML-special character (*, &, [, {, ...).
func ApplyFile(fs *flag.FlagSet, path string, skip func(string) bool) (provided map[string]bool, patterns map[string][]string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading config file: %w", err)
	}
	var root map[string]yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, nil, fmt.Errorf("%s: %v", path, err)
	}
	provided = make(map[string]bool)
	patterns = make(map[string][]string)
	for key, node := range root {
		if key == "patterns" {
			if err := yamlPatterns(path, node, patterns); err != nil {
				return nil, nil, err
			}
			continue
		}
		if reservedKeys[key] {
			return nil, nil, fmt.Errorf("%s:%d: %q cannot be set from a config file", path, node.Line, key)
		}
		if fs.Lookup(key) == nil {
			return nil, nil, fmt.Errorf("%s:%d: unknown option %q", path, node.Line, key)
		}
		if node.Kind != yaml.ScalarNode {
			return nil, nil, fmt.Errorf("%s:%d: %s: expected a single scalar value", path, node.Line, key)
		}
		provided[key] = true
		if skip != nil && skip(key) {
			continue // the command line already set this flag; it wins
		}
		if err := fs.Set(key, node.Value); err != nil {
			return nil, nil, fmt.Errorf("%s:%d: %s: %v", path, node.Line, key, err)
		}
	}
	return provided, patterns, nil
}

// yamlPatterns validates and collects the "patterns" mapping,
// fail-fast: names and regexes must be non-empty, and every regex
// must compile (-i is layered on at use time and cannot invalidate a
// pattern that compiles here).
func yamlPatterns(path string, node yaml.Node, patterns map[string][]string) error {
	var m map[string]yaml.Node
	if err := node.Decode(&m); err != nil {
		return fmt.Errorf("%s:%d: patterns: expected a mapping of name to regex (or list of regexes): %v", path, node.Line, err)
	}
	for name, v := range m {
		if name == "" {
			return fmt.Errorf("%s:%d: patterns: entry needs a name", path, v.Line)
		}
		var pats []string
		switch v.Kind {
		case yaml.ScalarNode:
			pats = []string{v.Value}
		case yaml.SequenceNode:
			if err := v.Decode(&pats); err != nil {
				return fmt.Errorf("%s:%d: patterns.%s: %v", path, v.Line, name, err)
			}
		default:
			return fmt.Errorf("%s:%d: patterns.%s: expected a regex or a list of regexes", path, v.Line, name)
		}
		for _, p := range pats {
			if p == "" {
				return fmt.Errorf("%s:%d: patterns.%s: empty pattern", path, v.Line, name)
			}
			if _, err := regexp.Compile(p); err != nil {
				return fmt.Errorf("%s:%d: patterns.%s: %v (RE2 syntax; no lookaround or backreferences)", path, v.Line, name, err)
			}
		}
		patterns[name] = append(patterns[name], pats...)
	}
	return nil
}
