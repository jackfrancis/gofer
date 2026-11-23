package webui

import (
	"strings"
	"testing"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// Each item is drawn in GitHub's state: the Octicon and colour github.com uses, with
// the state spelled out for a screen reader. Merged and closed outrank draft, as on
// GitHub; an unknown state reads as open.
func TestStateOf(t *testing.T) {
	pr := func(ref worklist.GitHubRef) worklist.WorkItem {
		return worklist.WorkItem{Type: worklist.TypePullRequest, GitHub: ref}
	}
	issue := func(ref worklist.GitHubRef) worklist.WorkItem {
		return worklist.WorkItem{Type: worklist.TypeIssue, GitHub: ref}
	}
	cases := []struct {
		name string
		it   worklist.WorkItem
		want itemState
	}{
		{"open PR", pr(worklist.GitHubRef{State: "open"}), statePROpen},
		{"PR with no state", pr(worklist.GitHubRef{}), statePROpen},
		{"draft PR", pr(worklist.GitHubRef{State: "open", Draft: true}), statePRDraft},
		{"merged PR", pr(worklist.GitHubRef{State: "closed", Merged: true}), statePRMerged},
		{"closed draft PR", pr(worklist.GitHubRef{State: "closed", Draft: true}), statePRClosed},
		{"open issue", issue(worklist.GitHubRef{State: "open"}), stateIssueOpen},
		{"completed issue", issue(worklist.GitHubRef{State: "closed", StateReason: "completed"}), stateIssueDone},
		{"closed issue, no reason", issue(worklist.GitHubRef{State: "closed"}), stateIssueDone},
		{"not planned issue", issue(worklist.GitHubRef{State: "closed", StateReason: "not_planned"}), stateIssueNotPlanned},
	}
	for _, tc := range cases {
		if got := stateOf(tc.it); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got.label, tc.want.label)
		}
	}
}

// The radar and the thread page put the state icon before the title, in place of the
// old "PR"/"Issue" label.
func TestStateIconRendersBeforeTitle(t *testing.T) {
	draft := worklist.WorkItem{ID: "github:o/r#1", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Number: 1, Repo: "o/r", Title: "A draft", State: "open", Draft: true}}
	issue := worklist.WorkItem{ID: "github:o/r#2", Type: worklist.TypeIssue, GitHub: worklist.GitHubRef{Number: 2, Repo: "o/r", Title: "An issue", State: "open"}}

	out := renderPage(t, pageData{View: "worklist", Items: []worklist.WorkItem{draft, issue}})
	for _, want := range []string{
		`<div class="zz-title"><span class="zz-state zz-state--draft" title="Draft pull request">`,
		`aria-label="Draft pull request"`,
		`<div class="zz-title"><span class="zz-state zz-state--open" title="Open issue">`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("radar missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `Label--secondary">PR<`) || strings.Contains(out, `Label--secondary">Issue<`) {
		t.Fatal("the PR/Issue text label should be gone")
	}

	thread := renderPage(t, pageData{View: "thread", Item: draft})
	if !strings.Contains(thread, `<div class="zz-title"><span class="zz-state zz-state--draft"`) {
		t.Fatalf("thread page missing the state icon:\n%s", thread)
	}
}
