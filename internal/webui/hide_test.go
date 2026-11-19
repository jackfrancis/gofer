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

	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// Hide sets the whole item's hidden time (so it leaves the radar but keeps its
// thread), Unhide clears it, and both redirect (PRG); an unknown item or an
// anonymous request changes nothing.
func TestHideItemTogglesWholeItem(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	thread := []worklist.Message{{ID: "q", Role: worklist.RoleUser, Content: "q"}}
	store := worklist.NewMemoryStore()
	store.Seed("u1",
		worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, Thread: thread},
		worklist.WorkItem{ID: "pr2", Type: worklist.TypePullRequest})
	h := &Handler{sessions: fakeSessions{&session.User{ID: "u1"}}, store: store, now: func() time.Time { return now }}
	post := func(h *Handler, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/items/hide", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.HideItem(rec, req)
		return rec
	}
	get := func(id string) worklist.WorkItem {
		items, _ := store.List(context.Background(), "u1")
		for _, it := range items {
			if it.ID == id {
				return it
			}
		}
		t.Fatalf("%s missing", id)
		return worklist.WorkItem{}
	}

	rec := post(h, "id=pr1")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?hidden=pr1" {
		t.Fatalf("hide: status %d to %q, want 303 to the radar with Undo for pr1", rec.Code, rec.Header().Get("Location"))
	}
	if got := get("pr1"); !got.Meta.HiddenAt.Equal(now) || len(got.Thread) != 1 {
		t.Fatalf("hide must stamp HiddenAt and keep the thread, got %v %+v", got.Meta.HiddenAt, got.Thread)
	}
	if !get("pr2").Meta.HiddenAt.IsZero() {
		t.Fatal("hiding one item must not touch another")
	}
	res, err := worklist.Resolve(context.Background(), store, &recordPipeline{}, now, "u1", worklist.DefaultSort, true)
	if err != nil || len(res.Items) != 1 || res.Items[0].ID != "pr2" {
		t.Fatalf("a hidden item leaves the radar, got %+v (%v)", res.Items, err)
	}

	// A repeated Hide (a stale tab, a double submit) keeps the original hide time, so
	// GitHub activity after the first Hide still resurfaces the item.
	later := &Handler{sessions: h.sessions, store: store, now: func() time.Time { return now.Add(time.Hour) }}
	post(later, "id=pr1")
	if got := get("pr1"); !got.Meta.HiddenAt.Equal(now) {
		t.Fatalf("a repeated Hide moved HiddenAt to %v, want the original %v", got.Meta.HiddenAt, now)
	}
	merged := worklist.MergeRunItems([]worklist.WorkItem{get("pr1")},
		[]worklist.WorkItem{{ID: "pr1", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{UpdatedAt: now.Add(time.Minute)}}})
	if !merged[0].OnRadar() {
		t.Fatal("a comment between the first and a repeated Hide must still resurface the item")
	}

	rec = post(h, "id=pr1&hidden=false&back=thread")
	if rec.Header().Get("Location") != "/items/thread?id=pr1" {
		t.Fatalf("unhide from the thread returns to it, got %q", rec.Header().Get("Location"))
	}
	if !get("pr1").Meta.HiddenAt.IsZero() {
		t.Fatal("unhide must clear HiddenAt")
	}

	if rec := post(h, "id=missing"); rec.Code != http.StatusSeeOther {
		t.Fatalf("an unknown item is a safe no-op, status %d", rec.Code)
	}
	anon := &Handler{sessions: fakeSessions{nil}, store: store, now: func() time.Time { return now }}
	post(anon, "id=pr2")
	if !get("pr2").Meta.HiddenAt.IsZero() {
		t.Fatal("an anonymous request must not hide anything")
	}
}

// offRadarPRs returns a hidden and a closed PR built by mk, each in a state that
// would otherwise make it eligible.
func offRadarPRs(mk func(id string) worklist.WorkItem, at time.Time) []worklist.WorkItem {
	hidden, closed := mk("pr-hidden"), mk("pr-closed")
	hidden.Meta.HiddenAt = at
	closed.Meta.CompletedAt = at
	return []worklist.WorkItem{hidden, closed}
}

// Neither batch action dispatches anything for, or writes a prompt into, a hidden or
// closed PR — including a stalled one Review all would otherwise retry.
func TestBatchActionsSkipHiddenAndClosedPRs(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	user := fakeSessions{&session.User{ID: "u1"}}
	opts := []ModelOption{
		{Value: "c0|default", Endpoint: "https://default", Model: "default"},
		{Value: "c1|alt", Endpoint: "https://alt", Model: "alt"},
	}
	threads := func(store worklist.Store) map[string]int {
		items, _ := store.List(context.Background(), "u1")
		n := map[string]int{}
		for _, it := range items {
			n[it.ID] = len(it.Thread)
		}
		return n
	}

	t.Run("review all", func(t *testing.T) {
		for _, mk := range map[string]func(string) worklist.WorkItem{
			"unreviewed": func(id string) worklist.WorkItem {
				return worklist.WorkItem{ID: id, Type: worklist.TypePullRequest}
			},
			"stalled": func(id string) worklist.WorkItem {
				return worklist.WorkItem{ID: id, Type: worklist.TypePullRequest, Thread: []worklist.Message{
					{ID: "q", Role: worklist.RoleUser, Content: reviewPrompt, Kind: worklist.KindReviewRequest, At: now.Add(-time.Hour)},
				}}
			},
		} {
			store := worklist.NewMemoryStore()
			off := offRadarPRs(mk, now.Add(-time.Minute))
			store.Seed("u1", append(off, mk("pr-open"))...)
			before := threads(store)
			pipe := &recordPipeline{}
			h := &Handler{sessions: user, store: store, pipeline: pipe, convEnabled: true, options: opts, now: func() time.Time { return now }}
			h.ReviewAllPRs(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/items/review-all", nil))

			if len(pipe.reviewed) != 1 || pipe.reviewed[0] != "pr-open" {
				t.Fatalf("only the visible open PR is reviewed, got %v", pipe.reviewed)
			}
			after := threads(store)
			for _, it := range off {
				if after[it.ID] != before[it.ID] {
					t.Errorf("%s: Review all wrote a prompt into an off-radar PR", it.ID)
				}
			}
		}
	})

	t.Run("second opinion", func(t *testing.T) {
		mk := func(id string) worklist.WorkItem { return reviewedByModel(id, "default", now) }
		store := worklist.NewMemoryStore()
		off := offRadarPRs(mk, now.Add(-time.Minute))
		store.Seed("u1", append(off, mk("pr-open"))...)
		before := threads(store)
		pipe := &recordPipeline{}
		h := &Handler{sessions: user, store: store, pipeline: pipe, convEnabled: true, options: opts, now: func() time.Time { return now }}
		req := httptest.NewRequest(http.MethodPost, "/items/second-opinion-all", strings.NewReader("choice=c1|alt"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		h.SecondOpinionAllPRs(httptest.NewRecorder(), req)

		if len(pipe.secondOpinions) != 1 || pipe.secondOpinions[0] != "pr-open" {
			t.Fatalf("only the visible open PR gets a second opinion, got %v", pipe.secondOpinions)
		}
		after := threads(store)
		for _, it := range off {
			if after[it.ID] != before[it.ID] {
				t.Errorf("%s: the second-opinion batch wrote a prompt into an off-radar PR", it.ID)
			}
		}
	})

	t.Run("only off-radar PRs", func(t *testing.T) {
		store := worklist.NewMemoryStore()
		store.Seed("u1", append(
			offRadarPRs(func(id string) worklist.WorkItem { return worklist.WorkItem{ID: id, Type: worklist.TypePullRequest} }, now),
			offRadarPRs(func(id string) worklist.WorkItem { return reviewedByModel(id+"-reviewed", "default", now) }, now)...)...)
		pipe := &recordPipeline{}
		h := &Handler{sessions: user, store: store, pipeline: pipe, convEnabled: true, options: opts, now: func() time.Time { return now }}
		h.ReviewAllPRs(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/items/review-all", nil))
		h.SecondOpinionAllPRs(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/items/second-opinion-all", nil))
		if len(pipe.turns) != 0 {
			t.Fatalf("nothing on the radar, nothing dispatched; got %v", pipe.turns)
		}
	})
}

// Every radar row offers Hide (with or without the assistant); the thread page
// offers "Hide item", or Unhide (back to the thread) once the item is hidden.
func TestHideItemControls(t *testing.T) {
	pr := worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Number: 1, Repo: "o/r", Title: "A PR"}}
	for _, conv := range []bool{true, false} {
		out := renderPage(t, pageData{View: "worklist", Items: []worklist.WorkItem{pr}, ConvEnabled: conv})
		if !strings.Contains(out, `action="/items/hide"`) || !strings.Contains(out, `name="id" value="pr1"`) {
			t.Fatalf("conv=%v: expected a Hide form on the radar row, got:\n%s", conv, out)
		}
	}

	out := renderPage(t, pageData{View: "thread", Item: pr, ConvEnabled: true, User: &session.User{ID: "u1"}})
	if !strings.Contains(out, `action="/items/hide"`) || !strings.Contains(out, ">Hide item</button>") {
		t.Fatalf("expected a Hide item control on the thread, got:\n%s", out)
	}

	pr.Meta.HiddenAt = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	out = renderPage(t, pageData{View: "thread", Item: pr, ConvEnabled: true, User: &session.User{ID: "u1"}})
	if strings.Contains(out, ">Hide item</button>") || !strings.Contains(out, `name="back" value="thread"`) || !strings.Contains(out, "Hidden from your radar") {
		t.Fatalf("a hidden item's thread offers Unhide instead, got:\n%s", out)
	}
}

// failingStore fails every write, as a full disk or a locked database would.
type failingStore struct{ worklist.Store }

func (failingStore) Update(context.Context, string, func([]worklist.WorkItem) ([]worklist.WorkItem, error)) error {
	return errors.New("disk full")
}

// A hide or unhide whose write fails says so instead of redirecting as if it worked.
func TestHideItemReportsFailedWrite(t *testing.T) {
	mem := worklist.NewMemoryStore()
	mem.Seed("u1", worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest})
	h := New(fakeSessions{&session.User{ID: "u1"}}, failingStore{mem}, &recordPipeline{}, reconnectProviders{}, true, nil)
	for _, tc := range []struct{ body, verb, retry string }{
		{"id=pr1", "hide", `href="/"`},                                               // the radar row still offers Hide
		{"id=pr1&hidden=false", "unhide", `href="/?hidden=pr1"`},                     // the radar's Undo for the still-hidden item
		{"id=pr1&hidden=false&back=thread", "unhide", `href="/items/thread?id=pr1"`}, // the thread's Unhide
	} {
		body, verb := tc.body, tc.verb
		req := httptest.NewRequest(http.MethodPost, "/items/hide", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.HideItem(rec, req)
		if rec.Code != http.StatusInternalServerError || rec.Header().Get("Location") != "" {
			t.Fatalf("%s: status %d to %q, want a 500 page, not a redirect", verb, rec.Code, rec.Header().Get("Location"))
		}
		out := rec.Body.String()
		if !strings.Contains(out, "gofer couldn&#39;t "+verb+" that item") || strings.Contains(out, "discovering your work") {
			t.Fatalf("%s: the page must explain the failed change, got:\n%s", verb, out)
		}
		if !strings.Contains(out, tc.retry+">Try again</a>") {
			t.Fatalf("%s (%s): Try again must lead back to the same action (%s), got:\n%s", verb, body, tc.retry, out)
		}
	}
}

// After a hide the radar offers Undo for that item, which is no longer listed; it
// offers nothing for an item that is visible again or unknown.
func TestRadarOffersUndoAfterHide(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	store := worklist.NewMemoryStore()
	hidden := worklist.WorkItem{ID: "github:o/r#1", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1, Title: "Hidden PR"}}
	hidden.Meta.HiddenAt = now
	visible := worklist.WorkItem{ID: "github:o/r#2", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: "o/r", Number: 2, Title: "Visible PR"}}
	store.Seed("u1", hidden, visible)
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, &recordPipeline{}, reconnectProviders{}, true, nil)
	page := func(target string) string {
		rec := httptest.NewRecorder()
		h.Index(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec.Body.String()
	}

	out := page("/?hidden=" + url.QueryEscape(hidden.ID))
	if !strings.Contains(out, "Hidden <b>Hidden PR</b>") || !strings.Contains(out, ">Undo</button>") ||
		!strings.Contains(out, `name="id" value="github:o/r#1"`) || !strings.Contains(out, `name="hidden" value="false"`) {
		t.Fatalf("expected an Undo for the hidden item, got:\n%s", out)
	}
	for _, target := range []string{"/", "/?hidden=" + url.QueryEscape(visible.ID), "/?hidden=nope"} {
		if strings.Contains(page(target), ">Undo</button>") {
			t.Errorf("%s: no Undo expected", target)
		}
	}
}

// Reset Conversations counts every thread it will clear: hidden and closed items'
// threads too, not just the ones on the radar.
func TestResetCountIncludesOffRadarThreads(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	thread := []worklist.Message{{ID: "q", Role: worklist.RoleUser, Content: "q"}}
	mk := func(n int) worklist.WorkItem {
		return worklist.WorkItem{ID: fmt.Sprintf("pr%d", n), Type: worklist.TypePullRequest, Thread: thread,
			GitHub: worklist.GitHubRef{Repo: "o/r", Number: n, Title: fmt.Sprintf("PR %d", n)}}
	}
	closed := mk(3)
	closed.Meta.CompletedAt = now
	store := worklist.NewMemoryStore()
	store.Seed("u1", mk(1), mk(2), closed, worklist.WorkItem{ID: "pr4", Type: worklist.TypePullRequest})
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, &recordPipeline{}, reconnectProviders{}, true, nil)
	h.now = func() time.Time { return now }

	req := httptest.NewRequest(http.MethodPost, "/items/hide", strings.NewReader("id=pr2"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.HideItem(httptest.NewRecorder(), req)

	rec := httptest.NewRecorder()
	h.Index(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if out := rec.Body.String(); !strings.Contains(out, "Reset Conversations (3)") {
		t.Fatalf("Reset clears three threads (one visible, one hidden, one closed), so it must say 3, got:\n%s", out)
	}
}

// countingStore counts full worklist reads.
type countingStore struct {
	worklist.Store
	lists int
}

func (c *countingStore) List(ctx context.Context, owner string) ([]worklist.WorkItem, error) {
	c.lists++
	return c.Store.List(ctx, owner)
}

// The radar, with its Reset count and Undo banner, is built from one read of the
// worklist, so the two can never disagree about an item hidden in between.
func TestRadarReadsWorklistOnce(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	thread := []worklist.Message{{ID: "q", Role: worklist.RoleUser, Content: "q"}}
	hidden := worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, Thread: thread, GitHub: worklist.GitHubRef{Title: "Hidden PR"}}
	hidden.Meta.HiddenAt = now
	mem := worklist.NewMemoryStore()
	mem.Seed("u1", hidden, worklist.WorkItem{ID: "pr2", Type: worklist.TypePullRequest, Thread: thread})
	store := &countingStore{Store: mem}
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, &recordPipeline{}, reconnectProviders{}, true, nil)

	rec := httptest.NewRecorder()
	h.Index(rec, httptest.NewRequest(http.MethodGet, "/?hidden=pr1", nil))
	out := rec.Body.String()
	if store.lists != 1 {
		t.Fatalf("the radar read the worklist %d times, want 1", store.lists)
	}
	if !strings.Contains(out, "Reset Conversations (2)") || !strings.Contains(out, ">Undo</button>") {
		t.Fatalf("Reset count and Undo must still come from that one read, got:\n%s", out)
	}
}
