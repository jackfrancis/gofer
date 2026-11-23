package webui

import (
	"fmt"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// ciView is a pull request's CI summary as the radar draws it, under the labels:
// a line for the jobs that passed, one for the jobs still running, and one per
// failed job, naming it. A count of zero draws no line.
type ciView struct {
	Passed  string   // "N CI jobs successful", or ""
	Pending string   // "N CI jobs in progress", or ""
	Failed  []string // each failed job's full name
	More    string   // failed jobs past the stored bound, or ""
	Partial bool     // part of the CI could not be read
}

// ciSummary returns the item's CI view, or nil when there is nothing to show: no
// summary stored (an issue, a pull request enrich has not read), or no jobs.
func ciSummary(it worklist.WorkItem) *ciView {
	c := it.GitHub.CI
	if c == nil {
		return nil
	}
	v := &ciView{
		Passed:  ciCount(c.Passed, "successful"),
		Pending: ciCount(c.Pending, "in progress"),
		Failed:  c.Failed,
		Partial: c.Partial,
	}
	if c.FailedOmitted > 0 {
		v.More = fmt.Sprintf("and %d more failed", c.FailedOmitted)
	}
	if v.Passed == "" && v.Pending == "" && len(v.Failed) == 0 && v.More == "" {
		return nil
	}
	return v
}

func ciCount(n int, what string) string {
	switch {
	case n <= 0:
		return ""
	case n == 1:
		return "1 CI job " + what
	default:
		return fmt.Sprintf("%d CI jobs %s", n, what)
	}
}
