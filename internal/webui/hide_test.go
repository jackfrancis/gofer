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
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("hide: status %d to %q, want 303 to the radar", rec.Code, rec.Header().Get("Location"))
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
