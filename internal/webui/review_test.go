package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/worklist"
)

type fakeSessions struct{ user *session.User }

func (f fakeSessions) CurrentUser(*http.Request) *session.User { return f.user }

type recordPipeline struct {
	reviewed          []string
	reviewEndpoint    string
	reviewModel       string
	refreshed         int
	converses         []string
	converseModels    []string
	converseEndpoints []string
	secondOpinions    []string
	soModels          []string
	soEndpoints       []string
}

func (p *recordPipeline) EnsureBackfill(context.Context, string) error { return nil }
func (p *recordPipeline) Converse(_ context.Context, _, itemID, endpoint, model string) error {
	p.converses = append(p.converses, itemID)
	p.converseModels = append(p.converseModels, model)
	p.converseEndpoints = append(p.converseEndpoints, endpoint)
	return nil
}
func (p *recordPipeline) ReviewAll(_ context.Context, _ string, ids []string, endpoint, model string) error {
	p.reviewed = append(p.reviewed, ids...)
	p.reviewEndpoint = endpoint
	p.reviewModel = model
	return nil
}
func (p *recordPipeline) Refresh(context.Context, string) error { p.refreshed++; return nil }
func (p *recordPipeline) SecondOpinion(_ context.Context, _, itemID, endpoint, model string) error {
	p.secondOpinions = append(p.secondOpinions, itemID)
	p.soModels = append(p.soModels, model)
	p.soEndpoints = append(p.soEndpoints, endpoint)
	return nil
}
func (p *recordPipeline) SecondOpinionAll(_ context.Context, _ string, ids []string, endpoint, model string) error {
	p.secondOpinions = append(p.secondOpinions, ids...)
	p.soModels = append(p.soModels, model)
	p.soEndpoints = append(p.soEndpoints, endpoint)
	return nil
}

// The thread view stops spinning and shows a retry hint once a pending reply has
// stalled, and hides it while a reply is still plausibly in-flight.
func TestThreadStalledHint(t *testing.T) {
	pr := worklist.WorkItem{ID: "github:o/r#1", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Number: 1, Repo: "o/r", Title: "A PR"}}
	pr.Thread = []worklist.Message{{Role: worklist.RoleUser, Content: "Can you review this PR?"}}

	if out := renderPage(t, pageData{View: "thread", Item: pr, ConvEnabled: true, ReplyStalled: true}); !strings.Contains(out, "Resend to try again") {
		t.Fatalf("expected the stalled retry hint, got:\n%s", out)
	}
	if out := renderPage(t, pageData{View: "thread", Item: pr, ConvEnabled: true, ReplyStalled: false}); strings.Contains(out, "Resend to try again") {
		t.Fatal("hint should be hidden when a reply is not stalled")
	}
}

// The review-panel's programmatic turns are chrome, not chat: the independent-review
// request renders as a small "@user has requested a 2nd review" notice (never a user
// bubble), and the consensus/synthesis prompt is hidden entirely — only its verdict
// reply shows.
func TestThreadHidesProgrammaticPrompts(t *testing.T) {
	pr := worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Number: 1, Repo: "o/r", Title: "A PR"}}
	pr.Thread = []worklist.Message{
		{Role: worklist.RoleUser, Content: independentReviewPrompt, Kind: worklist.KindReviewRequest},
		{Role: worklist.RoleAgent, Content: "Independent review body.", Model: "alt"},
		{Role: worklist.RoleUser, Content: "Do all the reviews of this PR agree?", Kind: worklist.KindSynthesisRequest},
		{Role: worklist.RoleAgent, Content: "The reviews align.", Model: "default", Verdict: worklist.VerdictAgree},
	}
	out := renderPage(t, pageData{View: "thread", Item: pr, ConvEnabled: true, User: &session.User{Login: "jackfrancis", Name: "Jack"}})

	if !strings.Contains(out, "@jackfrancis has requested a 2nd review") {
		t.Fatalf("expected the 2nd-review notice, got:\n%s", out)
	}
	if strings.Contains(out, "Give an independent review of this PR") {
		t.Fatal("the independent-review prompt should not be shown verbatim")
	}
	if strings.Contains(out, "Do all the reviews of this PR") {
		t.Fatal("the synthesis prompt should be hidden")
	}
	if !strings.Contains(out, "Independent review body.") || !strings.Contains(out, "The reviews align.") {
		t.Fatalf("agent replies should still render, got:\n%s", out)
	}
	if strings.Contains(out, "zz-msg--user") {
		t.Fatal("a programmatic request must not render as a user chat bubble")
	}
}

// HideMessage toggles the hidden flag on the addressed thread turn, leaves the others
// untouched, and treats an out-of-range index as a safe no-op.
func TestHideMessageTogglesFlag(t *testing.T) {
	store := worklist.NewMemoryStore()
	store.Seed("u1", worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, Thread: []worklist.Message{
		{Role: worklist.RoleUser, Content: "q"},
		{Role: worklist.RoleAgent, Content: "a"},
	}})
	h := &Handler{sessions: fakeSessions{&session.User{ID: "u1"}}, store: store, convEnabled: true, now: time.Now}

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/items/thread/hide", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.HideMessage(rec, req)
		return rec
	}

	if rec := post("id=pr1&msg=1&hidden=true"); rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (Post/Redirect/Get)", rec.Code)
	}
	items, _ := store.List(context.Background(), "u1")
	if !items[0].Thread[1].Hidden {
		t.Fatal("expected the agent reply (index 1) to be hidden")
	}
	if items[0].Thread[0].Hidden {
		t.Fatal("the other turn (index 0) must be untouched")
	}

	post("id=pr1&msg=1&hidden=false")
	items, _ = store.List(context.Background(), "u1")
	if items[0].Thread[1].Hidden {
		t.Fatal("expected the agent reply to be unhidden")
	}

	if rec := post("id=pr1&msg=9&hidden=true"); rec.Code != http.StatusSeeOther {
		t.Fatalf("out-of-range index status = %d, want 303 (safe no-op)", rec.Code)
	}
}

// A hidden thread turn renders collapsed (its content withheld) with an Unhide
// control; a visible turn renders with a Hide control that posts its index.
func TestThreadHideControls(t *testing.T) {
	pr := worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Number: 1, Repo: "o/r", Title: "A PR"}}
	pr.Thread = []worklist.Message{
		{Role: worklist.RoleUser, Content: "visible question"},
		{Role: worklist.RoleAgent, Content: "secret reply", Hidden: true},
	}
	out := renderPage(t, pageData{View: "thread", Item: pr, ConvEnabled: true, User: &session.User{ID: "u1"}})

	if !strings.Contains(out, `action="/items/thread/hide"`) || !strings.Contains(out, ">Hide</button>") {
		t.Fatalf("expected a Hide control on the visible turn, got:\n%s", out)
	}
	if strings.Contains(out, "secret reply") {
		t.Fatal("a hidden turn's content must not be rendered")
	}
	if !strings.Contains(out, ">Unhide</button>") {
		t.Fatalf("expected an Unhide control on the hidden turn, got:\n%s", out)
	}
}

// "Review all PRs" targets only PRs that still need a first review: it skips a PR
// whose review is plausibly in-flight (fresh pending turn), re-dispatches one whose
// pending review has gone stale (a failed run), skips a PR already reviewed, still
// reviews a PR that has only been discussed, and never touches issues.
func TestReviewAllPRsSelfHeals(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	store := worklist.NewMemoryStore()
	rr := func(at time.Time) worklist.Message {
		return worklist.Message{Role: worklist.RoleUser, Content: "r", Kind: worklist.KindReviewRequest, At: at}
	}
	fresh := worklist.WorkItem{ID: "pr-fresh", Type: worklist.TypePullRequest, Thread: []worklist.Message{rr(now.Add(-1 * time.Minute))}}
	stale := worklist.WorkItem{ID: "pr-stale", Type: worklist.TypePullRequest, Thread: []worklist.Message{rr(now.Add(-10 * time.Minute))}}
	reviewed := worklist.WorkItem{ID: "pr-reviewed", Type: worklist.TypePullRequest, Thread: []worklist.Message{rr(now.Add(-10 * time.Minute)), {Role: worklist.RoleAgent, At: now.Add(-9 * time.Minute)}}}
	discussed := worklist.WorkItem{ID: "pr-discussed", Type: worklist.TypePullRequest, Thread: []worklist.Message{{Role: worklist.RoleUser, Content: "hi", At: now.Add(-10 * time.Minute)}, {Role: worklist.RoleAgent, At: now.Add(-9 * time.Minute)}}}
	issue := worklist.WorkItem{ID: "iss", Type: worklist.TypeIssue}
	store.Seed("u1", fresh, stale, reviewed, discussed, issue)

	pipe := &recordPipeline{}
	h := &Handler{sessions: fakeSessions{&session.User{ID: "u1"}}, store: store, pipeline: pipe, convEnabled: true, now: func() time.Time { return now }}

	h.ReviewAllPRs(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/items/review-all", nil))

	got := map[string]bool{}
	for _, id := range pipe.reviewed {
		got[id] = true
	}
	if got["pr-fresh"] {
		t.Error("a fresh in-flight review should be skipped")
	}
	if !got["pr-stale"] {
		t.Error("a stale pending review should be re-dispatched (self-heal)")
	}
	if got["pr-reviewed"] {
		t.Error("a PR already reviewed should be skipped")
	}
	if !got["pr-discussed"] {
		t.Error("a PR only discussed (never reviewed) should still be reviewed")
	}
	if got["iss"] {
		t.Error("issues should never be reviewed")
	}
}

// reviewedByModel builds a PR reviewed once by the given model (stamped on the agent
// reply), so the bulk 2nd-opinion tests can exercise first-review-model detection.
func reviewedByModel(id, model string, now time.Time) worklist.WorkItem {
	return worklist.WorkItem{ID: id, Type: worklist.TypePullRequest, Thread: []worklist.Message{
		{Role: worklist.RoleUser, Content: "r", Kind: worklist.KindReviewRequest, At: now.Add(-10 * time.Minute)},
		{Role: worklist.RoleAgent, Content: "a", Model: model, At: now.Add(-9 * time.Minute)},
	}}
}

// When the eligible PRs' first reviews are homogeneous, the bulk "Get 2nd Opinion"
// runs the review panel using the model the user picked from the menu — always a
// different model than the first review (a tampered pick falls back to a different
// one). It requires an alternative model.
func TestSecondOpinionAllMenuMode(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	opts := []ModelOption{
		{Value: "c0|default", Endpoint: "https://default", Model: "default"},
		{Value: "c1|alt", Endpoint: "https://alt", Model: "alt"},
	}
	newHandler := func(store worklist.Store, pipe Pipeline) *Handler {
		return &Handler{sessions: fakeSessions{&session.User{ID: "u1"}}, store: store, pipeline: pipe, convEnabled: true, options: opts, now: func() time.Time { return now }}
	}
	post := func(h *Handler, body string) {
		req := httptest.NewRequest(http.MethodPost, "/items/second-opinion-all", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		h.SecondOpinionAllPRs(httptest.NewRecorder(), req)
	}

	// Two PRs both first-reviewed by "default" (homogeneous); the user picks "alt".
	store := worklist.NewMemoryStore()
	store.Seed("u1", reviewedByModel("pr1", "default", now), reviewedByModel("pr2", "default", now),
		worklist.WorkItem{ID: "pr-none", Type: worklist.TypePullRequest}) // unreviewed: skipped
	pipe := &recordPipeline{}
	post(newHandler(store, pipe), "choice=c1|alt")
	if len(pipe.secondOpinions) != 2 {
		t.Fatalf("expected both reviewed PRs dispatched, got %v", pipe.secondOpinions)
	}
	if len(pipe.soModels) != 1 || pipe.soModels[0] != "alt" || pipe.soEndpoints[0] != "https://alt" {
		t.Fatalf("menu mode should dispatch the chosen 'alt' for all: models=%v endpoints=%v", pipe.soModels, pipe.soEndpoints)
	}
	items, _ := store.List(context.Background(), "u1")
	for _, it := range items {
		if it.ID == "pr1" {
			last := it.Thread[len(it.Thread)-1]
			if last.Content != independentReviewPrompt || last.Kind != worklist.KindReviewRequest {
				t.Fatalf("expected a tagged independent-review prompt, got %+v", last)
			}
		}
	}

	// A tampered pick of the excluded (first-review) model falls back to a different one.
	store2 := worklist.NewMemoryStore()
	store2.Seed("u1", reviewedByModel("pr1", "default", now))
	pipe2 := &recordPipeline{}
	post(newHandler(store2, pipe2), "choice=c0|default")
	if len(pipe2.soModels) != 1 || pipe2.soModels[0] != "alt" {
		t.Fatalf("a pick of the first-review model must fall back to a different one, got %v", pipe2.soModels)
	}

	// Disabled with no alternative model configured.
	off := &recordPipeline{}
	ho := &Handler{sessions: fakeSessions{&session.User{ID: "u1"}}, store: store, pipeline: off, convEnabled: true, options: opts[:1], now: func() time.Time { return now }}
	ho.SecondOpinionAllPRs(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/items/second-opinion-all", nil))
	if len(off.secondOpinions) != 0 {
		t.Fatal("bulk 2nd opinion must not dispatch without an alternative model")
	}
}

// When the eligible PRs' first reviews are heterogeneous, the bulk "Get 2nd Opinion"
// engages immediately (no menu) and auto-picks a different model per PR.
func TestSecondOpinionAllAutoMode(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	store := worklist.NewMemoryStore()
	store.Seed("u1", reviewedByModel("pr1", "default", now), reviewedByModel("pr2", "alt", now))
	pipe := &recordPipeline{}
	opts := []ModelOption{
		{Value: "c0|default", Endpoint: "https://default", Model: "default"},
		{Value: "c1|alt", Endpoint: "https://alt", Model: "alt"},
	}
	h := &Handler{sessions: fakeSessions{&session.User{ID: "u1"}}, store: store, pipeline: pipe, convEnabled: true, options: opts, now: func() time.Time { return now }}

	// No choice posted: heterogeneous -> immediate, auto-pick per PR.
	h.SecondOpinionAllPRs(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/items/second-opinion-all", nil))

	if len(pipe.secondOpinions) != 2 || len(pipe.soModels) != 2 {
		t.Fatalf("expected a per-PR dispatch for both, got items=%v models=%v", pipe.secondOpinions, pipe.soModels)
	}
	got := map[string]string{}
	for i, id := range pipe.secondOpinions {
		got[id] = pipe.soModels[i]
	}
	if got["pr1"] != "alt" {
		t.Errorf("pr1 first-reviewed by default -> 2nd opinion should differ (alt), got %q", got["pr1"])
	}
	if got["pr2"] != "default" {
		t.Errorf("pr2 first-reviewed by alt -> 2nd opinion should differ (default), got %q", got["pr2"])
	}
}

// Refresh forces a re-ingest for the signed-in owner and redirects (PRG); an
// anonymous request neither dispatches nor errors.
func TestRefreshReingests(t *testing.T) {
	pipe := &recordPipeline{}
	h := &Handler{sessions: fakeSessions{&session.User{ID: "u1"}}, pipeline: pipe}

	rec := httptest.NewRecorder()
	h.Refresh(rec, httptest.NewRequest(http.MethodPost, "/items/refresh", nil))
	if pipe.refreshed != 1 {
		t.Fatalf("Refresh dispatches = %d, want 1", pipe.refreshed)
	}
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (Post/Redirect/Get)", rec.Code)
	}

	anon := &recordPipeline{}
	ha := &Handler{sessions: fakeSessions{nil}, pipeline: anon}
	ha.Refresh(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/items/refresh", nil))
	if anon.refreshed != 0 {
		t.Fatal("an anonymous refresh must not dispatch")
	}
}

// Reset Conversations clears the thread and its read cursor on every one of the
// owner's items — including ones hidden from the radar — while leaving the work
// itself (GitHub fields, signals, metadata overrides) untouched, and redirects (PRG).
func TestResetConversationsClearsThreads(t *testing.T) {
	read := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	reviewed := worklist.WorkItem{
		ID: "pr1", Type: worklist.TypePullRequest,
		GitHub:  worklist.GitHubRef{Number: 1, Repo: "o/r", Title: "A PR"},
		Signals: worklist.Signals{Comments: 7},
		Thread: []worklist.Message{
			{Role: worklist.RoleUser, Content: reviewPrompt, Kind: worklist.KindReviewRequest},
			{Role: worklist.RoleAgent, Content: "Looks good.", Model: "default"},
		},
		ThreadReadAt: read,
	}
	hidden := worklist.WorkItem{ID: "pr2", Type: worklist.TypePullRequest,
		Thread: []worklist.Message{{Role: worklist.RoleAgent, Content: "old reply"}}}
	hidden.Meta.HiddenAt = read
	issue := worklist.WorkItem{ID: "issue1", Type: worklist.TypeIssue, GitHub: worklist.GitHubRef{Title: "An issue"}}

	store := worklist.NewMemoryStore()
	store.Seed("u1", reviewed, hidden, issue)
	h := &Handler{sessions: fakeSessions{&session.User{ID: "u1"}}, store: store}

	rec := httptest.NewRecorder()
	h.ResetConversations(rec, httptest.NewRequest(http.MethodPost, "/items/reset-conversations", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (Post/Redirect/Get)", rec.Code)
	}

	items, err := store.List(context.Background(), "u1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3 (a reset clears conversations, never work)", len(items))
	}
	byID := map[string]worklist.WorkItem{}
	for _, it := range items {
		if len(it.Thread) != 0 {
			t.Errorf("%s: thread = %d messages, want 0", it.ID, len(it.Thread))
		}
		if !it.ThreadReadAt.IsZero() {
			t.Errorf("%s: ThreadReadAt = %v, want zero", it.ID, it.ThreadReadAt)
		}
		byID[it.ID] = it
	}
	if got := byID["pr1"]; got.GitHub.Title != "A PR" || got.Signals.Comments != 7 {
		t.Errorf("pr1 work was altered: %+v", got.GitHub)
	}
	if !byID["pr2"].Meta.HiddenAt.Equal(read) {
		t.Error("a hidden item must stay hidden after a conversation reset")
	}

	// Every PR is a first-review candidate again — the point of the reset.
	if !byID["pr1"].NeedsReview(read, reviewStaleAfter) {
		t.Error("pr1 should need a review again after the reset")
	}

	anonStore := worklist.NewMemoryStore()
	anonStore.Seed("u1", reviewed)
	ha := &Handler{sessions: fakeSessions{nil}, store: anonStore}
	ha.ResetConversations(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/items/reset-conversations", nil))
	if got, _ := anonStore.List(context.Background(), "u1"); len(got[0].Thread) == 0 {
		t.Fatal("an anonymous reset must not clear anything")
	}
}

// SecondOpinion appends the review prompt and dispatches a review by the second
// model, then redirects to the thread; it no-ops when no second model is configured.
func TestSecondOpinionDispatches(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := worklist.NewMemoryStore()
	store.Seed("u1", worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest})
	pipe := &recordPipeline{}
	opts := []ModelOption{
		{Value: "c0|default-model", Label: "default-model", Endpoint: "https://default", Model: "default-model"},
		{Value: "c1|gpt-5.6-sol", Label: "gpt-5.6-sol", Endpoint: "https://second", Model: "gpt-5.6-sol"},
	}
	h := &Handler{sessions: fakeSessions{&session.User{ID: "u1"}}, store: store, pipeline: pipe, convEnabled: true, options: opts, now: func() time.Time { return now }}

	req := httptest.NewRequest(http.MethodPost, "/items/second-opinion", strings.NewReader("id=pr1&choice=c1|gpt-5.6-sol"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.SecondOpinion(rec, req)

	if len(pipe.secondOpinions) != 1 || pipe.secondOpinions[0] != "pr1" {
		t.Fatalf("dispatched = %v, want [pr1]", pipe.secondOpinions)
	}
	if len(pipe.soModels) != 1 || pipe.soModels[0] != "gpt-5.6-sol" || pipe.soEndpoints[0] != "https://second" {
		t.Fatalf("dispatched choice wrong: models=%v endpoints=%v", pipe.soModels, pipe.soEndpoints)
	}
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (Post/Redirect/Get)", rec.Code)
	}
	items, _ := store.List(context.Background(), "u1")
	if n := len(items[0].Thread); n == 0 || items[0].Thread[n-1].Content != independentReviewPrompt {
		t.Fatalf("expected the independent-review prompt appended, got thread %+v", items[0].Thread)
	}

	// Disabled (no alternative options configured): redirect, no dispatch.
	off := &recordPipeline{}
	ho := &Handler{sessions: fakeSessions{&session.User{ID: "u1"}}, store: store, pipeline: off, now: func() time.Time { return now }}
	ho.SecondOpinion(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/items/second-opinion", strings.NewReader("id=pr1&choice=c1|gpt-5.6-sol")))
	if len(off.secondOpinions) != 0 {
		t.Fatal("second opinion must not dispatch when disabled")
	}
}

// A normal Discuss turn routes to the connection+model chosen in the thread's picker;
// an unknown or missing value falls back to the default (option 0).
func TestThreadPostUsesSelectedModel(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := worklist.NewMemoryStore()
	store.Seed("u1", worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest})
	pipe := &recordPipeline{}
	opts := []ModelOption{
		{Value: "c0|default-model", Label: "default-model", Endpoint: "https://default", Model: "default-model"},
		{Value: "c1|gpt-5.6-sol", Label: "gpt-5.6-sol", Endpoint: "https://second", Model: "gpt-5.6-sol"},
	}
	h := &Handler{sessions: fakeSessions{&session.User{ID: "u1"}}, store: store, pipeline: pipe, convEnabled: true, options: opts, now: func() time.Time { return now }}

	// Picks the alternative connection+model.
	req := httptest.NewRequest(http.MethodPost, "/items/thread", strings.NewReader("id=pr1&content=hi&choice=c1|gpt-5.6-sol"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ThreadPost(httptest.NewRecorder(), req)
	if len(pipe.converseModels) != 1 || pipe.converseModels[0] != "gpt-5.6-sol" || pipe.converseEndpoints[0] != "https://second" {
		t.Fatalf("converse choice wrong: models=%v endpoints=%v", pipe.converseModels, pipe.converseEndpoints)
	}

	// An unknown choice falls back to the default option.
	req2 := httptest.NewRequest(http.MethodPost, "/items/thread", strings.NewReader("id=pr1&content=hi&choice=bogus"))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ThreadPost(httptest.NewRecorder(), req2)
	if len(pipe.converseModels) != 2 || pipe.converseModels[1] != "default-model" || pipe.converseEndpoints[1] != "https://default" {
		t.Fatalf("fallback choice wrong: models=%v endpoints=%v", pipe.converseModels, pipe.converseEndpoints)
	}
}
