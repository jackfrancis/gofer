package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// Each item is drawn in GitHub's state: the Octicon and colour github.com uses, with
// the state spelled out for a screen reader. Merged and closed outrank draft, as on
// GitHub; an unknown state on a live item reads as open. A retired item was last seen
// open and may since be merged, closed or simply no longer involve the user, so its
// state is unknown unless the stored one is already closed. A close reason is named
// only when GitHub gave it.
func TestStateOf(t *testing.T) {
	pr := func(ref worklist.GitHubRef) worklist.WorkItem {
		return worklist.WorkItem{Type: worklist.TypePullRequest, GitHub: ref}
	}
	issue := func(ref worklist.GitHubRef) worklist.WorkItem {
		return worklist.WorkItem{Type: worklist.TypeIssue, GitHub: ref}
	}
	retired := func(it worklist.WorkItem) worklist.WorkItem {
		it.Meta.CompletedAt = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
		return it
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
		{"closed issue, no reason", issue(worklist.GitHubRef{State: "closed"}), stateIssueClosed},
		{"closed issue, unknown reason", issue(worklist.GitHubRef{State: "closed", StateReason: "someday"}), stateIssueClosed},
		{"not planned issue", issue(worklist.GitHubRef{State: "closed", StateReason: "not_planned"}), stateIssueNotPlan},
		{"duplicate issue", issue(worklist.GitHubRef{State: "closed", StateReason: "duplicate"}), stateIssueDuplicate},
		{"retired open PR", retired(pr(worklist.GitHubRef{State: "open"})), statePRUnknown},
		{"retired draft PR", retired(pr(worklist.GitHubRef{State: "open", Draft: true})), statePRUnknown},
		{"retired merged PR", retired(pr(worklist.GitHubRef{State: "closed", Merged: true})), statePRMerged},
		{"retired closed PR", retired(pr(worklist.GitHubRef{State: "closed"})), statePRClosed},
		{"retired open issue", retired(issue(worklist.GitHubRef{State: "open"})), stateIssueUnknown},
		{"retired closed issue", retired(issue(worklist.GitHubRef{State: "closed", StateReason: "duplicate"})), stateIssueDuplicate},
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

// A retired PR's Discuss thread stays reachable; its header must not call the PR open
// when gofer last saw it open and has since lost track of it.
func TestThreadRetiredItemStateUnknown(t *testing.T) {
	pr := worklist.WorkItem{ID: "github:o/r#1", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Number: 1, Repo: "o/r", Title: "Gone", State: "open"}}
	pr.Meta.CompletedAt = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	out := renderPage(t, pageData{View: "thread", Item: pr})
	if strings.Contains(out, "Open pull request") {
		t.Fatalf("retired PR shown as open:\n%s", out)
	}
	if !strings.Contains(out, `aria-label="Pull request (no longer on your radar; current state unknown)"`) {
		t.Fatalf("retired PR should read as state unknown:\n%s", out)
	}
}

// The Octicons licence ships with the binary that carries the copied paths.
func TestOcticonsLicenseServed(t *testing.T) {
	want, err := fs.ReadFile(staticFS, "static/octicons-LICENSE.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(want), "Copyright (c)") || !strings.Contains(string(want), "Permission is hereby granted") {
		t.Fatal("embedded licence lacks the copyright and permission notice")
	}
	rec := httptest.NewRecorder()
	(&Handler{}).Static().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/octicons-LICENSE.txt", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != string(want) {
		t.Fatalf("GET /static/octicons-LICENSE.txt: %d", rec.Code)
	}
}
