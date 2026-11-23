package webui

import (
	"fmt"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// ciView is a pull request's CI summary as the page draws it (under the labels on
// the radar, in the header of the Discuss page): a line for the jobs that
// succeeded, one for the jobs still running, and one per failed job, naming it and
// linking it where GitHub's pull request does. A count of zero draws no line.
type ciView struct {
	Passed  string           // "N CI jobs successful", or ""
	Pending string           // "N CI jobs in progress", or ""
	Failed  []worklist.CIJob // each failed job's full name and link
	More    string           // failed jobs past the stored bound, or ""
	Partial bool             // part of the CI could not be read
	// Historical marks an item off the open radar (closed, merged, or no longer
	// involving the user): enrich no longer reads its CI, so the summary is the
	// last one read and is labeled so.
	Historical bool
}

// ciSummary returns the item's CI view, or nil when there is nothing to show: no
// summary stored (an issue, a pull request enrich has not read), or a whole read
// that counted no jobs.
func ciSummary(it worklist.WorkItem) *ciView {
	c := it.GitHub.CI
	if c == nil {
		return nil
	}
	v := &ciView{
		Passed:  ciCount(c.Passed, "successful"),
		Pending: ciCount(c.Pending, "in progress"),
		Failed:  c.FailedJobs(),
		Partial: c.Partial,

		Historical: !it.Meta.CompletedAt.IsZero(),
	}
	if c.FailedOmitted > 0 {
		v.More = fmt.Sprintf("and %d more failed", c.FailedOmitted)
	}
	// No jobs counted is "nothing to show" only when the read was whole: a partial
	// read with no counts (statuses empty, check runs refused) still says so.
	if v.Passed == "" && v.Pending == "" && len(v.Failed) == 0 && v.More == "" && !v.Partial {
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
