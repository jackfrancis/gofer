package webui

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/jackfrancis/gofer/internal/worklist"
)

func renderPage(t *testing.T, data pageData) string {
	t.Helper()
	tmpl := template.Must(template.New("webui").Funcs(funcs).ParseFS(templatesFS, "templates/*.html"))
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "page.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

// The "Review all PRs" toolbar button appears only when the assistant is
// configured and the radar holds at least one pull request, and posts to the
// concurrent-review endpoint.
func TestWorklistReviewAllButton(t *testing.T) {
	pr := worklist.WorkItem{ID: "github:o/r#1", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Number: 1, Repo: "o/r", Title: "A PR"}}
	issue := worklist.WorkItem{ID: "github:o/r#2", Type: worklist.TypeIssue, GitHub: worklist.GitHubRef{Number: 2, Repo: "o/r", Title: "An issue"}}

	// Shown when conversation is enabled and there are PRs needing review; the count
	// is surfaced.
	out := renderPage(t, pageData{View: "worklist", Items: []worklist.WorkItem{pr, pr, issue}, ConvEnabled: true, ReviewCount: 2})
	if !strings.Contains(out, `action="/items/review-all"`) {
		t.Fatalf("expected the Review all PRs form, got:\n%s", out)
	}
	if !strings.Contains(out, "Review all PRs (2)") {
		t.Fatalf("expected the button to show the review count, got:\n%s", out)
	}

	// Hidden when no PR needs a review.
	if out := renderPage(t, pageData{View: "worklist", Items: []worklist.WorkItem{issue}, ConvEnabled: true, ReviewCount: 0}); strings.Contains(out, "/items/review-all") {
		t.Fatal("button should be hidden when no PR needs a review")
	}

	// Hidden when the assistant is not configured.
	if out := renderPage(t, pageData{View: "worklist", Items: []worklist.WorkItem{pr}, ConvEnabled: false, ReviewCount: 1}); strings.Contains(out, "/items/review-all") {
		t.Fatal("button should be hidden when the conversation is disabled")
	}

	// With more than one model configured it becomes a dropdown of every model.
	menu := renderPage(t, pageData{View: "worklist", Items: []worklist.WorkItem{pr}, ConvEnabled: true, ReviewCount: 2, ReviewModelOptions: []ModelOption{{Value: "c0|a", Label: "model-a"}, {Value: "c1|b", Label: "model-b"}}})
	if !strings.Contains(menu, "<details") || !strings.Contains(menu, `value="c0|a"`) || !strings.Contains(menu, "model-b") {
		t.Fatalf("expected a model dropdown for review-all, got:\n%s", menu)
	}
	if !strings.Contains(menu, `action="/items/review-all"`) {
		t.Fatalf("dropdown items should post to review-all, got:\n%s", menu)
	}
}

// The Refresh action is always available on the radar, independent of the model or
// whether any PRs are present.
func TestWorklistRefreshButton(t *testing.T) {
	issue := worklist.WorkItem{ID: "github:o/r#2", Type: worklist.TypeIssue, GitHub: worklist.GitHubRef{Number: 2, Repo: "o/r", Title: "An issue"}}
	out := renderPage(t, pageData{View: "worklist", Items: []worklist.WorkItem{issue}, ConvEnabled: false, ReviewCount: 0})
	if !strings.Contains(out, `action="/items/refresh"`) {
		t.Fatalf("expected the Refresh form even with no PRs and no model, got:\n%s", out)
	}
}

// "Reset Conversations" appears only when something on the radar carries a
// conversation, and shows how many will be cleared.
func TestWorklistResetConversationsButton(t *testing.T) {
	pr := worklist.WorkItem{ID: "github:o/r#1", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Number: 1, Repo: "o/r", Title: "A PR"}}

	out := renderPage(t, pageData{View: "worklist", Items: []worklist.WorkItem{pr}, ConvEnabled: true, ResetCount: 3})
	if !strings.Contains(out, `action="/items/reset-conversations"`) {
		t.Fatalf("expected the Reset Conversations form, got:\n%s", out)
	}
	if !strings.Contains(out, "Reset Conversations (3)") {
		t.Fatalf("expected the button to show how many conversations it clears, got:\n%s", out)
	}

	if out := renderPage(t, pageData{View: "worklist", Items: []worklist.WorkItem{pr}, ConvEnabled: true, ResetCount: 0}); strings.Contains(out, "/items/reset-conversations") {
		t.Fatal("button should be hidden when there is no conversation to clear")
	}
}

// The "Get 2nd Opinion" toolbar button renders as a model dropdown when the eligible
// PRs' first reviews are homogeneous ("menu"), as an immediate button when they are
// heterogeneous ("auto"), and not at all when the mode is empty.
func TestWorklistSecondOpinionButton(t *testing.T) {
	pr := worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Number: 1, Repo: "o/r", Title: "A PR"}}

	// Menu mode: a dropdown of the offered models (the first-review model is excluded upstream).
	menu := renderPage(t, pageData{View: "worklist", Items: []worklist.WorkItem{pr}, ConvEnabled: true, SecondOpinionEnabled: true, SecondOpinionCount: 3, SecondOpinionMode: "menu", SecondOpinionMenuOptions: []ModelOption{{Value: "c1|alt", Label: "alt"}}})
	if !strings.Contains(menu, `action="/items/second-opinion-all"`) || !strings.Contains(menu, "Get 2nd Opinion (3)") {
		t.Fatalf("expected the 2nd-opinion dropdown, got:\n%s", menu)
	}
	if !strings.Contains(menu, "<details") || !strings.Contains(menu, `value="c1|alt"`) {
		t.Fatalf("expected the menu options, got:\n%s", menu)
	}

	// Auto mode: an immediate button, no model menu.
	auto := renderPage(t, pageData{View: "worklist", Items: []worklist.WorkItem{pr}, ConvEnabled: true, SecondOpinionEnabled: true, SecondOpinionCount: 2, SecondOpinionMode: "auto"})
	if !strings.Contains(auto, `action="/items/second-opinion-all"`) || !strings.Contains(auto, "Get 2nd Opinion (2)") {
		t.Fatalf("expected the immediate 2nd-opinion button, got:\n%s", auto)
	}
	if strings.Contains(auto, "2nd opinion with") {
		t.Fatal("auto mode should not render a model menu")
	}

	// Hidden when the mode is empty (no eligible PRs / no different model / disabled).
	if out := renderPage(t, pageData{View: "worklist", Items: []worklist.WorkItem{pr}, ConvEnabled: true, SecondOpinionEnabled: true, SecondOpinionMode: ""}); strings.Contains(out, "/items/second-opinion-all") {
		t.Fatal("2nd-opinion button should be hidden when the mode is empty")
	}
}

// The radar shows the handshake cue when a consensus synthesis found the reviews in
// agreement and the masks cue for disagreement, and keeps the sparkle cue for a plain
// discussion with no synthesis.
func TestWorklistAgreementBadge(t *testing.T) {
	base := func(id string, thread []worklist.Message) worklist.WorkItem {
		return worklist.WorkItem{ID: id, Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Number: 1, Repo: "o/r", Title: id}, Thread: thread}
	}
	synth := func(verdict string) []worklist.Message {
		return []worklist.Message{
			{Role: worklist.RoleUser, Kind: worklist.KindReviewRequest},
			{Role: worklist.RoleAgent, Content: "review"},
			{Role: worklist.RoleUser, Kind: worklist.KindSynthesisRequest},
			{Role: worklist.RoleAgent, Content: "verdict", Verdict: verdict},
		}
	}
	if out := renderPage(t, pageData{View: "worklist", Items: []worklist.WorkItem{base("pr-agree", synth(worklist.VerdictAgree))}, ConvEnabled: true}); !strings.Contains(out, "🤝") {
		t.Fatalf("expected the agreement badge, got:\n%s", out)
	}
	if out := renderPage(t, pageData{View: "worklist", Items: []worklist.WorkItem{base("pr-dis", synth(worklist.VerdictDisagree))}, ConvEnabled: true}); !strings.Contains(out, "🎭") {
		t.Fatalf("expected the disagreement badge, got:\n%s", out)
	}
	plain := renderPage(t, pageData{View: "worklist", Items: []worklist.WorkItem{base("pr-chat", []worklist.Message{{Role: worklist.RoleUser, Content: "hi"}, {Role: worklist.RoleAgent, Content: "hello"}})}, ConvEnabled: true})
	if !strings.Contains(plain, "✨") {
		t.Fatalf("expected the discussion cue, got:\n%s", plain)
	}
	if strings.Contains(plain, "🤝") || strings.Contains(plain, "🎭") {
		t.Fatal("a plain thread must not show an agreement badge")
	}
}

// The thread view shows the 2nd-opinion button only when a second model is
// configured, and attributes an agent reply to the model that produced it.
func TestThreadSecondOpinionAndModelLabel(t *testing.T) {
	pr := worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Number: 1, Repo: "o/r", Title: "A PR"}}
	pr.Thread = []worklist.Message{{Role: worklist.RoleAgent, Content: "looks good", Model: "gpt-5"}}

	on := renderPage(t, pageData{View: "thread", Item: pr, ConvEnabled: true, SecondOpinionEnabled: true, SecondOpinionOptions: []ModelOption{{Value: "c1|gpt-5.6-sol", Label: "gpt-5.6-sol"}}})
	if !strings.Contains(on, `action="/items/second-opinion"`) {
		t.Fatalf("expected the 2nd-opinion form, got:\n%s", on)
	}
	if !strings.Contains(on, `<option value="c1|gpt-5.6-sol">gpt-5.6-sol</option>`) {
		t.Fatalf("expected the option in the picker, got:\n%s", on)
	}
	if !strings.Contains(on, "via gpt-5") {
		t.Fatalf("expected the model attribution, got:\n%s", on)
	}
	if off := renderPage(t, pageData{View: "thread", Item: pr, ConvEnabled: true, SecondOpinionEnabled: false}); strings.Contains(off, "/items/second-opinion") {
		t.Fatal("2nd-opinion form should be hidden when no second model is configured")
	}
}

// The thread's model picker lists every configured model (default first) when more
// than one is configured, and is hidden for a single-option deployment.
func TestThreadModelPicker(t *testing.T) {
	pr := worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Number: 1, Repo: "o/r", Title: "A PR"}}

	multi := renderPage(t, pageData{View: "thread", Item: pr, ConvEnabled: true, ModelOptions: []ModelOption{
		{Value: "c0|claude-opus-4.8", Label: "claude-opus-4.8"},
		{Value: "c1|gpt-5.4", Label: "gpt-5.4 (OpenAI)"},
	}})
	if !strings.Contains(multi, `<select name="choice">`) {
		t.Fatalf("expected the model picker, got:\n%s", multi)
	}
	if !strings.Contains(multi, `<option value="c0|claude-opus-4.8">claude-opus-4.8</option>`) || !strings.Contains(multi, `<option value="c1|gpt-5.4">gpt-5.4 (OpenAI)</option>`) {
		t.Fatalf("expected both labeled options, got:\n%s", multi)
	}

	single := renderPage(t, pageData{View: "thread", Item: pr, ConvEnabled: true, ModelOptions: []ModelOption{{Value: "c0|claude-opus-4.8", Label: "claude-opus-4.8"}}})
	if strings.Contains(single, `name="choice"`) {
		t.Fatal("model picker should be hidden for a single-option deployment")
	}
}
