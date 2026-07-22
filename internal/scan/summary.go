package scan

import (
	"fmt"
	"io"
	"strings"
)

// PrintSummary writes the end-of-run account to w (stderr). It always
// prints — including after SIGINT — and separates objects scanned to
// EOF, stopped early by request, partially scanned, skipped (with
// reasons), and failed (§10, M-06).
func PrintSummary(w io.Writer, r *RunResult, listOnly bool) {
	c := r.Counters
	fmt.Fprintln(w, "---")
	status := "completed"
	switch {
	case r.Interrupted:
		status = "interrupted"
	case r.TimedOut:
		status = "stopped: -overall-timeout exceeded"
	case r.WriteErr != nil:
		status = fmt.Sprintf("stopped: stdout write failed (%v)", r.WriteErr)
	case r.ListingErr != nil:
		status = "failed while listing"
	}
	fmt.Fprintf(w, "s3logscan: %s in %s\n", status, r.Elapsed.Round(1e6))

	fmt.Fprintf(w, "  listed %d, survived filters %d\n", c.Listed.Load(), c.Survived.Load())

	var skips []string
	appendSkip := func(n int64, label string) {
		if n > 0 {
			skips = append(skips, fmt.Sprintf("%d %s", n, label))
		}
	}
	appendSkip(c.FoldersSkipped.Load(), "folder markers")
	appendSkip(c.ArchivedSkipped.Load(), "archived without restored copy")
	appendSkip(c.OversizeSkipped.Load(), "over -max-size")
	appendSkip(c.TimeFiltered.Load(), "outside time window")
	appendSkip(c.ExtFiltered.Load(), "extension filtered")
	appendSkip(c.KeyFiltered.Load(), "key-pattern filtered")
	if len(skips) > 0 {
		fmt.Fprintf(w, "  filtered out: %s\n", strings.Join(skips, ", "))
	}

	if listOnly {
		fmt.Fprintf(w, "  list-only mode: %d objects reported, no downloads\n", c.MatchedObjects.Load())
	} else {
		fmt.Fprintf(w, "  scanned to EOF %d, stopped early by request %d, partially scanned %d\n",
			c.ScannedFully.Load(), c.StoppedEarly.Load(), c.ScannedPartially.Load())
		fmt.Fprintf(w, "  matched objects %d, matched lines %d\n", c.MatchedObjects.Load(), c.MatchedLines.Load())
		fmt.Fprintf(w, "  compressed bytes downloaded %d\n", c.BytesDownloaded.Load())
		if n := c.OversizedLines.Load(); n > 0 {
			fmt.Fprintf(w, "  oversized lines truncated %d\n", n)
		}
	}

	var errs []string
	appendErr := func(n int64, label string) {
		if n > 0 {
			errs = append(errs, fmt.Sprintf("%s %d", label, n))
		}
	}
	appendErr(c.AccessDenied.Load(), "accessDenied")
	appendErr(c.NotFound.Load(), "notFound")
	appendErr(c.ChangedAfterListing.Load(), "changedAfterListing")
	appendErr(c.Corrupt.Load(), "corrupt")
	appendErr(c.Timeout.Load(), "timeout")
	appendErr(c.ArchivedUnavailable.Load(), "archivedUnavailable")
	appendErr(c.Throttled.Load(), "throttled")
	appendErr(c.OtherErrors.Load(), "other")
	if len(errs) > 0 {
		fmt.Fprintf(w, "  object errors: %s\n", strings.Join(errs, ", "))
	}

	if ids := r.AppIDs.Sorted(); len(ids) > 0 {
		fmt.Fprintf(w, "  application IDs discovered (%d):\n", len(ids))
		for _, id := range ids {
			fmt.Fprintf(w, "    %s\n", id)
		}
	}
}

// ExitCode maps the run result to the stable exit codes (§10, H-08):
//
//	0    completed; ≥1 match; no object errors or partial scans
//	1    completed; no matches
//	2    fatal usage, configuration, credential, or listing error
//	3    completed with ≥1 object error or partially scanned object,
//	     the overall timeout expired, or stdout failed mid-run
//	130  interrupted (SIGINT/SIGTERM); summary is still printed
//
// -overall-timeout deliberately maps to 3, not 130: a configured
// deadline is a partial run, not an external interruption (H-02).
func ExitCode(r *RunResult) int {
	switch {
	case r.ListingErr != nil:
		return 2
	case r.Interrupted:
		return 130
	case r.TimedOut:
		return 3
	case r.WriteErr != nil:
		return 3
	case r.Counters.ObjectErrors() > 0 || r.Counters.ScannedPartially.Load() > 0:
		return 3
	case r.Counters.MatchedLines.Load() > 0 || r.Counters.MatchedObjects.Load() > 0:
		return 0
	default:
		return 1
	}
}
