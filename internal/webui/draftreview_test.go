package webui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/comment"
	"github.com/jackfrancis/gofer/internal/ingest"
	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/worklist"
)

const draftCommit = "0123456789abcdef0123456789abcdef01234567"

// draftDiff is the digest of the whole diff an approving draft approves.
var draftDiff = worklist.PatchDigest("the whole diff")

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
		{ID: "d", Role: worklist.RoleUser, Content: draftReviewPrompt, Kind: worklist.KindDraftReviewRequest, At: at, ReadRevKnown: true},
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
	if page, _ := threadPage(t, h); !strings.Contains(page, "disabled title=\"There is nothing in this conversation to draft") {
		t.Error("a pull request with nothing answered yet does not disable the button")
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

// A thread that ends with the draft it offers shows no Draft Review Comments button:
// drafting again would only repeat it. A turn after it, or a confirmation of it,
// brings the button back; an unconnected user keeps the connect link.
func TestThreadHidesRedundantDraftButton(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	connected := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
	h, _, _ := draftFixture(t, connected, draftedThread(old))
	page, _ := threadPage(t, h)
	if strings.Contains(page, `action="/items/draft-review"`) || strings.Contains(page, ">Draft Review Comments<") {
		t.Error("the thread offers drafting again right under its draft")
	}
	if !strings.Contains(page, `action="/items/draft-review/create"`) {
		t.Error("the draft's selection is gone too")
	}

	more := append(draftedThread(old), worklist.Message{ID: "m", Role: worklist.RoleUser, Content: "Also check y.", At: old},
		worklist.Message{ID: "n", ReplyTo: "m", Role: worklist.RoleAgent, Content: "Checked.", At: old})
	h, _, _ = draftFixture(t, connected, more)
	if page, _ := threadPage(t, h); !strings.Contains(page, `action="/items/draft-review"`) {
		t.Error("the conversation moved on since the draft, but no button")
	}

	confirmed := draftedThread(old)
	confirmed[3].DraftReceipt = &worklist.DraftReceipt{State: worklist.DraftUnknown, At: old}
	h, _, _ = draftFixture(t, connected, confirmed)
	if page, _ := threadPage(t, h); !strings.Contains(page, `action="/items/draft-review"`) {
		t.Error("a confirmed draft (which says to draft again to start over) left no button")
	}

	// A draft requested before gofer recorded revisions keeps the button, as it did
	// then: toggles of that time left no trace to judge it by.
	legacy := draftedThread(old)
	legacy[2].ReadRevKnown = false
	h, _, _ = draftFixture(t, connected, legacy)
	if page, _ := threadPage(t, h); !strings.Contains(page, `action="/items/draft-review"`) {
		t.Error("a draft whose request recorded no revision hid the button")
	}

	h, _, _ = draftFixture(t, &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true}}, draftedThread(old))
	if page, _ := threadPage(t, h); !strings.Contains(page, "Connect GitHub to comment") {
		t.Error("an unconnected user lost the connect link")
	}
}

// A stale page's redundant draft request appends and dispatches nothing. The result
// page's "Draft again", offered when GitHub refused the draft as out of date, goes
// through only while that very draft (turn key and digest) is still the one offered:
// one naming another draft, or posted after a newer draft replaced it, is held to the
// ordinary check.
func TestDraftReviewRefusesRedundantRedraft(t *testing.T) {
	post := func(h *Handler, form url.Values) {
		req := httptest.NewRequest(http.MethodPost, "/items/draft-review", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		h.DraftReview(httptest.NewRecorder(), req)
	}
	turns := func(store worklist.Store) int {
		it, _, _ := store.Get(context.Background(), "github:42", draftItemID)
		return len(it.Thread)
	}
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
	th := draftedThread(time.Now().Add(-time.Hour))
	digest := th[3].Draft.Digest()
	h, p, store := draftFixture(t, c, th)
	for name, form := range map[string]url.Values{
		"plain":         {"id": {draftItemID}},
		"unbound":       {"id": {draftItemID}, "redraft": {"1"}},
		"other turn":    {"id": {draftItemID}, "redraft": {"1"}, "msg": {"r"}, "digest": {digest}},
		"other digest":  {"id": {draftItemID}, "redraft": {"1"}, "msg": {"a"}, "digest": {"0000"}},
		"digest absent": {"id": {draftItemID}, "redraft": {"1"}, "msg": {"a"}},
	} {
		post(h, form)
		if n := turns(store); n != len(th) || len(p.turns) != 0 {
			t.Fatalf("%s: a redundant draft request was appended or dispatched: %d turns, %+v", name, n, p.turns)
		}
	}
	rejected := url.Values{"id": {draftItemID}, "redraft": {"1"}, "msg": {"a"}, "digest": {digest}}
	post(h, rejected)
	if n := turns(store); n != len(th)+1 || len(p.turns) != 1 {
		t.Fatalf("the out-of-date redraft was refused: %d turns, %+v", n, p.turns)
	}

	// Another tab already redrafted, and the newer draft landed: the old result
	// page's button must not retire it, neither while it is redundant nor after a
	// discussion since made it not so.
	newerDraft := func(extra ...worklist.Message) []worklist.Message {
		th := append(draftedThread(time.Now().Add(-time.Hour)),
			worklist.Message{ID: "d2", Role: worklist.RoleUser, Content: draftReviewPrompt, Kind: worklist.KindDraftReviewRequest, At: time.Now().Add(-time.Hour), ReadRevKnown: true},
			worklist.Message{ID: "a2", ReplyTo: "d2", Role: worklist.RoleAgent, Content: "Drafted 1.", At: time.Now().Add(-time.Hour), Draft: &worklist.DraftReview{
				Repo: "o/r", Number: 1, Commit: strings.Repeat("b", 40), BaseRef: "main",
				Patches:  map[string]string{"a.go": worklist.PatchDigest("pa2")},
				Comments: []worklist.DraftComment{{Path: "a.go", Line: 4, Side: "RIGHT", Body: "Rename y."}},
			}})
		return append(th, extra...)
	}
	for name, th := range map[string][]worklist.Message{
		"redundant": newerDraft(),
		"discussed since": newerDraft(
			worklist.Message{ID: "m", Role: worklist.RoleUser, Content: "Also y?", At: time.Now().Add(-time.Hour)},
			worklist.Message{ID: "n", ReplyTo: "m", Role: worklist.RoleAgent, Content: "Yes.", At: time.Now().Add(-time.Hour)}),
	} {
		h, p, store = draftFixture(t, c, th)
		post(h, rejected)
		if n := turns(store); n != len(th) || len(p.turns) != 0 {
			t.Errorf("%s: a stale result page retired the newer draft: %d turns, %+v", name, n, p.turns)
		}
		// The thread's own button still drafts when the conversation moved on.
		if name == "discussed since" {
			post(h, url.Values{"id": {draftItemID}})
			if n := turns(store); n != len(th)+1 {
				t.Errorf("the thread's button no longer drafts after a discussion: %d turns", n)
			}
		}
	}
}

// Hiding or unhiding a turn before the draft request changes what the drafter would
// read, so the thread offers drafting again; the toggle is stamped.
func TestHidingAnEarlierTurnReopensDrafting(t *testing.T) {
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
	th := draftedThread(time.Now().Add(-time.Hour))
	h, _, store := draftFixture(t, c, th)
	if page, _ := threadPage(t, h); strings.Contains(page, `action="/items/draft-review"`) {
		t.Fatal("the thread offers drafting again right under its draft")
	}
	req := httptest.NewRequest(http.MethodPost, "/items/thread/hide", strings.NewReader(url.Values{"id": {draftItemID}, "msg": {"r"}, "hidden": {"true"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.HideMessage(httptest.NewRecorder(), req)
	it, _, _ := store.Get(context.Background(), "github:42", draftItemID)
	if !it.Thread[1].Hidden || it.Thread[1].HiddenRev != 1 {
		t.Fatalf("the hide took no revision: %+v", it.Thread[1])
	}
	if page, _ := threadPage(t, h); !strings.Contains(page, `action="/items/draft-review"`) {
		t.Error("an earlier turn was hidden since the draft, but no button")
	}
}

// When GitHub refuses the draft as out of date, the result page offers drafting
// again, since the thread hides its button under the draft.
func TestCreateDraftReviewStaleOffersRedraft(t *testing.T) {
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}, result: comment.ReviewResult{Stale: true, Error: "The pull request changed."}}
	th := draftedThread(time.Now().Add(-time.Hour))
	h, _, _ := draftFixture(t, c, th)
	rec := postCreate(h, draftConfirmForm(th), "Sec-Fetch-Site", "same-origin")
	if body := rec.Body.String(); rec.Code != http.StatusConflict || !strings.Contains(body, `name="redraft" value="1"`) || !strings.Contains(body, `action="/items/draft-review"`) ||
		!strings.Contains(body, `name="msg" value="a"`) || !strings.Contains(body, `name="digest" value="`+th[3].Draft.Digest()+`"`) {
		t.Errorf("status %d, no redraft form:\n%s", rec.Code, body)
	}
	c.result = comment.ReviewResult{Error: "You already have a pending review."}
	if body := postCreate(h, draftConfirmForm(th), "Sec-Fetch-Site", "same-origin").Body.String(); strings.Contains(body, `name="redraft"`) {
		t.Error("a refusal that is not about an out-of-date draft offers drafting again")
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
	// The request records the thread's visibility revision when it is saved.
	{
		c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
		th := draftedThread(time.Now().Add(-time.Hour))[:2]
		th[0].HiddenRev, th[1].HiddenRev = 4, 7
		h, _, store := draftFixture(t, c, th)
		req := httptest.NewRequest(http.MethodPost, "/items/draft-review", strings.NewReader(url.Values{"id": {draftItemID}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		h.DraftReview(httptest.NewRecorder(), req)
		it, _, _ := store.Get(context.Background(), "github:42", draftItemID)
		if got := it.Thread[len(it.Thread)-1]; got.Kind != worklist.KindDraftReviewRequest || got.ReadRev != 7 || !got.ReadRevKnown {
			t.Errorf("the request did not record the thread's revision: %+v", got)
		}
	}
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
	if last.ReadRev != 0 || !last.ReadRevKnown {
		t.Errorf("ReadRev = %d (known %v) on a thread nothing was ever hidden in", last.ReadRev, last.ReadRevKnown)
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

// An approving draft offers its approval as its own checkbox, showing the exact
// commands; confirming it sends them in the review body after the summary, leaving
// it unchecked sends none, and a draft that does not approve never sends them,
// whatever the form says.
func TestCreateDraftReviewApproval(t *testing.T) {
	approving := func() []worklist.Message {
		th := draftedThread(time.Now().Add(-time.Hour))
		th[3].Draft.Approve, th[3].Draft.Diff = true, draftDiff
		return th
	}
	create := func(t *testing.T, th []worklist.Message, form url.Values) (comment.Review, string) {
		t.Helper()
		c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}, result: comment.ReviewResult{Created: true}}
		h, _, _ := draftFixture(t, c, th)
		form.Set("id", draftItemID)
		form.Set("msg", "a")
		form.Set("digest", th[3].Draft.Digest())
		rec := postCreate(h, form, "Sec-Fetch-Site", "same-origin")
		if rec.Code != http.StatusOK || len(c.reviews) != 1 {
			t.Fatalf("status %d, reviews %+v\n%s", rec.Code, c.reviews, rec.Body.String())
		}
		return c.reviews[0], rec.Body.String()
	}

	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
	h, _, _ := draftFixture(t, c, approving())
	page, _ := threadPage(t, h)
	for _, want := range []string{`name="approve" value="1" checked`, "<pre class=\"zz-draft-text\">/lgtm\n/approve</pre>"} {
		if !strings.Contains(page, want) {
			t.Errorf("thread lacks %q", want)
		}
	}
	h, _, _ = draftFixture(t, c, draftedThread(time.Now().Add(-time.Hour)))
	if page, _ := threadPage(t, h); strings.Contains(page, `name="approve"`) {
		t.Error("a draft that does not approve offers an approval")
	}

	rv, body := create(t, approving(), url.Values{"summary": {"1"}, "approve": {"1"}})
	if rv.Body != "- Add a test.\n\n/lgtm\n/approve" || len(rv.Comments) != 0 {
		t.Errorf("summary and approval: body %q, %d comments", rv.Body, len(rv.Comments))
	}
	if rv.Diff != draftDiff {
		t.Errorf("approval sent without its diff to check: %q", rv.Diff)
	}
	if !strings.Contains(body, "a summary and the approval commands") {
		t.Errorf("result page:\n%s", body)
	}
	if rv, _ := create(t, approving(), url.Values{"approve": {"1"}}); rv.Body != worklist.ApprovalCommands {
		t.Errorf("approval only: body %q", rv.Body)
	}
	if rv, _ := create(t, approving(), url.Values{"summary": {"1"}}); rv.Body != "- Add a test." || rv.Diff != "" {
		t.Errorf("approval left out: body %q, diff %q", rv.Body, rv.Diff)
	}
	if rv, _ := create(t, draftedThread(time.Now().Add(-time.Hour)), url.Values{"summary": {"1"}, "approve": {"1"}}); rv.Body != "- Add a test." {
		t.Errorf("a draft that does not approve sent: %q", rv.Body)
	}
}

// A draft that only approves (no comment, no summary) is offered and creates a
// review whose body is the approval commands.
func TestCreateDraftReviewApprovalOnly(t *testing.T) {
	th := draftedThread(time.Now().Add(-time.Hour))
	th[3].Draft = &worklist.DraftReview{Repo: "o/r", Number: 1, Commit: draftCommit, BaseRef: "main", Approve: true, Diff: draftDiff}
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}, result: comment.ReviewResult{Created: true}}
	h, _, _ := draftFixture(t, c, th)
	page, _ := threadPage(t, h)
	if !strings.Contains(page, `name="approve" value="1" checked`) || strings.Contains(page, `name="summary"`) {
		t.Fatalf("approval-only draft not offered as such")
	}
	rec := postCreate(h, url.Values{"id": {draftItemID}, "msg": {"a"}, "digest": {th[3].Draft.Digest()}, "approve": {"1"}}, "Sec-Fetch-Site", "same-origin")
	if rec.Code != http.StatusOK || len(c.reviews) != 1 || c.reviews[0].Body != "/lgtm\n/approve" || len(c.reviews[0].Comments) != 0 {
		t.Fatalf("status %d, reviews %+v", rec.Code, c.reviews)
	}
	if !strings.Contains(rec.Body.String(), "created a pending review with the approval commands.") {
		t.Errorf("result page:\n%s", rec.Body.String())
	}
}

// GitHub's submit dialog can open with an empty summary that replaces the review's
// body, so the body sent is recorded on the receipt and shown, with a copy button
// and the warning, on the result page, on a replay, and on the thread afterwards; an
// unknown outcome shows it too. A review of inline comments only has no body to show.
func TestCreateDraftReviewShowsTheBodyToPaste(t *testing.T) {
	const want = "- Add a test.\n\n/lgtm\n/approve"
	th := draftedThread(time.Now().Add(-time.Hour))
	th[3].Draft.Approve, th[3].Draft.Diff = true, draftDiff
	form := url.Values{"id": {draftItemID}, "msg": {"a"}, "digest": {th[3].Draft.Digest()}, "summary": {"1"}, "approve": {"1"}}
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}, result: comment.ReviewResult{Created: true, URL: "https://github.com/o/r/pull/1#pullrequestreview-9"}}
	h, _, store := draftFixture(t, c, th)
	shows := func(t *testing.T, where, page string) {
		t.Helper()
		for _, s := range []string{
			"<pre class=\"zz-draft-text\">" + want + "</pre>",
			`data-copy="- Add a test.` + "\n\n" + `/lgtm` + "\n" + `/approve"`,
			"Finish your review", "the approval commands would never reach the pull request",
		} {
			if !strings.Contains(page, s) {
				t.Errorf("%s lacks %q", where, s)
			}
		}
	}
	rec := postCreate(h, form, "Sec-Fetch-Site", "same-origin")
	shows(t, "result page", rec.Body.String())
	if !strings.Contains(rec.Body.String(), `<script src="/static/copy.js" defer></script>`) {
		t.Error("the result page does not load copy.js")
	}
	it, _, _ := store.Get(context.Background(), "github:42", draftItemID)
	if r := it.Thread[3].DraftReceipt; r == nil || r.State != worklist.DraftCreated || r.Body != want {
		t.Fatalf("receipt = %+v", r)
	}
	page, _ := threadPage(t, h)
	shows(t, "thread", page)
	shows(t, "replay", postCreate(h, form, "Sec-Fetch-Site", "same-origin").Body.String())

	th = draftedThread(time.Now().Add(-time.Hour))
	c = &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}, result: comment.ReviewResult{Unknown: true, Error: "lost"}}
	h, _, _ = draftFixture(t, c, th)
	out := postCreate(h, url.Values{"id": {draftItemID}, "msg": {"a"}, "digest": {th[3].Draft.Digest()}, "summary": {"1"}}, "Sec-Fetch-Site", "same-origin").Body.String()
	if !strings.Contains(out, "<pre class=\"zz-draft-text\">- Add a test.</pre>") || strings.Contains(out, "approval commands would never") {
		t.Errorf("unknown outcome page:\n%s", out)
	}

	th = draftedThread(time.Now().Add(-time.Hour))
	c = &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}, result: comment.ReviewResult{Created: true}}
	h, _, _ = draftFixture(t, c, th)
	if out := postCreate(h, url.Values{"id": {draftItemID}, "msg": {"a"}, "digest": {th[3].Draft.Digest()}, "c": {"0"}}, "Sec-Fetch-Site", "same-origin").Body.String(); strings.Contains(out, "zz-review-body") {
		t.Error("a review with no body shows one to paste")
	}
}

// When the newest draft read a revision the latest review did not (a push or a
// retarget since), the thread offers "Review the current revision": it appends a
// review request, keeping the conversation, and dispatches it to the default model.
// With the review current, or a reply in flight, it does nothing.
func TestReviewAgain(t *testing.T) {
	const pushed = "fedcba9876543210fedcba9876543210fedcba98"
	post := func(h *Handler) {
		req := httptest.NewRequest(http.MethodPost, "/items/review-again", strings.NewReader(url.Values{"id": {draftItemID}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ReviewAgain(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d", rec.Code)
		}
	}
	outdated := func() []worklist.Message {
		th := draftedThread(time.Now().Add(-time.Hour))
		th[1].Head, th[1].Base = draftCommit, "main"
		th[3].Head, th[3].Base = pushed, "main"
		return th
	}
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
	h, p, store := draftFixture(t, c, outdated())
	page, _ := threadPage(t, h)
	if !strings.Contains(page, `action="/items/review-again"`) || !strings.Contains(page, "Review the current revision") {
		t.Fatal("an outdated review offers no review of the current revision")
	}
	post(h)
	it, _, _ := store.Get(context.Background(), "github:42", draftItemID)
	last := it.Thread[len(it.Thread)-1]
	if last.Kind != worklist.KindReviewRequest || last.Content != reviewAgainPrompt || len(p.turns) != 1 || p.turns[0].Turn != last.ID || p.converseModels[0] != "m0" {
		t.Fatalf("appended %+v, dispatched %+v models %v", last, p.turns, p.converseModels)
	}
	if len(it.Thread) != 5 {
		t.Errorf("the conversation was not kept: %d turns", len(it.Thread))
	}
	post(h) // the review is now pending
	if len(p.turns) != 1 {
		t.Error("a second click dispatched again")
	}

	th := outdated()
	th[3].Head = draftCommit // the draft read what the review read
	h, p, _ = draftFixture(t, c, th)
	if page, _ := threadPage(t, h); strings.Contains(page, "/items/review-again") {
		t.Error("a current review offers another")
	}
	post(h)
	if len(p.turns) != 0 {
		t.Error("a current review was reviewed again")
	}
}

// A draft stored under older rules (here, with a live command in its text) is not
// offered and cannot be confirmed; the thread says to draft again. A confirmed one
// still shows its receipt.
func TestOutdatedDraftIsRefused(t *testing.T) {
	th := draftedThread(time.Now().Add(-time.Hour))
	th[3].Draft.Body = "Ship it.\n/lgtm"
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}, result: comment.ReviewResult{Created: true}}
	h, _, _ := draftFixture(t, c, th)
	page, _ := threadPage(t, h)
	if strings.Contains(page, `action="/items/draft-review/create"`) || !strings.Contains(page, "saved before gofer's current checks") {
		t.Errorf("outdated draft offered, or no note:\n%s", page)
	}
	if !strings.Contains(page, `action="/items/draft-review"`) {
		t.Error("drafting again is not offered over an outdated draft")
	}
	rec := postCreate(h, url.Values{"id": {draftItemID}, "msg": {"a"}, "digest": {th[3].Draft.Digest()}, "summary": {"1"}}, "Sec-Fetch-Site", "same-origin")
	if rec.Code != http.StatusConflict || len(c.reviews) != 0 || !strings.Contains(rec.Body.String(), "draft the review comments again") {
		t.Fatalf("status %d, reviews %+v\n%s", rec.Code, c.reviews, rec.Body.String())
	}
	th[3].DraftReceipt = &worklist.DraftReceipt{State: worklist.DraftCreated, URL: "https://github.com/o/r/pull/1#pullrequestreview-1", At: time.Now()}
	h, _, _ = draftFixture(t, c, th)
	if page, _ := threadPage(t, h); !strings.Contains(page, "Pending review created on GitHub") {
		t.Error("a confirmed outdated draft hides its receipt")
	}
}

// The body is saved in the claim, before GitHub is contacted, so a confirmation cut
// off after GitHub created the review still shows the text to paste.
func TestClaimSavesTheBody(t *testing.T) {
	th := draftedThread(time.Now().Add(-time.Hour))
	th[3].Draft.Approve, th[3].Draft.Diff = true, draftDiff
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}, result: comment.ReviewResult{Created: true}}
	h, _, store := draftFixture(t, c, th)
	var during *worklist.DraftReceipt
	c.during = func() {
		it, _, _ := store.Get(context.Background(), "github:42", draftItemID)
		during = it.Thread[3].DraftReceipt
	}
	postCreate(h, url.Values{"id": {draftItemID}, "msg": {"a"}, "digest": {th[3].Draft.Digest()}, "approve": {"1"}}, "Sec-Fetch-Site", "same-origin")
	if during == nil || during.State != worklist.DraftCreating || during.Body != worklist.ApprovalCommands {
		t.Fatalf("claim while GitHub was asked = %+v", during)
	}
	// Interrupted there: the thread shows the body with the warning.
	th[3].DraftReceipt = during
	h, _, _ = draftFixture(t, c, th)
	if page, _ := threadPage(t, h); !strings.Contains(page, "<pre class=\"zz-draft-text\">/lgtm\n/approve</pre>") || !strings.Contains(page, "Finish your review") {
		t.Errorf("an interrupted claim shows no body:\n%s", page)
	}
}

// Drafting works from any answered discussion, not only a review request: a
// Discuss question answered with an assessment is enough, so the button is enabled.
// With no review yet the thread still offers one, saying it is needed only to draft
// an approval; it appends a review request (keeping the conversation) and
// dispatches it to the default model. While a reply is in flight the reason is to
// wait, and no review is offered.
func TestDraftFromDiscussionWithoutReview(t *testing.T) {
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
	asked := []worklist.Message{{ID: "q", Role: worklist.RoleUser, Content: "Any review comments, or is this ready to merge?", At: time.Now().Add(-time.Hour)}, {ID: "r", ReplyTo: "q", Role: worklist.RoleAgent, Content: "I'd hold /lgtm for one correction.", At: time.Now().Add(-time.Hour)}}
	h, p, store := draftFixture(t, c, asked)
	page, _ := threadPage(t, h)
	i := strings.Index(page, `action="/items/draft-review"`)
	if i < 0 || strings.Contains(page[i:strings.Index(page[i:], "</form>")+i], "disabled") {
		t.Fatalf("drafting from a discussion is not enabled:\n%s", page)
	}
	for _, want := range []string{"A review is needed only to draft an approval", `name="review" value="first"`, ">Review this pull request</button>"} {
		if !strings.Contains(page, want) {
			t.Errorf("thread lacks %q", want)
		}
	}
	// The draft request is accepted and dispatched.
	req := httptest.NewRequest(http.MethodPost, "/items/draft-review", strings.NewReader(url.Values{"id": {draftItemID}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.DraftReview(httptest.NewRecorder(), req)
	it, _, _ := store.Get(context.Background(), "github:42", draftItemID)
	if last := it.Thread[len(it.Thread)-1]; last.Kind != worklist.KindDraftReviewRequest || len(p.turns) != 1 {
		t.Fatalf("draft from a discussion: appended %+v, dispatched %+v", last, p.turns)
	}
	// The optional review is still available, and once in flight the reason is to wait.
	h, p, store = draftFixture(t, c, asked)
	postReview(h, "first")
	it, _, _ = store.Get(context.Background(), "github:42", draftItemID)
	last := it.Thread[len(it.Thread)-1]
	if len(it.Thread) != 3 || last.Kind != worklist.KindReviewRequest || last.Content != reviewPrompt || len(p.turns) != 1 || p.converseModels[0] != "m0" {
		t.Fatalf("appended %+v, dispatched %+v", last, p.turns)
	}
	page, _ = threadPage(t, h)
	if !strings.Contains(page, `<p class="zz-meta zz-why">Wait for the reply in progress`) || strings.Contains(page, ">Review this pull request</button>") {
		t.Errorf("pending review: reason or button wrong:\n%s", page)
	}
	// A reviewed pull request offers no review from here, and the button is enabled.
	h, p, _ = draftFixture(t, c, draftedThread(time.Now().Add(-time.Hour))[:2])
	page, _ = threadPage(t, h)
	if strings.Contains(page, "zz-why") || strings.Contains(page, ">Review this pull request</button>") {
		t.Errorf("a reviewed pull request shows a reason or a review button")
	}
	for _, action := range []string{"first", "again", ""} {
		postReview(h, action)
	}
	if len(p.turns) != 0 {
		t.Error("a reviewed pull request with a current review was reviewed again")
	}
}

// postReview posts a ReviewAgain form naming action ("" for a form rendered before
// forms named one).
func postReview(h *Handler, action string, turn ...string) *httptest.ResponseRecorder {
	form := url.Values{"id": {draftItemID}}
	if action != "" {
		form.Set("review", action)
	}
	if len(turn) > 0 {
		form.Set("turn", turn[0])
	}
	req := httptest.NewRequest(http.MethodPost, "/items/review-again", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ReviewAgain(rec, req)
	return rec
}

// pageAt renders the thread at the URL a POST redirected to.
func pageAt(t *testing.T, h *Handler, location string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Thread(rec, httptest.NewRequest(http.MethodGet, location, nil))
	return rec.Body.String()
}

// While a review request has no answer, no second one is added: the thread offers
// "Retry the earlier review", naming that turn, which retries it (same key, a new
// attempt), so a run still in flight is shared and the store keeps one answer. That
// holds however the first ended: stalled, followed by a Discuss turn, or ended by a
// reconnect notice (retried at once). "Review this pull request" is not offered then,
// and its form adds nothing.
func TestEarlierReviewIsRetriedNotDoubled(t *testing.T) {
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
	review := func(age time.Duration) worklist.Message {
		return worklist.Message{ID: "q", Role: worklist.RoleUser, Content: reviewPrompt, Kind: worklist.KindReviewRequest, At: time.Now().Add(-age)}
	}
	for name, th := range map[string][]worklist.Message{
		"stalled": {review(6 * time.Minute)},
		"stalled, then a Discuss turn answered": {review(6 * time.Minute),
			{ID: "d", Role: worklist.RoleUser, Content: "any thoughts?", At: time.Now().Add(-5*time.Minute - time.Second)},
			{ID: "e", ReplyTo: "d", Role: worklist.RoleAgent, Content: "Some.", At: time.Now().Add(-5 * time.Minute)}},
		"ended by a reconnect notice": {review(time.Minute),
			{ID: "n", ReplyTo: "q", Role: worklist.RoleAgent, Content: worklist.ReconnectNotice, Notice: worklist.NoticeReconnect, At: time.Now()}},
	} {
		h, p, store := draftFixture(t, c, th)
		page, _ := threadPage(t, h)
		if !strings.Contains(page, ">Retry the earlier review</button>") || !strings.Contains(page, `name="turn" value="q"`) || strings.Contains(page, ">Review this pull request</button>") {
			t.Errorf("%s: the earlier review is not offered for retry alone", name)
		}
		postReview(h, "first") // a stale page's first-review form adds nothing beside it
		if len(p.turns) != 0 {
			t.Errorf("%s: a second review request was dispatched", name)
		}
		postReview(h, "retry", "q")
		it, _, _ := store.Get(context.Background(), "github:42", draftItemID)
		q := it.Thread[0]
		if len(it.Thread) != len(th) || len(p.turns) != 1 || p.turns[0].Turn != "q" || p.turns[0].Attempt == "" || q.Attempt != p.turns[0].Attempt {
			t.Errorf("%s: thread %d turns (want %d), dispatched %+v, turn %+v", name, len(it.Thread), len(th), p.turns, q)
		}
	}
	// A run still in flight shares the retry: the turn keeps that run's attempt.
	h, p, store := draftFixture(t, c, []worklist.Message{review(6 * time.Minute)})
	p.shared = true
	postReview(h, "retry", "q")
	if it, _, _ := store.Get(context.Background(), "github:42", draftItemID); len(it.Thread) != 1 || it.Thread[0].Attempt != "" {
		t.Errorf("a shared run's turn: %+v", it.Thread)
	}
	// With no review request at all, "Review this pull request" adds one.
	h, p, store = draftFixture(t, c, []worklist.Message{{ID: "d", Role: worklist.RoleUser, Content: "hi", At: time.Now().Add(-time.Hour)}, {ID: "e", ReplyTo: "d", Role: worklist.RoleAgent, Content: "hello", At: time.Now().Add(-time.Hour)}})
	postReview(h, "first")
	if it, _, _ := store.Get(context.Background(), "github:42", draftItemID); len(it.Thread) != 3 || it.Thread[2].Kind != worklist.KindReviewRequest || len(p.turns) != 1 || p.turns[0].Turn != it.Thread[2].ID {
		t.Errorf("no new review request: %+v", it.Thread)
	}
}

// A hidden unanswered review request stays hidden: the thread says to unhide it to
// retry it, offers neither button, and no form retries or doubles it.
func TestHiddenEarlierReviewWaitsForUnhide(t *testing.T) {
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
	q := worklist.Message{ID: "q", Role: worklist.RoleUser, Content: reviewPrompt, Kind: worklist.KindReviewRequest, Hidden: true, At: time.Now().Add(-6 * time.Minute)}
	h, p, store := draftFixture(t, c, []worklist.Message{q})
	page, _ := threadPage(t, h)
	if !strings.Contains(page, "Unhide it to retry it") || strings.Contains(page, ">Retry the earlier review</button>") || strings.Contains(page, ">Review this pull request</button>") {
		t.Errorf("hidden request: wrong offer:\n%s", page)
	}
	postReview(h, "retry", "q")
	postReview(h, "first")
	if it, _, _ := store.Get(context.Background(), "github:42", draftItemID); len(p.turns) != 0 || len(it.Thread) != 1 || !it.Thread[0].Hidden {
		t.Errorf("a hidden request was retried, doubled or unhidden: %+v, dispatched %+v", it.Thread, p.turns)
	}
}

// A 2nd opinion is retried only blind, by a model it may use; with none, it is not
// retried at all, never sent as an ordinary review that would still count as
// independent.
func TestEarlierSecondOpinionStaysBlind(t *testing.T) {
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
	q := worklist.Message{ID: "q", Role: worklist.RoleUser, Content: worklist.IndependentReviewPrompt, Kind: worklist.KindReviewRequest, At: time.Now().Add(-6 * time.Minute)}
	h, p, _ := draftFixture(t, c, []worklist.Message{q})
	postReview(h, "retry", "q")
	if len(p.secondOpinions) != 1 || len(p.converses) != 0 {
		t.Errorf("a 2nd opinion was not retried blind: second opinions %v, converses %v", p.secondOpinions, p.converses)
	}
	h, p, _ = draftFixture(t, c, []worklist.Message{q})
	h.options = nil // no model a 2nd opinion may use
	page, _ := threadPage(t, h)
	if !strings.Contains(page, "no model it may use is configured") || strings.Contains(page, ">Retry the earlier review</button>") {
		t.Errorf("an unrunnable 2nd opinion is offered for retry:\n%s", page)
	}
	postReview(h, "retry", "q")
	if len(p.turns) != 0 {
		t.Errorf("an unrunnable 2nd opinion was dispatched: %+v", p.turns)
	}
}

// A dispatch the backend certainly refused is said so on the thread, whichever turn
// is the latest, and leaves nothing looking pending: a new review request is offered
// for retry at once, and a retried one gets its time back. An uncertain failure
// keeps the turn pending.
func TestRefusedReviewDispatchIsShown(t *testing.T) {
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
	answered := []worklist.Message{{ID: "d", Role: worklist.RoleUser, Content: "hi", At: time.Now().Add(-time.Hour)}, {ID: "e", ReplyTo: "d", Role: worklist.RoleAgent, Content: "hello", At: time.Now().Add(-time.Hour)}}
	h, p, store := draftFixture(t, c, answered)
	p.err = ingest.ErrNotAccepted
	rec := postReview(h, "first")
	page := pageAt(t, h, rec.Header().Get("Location"))
	if !strings.Contains(page, "the agent backend refused it") || !strings.Contains(page, ">Retry the earlier review</button>") {
		t.Errorf("a refused new review: no message, or not retryable at once:\n%s", page)
	}
	it, _, _ := store.Get(context.Background(), "github:42", draftItemID)
	if it.ReplyPending(time.Now(), reviewStaleAfter) {
		t.Error("a refused new review still looks pending")
	}
	// Retrying an older review after a later answered Discuss turn: the latest turn is
	// not stalled, but the refusal shows all the same.
	older := append([]worklist.Message{{ID: "q", Role: worklist.RoleUser, Content: reviewPrompt, Kind: worklist.KindReviewRequest, At: time.Now().Add(-2 * time.Hour)}}, answered...)
	h, p, _ = draftFixture(t, c, older)
	p.err = ingest.ErrNotAccepted
	rec = postReview(h, "retry", "q")
	if page := pageAt(t, h, rec.Header().Get("Location")); !strings.Contains(page, "the agent backend refused it") || !strings.Contains(page, ">Retry the earlier review</button>") {
		t.Errorf("a refused retry of an older review shows no message:\n%s", page)
	}
	// An uncertain failure: a run may exist, so the turn stays pending and no refusal shows.
	h, p, store = draftFixture(t, c, answered)
	p.err = errors.New("lost answer")
	rec = postReview(h, "first")
	it, _, _ = store.Get(context.Background(), "github:42", draftItemID)
	if !it.ReplyPending(time.Now(), reviewStaleAfter) || strings.Contains(rec.Header().Get("Location"), "review_refused") {
		t.Error("an uncertain dispatch was treated as refused")
	}
}

// Each form does only its own action, checked inside the store update: an old
// "Review the current revision" form (named, or from before forms named one) does
// nothing once the conversation was reset, and "Review this pull request" does
// nothing once the pull request is reviewed.
func TestReviewFormsDoOnlyTheirOwnAction(t *testing.T) {
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
	h, p, _ := draftFixture(t, c, nil) // a reset conversation
	postReview(h, "again")
	postReview(h, "")
	postReview(h, "bogus")
	if len(p.turns) != 0 {
		t.Errorf("a re-review form started a first review in a reset thread: %+v", p.turns)
	}
	h, p, _ = draftFixture(t, c, draftedThread(time.Now().Add(-time.Hour))[:2])
	postReview(h, "first")
	if len(p.turns) != 0 {
		t.Error("a first-review form reviewed a reviewed pull request")
	}
}

// The stylesheet marks a disabled button as such.
func TestDisabledButtonsLookDisabled(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Handler{}).Static().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/app.css", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), ".btn:disabled") {
		t.Fatalf("app.css (%d) has no disabled button style", rec.Code)
	}
}

// An unusable 2nd opinion never blocks an ordinary review: past a newer one no model
// may run, an older unanswered ordinary request is offered for retry; and once a
// notice ended the 2nd opinion's run (it can no longer answer), a new ordinary review
// may be asked for, as an ordinary review, not a blind one. While it may still
// answer, it blocks, and the thread says why.
func TestUnusableSecondOpinionDoesNotBlock(t *testing.T) {
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
	ago := func(d time.Duration) time.Time { return time.Now().Add(-d) }
	ordinary := worklist.Message{ID: "q1", Role: worklist.RoleUser, Content: reviewPrompt, Kind: worklist.KindReviewRequest, At: ago(2 * time.Hour)}
	panel := worklist.Message{ID: "q2", Role: worklist.RoleUser, Content: worklist.IndependentReviewPrompt, Kind: worklist.KindReviewRequest, At: ago(time.Hour)}
	ended := worklist.Message{ID: "n", ReplyTo: "q2", Role: worklist.RoleAgent, Content: worklist.ReconnectNotice, Notice: worklist.NoticeReconnect, At: ago(time.Hour)}

	h, p, _ := draftFixture(t, c, []worklist.Message{ordinary, panel, ended})
	h.options = nil // only the run's default model: no 2nd opinion can run
	if page, _ := threadPage(t, h); !strings.Contains(page, `name="turn" value="q1"`) {
		t.Errorf("the older ordinary request is not offered:\n%s", page)
	}
	postReview(h, "retry", "q1")
	if len(p.converses) != 1 || len(p.secondOpinions) != 0 || p.turns[0].Turn != "q1" {
		t.Errorf("retry of the ordinary request: converses %v, second opinions %v, turns %+v", p.converses, p.secondOpinions, p.turns)
	}

	h, p, store := draftFixture(t, c, []worklist.Message{panel, ended})
	h.options = nil
	if page, _ := threadPage(t, h); !strings.Contains(page, ">Review this pull request</button>") {
		t.Errorf("an ended, unusable 2nd opinion blocks a first review:\n%s", page)
	}
	postReview(h, "first")
	it, _, _ := store.Get(context.Background(), "github:42", draftItemID)
	last := it.Thread[len(it.Thread)-1]
	if len(it.Thread) != 3 || last.Content != reviewPrompt || last.IsPanelReview() || len(p.converses) != 1 || len(p.secondOpinions) != 0 {
		t.Errorf("first review beside an ended 2nd opinion: %+v, converses %v, second opinions %v", it.Thread, p.converses, p.secondOpinions)
	}

	h, p, _ = draftFixture(t, c, []worklist.Message{panel})
	h.options = nil
	if page, _ := threadPage(t, h); !strings.Contains(page, "no model it may use is configured") || strings.Contains(page, ">Review this pull request</button>") {
		t.Errorf("a 2nd opinion that may still answer does not block:\n%s", page)
	}
	postReview(h, "first")
	if len(p.turns) != 0 {
		t.Error("a review was added beside a 2nd opinion that may still answer")
	}
}

// After the backend refuses "Review the current revision", the button retries that
// request (naming it) instead of asking again, so an outage never piles up requests
// for the same revision; the old form adds nothing.
func TestRefusedReReviewIsRetried(t *testing.T) {
	const pushed = "fedcba9876543210fedcba9876543210fedcba98"
	th := draftedThread(time.Now().Add(-time.Hour))
	th[1].Head, th[1].Base = draftCommit, "main"
	th[3].Head, th[3].Base = pushed, "main"
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
	h, p, store := draftFixture(t, c, th)
	p.err = ingest.ErrNotAccepted
	rec := postReview(h, "again")
	it, _, _ := store.Get(context.Background(), "github:42", draftItemID)
	asked := it.Thread[len(it.Thread)-1]
	if len(it.Thread) != 5 || asked.Kind != worklist.KindReviewRequest {
		t.Fatalf("thread after the refused re-review: %+v", it.Thread)
	}
	page := pageAt(t, h, rec.Header().Get("Location"))
	if !strings.Contains(page, ">Retry the earlier review</button>") || !strings.Contains(page, `name="turn" value="`+asked.ID+`"`) || !strings.Contains(page, "That review attempt did not start") {
		t.Errorf("the refused re-review is not offered for retry:\n%s", page)
	}
	postReview(h, "again") // the old form, clicked again during the outage
	if it, _, _ := store.Get(context.Background(), "github:42", draftItemID); len(it.Thread) != 5 {
		t.Errorf("a second re-review request was added: %d turns", len(it.Thread))
	}
	p.err = nil
	postReview(h, "retry", asked.ID)
	it, _, _ = store.Get(context.Background(), "github:42", draftItemID)
	if len(it.Thread) != 5 || p.turns[len(p.turns)-1].Turn != asked.ID || it.Thread[4].Attempt == "" {
		t.Errorf("the refused re-review was not retried as itself: %+v", p.turns)
	}
	// The refusal URL now describes an attempt that was superseded: with the retry
	// running, it shows no refusal.
	if page := pageAt(t, h, rec.Header().Get("Location")); strings.Contains(page, "That review attempt did not start") {
		t.Error("a superseded refusal is still shown while the retry runs")
	}
}

// An outstanding re-review stays the one to retry even after a later draft read the
// same revision: requests record no revision, so any unanswered one, wherever it sits,
// is retried rather than a second asked for while the first can still finish.
func TestOutstandingReReviewSurvivesARedraft(t *testing.T) {
	const pushed = "fedcba9876543210fedcba9876543210fedcba98"
	ago := func(d time.Duration) time.Time { return time.Now().Add(-d) }
	th := draftedThread(ago(time.Hour))
	th[1].Head, th[1].Base = draftCommit, "main"
	th[3].Head, th[3].Base = pushed, "main"
	th = append(th,
		worklist.Message{ID: "rr", Role: worklist.RoleUser, Content: reviewAgainPrompt, Kind: worklist.KindReviewRequest, At: ago(12 * time.Minute)},
		worklist.Message{ID: "d2", Role: worklist.RoleUser, Content: draftReviewPrompt, Kind: worklist.KindDraftReviewRequest, At: ago(6 * time.Minute)},
		worklist.Message{ID: "a2", ReplyTo: "d2", Role: worklist.RoleAgent, Content: "The latest review is out of date.", Head: pushed, Base: "main", At: ago(6 * time.Minute)},
	)
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
	h, p, store := draftFixture(t, c, th)
	page, _ := threadPage(t, h)
	if !strings.Contains(page, `name="turn" value="rr"`) || !strings.Contains(page, ">Retry the earlier review</button>") || strings.Contains(page, `name="review" value="again"`) {
		t.Errorf("the outstanding re-review is not the one offered:\n%s", page)
	}
	postReview(h, "again")
	if it, _, _ := store.Get(context.Background(), "github:42", draftItemID); len(it.Thread) != len(th) || len(p.turns) != 0 {
		t.Fatalf("a second re-review was asked for: %d turns, dispatched %+v", len(it.Thread), p.turns)
	}
	postReview(h, "retry", "rr")
	if len(p.turns) != 1 || p.turns[0].Turn != "rr" {
		t.Errorf("the outstanding re-review was not retried: %+v", p.turns)
	}
}

// A dispatch the backend certainly refused is recorded on the turn for that attempt,
// so a 2nd opinion whose only attempts never ran blocks nothing, even with no model
// left to retry it; a later attempt is not covered by an earlier refusal.
func TestRefusedSecondOpinionDoesNotBlock(t *testing.T) {
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
	// The 2nd opinion submission itself records the refusal.
	h, p, store := draftFixture(t, c, draftedThread(time.Now().Add(-time.Hour))[:2])
	p.err = ingest.ErrNotAccepted
	req := httptest.NewRequest(http.MethodPost, "/items/second-opinion", strings.NewReader(url.Values{"id": {draftItemID}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.SecondOpinion(httptest.NewRecorder(), req)
	it, _, _ := store.Get(context.Background(), "github:42", draftItemID)
	last := it.Thread[len(it.Thread)-1]
	if !last.IsPanelReview() || !last.Refused || last.RefusedAttempt != "" || !it.Ended(last.ID) {
		t.Fatalf("the refused 2nd opinion was not recorded: %+v", last)
	}
	// A day later, with no model left to retry it, it blocks nothing.
	panel := worklist.Message{ID: "q", Role: worklist.RoleUser, Content: worklist.IndependentReviewPrompt, Kind: worklist.KindReviewRequest, Refused: true, At: time.Now().Add(-24 * time.Hour)}
	h, p, _ = draftFixture(t, c, []worklist.Message{panel})
	h.options = nil
	if page, _ := threadPage(t, h); !strings.Contains(page, ">Review this pull request</button>") {
		t.Errorf("a refused 2nd opinion still blocks a first review:\n%s", page)
	}
	postReview(h, "first")
	if len(p.converses) != 1 || len(p.secondOpinions) != 0 {
		t.Errorf("first review beside a refused 2nd opinion: converses %v, second opinions %v", p.converses, p.secondOpinions)
	}
	// A refusal covers only its attempt: a retry's new attempt may still answer.
	retried := panel
	retried.Attempt = "a2"
	h, p, _ = draftFixture(t, c, []worklist.Message{retried})
	h.options = nil
	if page, _ := threadPage(t, h); strings.Contains(page, ">Review this pull request</button>") {
		t.Error("a refusal of an earlier attempt unblocked a later one")
	}
}

// When a 2nd opinion's submission and then its retry are both refused, rolling the
// retry back restores the original attempt with its refusal record: the turn still
// cannot answer, so once no model is left to retry it, it blocks nothing.
func TestRefusedRetryKeepsTheOriginalRefusal(t *testing.T) {
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
	panel := worklist.Message{ID: "q", Role: worklist.RoleUser, Content: worklist.IndependentReviewPrompt, Kind: worklist.KindReviewRequest, Refused: true, At: time.Now().Add(-time.Hour)}
	h, p, store := draftFixture(t, c, []worklist.Message{panel})
	p.err = ingest.ErrNotAccepted
	postReview(h, "retry", "q")
	if len(p.secondOpinions) != 1 {
		t.Fatalf("the retry was not dispatched blind: %v", p.secondOpinions)
	}
	it, _, _ := store.Get(context.Background(), "github:42", draftItemID)
	if q := it.Thread[0]; q.Attempt != "" || !q.Refused || q.RefusedAttempt != "" || !it.Ended("q") {
		t.Fatalf("after the refused retry: %+v (ended %v)", q, it.Ended("q"))
	}
	h, _, _ = draftFixture(t, c, it.Thread)
	h.options = nil
	if page, _ := threadPage(t, h); !strings.Contains(page, ">Review this pull request</button>") {
		t.Errorf("the twice-refused 2nd opinion blocks a first review:\n%s", page)
	}
}

// A review request the latest review superseded (asked before it landed) does not
// block a review of the current revision, even hidden and unanswered: only requests
// asked since the latest review count.
func TestSupersededHiddenRequestDoesNotBlockReReview(t *testing.T) {
	const pushed = "fedcba9876543210fedcba9876543210fedcba98"
	old := worklist.Message{ID: "h0", Role: worklist.RoleUser, Content: reviewPrompt, Kind: worklist.KindReviewRequest, Hidden: true, At: time.Now().Add(-3 * time.Hour)}
	th := append([]worklist.Message{old}, draftedThread(time.Now().Add(-time.Hour))...)
	th[2].Head, th[2].Base = draftCommit, "main" // the review's answer
	th[4].Head, th[4].Base = pushed, "main"      // the draft's reply: the PR moved
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
	h, p, store := draftFixture(t, c, th)
	page, _ := threadPage(t, h)
	if !strings.Contains(page, `name="review" value="again"`) || strings.Contains(page, "Unhide it to retry it") {
		t.Errorf("a superseded hidden request blocks the re-review:\n%s", page)
	}
	postReview(h, "again")
	if it, _, _ := store.Get(context.Background(), "github:42", draftItemID); len(it.Thread) != len(th)+1 || len(p.turns) != 1 || !it.Thread[0].Hidden {
		t.Errorf("re-review: %d turns, dispatched %+v", len(it.Thread), p.turns)
	}
}

// A 2nd opinion the backend certainly refused is not shown as working: with no run
// started it is not pending, and it is retryable at once.
func TestRefusedSecondOpinionIsNotPending(t *testing.T) {
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
	h, p, store := draftFixture(t, c, draftedThread(time.Now().Add(-time.Hour))[:2])
	p.err = ingest.ErrNotAccepted
	req := httptest.NewRequest(http.MethodPost, "/items/second-opinion", strings.NewReader(url.Values{"id": {draftItemID}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.SecondOpinion(httptest.NewRecorder(), req)
	it, _, _ := store.Get(context.Background(), "github:42", draftItemID)
	now := time.Now()
	if it.ReplyPending(now, reviewStaleAfter) {
		t.Error("a refused 2nd opinion is pending")
	}
	if i, ok := it.RetryableTurn(now, reviewStaleAfter); !ok || it.TurnKey(i) != it.Thread[len(it.Thread)-1].ID {
		t.Error("a refused 2nd opinion is not retryable at once")
	}
	if page, _ := threadPage(t, h); strings.Contains(page, "zz-pending") {
		t.Error("a refused 2nd opinion shows the working cue")
	}
}

// Review all PRs never retries a 2nd opinion through its ordinary path (the model
// would read the conversation while the answer still counts as independent), nor
// adds a review beside it: it leaves the pull request to the thread's blind Retry.
func TestReviewAllLeavesSecondOpinionsToTheThread(t *testing.T) {
	c := &fakeReviewCommenter{fakeCommenter: fakeCommenter{enabled: true, connected: true}}
	panel := worklist.Message{ID: "q", Role: worklist.RoleUser, Content: worklist.IndependentReviewPrompt, Kind: worklist.KindReviewRequest, Refused: true, At: time.Now()}
	h, p, store := draftFixture(t, c, []worklist.Message{panel})
	h.ReviewAllPRs(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/items/review-all", nil))
	it, _, _ := store.Get(context.Background(), "github:42", draftItemID)
	if len(p.reviewed) != 0 || len(it.Thread) != 1 || it.Thread[0].Attempt != "" {
		t.Errorf("Review all touched the 2nd opinion: reviewed %v, thread %+v", p.reviewed, it.Thread)
	}
	// An ordinary stalled review is still retried by the batch.
	ordinary := panel
	ordinary.Content = reviewPrompt
	h, p, _ = draftFixture(t, c, []worklist.Message{ordinary})
	h.ReviewAllPRs(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/items/review-all", nil))
	if len(p.reviewed) != 1 {
		t.Errorf("Review all skipped an ordinary stalled review: %v", p.reviewed)
	}
}

// The radar counts only the pull requests Review all PRs will review: a 2nd opinion
// awaiting a retry is left to the thread, so it is not counted, and with nothing else
// the button is not shown. The count and the action use one check.
func TestReviewAllCountsOnlyWhatItReviews(t *testing.T) {
	refused := worklist.WorkItem{ID: "github:o/r#1", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1, Title: "panel", State: "open"},
		Thread: []worklist.Message{{ID: "q", Role: worklist.RoleUser, Content: worklist.IndependentReviewPrompt, Kind: worklist.KindReviewRequest, Refused: true, At: time.Now()}}}
	ordinary := func(n int) worklist.WorkItem {
		return worklist.WorkItem{ID: "github:o/r#" + strconv.Itoa(n), Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: "o/r", Number: n, Title: "pr", State: "open"}}
	}
	radar := func(items ...worklist.WorkItem) (string, *recordPipeline, *Handler) {
		store := worklist.NewMemoryStore()
		store.Seed("u1", items...)
		pipe := &recordPipeline{}
		h := New(fakeSessions{&session.User{ID: "u1"}}, store, pipe, reconnectProviders{}, true, nil)
		rec := httptest.NewRecorder()
		h.Index(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		return rec.Body.String(), pipe, h
	}
	if out, _, _ := radar(refused); strings.Contains(out, "Review all PRs") {
		t.Error("Review all PRs is offered with only a 2nd opinion awaiting a retry")
	}
	out, pipe, h := radar(refused, ordinary(2), ordinary(3))
	if !strings.Contains(out, "Review all PRs (2)") || strings.Contains(out, "Review all PRs (3)") {
		t.Errorf("the radar's count includes the 2nd opinion:\n%s", out)
	}
	h.ReviewAllPRs(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/items/review-all", nil))
	if len(pipe.reviewed) != 2 {
		t.Errorf("Review all dispatched %v, want the 2 it counted", pipe.reviewed)
	}
}
