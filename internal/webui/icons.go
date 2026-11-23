package webui

import (
	"html/template"
	"strings"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// itemState is one of the states GitHub draws an issue or pull request in: the
// Octicon it uses, its colour (a zz-state--* class), and the words a screen
// reader and the tooltip give.
type itemState struct {
	class string
	label string
	icon  string // an <svg> body: one or more <path> elements on a 16x16 grid
}

// Primer Octicons (MIT, github.com/primer/octicons), 16px.
const (
	octPullRequest       = `<path d="M1.5 3.25a2.25 2.25 0 1 1 3 2.122v5.256a2.251 2.251 0 1 1-1.5 0V5.372A2.25 2.25 0 0 1 1.5 3.25Zm5.677-.177L9.573.677A.25.25 0 0 1 10 .854V2.5h1A2.5 2.5 0 0 1 13.5 5v5.628a2.251 2.251 0 1 1-1.5 0V5a1 1 0 0 0-1-1h-1v1.646a.25.25 0 0 1-.427.177L7.177 3.427a.25.25 0 0 1 0-.354ZM3.75 2.5a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm0 9.5a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm8.25.75a.75.75 0 1 0 1.5 0 .75.75 0 0 0-1.5 0Z"/>`
	octPullRequestDraft  = `<path d="M3.25 1A2.25 2.25 0 0 1 4 5.372v5.256a2.251 2.251 0 1 1-1.5 0V5.372A2.251 2.251 0 0 1 3.25 1Zm9.5 14a2.25 2.25 0 1 1 0-4.5 2.25 2.25 0 0 1 0 4.5ZM2.5 3.25a.75.75 0 1 0 1.5 0 .75.75 0 0 0-1.5 0ZM3.25 12a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm9.5 0a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5ZM14 7.5a1.25 1.25 0 1 1-2.5 0 1.25 1.25 0 0 1 2.5 0Zm0-4.25a1.25 1.25 0 1 1-2.5 0 1.25 1.25 0 0 1 2.5 0Z"/>`
	octMerge             = `<path d="M5.45 5.154A4.25 4.25 0 0 0 9.25 7.5h1.378a2.251 2.251 0 1 1 0 1.5H9.25A5.734 5.734 0 0 1 5 7.123v3.505a2.25 2.25 0 1 1-1.5 0V5.372a2.25 2.25 0 1 1 1.95-.218ZM4.25 13.5a.75.75 0 1 0 0-1.5.75.75 0 0 0 0 1.5Zm8.5-4.5a.75.75 0 1 0 0-1.5.75.75 0 0 0 0 1.5ZM5 3.25a.75.75 0 1 0 0 .005V3.25Z"/>`
	octPullRequestClosed = `<path d="M3.25 1A2.25 2.25 0 0 1 4 5.372v5.256a2.251 2.251 0 1 1-1.5 0V5.372A2.251 2.251 0 0 1 3.25 1Zm9.5 5.5a.75.75 0 0 1 .75.75v3.378a2.251 2.251 0 1 1-1.5 0V7.25a.75.75 0 0 1 .75-.75Zm-2.03-5.273a.75.75 0 0 1 1.06 0l.97.97.97-.97a.748.748 0 0 1 1.265.332.75.75 0 0 1-.205.729l-.97.97.97.97a.751.751 0 0 1-.018 1.042.751.751 0 0 1-1.042.018l-.97-.97-.97.97a.749.749 0 0 1-1.275-.326.749.749 0 0 1 .215-.734l.97-.97-.97-.97a.75.75 0 0 1 0-1.06ZM2.5 3.25a.75.75 0 1 0 1.5 0 .75.75 0 0 0-1.5 0ZM3.25 12a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm9.5 0a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Z"/>`
	octIssueOpened       = `<path d="M8 9.5a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3Z"/><path d="M8 0a8 8 0 1 1 0 16A8 8 0 0 1 8 0ZM1.5 8a6.5 6.5 0 1 0 13 0 6.5 6.5 0 0 0-13 0Z"/>`
	octIssueClosed       = `<path d="M11.28 6.78a.75.75 0 0 0-1.06-1.06L7.25 8.69 5.78 7.22a.75.75 0 0 0-1.06 1.06l2 2a.75.75 0 0 0 1.06 0l3.5-3.5Z"/><path d="M16 8A8 8 0 1 1 0 8a8 8 0 0 1 16 0Zm-1.5 0a6.5 6.5 0 1 0-13 0 6.5 6.5 0 0 0 13 0Z"/>`
	octSkip              = `<path d="M8 0a8 8 0 1 1 0 16A8 8 0 0 1 8 0ZM1.5 8a6.5 6.5 0 1 0 13 0 6.5 6.5 0 0 0-13 0Zm9.78-2.22-5.5 5.5a.749.749 0 0 1-1.275-.326.749.749 0 0 1 .215-.734l5.5-5.5a.751.751 0 0 1 1.042.018.751.751 0 0 1 .018 1.042Z"/>`
)

var (
	statePROpen          = itemState{"zz-state--open", "Open pull request", octPullRequest}
	statePRDraft         = itemState{"zz-state--draft", "Draft pull request", octPullRequestDraft}
	statePRMerged        = itemState{"zz-state--merged", "Merged pull request", octMerge}
	statePRClosed        = itemState{"zz-state--closed", "Closed pull request", octPullRequestClosed}
	stateIssueOpen       = itemState{"zz-state--open", "Open issue", octIssueOpened}
	stateIssueDone       = itemState{"zz-state--done", "Closed issue (completed)", octIssueClosed}
	stateIssueNotPlanned = itemState{"zz-state--muted", "Closed issue (not planned)", octSkip}
)

// stateOf picks the state GitHub would draw the item in. An unknown state reads
// as open: the radar lists open work (see worklist.WorkItem.OnRadar).
func stateOf(it worklist.WorkItem) itemState {
	closed := strings.EqualFold(it.GitHub.State, "closed")
	if it.Type == worklist.TypePullRequest {
		switch {
		case it.GitHub.Merged:
			return statePRMerged
		case closed:
			return statePRClosed
		case it.GitHub.Draft:
			return statePRDraft
		}
		return statePROpen
	}
	if closed {
		if it.GitHub.StateReason == "not_planned" {
			return stateIssueNotPlanned
		}
		return stateIssueDone
	}
	return stateIssueOpen
}

// stateIcon renders the item's GitHub state icon. Every string in it is one of
// the constants above, so nothing from the item reaches the markup.
func stateIcon(it worklist.WorkItem) template.HTML {
	s := stateOf(it)
	return template.HTML(`<span class="zz-state ` + s.class + `" title="` + s.label + `">` +
		`<svg class="zz-state-icon" viewBox="0 0 16 16" width="16" height="16" role="img" aria-label="` + s.label + `" focusable="false">` +
		s.icon + `</svg></span>`)
}
