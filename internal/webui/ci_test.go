package webui

import (
	"strings"
	"testing"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// A pull request's CI renders under its labels: a line each for the jobs that
// passed and are still running, then every failed job by its full name, escaped.
// A zero count draws no line, and an item with no summary (or no jobs) no section.
func TestWorklistRendersCISummary(t *testing.T) {
	items := []worklist.WorkItem{
		{ID: "github:o/r#1", Type: worklist.TypePullRequest,
			GitHub: worklist.GitHubRef{Number: 1, Repo: "o/r", Title: "Busy PR", CI: &worklist.CISummary{
				Passed: 12, Pending: 1, Failed: []string{"pull-kubernetes-e2e-gce", "<script>x</script>", "evil-link"},
				FailedURLs:    []string{"https://prow.k8s.io/view/gs/kubernetes-ci-logs/pr-logs/pull/1/pull-kubernetes-e2e-gce/7", "", "javascript:alert(1)"},
				FailedOmitted: 3, Partial: true}},
			Signals: worklist.Signals{Labels: []string{"kind/bug"}}},
		{ID: "github:o/r#2", Type: worklist.TypePullRequest,
			GitHub: worklist.GitHubRef{Number: 2, Repo: "o/r", Title: "Green PR", CI: &worklist.CISummary{Passed: 1}}},
		{ID: "github:o/r#3", Type: worklist.TypePullRequest,
			GitHub: worklist.GitHubRef{Number: 3, Repo: "o/r", Title: "No jobs", CI: &worklist.CISummary{SHA: "s"}}},
		{ID: "github:o/r#4", Type: worklist.TypeIssue, GitHub: worklist.GitHubRef{Number: 4, Repo: "o/r", Title: "An issue"}},
	}
	out := renderPage(t, pageData{View: "worklist", Items: items, LabelCSS: labelStylesheet(items)})
	section := func(from, to string) string {
		s := out[strings.Index(out, from):]
		if to != "" {
			s = s[:strings.Index(s, to)]
		}
		return s
	}

	busy := section("Busy PR", "Green PR")
	labels, ci, bar := strings.Index(busy, "zz-labels"), strings.Index(busy, `class="zz-ci"`), strings.Index(busy, "zz-bar")
	if labels < 0 || ci < labels || bar < ci {
		t.Fatalf("CI section must sit below the labels, above the bar:\n%s", busy)
	}
	for _, want := range []string{
		`<li class="zz-ci-pass">12 CI jobs successful</li>`,
		`<li class="zz-ci-pending">1 CI job in progress</li>`,
		`<li class="zz-ci-fail"><a href="https://prow.k8s.io/view/gs/kubernetes-ci-logs/pr-logs/pull/1/pull-kubernetes-e2e-gce/7" target="_blank" rel="noopener noreferrer">pull-kubernetes-e2e-gce</a></li>`,
		`<li class="zz-ci-fail">&lt;script&gt;x&lt;/script&gt;</li>`,
		`<li class="zz-ci-fail">evil-link</li>`,
		`<li class="zz-ci-fail">and 3 more failed</li>`,
		`<li class="zz-ci-note">Some CI results could not be read.</li>`,
	} {
		if !strings.Contains(busy, want) {
			t.Errorf("missing %s in:\n%s", want, busy)
		}
	}

	green := section("Green PR", "No jobs")
	if !strings.Contains(green, `<li class="zz-ci-pass">1 CI job successful</li>`) ||
		strings.Contains(green, "zz-ci-pending") || strings.Contains(green, "zz-ci-fail") {
		t.Errorf("a green PR shows only its passing count:\n%s", green)
	}
	for _, s := range []string{section("No jobs", "An issue"), section("An issue", "")} {
		if strings.Contains(s, `class="zz-ci"`) {
			t.Errorf("no jobs, no CI section:\n%s", s)
		}
	}
}

// A partial read that counted nothing (no statuses, check runs refused) is not a
// confirmed empty result: the section shows, with only the note.
func TestCISummaryPartialWithNoCounts(t *testing.T) {
	v := ciSummary(worklist.WorkItem{GitHub: worklist.GitHubRef{CI: &worklist.CISummary{SHA: "s", Partial: true}}})
	if v == nil || !v.Partial || v.Passed != "" || v.Pending != "" || len(v.Failed) != 0 {
		t.Fatalf("view = %+v", v)
	}
	if ciSummary(worklist.WorkItem{GitHub: worklist.GitHubRef{CI: &worklist.CISummary{SHA: "s"}}}) != nil {
		t.Fatal("a whole read with no jobs shows nothing")
	}
}

// The Discuss page shows the same CI summary at the top, in its header, before the
// conversation; an item with none shows no section there.
func TestThreadRendersCISummary(t *testing.T) {
	pr := worklist.WorkItem{ID: "github:o/r#1", Type: worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Number: 1, Repo: "o/r", Title: "A PR", CI: &worklist.CISummary{
			Passed: 3, Pending: 2, Failed: []string{"pull-e2e"}, FailedURLs: []string{"https://prow.k8s.io/view/x/1"}}}}
	out := renderPage(t, pageData{View: "thread", Item: pr, ConvEnabled: true})
	head, ci, chat := strings.Index(out, "zz-thread-head"), strings.Index(out, `class="zz-ci"`), strings.Index(out, "zz-chat")
	if head < 0 || ci < head || chat < ci {
		t.Fatalf("CI section must sit in the thread header, before the conversation:\n%s", out)
	}
	for _, want := range []string{
		`<li class="zz-ci-pass">3 CI jobs successful</li>`,
		`<li class="zz-ci-pending">2 CI jobs in progress</li>`,
		`<li class="zz-ci-fail"><a href="https://prow.k8s.io/view/x/1" target="_blank" rel="noopener noreferrer">pull-e2e</a></li>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	pr.GitHub.CI = nil
	if out := renderPage(t, pageData{View: "thread", Item: pr, ConvEnabled: true}); strings.Contains(out, `class="zz-ci"`) {
		t.Fatal("no CI summary, no section")
	}
}
