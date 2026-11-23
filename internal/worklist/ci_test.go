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
	c.AddFailed(strings.Repeat("é", MaxCINameBytes), "") // 2 bytes each
	for i := range MaxCIFailedNames + 4 {
		c.AddFailed(fmt.Sprintf("job-%d", i), fmt.Sprintf("https://prow.example/view/%d", i))
	}
	if len(c.Failed) != MaxCIFailedNames || len(c.FailedURLs) != MaxCIFailedNames || c.FailedOmitted != 5 {
		t.Fatalf("kept %d names, %d links, omitted %d", len(c.Failed), len(c.FailedURLs), c.FailedOmitted)
	}
	if c.FailedURLs[0] != "" || c.FailedURLs[1] != "https://prow.example/view/0" {
		t.Fatalf("links not aligned with names: %q", c.FailedURLs[:2])
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

// A pull request's write-back that carries no CI keeps the stored summary; one that
// carries a summary replaces it; an issue never takes one from the store.
func TestMergeKeepsCI(t *testing.T) {
	ci := &CISummary{SHA: "s", Passed: 1, Failed: []string{"e2e"}}
	stored := []WorkItem{
		{ID: "pr", Type: TypePullRequest, GitHub: GitHubRef{CI: ci}},
		{ID: "pr2", Type: TypePullRequest, GitHub: GitHubRef{CI: ci}},
		{ID: "issue", Type: TypeIssue, GitHub: GitHubRef{CI: ci}},
	}
	fresh := &CISummary{SHA: "t", Pending: 2}
	got := MergeRunItems(stored, []WorkItem{
		{ID: "pr", Type: TypePullRequest},
		{ID: "pr2", Type: TypePullRequest, GitHub: GitHubRef{CI: fresh}},
		{ID: "issue", Type: TypeIssue},
	})
	if !got[0].GitHub.CI.Equal(ci) || !got[1].GitHub.CI.Equal(fresh) || got[2].GitHub.CI != nil {
		t.Fatalf("merged CI: %+v, %+v, %+v", got[0].GitHub.CI, got[1].GitHub.CI, got[2].GitHub.CI)
	}
	got[0].GitHub.CI.Failed[0] = "changed"
	if ci.Failed[0] != "e2e" {
		t.Fatal("the merge must not alias the stored summary")
	}
}

// A CI link reaches an href, so only an absolute http(s) URL with a host passes,
// within the length bound.
func TestCIJobURL(t *testing.T) {
	for in, ok := range map[string]bool{
		"https://prow.k8s.io/view/gs/kubernetes-ci-logs/pr-logs/pull/1/job/2": true,
		"http://ci.example/job/1":      true,
		"":                             false,
		"javascript:alert(1)":          false,
		"data:text/html,x":             false,
		"//prow.k8s.io/view/x":         false,
		"/view/x":                      false,
		"https://":                     false,
		"https://user:pw@ci.example/x": false,
		"ftp://ci.example/x":           false,
		"https://ci.example/" + strings.Repeat("x", MaxCIURLBytes): false,
	} {
		if got := CIJobURL(in); (got != "") != ok || (ok && got != in) {
			t.Errorf("CIJobURL(%q) = %q, want ok=%v", in, got, ok)
		}
	}
}

// FailedJobs pairs names with their links, re-checks each link, and leaves a job
// without one when the stored lists do not line up.
func TestCISummaryFailedJobs(t *testing.T) {
	c := CISummary{Failed: []string{"a", "b", "c"}, FailedURLs: []string{"https://ci.example/a", "javascript:x"}}
	got := c.FailedJobs()
	want := []CIJob{{"a", "https://ci.example/a"}, {"b", ""}, {"c", ""}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("FailedJobs = %v, want %v", got, want)
	}
	if a, b := (&CISummary{Failed: []string{"a"}, FailedURLs: []string{"https://x.example/1"}}),
		(&CISummary{Failed: []string{"a"}, FailedURLs: []string{"https://x.example/2"}}); a.Equal(b) {
		t.Fatal("summaries with different links must differ")
	}
}

// Projection cuts the links with the names they belong to, dropping (not cutting) a
// link too long to keep whole; the write-back restores both lists.
func TestProjectionCutsCILinksWithNames(t *testing.T) {
	ci := &CISummary{SHA: "abc"}
	for i := range 2 * projectedList {
		link := fmt.Sprintf("https://ci.example/%d", i)
		if i == 0 {
			link = "https://ci.example/" + strings.Repeat("x", projectedString)
		}
		ci.AddFailed(fmt.Sprintf("job-%d", i), link)
	}
	stored := WorkItem{ID: "big", Type: TypePullRequest,
		GitHub: GitHubRef{Title: strings.Repeat("t", MaxBriefBytes), CI: ci}}
	read := Project(stored)
	got := read.GitHub.CI
	if len(got.FailedURLs) != len(got.Failed) || got.FailedURLs[0] != "" || got.FailedURLs[1] != "https://ci.example/1" {
		t.Fatalf("cut links: %d for %d names, first %q", len(got.FailedURLs), len(got.Failed), got.FailedURLs[:2])
	}
	if len(ci.FailedURLs[0]) <= projectedString {
		t.Fatal("projection must not write through to the stored links")
	}
	b, _ := json.Marshal(read)
	var back WorkItem
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if merged := MergeRunItems([]WorkItem{stored}, []WorkItem{back})[0]; !merged.GitHub.CI.Equal(ci) {
		t.Fatal("write-back must restore the stored links")
	}
}
