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

// Hide stamps the whole item's hidden time and deletes its signals and score (so it
// leaves the ranked radar but keeps its GitHub fields and thread); it stays hidden
// whatever GitHub shows. Unhide brings it back empty, awaiting a refresh, and asks
// for one. Both redirect (PRG); an unknown item or an anonymous request changes
// nothing.
func TestHideItemTogglesWholeItem(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	thread := []worklist.Message{{ID: "q", Role: worklist.RoleUser, Content: "q"}}
	store := worklist.NewMemoryStore()
	store.Seed("u1",
		worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, Thread: thread,
			GitHub:  worklist.GitHubRef{Title: "A PR", LabelColors: map[string]string{"bug": "d73a4a"}},
			Signals: worklist.Signals{Labels: []string{"bug"}, Reasons: []worklist.Reason{worklist.ReasonCommented}},
			Meta:    worklist.Metadata{Rank: 0.4, Relevance: 0.5, Rationale: "on your radar as a comment you left"}},
		worklist.WorkItem{ID: "pr2", Type: worklist.TypePullRequest})
	pipe := &recordPipeline{}
	h := &Handler{sessions: fakeSessions{&session.User{ID: "u1"}}, store: store, pipeline: pipe, now: func() time.Time { return now }}
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
	got := get("pr1")
	if !got.Meta.HiddenAt.Equal(now) || len(got.Thread) != 1 || got.GitHub.Title != "A PR" {
		t.Fatalf("hide must stamp HiddenAt and keep the thread and GitHub fields, got %+v", got)
	}
	if got.Meta.Rank != 0 || got.Meta.Rationale != "" || len(got.Signals.Reasons) != 0 || len(got.Signals.Labels) != 1 {
		t.Fatalf("hide must delete the signals (but the labels) and the score, got %+v %+v", got.Signals, got.Meta)
	}
	if !get("pr2").Meta.HiddenAt.IsZero() || pipe.refreshed+pipe.followUps != 0 {
		t.Fatal("hiding one item must not touch another, nor refresh")
	}
	res, err := worklist.Resolve(context.Background(), store, pipe, now, "u1", worklist.DefaultSort, true)
	if err != nil || len(res.Items) != 1 || res.Items[0].ID != "pr2" || len(res.Hidden) != 1 || res.Hidden[0].ID != "pr1" {
		t.Fatalf("a hidden item leaves the ranked radar for the hidden section, got %+v / %+v (%v)", res.Items, res.Hidden, err)
	}

	// A repeated Hide (a stale tab, a double submit) keeps the original hide time.
	later := &Handler{sessions: h.sessions, store: store, pipeline: pipe, now: func() time.Time { return now.Add(time.Hour) }}
	post(later, "id=pr1")
	if got := get("pr1"); !got.Meta.HiddenAt.Equal(now) {
		t.Fatalf("a repeated Hide moved HiddenAt to %v, want the original %v", got.Meta.HiddenAt, now)
	}
	// New GitHub activity does not bring it back.
	merged := worklist.MergeRunItems([]worklist.WorkItem{get("pr1")},
		[]worklist.WorkItem{{ID: "pr1", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{UpdatedAt: now.Add(time.Minute)}}})
	if merged[0].OnRadar() {
		t.Fatal("a hidden item stays hidden whatever GitHub shows")
	}

	rec = post(h, "id=pr1&hidden=false&back=thread")
	if rec.Header().Get("Location") != "/items/thread?id=pr1" {
		t.Fatalf("unhide from the thread returns to it, got %q", rec.Header().Get("Location"))
	}
	if got := get("pr1"); got.IsHidden() || !got.Meta.AwaitingRefresh || len(got.Thread) != 1 {
		t.Fatalf("unhide brings the item back, thread intact, awaiting a refresh, got %+v", got)
	}
	if pipe.followUps != 1 || pipe.refreshed != 0 {
		t.Fatalf("unhide asks for a follow-up refresh to fill the item in, got %d follow-ups, %d refreshes", pipe.followUps, pipe.refreshed)
	}
	if rec := post(h, "id=pr1&hidden=false"); rec.Header().Get("Location") != "/" || pipe.followUps != 1 {
		t.Fatalf("unhiding a visible item changes nothing and refreshes nothing, got %q, %d", rec.Header().Get("Location"), pipe.followUps)
	}
	if rec := post(h, "id=pr2&back=thread"); rec.Header().Get("Location") != "/items/thread?id=pr2" || !get("pr2").IsHidden() {
		t.Fatalf("hide from the thread returns to it, got %q", rec.Header().Get("Location"))
	}

	if rec := post(h, "id=missing"); rec.Code != http.StatusSeeOther {
		t.Fatalf("an unknown item is a safe no-op, status %d", rec.Code)
	}
	anon := &Handler{sessions: fakeSessions{nil}, store: store, pipeline: pipe, now: func() time.Time { return now }}
	post(anon, "id=pr1")
	if get("pr1").IsHidden() {
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
// offers "Hide item", or Unhide (back to the thread) once the item is hidden, and a
// hidden item's thread takes no new turn or review panel.
func TestHideItemControls(t *testing.T) {
	pr := worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Number: 1, Repo: "o/r", Title: "A PR"}}
	for _, conv := range []bool{true, false} {
		out := renderPage(t, pageData{View: "worklist", Items: []worklist.WorkItem{pr}, ConvEnabled: conv})
		if !strings.Contains(out, `action="/items/hide"`) || !strings.Contains(out, `name="id" value="pr1"`) {
			t.Fatalf("conv=%v: expected a Hide form on the radar row, got:\n%s", conv, out)
		}
	}

	opts := []ModelOption{{Value: "a"}, {Value: "b"}}
	out := renderPage(t, pageData{View: "thread", Item: pr, ConvEnabled: true, SecondOpinionEnabled: true, SecondOpinionOptions: opts, User: &session.User{ID: "u1"}})
	if !strings.Contains(out, `action="/items/hide"`) || !strings.Contains(out, ">Hide item</button>") {
		t.Fatalf("expected a Hide item control on the thread, got:\n%s", out)
	}
	if !strings.Contains(out, `action="/items/thread"`) || !strings.Contains(out, `action="/items/second-opinion"`) {
		t.Fatalf("a visible item's thread takes turns, got:\n%s", out)
	}

	pr.Meta.HiddenAt = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	// Thread disables the conversation controls for a hidden item (ItemHidden).
	out = renderPage(t, pageData{View: "thread", Item: pr, ItemHidden: true, Assistant: true, User: &session.User{ID: "u1"}})
	if strings.Contains(out, ">Hide item</button>") || !strings.Contains(out, `name="back" value="thread"`) || !strings.Contains(out, "Hidden from your radar until you unhide it") {
		t.Fatalf("a hidden item's thread offers Unhide instead, got:\n%s", out)
	}
	if strings.Contains(out, `action="/items/thread"`) || strings.Contains(out, `action="/items/second-opinion"`) || !strings.Contains(out, "Unhide it to discuss it.") {
		t.Fatalf("a hidden item's thread takes no new turn, got:\n%s", out)
	}
	if strings.Contains(out, "not configured") {
		t.Fatalf("a hidden item is not an unconfigured assistant, got:\n%s", out)
	}

	// The turns of a hidden item's thread carry no Hide or Unhide of their own.
	pr.Thread = []worklist.Message{
		{ID: "q", Role: worklist.RoleUser, Content: "q"},
		{ID: "a", Role: worklist.RoleAgent, Content: "a", ReplyTo: "q", Hidden: true},
	}
	out = renderPage(t, pageData{View: "thread", Item: pr, ItemHidden: true, User: &session.User{ID: "u1"}})
	if strings.Contains(out, `action="/items/thread/hide"`) || !strings.Contains(out, "Hidden reply") {
		t.Fatalf("a hidden item's turns offer no Hide or Unhide, got:\n%s", out)
	}
	pr.Meta.HiddenAt = time.Time{}
	if out := renderPage(t, pageData{View: "thread", Item: pr, ConvEnabled: true, User: &session.User{ID: "u1"}}); strings.Count(out, `action="/items/thread/hide"`) != 2 {
		t.Fatalf("a visible item's turns offer Hide and Unhide, got:\n%s", out)
	}
	if out := renderPage(t, pageData{View: "thread", Item: pr, User: &session.User{ID: "u1"}}); !strings.Contains(out, "The assistant is not configured.") {
		t.Fatalf("without an assistant the thread says so, got:\n%s", out)
	}
}

// With the assistant disabled, a hidden item's thread is still read-only: no
// per-message Hide or Unhide (which HideMessage would refuse anyway), and no
// invitation to discuss it once unhidden.
func TestHiddenThreadWithoutAssistant(t *testing.T) {
	store := worklist.NewMemoryStore()
	store.Seed("u1", worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, Meta: worklist.Metadata{HiddenAt: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)},
		Thread: []worklist.Message{
			{ID: "q", Role: worklist.RoleUser, Content: "q"},
			{ID: "a", Role: worklist.RoleAgent, Content: "a", ReplyTo: "q", Hidden: true},
		}})
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, &recordPipeline{}, reconnectProviders{}, false, nil)
	rec := httptest.NewRecorder()
	h.Thread(rec, httptest.NewRequest(http.MethodGet, "/items/thread?id=pr1", nil))
	out := rec.Body.String()
	if strings.Contains(out, `action="/items/thread/hide"`) || !strings.Contains(out, "This item is hidden.") || strings.Contains(out, "discuss it") {
		t.Fatalf("a hidden thread without an assistant is read-only and says only that it is hidden, got:\n%s", out)
	}
}

// A hidden item's row sits in the hidden section: greyscale, no score, no colored
// labels, no Discuss, with Unhide. An unhidden item awaiting its refresh shows that
// instead of a score.
func TestHiddenSectionRendering(t *testing.T) {
	at := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	hidden := worklist.WorkItem{ID: "pr-h", Type: worklist.TypePullRequest,
		GitHub:  worklist.GitHubRef{Number: 1, Repo: "o/r", Title: "Hidden PR", URL: "https://github.com/o/r/pull/1", LabelColors: map[string]string{"bug": "d73a4a"}},
		Signals: worklist.Signals{Labels: []string{"bug"}}, Meta: worklist.Metadata{HiddenAt: at}}
	back := worklist.WorkItem{ID: "pr-b", Type: worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Number: 2, Repo: "o/r", Title: "Back PR"}, Meta: worklist.Metadata{AwaitingRefresh: true}}
	out := renderPage(t, pageData{View: "worklist", Items: []worklist.WorkItem{back}, HiddenItems: []worklist.WorkItem{hidden}, ConvEnabled: true})

	i := strings.Index(out, `class="zz-hidden-list"`)
	if i < 0 || !strings.Contains(out, "Hidden (1)") {
		t.Fatalf("expected a hidden section, got:\n%s", out)
	}
	ranked, section := out[:i], out[i:]
	if strings.Contains(ranked, "Hidden PR") || !strings.Contains(section, "Hidden PR") || !strings.Contains(section, "o/r #1") {
		t.Fatalf("the hidden item is listed only in the hidden section, with its GitHub fields, got:\n%s", out)
	}
	for _, absent := range []string{"Relevance", "zz-bar", "zz-priority", "ghl-", "Discuss", ">Hide</button>"} {
		if strings.Contains(section, absent) {
			t.Errorf("the hidden section must not show %q, got:\n%s", absent, section)
		}
	}
	if !strings.Contains(section, `zz-item--hidden`) || !strings.Contains(section, `name="hidden" value="false"`) || !strings.Contains(section, ">Unhide</button>") || !strings.Contains(section, ">bug</span>") {
		t.Fatalf("a hidden row is greyscale, keeps its labels and offers Unhide, got:\n%s", section)
	}
	if !strings.Contains(ranked, "Waiting for the next refresh") || strings.Contains(ranked, "Relevance") {
		t.Fatalf("an unhidden item awaiting its refresh shows that instead of a score, got:\n%s", ranked)
	}
}

// A hidden item takes no Discuss turn, review panel, retry or turn hiding: Unhide it
// first.
func TestHiddenItemRefusesDiscuss(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	stalled := []worklist.Message{{ID: "q", Role: worklist.RoleUser, Content: "q", At: now.Add(-time.Hour)}}
	store := worklist.NewMemoryStore()
	store.Seed("u1", worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, Thread: stalled, Meta: worklist.Metadata{HiddenAt: now.Add(-time.Minute)}})
	pipe := &recordPipeline{}
	opts := []ModelOption{{Value: "c0|default", Model: "default"}, {Value: "c1|alt", Model: "alt"}}
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, pipe, reconnectProviders{}, true, opts)
	h.now = func() time.Time { return now }
	// Retry first: once a turn is appended, the stalled turn is no longer the last.
	for _, c := range []struct {
		path   string
		handle http.HandlerFunc
	}{
		{"/items/thread/retry?turn=q", h.ThreadRetry},
		{"/items/thread?content=hello", h.ThreadPost},
		{"/items/second-opinion?choice=c1%7Calt", h.SecondOpinion},
		{"/items/thread/hide?msg=q&hidden=true", h.HideMessage},
	} {
		path, handle := c.path, c.handle
		u, _ := url.Parse(path)
		form := u.Query()
		form.Set("id", "pr1")
		req := httptest.NewRequest(http.MethodPost, u.Path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		handle(httptest.NewRecorder(), req)
	}
	if len(pipe.turns) != 0 {
		t.Fatalf("a hidden item dispatches nothing, got %v", pipe.turns)
	}
	items, _ := store.List(context.Background(), "u1")
	if len(items[0].Thread) != 1 || !items[0].Thread[0].At.Equal(now.Add(-time.Hour)) || items[0].Thread[0].Hidden {
		t.Fatalf("a hidden item's thread is unchanged, got %+v", items[0].Thread)
	}

	rec := httptest.NewRecorder()
	h.Thread(rec, httptest.NewRequest(http.MethodGet, "/items/thread?id=pr1", nil))
	if out := rec.Body.String(); strings.Contains(out, `action="/items/thread"`) || strings.Contains(out, `action="/items/thread/retry"`) || strings.Contains(out, `action="/items/thread/hide"`) || strings.Contains(out, "not configured") || !strings.Contains(out, "Unhide it to discuss it.") {
		t.Fatalf("a hidden item's thread page is read-only, got:\n%s", out)
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
		{"id=pr1&hidden=false", "unhide", `href="/"`},                                // the radar's hidden section
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

// The radar lists open hidden items below the ranked ones, most recently hidden
// first, and no Undo banner; a hidden item that closed is not listed. The project
// filter narrows the hidden section too, and its checklist names the repositories
// of hidden items.
func TestRadarListsHiddenSection(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	mk := func(repo string, n int, title string) worklist.WorkItem {
		return worklist.WorkItem{ID: fmt.Sprintf("github:%s#%d", repo, n), Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: repo, Number: n, Title: title}}
	}
	early, late, closed := mk("o/r", 1, "Early hidden"), mk("o/r", 2, "Late hidden"), mk("o/r", 3, "Closed hidden")
	early.Meta.HiddenAt, late.Meta.HiddenAt = now.Add(-time.Hour), now
	closed.Meta.HiddenAt, closed.Meta.CompletedAt = now, now
	other := mk("x/y", 4, "Other hidden")
	other.Meta.HiddenAt = now.Add(-2 * time.Hour)
	store := worklist.NewMemoryStore()
	store.Seed("u1", early, mk("o/r", 5, "Visible PR"), late, closed, other)
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, &recordPipeline{}, reconnectProviders{}, true, nil)
	page := func(cookie string) string {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: projectsCookie, Value: cookie})
		}
		rec := httptest.NewRecorder()
		h.Index(rec, req)
		return rec.Body.String()
	}

	out := page("")
	iv, il, ie, io := strings.Index(out, "Visible PR"), strings.Index(out, "Late hidden"), strings.Index(out, "Early hidden"), strings.Index(out, "Other hidden")
	if iv < 0 || !(iv < il && il < ie && ie < io) || !strings.Contains(out, "Hidden (3)") {
		t.Fatalf("want the visible PR, then the hidden ones most recently hidden first, got:\n%s", out)
	}
	if strings.Contains(out, "Closed hidden") || strings.Contains(out, ">Undo</button>") {
		t.Fatalf("no closed hidden item and no Undo banner, got:\n%s", out)
	}
	if !strings.Contains(out, "1 item on your radar") {
		t.Fatalf("the radar count leaves the hidden section out, got:\n%s", out)
	}
	if !strings.Contains(out, `value="x/y" data-filter-project`) {
		t.Fatalf("the checklist names a repository with only hidden items, got:\n%s", out)
	}

	out = page(url.Values{"u": {"u1"}, "p": {"x/y"}}.Encode())
	if strings.Contains(out, "Late hidden") || !strings.Contains(out, "Other hidden") || !strings.Contains(out, "Hidden (1)") {
		t.Fatalf("the project filter narrows the hidden section, got:\n%s", out)
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

// The radar, with its Reset count and hidden section, is built from one read of the
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
	h.Index(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	out := rec.Body.String()
	if store.lists != 1 {
		t.Fatalf("the radar read the worklist %d times, want 1", store.lists)
	}
	if !strings.Contains(out, "Reset Conversations (2)") || !strings.Contains(out, "Hidden PR") {
		t.Fatalf("Reset count and the hidden section must still come from that one read, got:\n%s", out)
	}
}
