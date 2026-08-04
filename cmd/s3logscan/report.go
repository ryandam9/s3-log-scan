package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ansiSGR matches the color/style escape sequences the writer emits.
// The -md capture tees the exact bytes shown on screen, so a colored
// terminal run must be stripped back to plain text for the report.
var ansiSGR = regexp.MustCompile("\x1b\\[[0-9;]*m")

// reportPath is where -md writes: ~/logscan/<app-id>.md.
func reportPath(appID string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory for the -md report: %w", err)
	}
	return filepath.Join(home, "logscan", appID+".md"), nil
}

// fenceFor returns a backtick fence long enough that content can never
// close it early: one backtick more than the longest run in the
// content, minimum the standard three.
func fenceFor(content string) string {
	longest, run := 0, 0
	for _, r := range content {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	n := longest + 1
	if n < 3 {
		n = 3
	}
	return strings.Repeat("`", n)
}

// mdCodeBlock renders content as an sh-highlighted fenced code block.
func mdCodeBlock(content string) string {
	content = strings.TrimRight(content, "\n")
	if content == "" {
		content = "(none)"
	}
	fence := fenceFor(content)
	return fence + "sh\n" + content + "\n" + fence + "\n"
}

// writeMDReport renders and writes the -md Markdown report: the run's
// header facts, the matched file names, and the screen output exactly
// as it appeared (ANSI colors stripped).
func writeMDReport(path, appID, pattern string, scopes, matchedKeys []string, screen string, now time.Time) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# s3logscan — %s\n\n", appID)
	fmt.Fprintf(&b, "- **Generated**: %s\n", now.UTC().Format("2006-01-02 15:04:05 UTC"))
	fmt.Fprintf(&b, "- **Pattern**: `%s`\n", pattern)
	for _, s := range scopes {
		fmt.Fprintf(&b, "- **Scanned**: `%s`\n", s)
	}
	fmt.Fprintf(&b, "- **Files with matches**: %d\n\n", len(matchedKeys))

	b.WriteString("## Files with matches\n\n")
	b.WriteString(mdCodeBlock(strings.Join(matchedKeys, "\n")))
	b.WriteString("\n## Screen output\n\n")
	b.WriteString(mdCodeBlock(ansiSGR.ReplaceAllString(screen, "")))

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating report directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("writing report: %w", err)
	}
	return nil
}
