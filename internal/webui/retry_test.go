package webui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/ingest"
	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/storage"
	"github.com/jackfrancis/gofer/internal/worklist"
)

var retryNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

var retryOptions = []ModelOption{
	{Value: "c0|default", Label: "default", Endpoint: "https://default", Model: "default"},
	{Value: "c1|alt", Label: "alt", Endpoint: "https://alt", Model: "alt"},
}

// retryHandler is a thread-view handler whose clock the test moves.
func retryHandler(store worklist.Store, pipe *recordPipeline, now *time.Time) *Handler {
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, pipe, nil, true, retryOptions)
	h.now = func() time.Time { return *now }
	return h
}

func postRetry(h *Handler, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/items/thread/retry", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ThreadRetry(rec, req)
	return rec
}

func storedItem(t *testing.T, store worklist.Store, id string) worklist.WorkItem {
	t.Helper()
	items, err := store.List(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("item %s not stored", id)
	return worklist.WorkItem{}
}

func agentReplies(it worklist.WorkItem) []worklist.Message {
	var out []worklist.Message
	for _, m := range it.Thread {
		if m.Role == worklist.RoleAgent {
			out = append(out, m)
		}
	}
	return out
}

const retryForm = `action="/items/thread/retry"`

// Item 9's Verify clause: reopening an interrupted conversation can finish the
// original turn exactly once. The thread view offers Retry for the stalled turn; Retry
// re-dispatches that turn under its own ID and adds no turn; a second Retry dispatches
// nothing; and when the first run answers late as well as the retry, the turn keeps
// one answer and the page offers nothing more.
func TestThreadRetryFinishesTheOriginalTurnOnce(t *testing.T) {
	now := retryNow
	store := worklist.NewMemoryStore()
	item := worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, Thread: []worklist.Message{
		{ID: "t1", Role: worklist.RoleUser, Content: "Is this ready?", At: now.Add(-10 * time.Minute)},
	}}
	store.Seed("u1", item)
	pipe := &recordPipeline{}
	h := retryHandler(store, pipe, &now)

	// Reopening the conversation offers Retry for the original turn.
	page := openThread(t, h, "pr1")
	if !strings.Contains(page, retryForm) || !strings.Contains(page, `name="turn" value="t1"`) {
		t.Fatalf("the stalled thread must offer Retry for turn t1, got:\n%s", page)
	}
	if strings.Contains(page, "Resend") {
		t.Fatal("the stalled thread must not tell the user to resend")
	}

	// Retry dispatches the original turn, with the chosen model, and adds no turn.
	rec := postRetry(h, url.Values{"id": {"pr1"}, "turn": {"t1"}, "choice": {"c1|alt"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/items/thread?id=pr1" {
		t.Fatalf("retry must redirect back to the thread, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if len(pipe.turns) != 1 || pipe.turns[0] != (worklist.TurnRef{Item: "pr1", Turn: "t1"}) {
		t.Fatalf("retry must dispatch the original turn once, got %+v", pipe.turns)
	}
	if pipe.converseModels[0] != "alt" || pipe.converseEndpoints[0] != "https://alt" {
		t.Fatalf("retry must use the chosen model, got %v %v", pipe.converseModels, pipe.converseEndpoints)
	}
	got := storedItem(t, store, "pr1")
	if len(got.Thread) != 1 || got.Thread[0].ID != "t1" || !got.Thread[0].At.Equal(now) {
		t.Fatalf("retry must re-stamp the turn in place and add none, got %+v", got.Thread)
	}

	// The turn is pending again: the page polls instead of offering Retry, and a second
	// Retry (a double submit, or another tab) dispatches nothing.
	if page := openThread(t, h, "pr1"); strings.Contains(page, retryForm) {
		t.Fatal("a turn being retried must not offer Retry again")
	}
	postRetry(h, url.Values{"id": {"pr1"}, "turn": {"t1"}})
	if len(pipe.turns) != 1 {
		t.Fatalf("a second Retry must dispatch nothing, got %+v", pipe.turns)
	}

	// The first run was slow, not lost: it answers, then the retry run answers too.
	ctx := context.Background()
	late := worklist.Message{ID: "a-late", Content: "Late answer.", At: now.Add(time.Minute)}
	retried := worklist.Message{ID: "a-retry", Content: "Retried answer.", At: now.Add(2 * time.Minute)}
	if err := worklist.ApplyReply(ctx, store, "u1", "pr1", "t1", late); err != nil {
		t.Fatal(err)
	}
	if err := worklist.ApplyReply(ctx, store, "u1", "pr1", "t1", retried); err != nil {
		t.Fatal(err)
	}
	got = storedItem(t, store, "pr1")
	if replies := agentReplies(got); len(replies) != 1 || replies[0].ID != "a-late" || replies[0].ReplyTo != "t1" {
		t.Fatalf("the original turn must keep exactly one answer, got %+v", got.Thread)
	}

	// Long after, the answered turn offers no Retry and accepts none.
	now = now.Add(time.Hour)
	if page := openThread(t, h, "pr1"); strings.Contains(page, retryForm) {
		t.Fatal("an answered turn must not offer Retry")
	}
	postRetry(h, url.Values{"id": {"pr1"}, "turn": {"t1"}})
	if len(pipe.turns) != 1 {
		t.Fatalf("Retry of an answered turn must dispatch nothing, got %+v", pipe.turns)
	}
}

// A turn stored before turns had IDs is retried under its position key, which the
// run's reply then names.
func TestThreadRetryLegacyTurn(t *testing.T) {
	now := retryNow
	store := worklist.NewMemoryStore()
	store.Seed("u1", worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, Thread: []worklist.Message{
		{Role: worklist.RoleUser, Content: "hi", At: now.Add(-10 * time.Minute)},
	}})
	pipe := &recordPipeline{}
	h := retryHandler(store, pipe, &now)

	key := storedItem(t, store, "pr1").TurnKey(0)
	if page := openThread(t, h, "pr1"); !strings.Contains(page, `name="turn" value="`+key+`"`) {
		t.Fatalf("Retry must name the legacy turn's key %q, got:\n%s", key, page)
	}
	postRetry(h, url.Values{"id": {"pr1"}, "turn": {key}})
	if len(pipe.turns) != 1 || pipe.turns[0].Turn != key {
		t.Fatalf("want one dispatch for %q, got %+v", key, pipe.turns)
	}
	if got := storedItem(t, store, "pr1"); len(got.Thread) != 1 {
		t.Fatalf("retry must add no turn, got %+v", got.Thread)
	}
}

// Retry acts only on the turn the page offered, and only while it is stalled: a page
// made before the user wrote again, an unknown item, a missing turn, a turn still in
// flight, and a turn a reconnect notice answered dispatch nothing and change nothing.
func TestThreadRetryRefusals(t *testing.T) {
	now := retryNow
	old := now.Add(-10 * time.Minute)
	store := worklist.NewMemoryStore()
	store.Seed("u1",
		worklist.WorkItem{ID: "newer", Type: worklist.TypePullRequest, Thread: []worklist.Message{
			{ID: "t1", Role: worklist.RoleUser, Content: "first", At: old},
			{ID: "t2", Role: worklist.RoleUser, Content: "second", At: old},
		}},
		worklist.WorkItem{ID: "fresh", Type: worklist.TypePullRequest, Thread: []worklist.Message{
			{ID: "t1", Role: worklist.RoleUser, Content: "just now", At: now.Add(-time.Minute)},
		}},
		worklist.WorkItem{ID: "noticed", Type: worklist.TypePullRequest, Thread: []worklist.Message{
			{ID: "t1", Role: worklist.RoleUser, Content: "hi", At: old},
			{ID: "n1", ReplyTo: "t1", Role: worklist.RoleAgent, Notice: worklist.NoticeReconnect, Content: "Sign in again.", At: old.Add(time.Minute)},
		}},
	)
	before, _ := store.List(context.Background(), "u1")
	pipe := &recordPipeline{}
	h := retryHandler(store, pipe, &now)

	for _, form := range []url.Values{
		{"id": {"newer"}, "turn": {"t1"}}, // the user has written since the page was made
		{"id": {"missing"}, "turn": {"t1"}},
		{"id": {"newer"}},
		{"id": {"fresh"}, "turn": {"t1"}},
		{"id": {"noticed"}, "turn": {"t1"}},
	} {
		if rec := postRetry(h, form); rec.Code != http.StatusSeeOther {
			t.Fatalf("%v: status %d, want a redirect", form, rec.Code)
		}
	}
	if len(pipe.turns) != 0 {
		t.Fatalf("no refused Retry may dispatch, got %+v", pipe.turns)
	}
	after, _ := store.List(context.Background(), "u1")
	for i := range before {
		for j := range before[i].Thread {
			if !before[i].Thread[j].At.Equal(after[i].Thread[j].At) {
				t.Fatalf("a refused Retry must not re-stamp %s turn %d", before[i].ID, j)
			}
		}
	}
	if page := openThread(t, h, "noticed"); strings.Contains(page, retryForm) {
		t.Fatal("a turn a reconnect notice answered must not offer Retry")
	}

	// Signed out, or the assistant disabled: nothing is dispatched.
	off := New(fakeSessions{&session.User{ID: "u1"}}, store, pipe, nil, false, retryOptions)
	off.now = func() time.Time { return now }
	postRetry(off, url.Values{"id": {"newer"}, "turn": {"t2"}})
	anon := New(fakeSessions{}, store, pipe, nil, true, retryOptions)
	anon.now = func() time.Time { return now }
	postRetry(anon, url.Values{"id": {"newer"}, "turn": {"t2"}})
	if len(pipe.turns) != 0 {
		t.Fatalf("Retry must need a session and the assistant, got %+v", pipe.turns)
	}
}

// A stalled review-panel request is retried as what it was: a blind review by a model
// other than the item's first reviewer, even when the form names that reviewer.
func TestThreadRetryReviewPanelTurn(t *testing.T) {
	now := retryNow
	store := worklist.NewMemoryStore()
	item := reviewedByModel("pr1", "default", now.Add(-time.Hour))
	item.Thread = append(item.Thread, worklist.Message{ID: "so1", Role: worklist.RoleUser, Content: independentReviewPrompt, Kind: worklist.KindReviewRequest, At: now.Add(-10 * time.Minute)})
	store.Seed("u1", item)
	pipe := &recordPipeline{}
	h := retryHandler(store, pipe, &now)

	if page := openThread(t, h, "pr1"); !strings.Contains(page, `name="turn" value="so1"`) {
		t.Fatalf("the stalled review-panel request must offer Retry, got:\n%s", page)
	}
	postRetry(h, url.Values{"id": {"pr1"}, "turn": {"so1"}, "choice": {"c0|default"}})
	if len(pipe.converses) != 0 || len(pipe.secondOpinions) != 1 || pipe.turns[0] != (worklist.TurnRef{Item: "pr1", Turn: "so1"}) {
		t.Fatalf("want one blind review of so1, got converses=%v secondOpinions=%v turns=%+v", pipe.converses, pipe.secondOpinions, pipe.turns)
	}
	if pipe.soModels[0] != "alt" {
		t.Fatalf("the retried review must not be by the first reviewer, got %q", pipe.soModels[0])
	}
}

// The same exactly-once outcome survives a web-tier restart, against the durable
// store: Retry is sent before the restart, and after it both the first run's late
// answer and the retry's answer land, followed by another Retry from the old page.
func TestThreadRetryAcrossRestart(t *testing.T) {
	ctx := context.Background()
	now := retryNow
	dir := t.TempDir()
	db, err := storage.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Worklist().Upsert(ctx, "u1", worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, Thread: []worklist.Message{
		{ID: "t1", Role: worklist.RoleUser, Content: "Is this ready?", At: now.Add(-10 * time.Minute)},
	}}); err != nil {
		t.Fatal(err)
	}
	pipe := &recordPipeline{}
	postRetry(retryHandler(db.Worklist(), pipe, &now), url.Values{"id": {"pr1"}, "turn": {"t1"}})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart: a new process, the same volume.
	db, err = storage.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := db.Worklist()
	h := retryHandler(store, pipe, &now)
	postRetry(h, url.Values{"id": {"pr1"}, "turn": {"t1"}})
	for _, m := range []worklist.Message{
		{ID: "a-late", Content: "Late answer.", At: now.Add(time.Minute)},
		{ID: "a-retry", Content: "Retried answer.", At: now.Add(2 * time.Minute)},
	} {
		if err := worklist.ApplyReply(ctx, store, "u1", "pr1", "t1", m); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(time.Hour)
	postRetry(h, url.Values{"id": {"pr1"}, "turn": {"t1"}})

	if len(pipe.turns) != 1 || pipe.turns[0].Turn != "t1" {
		t.Fatalf("the turn must be dispatched once across the restart, got %+v", pipe.turns)
	}
	got := storedItem(t, store, "pr1")
	if replies := agentReplies(got); len(got.Thread) != 2 || len(replies) != 1 || replies[0].ID != "a-late" {
		t.Fatalf("the original turn must keep exactly one answer, got %+v", got.Thread)
	}
}

// A Retry the backend refused (ingest.ErrNotAccepted) started no run, so the turn
// is not left pending: its time is put back, the page says the attempt was refused
// and offers Retry again at once. A failure that may have started a run leaves the
// turn pending, so the page polls instead of offering a second run.
func TestThreadRetryRefusedDispatch(t *testing.T) {
	now := retryNow
	stalledAt := now.Add(-10 * time.Minute)
	store := worklist.NewMemoryStore()
	store.Seed("u1", worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, Thread: []worklist.Message{
		{ID: "t1", Role: worklist.RoleUser, Content: "Is this ready?", At: stalledAt},
	}})
	pipe := &recordPipeline{err: fmt.Errorf("aei: dispatch not sent: %w", ingest.ErrNotAccepted)}
	h := retryHandler(store, pipe, &now)

	rec := postRetry(h, url.Values{"id": {"pr1"}, "turn": {"t1"}})
	loc := rec.Header().Get("Location")
	if len(pipe.turns) != 1 || loc != "/items/thread?id=pr1&retry=refused" {
		t.Fatalf("a refused retry must be dispatched once and reported, got %+v, Location %q", pipe.turns, loc)
	}
	if got := storedItem(t, store, "pr1"); !got.Thread[0].At.Equal(stalledAt) {
		t.Fatalf("a refused retry must restore the turn's time, got %v", got.Thread[0].At)
	}
	rr := httptest.NewRecorder()
	h.Thread(rr, httptest.NewRequest(http.MethodGet, loc, nil))
	if page := rr.Body.String(); !strings.Contains(page, retryForm) || !strings.Contains(page, "could not be started") {
		t.Fatalf("after a refusal the page must say so and offer Retry, got:\n%s", page)
	}

	// Unknown acceptance: a run may exist, so the turn stays pending.
	pipe.err = errors.New("aei: dispatch: context deadline exceeded")
	rec = postRetry(h, url.Values{"id": {"pr1"}, "turn": {"t1"}})
	if len(pipe.turns) != 2 || rec.Header().Get("Location") != "/items/thread?id=pr1" {
		t.Fatalf("want a second dispatch with no refusal, got %+v, Location %q", pipe.turns, rec.Header().Get("Location"))
	}
	if got := storedItem(t, store, "pr1"); !got.Thread[0].At.Equal(now) {
		t.Fatalf("a retry whose acceptance is unknown must stay pending, got %v", got.Thread[0].At)
	}
	if page := openThread(t, h, "pr1"); strings.Contains(page, retryForm) {
		t.Fatal("a turn that may be in flight must not offer Retry")
	}
}

// A turn the user hid is not retried: a retry would send its content back to the
// model as the prompt. The page asks the user to unhide it first; an old Retry form,
// say from another tab, dispatches nothing; Review all reviews afresh instead of
// re-sending a hidden review request.
func TestThreadRetryHiddenTurn(t *testing.T) {
	now := retryNow
	old := now.Add(-10 * time.Minute)
	store := worklist.NewMemoryStore()
	store.Seed("u1",
		worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, Thread: []worklist.Message{
			{ID: "t1", Role: worklist.RoleUser, Content: "secret", Hidden: true, At: old},
		}},
		worklist.WorkItem{ID: "pr2", Type: worklist.TypePullRequest, Thread: []worklist.Message{
			{ID: "r1", Role: worklist.RoleUser, Content: reviewPrompt, Kind: worklist.KindReviewRequest, Hidden: true, At: old},
		}},
	)
	pipe := &recordPipeline{}
	h := retryHandler(store, pipe, &now)

	page := openThread(t, h, "pr1")
	if strings.Contains(page, retryForm) || !strings.Contains(page, "Unhide your message to retry it") {
		t.Fatalf("a hidden stalled turn must not offer Retry, got:\n%s", page)
	}
	postRetry(h, url.Values{"id": {"pr1"}, "turn": {"t1"}})
	if len(pipe.turns) != 0 {
		t.Fatalf("a hidden turn must not be retried, got %+v", pipe.turns)
	}
	if got := storedItem(t, store, "pr1"); !got.Thread[0].At.Equal(old) {
		t.Fatal("a refused Retry must not re-stamp the hidden turn")
	}

	h.ReviewAllPRs(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/items/review-all", nil))
	for _, ref := range pipe.turns {
		if ref.Item == "pr2" && ref.Turn == "r1" {
			t.Fatal("Review all must not re-send a hidden review request")
		}
	}
	if got := storedItem(t, store, "pr2"); !got.Thread[0].At.Equal(old) || len(got.Thread) != 2 {
		t.Fatalf("Review all must leave the hidden request alone and post a new one, got %+v", got.Thread)
	}
}

// Retry of a review-panel request offers exactly the models the handler will use:
// every configured model but the first reviewer's, default included. With A
// (default), B and C, and B the first reviewer, the picker offers A and C; a choice
// of B is never honoured. With no model but the first reviewer's, no Retry is offered.
func TestThreadRetryReviewPanelModels(t *testing.T) {
	now := retryNow
	abc := []ModelOption{
		{Value: "c0|A", Label: "A", Endpoint: "https://a", Model: "A"},
		{Value: "c1|B", Label: "B", Endpoint: "https://b", Model: "B"},
		{Value: "c2|C", Label: "C", Endpoint: "https://c", Model: "C"},
	}
	panel := func(id, first string) worklist.WorkItem {
		it := reviewedByModel(id, first, now.Add(-time.Hour))
		it.Thread = append(it.Thread, worklist.Message{ID: "so-" + id, Role: worklist.RoleUser, Content: independentReviewPrompt, Kind: worklist.KindReviewRequest, At: now.Add(-10 * time.Minute)})
		return it
	}
	store := worklist.NewMemoryStore()
	store.Seed("u1", panel("pr1", "B"), panel("pr2", "B"))
	pipe := &recordPipeline{}
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, pipe, nil, true, abc)
	h.now = func() time.Time { return now }

	page := openThread(t, h, "pr1")
	form := page[strings.Index(page, retryForm):]
	form = form[:strings.Index(form, "</form>")]
	if !strings.Contains(form, `value="c0|A"`) || !strings.Contains(form, `value="c2|C"`) || strings.Contains(form, `value="c1|B"`) {
		t.Fatalf("the Retry picker must offer A and C only, got:\n%s", form)
	}
	postRetry(h, url.Values{"id": {"pr1"}, "turn": {"so-pr1"}, "choice": {"c2|C"}})
	postRetry(h, url.Values{"id": {"pr2"}, "turn": {"so-pr2"}, "choice": {"c1|B"}})
	if len(pipe.soModels) != 2 || pipe.soModels[0] != "C" || pipe.soModels[1] != "A" {
		t.Fatalf("want C as chosen, then A for a choice of the first reviewer, got %v", pipe.soModels)
	}

	// Only the first reviewer's model is configured: nothing to retry with.
	only := worklist.NewMemoryStore()
	only.Seed("u1", panel("pr3", "A"))
	pipe = &recordPipeline{}
	h = New(fakeSessions{&session.User{ID: "u1"}}, only, pipe, nil, true, abc[:1])
	h.now = func() time.Time { return now }
	if page := openThread(t, h, "pr3"); strings.Contains(page, retryForm) {
		t.Fatal("no Retry may be offered when no model but the first reviewer's exists")
	}
	postRetry(h, url.Values{"id": {"pr3"}, "turn": {"so-pr3"}})
	if len(pipe.turns) != 0 {
		t.Fatalf("want no dispatch, got %+v", pipe.turns)
	}
}

// The browser can go away after Retry stamped the turn but before the dispatch was
// sent: the backend then refuses the run (ErrNotAccepted) because the request was
// canceled. No run exists, so the turn's time must still be put back, which needs a
// store write that the canceled request cannot stop. The durable store honours
// cancellation, so this runs against it.
func TestThreadRetryRefusedAfterDisconnect(t *testing.T) {
	ctx := context.Background()
	now := retryNow
	stalledAt := now.Add(-10 * time.Minute)
	db, err := storage.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := db.Worklist()
	if err := store.Upsert(ctx, "u1", worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, Thread: []worklist.Message{
		{ID: "t1", Role: worklist.RoleUser, Content: "Is this ready?", At: stalledAt},
	}}); err != nil {
		t.Fatal(err)
	}

	reqCtx, disconnect := context.WithCancel(ctx)
	defer disconnect()
	pipe := &recordPipeline{
		onDispatch: disconnect, // the browser goes away mid-dispatch
		err:        fmt.Errorf("aei: dispatch not sent: %w: %w", ingest.ErrNotAccepted, context.Canceled),
	}
	h := retryHandler(store, pipe, &now)
	form := url.Values{"id": {"pr1"}, "turn": {"t1"}}
	req := httptest.NewRequest(http.MethodPost, "/items/thread/retry", strings.NewReader(form.Encode())).WithContext(reqCtx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ThreadRetry(httptest.NewRecorder(), req)

	if len(pipe.turns) != 1 {
		t.Fatalf("want one (refused) dispatch, got %+v", pipe.turns)
	}
	if got := storedItem(t, store, "pr1"); !got.Thread[0].At.Equal(stalledAt) {
		t.Fatalf("a retry refused after the browser left must still restore the turn's time, got %v", got.Thread[0].At)
	}
	pipe.onDispatch, pipe.err = nil, nil
	if page := openThread(t, h, "pr1"); !strings.Contains(page, retryForm) {
		t.Fatal("reopening the thread must offer Retry again")
	}
}
