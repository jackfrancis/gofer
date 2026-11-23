package worklist

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A stored summary is bounded however many jobs failed: past MaxCIFailedNames the
// failures are only counted, and a long name is cut without splitting a character.
func TestCISummaryAddFailedBounds(t *testing.T) {
	var c CISummary
	c.AddFailed(strings.Repeat("é", MaxCINameBytes)) // 2 bytes each
	for i := range MaxCIFailedNames + 4 {
		c.AddFailed(fmt.Sprintf("job-%d", i))
	}
	if len(c.Failed) != MaxCIFailedNames || c.FailedOmitted != 5 {
		t.Fatalf("kept %d names, omitted %d", len(c.Failed), c.FailedOmitted)
	}
	if n := c.Failed[0]; len(n) > MaxCINameBytes || !strings.HasPrefix(strings.Repeat("é", MaxCINameBytes), n) {
		t.Fatalf("long name cut to %d bytes: %q", len(n), n)
	}
}

func TestCISummaryEqual(t *testing.T) {
	a := &CISummary{SHA: "s", Passed: 1, Failed: []string{"x"}}
	b := &CISummary{SHA: "s", Passed: 1, Failed: []string{"x"}}
	if !a.Equal(b) || !(*CISummary)(nil).Equal(nil) || a.Equal(nil) || (*CISummary)(nil).Equal(a) {
		t.Fatal("equal summaries must compare equal, and nil only to nil")
	}
	for _, o := range []*CISummary{
		{SHA: "t", Passed: 1, Failed: []string{"x"}},
		{SHA: "s", Passed: 2, Failed: []string{"x"}},
		{SHA: "s", Passed: 1, Pending: 1, Failed: []string{"x"}},
		{SHA: "s", Passed: 1, Failed: []string{"y"}},
		{SHA: "s", Passed: 1, Failed: []string{"x"}, FailedOmitted: 1},
		{SHA: "s", Passed: 1, Failed: []string{"x"}, Partial: true},
	} {
		if a.Equal(o) {
			t.Errorf("%+v must differ from %+v", o, a)
		}
	}
}

// An oversized item's CI failures are cut like any list, counted as omitted, and the
// run's unchanged write-back, through JSON as the agent plane carries it, restores
// the stored summary whole; a run that rewrote it keeps its own.
func TestProjectionCutsCISummary(t *testing.T) {
	ci := &CISummary{SHA: "abc", Passed: 3, Pending: 1, FailedOmitted: 2}
	for i := range 2 * projectedList {
		ci.Failed = append(ci.Failed, fmt.Sprintf("job-%02d-%s", i, strings.Repeat("x", 2*projectedString)))
	}
	stored := WorkItem{ID: "big", Type: TypePullRequest,
		GitHub: GitHubRef{Title: strings.Repeat("t", MaxBriefBytes), CI: ci}}
	read := Project(stored)
	if _, cut := read.Projected["github.ci"]; !cut {
		t.Fatalf("CI not marked cut: %v", read.Projected)
	}
	got := read.GitHub.CI
	if len(got.Failed) != projectedList || got.FailedOmitted != 2+projectedList || len(got.Failed[0]) > projectedString {
		t.Fatalf("cut CI: %d names, omitted %d, first %d bytes", len(got.Failed), got.FailedOmitted, len(got.Failed[0]))
	}
	if len(stored.GitHub.CI.Failed) != 2*projectedList || len(stored.GitHub.CI.Failed[0]) <= projectedString {
		t.Fatal("projection must not write through to the stored item")
	}

	b, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	var back WorkItem
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	merged := MergeRunItems([]WorkItem{stored}, []WorkItem{back})[0]
	if !merged.GitHub.CI.Equal(ci) {
		t.Fatalf("write-back must restore the stored CI, got %+v", merged.GitHub.CI)
	}

	back.GitHub.CI = &CISummary{SHA: "new", Passed: 9}
	if merged := MergeRunItems([]WorkItem{stored}, []WorkItem{back})[0]; merged.GitHub.CI.SHA != "new" {
		t.Fatalf("a rewritten CI summary must stand, got %+v", merged.GitHub.CI)
	}
}
