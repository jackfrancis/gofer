package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/comment"
	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/worklist"
)

const draftCommit = "0123456789abcdef0123456789abcdef01234567"

type fakeReviewCommenter struct {
	fakeCommenter
	result  comment.ReviewResult
	reviews []comment.Review
	during  func() // when set, PendingReview calls it first (to look at the store mid-request)
	expire  bool   // when set, PendingReview waits out the request's deadline and fails, as a slow GitHub read would
}

func (f *fakeReviewCommenter) PendingReview(ctx context.Context, _ string, rv comment.Review) (comment.ReviewResult, error) {
	if f.during != nil {
		f.during()
	}
	if f.expire {
		<-ctx.Done()
		f.reviews = append(f.reviews, rv)
		return comment.ReviewResult{}, ctx.Err()
	}
	f.reviews = append(f.reviews, rv)
	return f.result, nil
}

const draftItemID = "github:o/r#1"

func draftedThread(at time.Time) []worklist.Message {
	return []worklist.Message{
		{ID: "q", Role: worklist.RoleUser, Content: "Can you review this PR?", Kind: worklist.KindReviewRequest, At: at},
		{ID: "r", ReplyTo: "q", Role: worklist.RoleAgent, Content: "Rename x.", Model: "m1", At: at},
		{ID: "d", Role: worklist.RoleUser, Content: draftReviewPrompt, Kind: worklist.KindDraftReviewRequest, At: at},
		{ID: "a", ReplyTo: "d", Role: worklist.RoleAgent, Content: "Drafted 2.", At: at, Draft: &worklist.DraftReview{
			Repo: "o/r", Number: 1, Commit: draftCommit, BaseRef: "main", Body: "- Add a test.",
			Patches: map[string]string{"a.go": worklist.PatchDigest("pa"), "b.go": worklist.PatchDigest("pb")},
			Comments: []worklist.DraftComment{
				{Path: "a.go", Line: 3, Side: "RIGHT", Body: "Rename x."},
				{Path: "b.go", Line: 9, Side: "LEFT", Body: "Why remove this?"},
			},
		}},
	}
}

// draftFixture is one open pull request with thread, for a GitHub user, with the
// assistant on.
func draftFixture(t *testing.T, c Commenter, thread []worklist.Message) (*Handler, *recordPipeline, worklist.Store) {
	t.Helper()
	store := worklist.NewMemoryStore()
	store.Seed("github:42", worklist.WorkItem{
		ID: draftItemID, Source: "github", Type: worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1, Title: "fix", URL: "https://github.com/o/r/pull/1", State: "open"},
		Thread: thread,
	})
	p := &recordPipeline{}
	h := New(fakeSessions{&session.User{ID: "github:42", Provider: "github", Login: "octocat"}}, store, p, reconnectProviders{false}, true,
		[]ModelOption{{Value: "d", Label: "default", Endpoint: "e0", Model: "m0"}, {Value: "o", Label: "other", Endpoint: "e1", Model: "m1"}})
	if c != nil {
		h.UseCommenter(c)
	}
	return h, p, store
}

func threadPage(t *testing.T, h *Handler) (string, http.Header) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Thread(rec, httptest.NewRequest(http.MethodGet, "/items/thread?id="+url.QueryEscape(draftItemID), nil))
	return rec.Body.String(), rec.Header()
}

// The button sits at the bottom of a reviewed pull request's thread for a user who
// connected commenting, disabled while there is no review or a reply is in flight;
// an unconnected user gets the connect link; without commenting there is neither.
func TestThreadDraftReviewButton(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	reviewed := draftedThread(old)[:2]
	connected := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}

	h, _, _ := draftFixture(t, connected, reviewed)
	page, _ := threadPage(t, h)
	i := strings.Index(page, `action="/items/draft-review"`)
	if i < 0 || strings.Contains(page[i:strings.Index(page[i:], "</form>")+i], "disabled") {
		t.Fatalf("no enabled Draft Review Comments button:\n%s", page)
	}
	if j := strings.Index(page, `class="zz-chat-form"`); j < i {
		t.Error("the button is not below the thread, before the message box")
	}

	h, _, _ = draftFixture(t, connected, []worklist.Message{{ID: "q", Role: worklist.RoleUser, Content: "hi", At: old}})
	if page, _ := threadPage(t, h); !strings.Contains(page, "disabled title=\"Review this pull request first") {
		t.Error("an unreviewed pull request's button is not disabled")
	}
	pending := append(draftedThread(old)[:2], worklist.Message{ID: "p", Role: worklist.RoleUser, Content: "more?", At: time.Now()})
	h, _, _ = draftFixture(t, connected, pending)
	if page, _ := threadPage(t, h); !strings.Contains(page, "disabled title=\"Wait for the reply") {
		t.Error("the button is not disabled while a reply is in flight")
	}

	h, _, _ = draftFixture(t, &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true}}, reviewed)
	if page, _ := threadPage(t, h); !strings.Contains(page, `href="/auth/github-write/login">Connect GitHub to comment`) || strings.Contains(page, `action="/items/draft-review"`) {
		t.Error("an unconnected user does not get the connect link instead of the button")
	}
	for name, c := range map[string]Commenter{"disabled": &fakeReviewCommenter{}, "no reviews": &fakeCommenter{enabled: true, connected: true}} {
		h, _, _ = draftFixture(t, c, reviewed)
		if page, _ := threadPage(t, h); strings.Contains(page, "draft-review") || strings.Contains(page, "Connect GitHub to comment") {
			t.Errorf("%s: the thread offers drafting", name)
		}
	}
}

// The newest draft carries the selection form, naming its turn and digest; the
// thread lets it post its real origin.
func TestThreadShowsDraftSelection(t *testing.T) {
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
	th := draftedThread(time.Now().Add(-time.Hour))
	h, _, _ := draftFixture(t, c, th)
	page, hdr := threadPage(t, h)
	for _, want := range []string{
		`action="/items/draft-review/create"`, `name="msg" value="a"`, `name="digest" value="` + th[3].Draft.Digest() + `"`,
		`name="c" value="0" checked`, `name="c" value="1" checked`, `<code>b.go</code> line 9 (removed line)`, `name="summary" value="1" checked`,
		"asked for draft review comments",
		// The exact stored text each checkbox sends, and where it goes.
		`<pre class="zz-draft-text">Rename x.</pre>`, `<pre class="zz-draft-text">Why remove this?</pre>`, `<pre class="zz-draft-text">- Add a test.</pre>`,
		`Pending review on o/r #1, at commit <code>0123456</code> against <code>main</code>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("thread lacks %s", want)
		}
	}
	if hdr.Get("Referrer-Policy") != "same-origin" {
		t.Errorf("Referrer-Policy = %q", hdr.Get("Referrer-Policy"))
	}
}

// The button appends the draft request and dispatches it to the default model; a
// stale page (nothing reviewed yet) appends and dispatches nothing.
func TestDraftReviewDispatches(t *testing.T) {
	post := func(h *Handler) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/items/draft-review", strings.NewReader(url.Values{"id": {draftItemID}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.DraftReview(rec, req)
		return rec
	}
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
	h, p, store := draftFixture(t, c, draftedThread(time.Now().Add(-time.Hour))[:2])
	if rec := post(h); rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d", rec.Code)
	}
	it, _, _ := store.Get(context.Background(), "github:42", draftItemID)
	last := it.Thread[len(it.Thread)-1]
	if last.Kind != worklist.KindDraftReviewRequest || len(p.turns) != 1 || p.turns[0].Turn != last.ID || p.converseModels[0] != "m0" {
		t.Fatalf("appended %+v, dispatched %+v models %v", last, p.turns, p.converseModels)
	}
	// The request is now pending: a second click does nothing.
	post(h)
	if len(p.turns) != 1 {
		t.Errorf("a second click dispatched again: %+v", p.turns)
	}
	h, p, store = draftFixture(t, c, []worklist.Message{{ID: "q", Role: worklist.RoleUser, Content: "hi", At: time.Now().Add(-time.Hour)}})
	post(h)
	if it, _, _ := store.Get(context.Background(), "github:42", draftItemID); len(it.Thread) != 1 || len(p.turns) != 0 {
		t.Error("an unreviewed pull request took a draft request")
	}
}

func postCreate(h *Handler, form url.Values, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/items/draft-review/create", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.CreateDraftReview(rec, req)
	return rec
}

// Creating the pending review sends the stored draft's selected comments, anchored
// to its commit, never anything from the form but the selection.
func TestCreateDraftReviewSendsTheStoredSelection(t *testing.T) {
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}, result: comment.ReviewResult{Created: true, URL: "https://github.com/o/r/pull/1#pullrequestreview-9"}}
	th := draftedThread(time.Now().Add(-time.Hour))
	h, _, _ := draftFixture(t, c, th)
	rec := postCreate(h, url.Values{"id": {draftItemID}, "msg": {"a"}, "digest": {th[3].Draft.Digest()}, "c": {"1", "1", "7", "x"}, "body": {"injected"}, "path": {"evil"}},
		"Sec-Fetch-Site", "same-origin")
	if rec.Code != http.StatusOK || len(c.reviews) != 1 {
		t.Fatalf("status %d, reviews %+v\n%s", rec.Code, c.reviews, rec.Body.String())
	}
	rv := c.reviews[0]
	if rv.Repo != "o/r" || rv.Number != 1 || rv.Commit != draftCommit || rv.BaseRef != "main" || rv.Body != "" || len(rv.Comments) != 1 {
		t.Fatalf("review = %+v", rv)
	}
	if got := rv.Comments[0]; got.Path != "b.go" || got.Line != 9 || got.Side != "LEFT" || got.Body != "Why remove this?" {
		t.Errorf("comment = %+v", got)
	}
	if len(rv.Patches) != 1 || rv.Patches["b.go"] != worklist.PatchDigest("pb") {
		t.Errorf("patches sent = %v, want b.go's only", rv.Patches)
	}
	if !strings.Contains(rec.Body.String(), "Pending review created") || !strings.Contains(rec.Body.String(), "pullrequestreview-9") {
		t.Errorf("result page:\n%s", rec.Body.String())
	}
}

// Nothing is created cross-site, for a draft that is not the newest or whose digest
// does not match, with nothing selected, or on an item off the radar.
func TestCreateDraftReviewRefusals(t *testing.T) {
	th := draftedThread(time.Now().Add(-time.Hour))
	good := url.Values{"id": {draftItemID}, "msg": {"a"}, "digest": {th[3].Draft.Digest()}, "c": {"0"}, "summary": {"1"}}
	with := func(k, v string) url.Values {
		f := url.Values{}
		for key, vs := range good {
			f[key] = append([]string(nil), vs...)
		}
		f.Set(k, v)
		return f
	}
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
	h, _, store := draftFixture(t, c, th)
	if rec := postCreate(h, good, "Sec-Fetch-Site", "cross-site"); rec.Code != http.StatusForbidden {
		t.Errorf("cross-site: status %d", rec.Code)
	}
	if rec := postCreate(h, good, "Origin", "null"); rec.Code != http.StatusForbidden {
		t.Errorf("null origin: status %d", rec.Code)
	}
	for name, form := range map[string]url.Values{
		"stale digest":   with("digest", "00"),
		"not a draft":    with("msg", "r"),
		"nothing chosen": {"id": {draftItemID}, "msg": {"a"}, "digest": {th[3].Draft.Digest()}},
	} {
		if rec := postCreate(h, form, "Sec-Fetch-Site", "same-origin"); rec.Code < 400 {
			t.Errorf("%s: status %d", name, rec.Code)
		}
	}
	// A newer draft supersedes the one the page showed.
	newer := append(th, worklist.Message{ID: "d2", Role: worklist.RoleUser, Content: draftReviewPrompt, Kind: worklist.KindDraftReviewRequest},
		worklist.Message{ID: "a2", ReplyTo: "d2", Role: worklist.RoleAgent, Content: "Drafted 1.", Draft: &worklist.DraftReview{Repo: "o/r", Number: 1, Commit: draftCommit, BaseRef: "main", Body: "- x"}})
	_ = store.Update(context.Background(), "github:42", func(cur []worklist.WorkItem) ([]worklist.WorkItem, error) {
		cur[0].Thread = newer
		return cur, nil
	})
	if rec := postCreate(h, good, "Sec-Fetch-Site", "same-origin"); rec.Code != http.StatusConflict {
		t.Errorf("superseded draft: status %d", rec.Code)
	}
	_ = store.Update(context.Background(), "github:42", func(cur []worklist.WorkItem) ([]worklist.WorkItem, error) {
		cur[0].Thread = th
		cur[0].Meta.HiddenAt = time.Now()
		return cur, nil
	})
	if rec := postCreate(h, good, "Sec-Fetch-Site", "same-origin"); rec.Code != http.StatusConflict {
		t.Errorf("hidden item: status %d", rec.Code)
	}
	if len(c.reviews) != 0 {
		t.Errorf("a review was created: %+v", c.reviews)
	}
}

// GitHub's refusal is shown, and nothing claims a review was created.
func TestCreateDraftReviewShowsRefusal(t *testing.T) {
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}, result: comment.ReviewResult{Error: "You already have a pending review on this pull request"}}
	th := draftedThread(time.Now().Add(-time.Hour))
	h, _, _ := draftFixture(t, c, th)
	rec := postCreate(h, url.Values{"id": {draftItemID}, "msg": {"a"}, "digest": {th[3].Draft.Digest()}, "summary": {"1"}}, "Sec-Fetch-Site", "same-origin")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "No pending review created") || !strings.Contains(rec.Body.String(), "already have a pending review") {
		t.Errorf("status %d:\n%s", rec.Code, rec.Body.String())
	}
	if len(c.reviews) != 1 || c.reviews[0].Body != "- Add a test." || len(c.reviews[0].Comments) != 0 {
		t.Errorf("reviews = %+v", c.reviews)
	}
}

func draftConfirmForm(th []worklist.Message) url.Values {
	return url.Values{"id": {draftItemID}, "msg": {"a"}, "digest": {th[3].Draft.Digest()}, "c": {"0"}}
}

// A confirmation is recorded before GitHub is asked, so replaying the POST, even
// after the review was submitted or discarded on GitHub, never creates a second
// review: it shows what the first did, and the thread shows the receipt instead of
// the form.
func TestCreateDraftReviewReplayIsSafe(t *testing.T) {
	th := draftedThread(time.Now().Add(-time.Hour))
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}, result: comment.ReviewResult{Created: true, URL: "https://github.com/o/r/pull/1#pullrequestreview-9"}}
	h, _, store := draftFixture(t, c, th)
	c.during = func() {
		it, _, _ := store.Get(context.Background(), "github:42", draftItemID)
		if r := it.Thread[3].DraftReceipt; r == nil || r.State != worklist.DraftCreating {
			t.Errorf("GitHub was asked before the confirmation was claimed: %+v", r)
		}
	}
	if rec := postCreate(h, draftConfirmForm(th), "Sec-Fetch-Site", "same-origin"); rec.Code != http.StatusOK {
		t.Fatalf("first: %d", rec.Code)
	}
	c.during = nil
	rec := postCreate(h, draftConfirmForm(th), "Sec-Fetch-Site", "same-origin")
	if rec.Code != http.StatusOK || len(c.reviews) != 1 || !strings.Contains(rec.Body.String(), "already confirmed") || !strings.Contains(rec.Body.String(), "pullrequestreview-9") {
		t.Fatalf("replay: %d, %d reviews\n%s", rec.Code, len(c.reviews), rec.Body.String())
	}
	page, _ := threadPage(t, h)
	if strings.Contains(page, `action="/items/draft-review/create"`) || !strings.Contains(page, "Pending review created on GitHub") {
		t.Error("the thread still offers a confirmed draft")
	}
}

// An unknown outcome is recorded too: a replay creates nothing. A definite failure
// releases the claim, so the owner can try again.
func TestCreateDraftReviewUnknownAndFailure(t *testing.T) {
	th := draftedThread(time.Now().Add(-time.Hour))
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}, result: comment.ReviewResult{Unknown: true, Error: "lost"}}
	h, _, _ := draftFixture(t, c, th)
	postCreate(h, draftConfirmForm(th), "Sec-Fetch-Site", "same-origin")
	rec := postCreate(h, draftConfirmForm(th), "Sec-Fetch-Site", "same-origin")
	if len(c.reviews) != 1 || !strings.Contains(rec.Body.String(), "may have created a pending review") {
		t.Fatalf("replay after unknown: %d reviews\n%s", len(c.reviews), rec.Body.String())
	}
	if page, _ := threadPage(t, h); !strings.Contains(page, "gofer may have created this pending review") {
		t.Error("the thread does not say the outcome is unknown")
	}

	c = &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}, result: comment.ReviewResult{Error: "The pull request has new commits"}}
	h, _, _ = draftFixture(t, c, th)
	postCreate(h, draftConfirmForm(th), "Sec-Fetch-Site", "same-origin")
	postCreate(h, draftConfirmForm(th), "Sec-Fetch-Site", "same-origin")
	if len(c.reviews) != 2 {
		t.Errorf("a definite failure did not release the claim: %d attempts", len(c.reviews))
	}
}

// A write-back that points the item at another pull request retires its draft: the
// thread stops offering it and a confirmation from the old page is refused.
func TestCreateDraftReviewPinsTheDestination(t *testing.T) {
	th := draftedThread(time.Now().Add(-time.Hour))
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
	h, _, store := draftFixture(t, c, th)
	_ = store.Update(context.Background(), "github:42", func(cur []worklist.WorkItem) ([]worklist.WorkItem, error) {
		cur[0].GitHub.Repo, cur[0].GitHub.Number = "o/other", 7
		return cur, nil
	})
	if page, _ := threadPage(t, h); strings.Contains(page, `action="/items/draft-review/create"`) {
		t.Error("the thread offers a draft pinned to another pull request")
	}
	if rec := postCreate(h, draftConfirmForm(th), "Sec-Fetch-Site", "same-origin"); rec.Code != http.StatusConflict || len(c.reviews) != 0 {
		t.Errorf("status %d, reviews %+v", rec.Code, c.reviews)
	}
}

// A redraft that found nothing to draft retires the draft before it.
func TestEmptyRedraftRetiresTheDraft(t *testing.T) {
	th := append(draftedThread(time.Now().Add(-time.Hour)),
		worklist.Message{ID: "d2", Role: worklist.RoleUser, Content: draftReviewPrompt, Kind: worklist.KindDraftReviewRequest, At: time.Now().Add(-time.Hour)},
		worklist.Message{ID: "a2", ReplyTo: "d2", Role: worklist.RoleAgent, Content: "Nothing to draft.", At: time.Now().Add(-time.Hour)})
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
	h, _, _ := draftFixture(t, c, th)
	if page, _ := threadPage(t, h); strings.Contains(page, `action="/items/draft-review/create"`) {
		t.Error("the thread still offers the draft an empty redraft replaced")
	}
	if rec := postCreate(h, draftConfirmForm(th), "Sec-Fetch-Site", "same-origin"); rec.Code != http.StatusConflict || len(c.reviews) != 0 {
		t.Errorf("status %d, reviews %+v", rec.Code, c.reviews)
	}
}

// A request whose deadline ran out reading GitHub, before anything was sent, still
// releases its claim (settling has its own deadline), so the draft can be confirmed
// again.
func TestCreateDraftReviewReleasesAfterTheDeadline(t *testing.T) {
	old := draftReviewTimeout
	draftReviewTimeout = 50 * time.Millisecond
	t.Cleanup(func() { draftReviewTimeout = old })
	th := draftedThread(time.Now().Add(-time.Hour))
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}, expire: true}
	h, _, mem := draftFixture(t, c, th)
	// The store honours its context, as SQLite does, so a write under the request's
	// expired deadline would fail.
	store := ctxStore{mem}
	h.store = store
	if rec := postCreate(h, draftConfirmForm(th), "Sec-Fetch-Site", "same-origin"); rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d", rec.Code)
	}
	if it, _, _ := store.Get(context.Background(), "github:42", draftItemID); it.Thread[3].DraftReceipt != nil {
		t.Fatalf("the claim was not released: %+v", it.Thread[3].DraftReceipt)
	}
	c.expire = false
	c.result = comment.ReviewResult{Created: true}
	postCreate(h, draftConfirmForm(th), "Sec-Fetch-Site", "same-origin")
	if len(c.reviews) != 2 {
		t.Errorf("the draft could not be confirmed again: %d attempts", len(c.reviews))
	}
}

// ctxStore is a store whose writes fail once their context is done.
type ctxStore struct{ worklist.Store }

func (s ctxStore) Update(ctx context.Context, ownerID string, fn func([]worklist.WorkItem) ([]worklist.WorkItem, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Store.Update(ctx, ownerID, fn)
}
