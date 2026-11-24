package webui

import (
	"context"
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

// While a reply is in flight the thread page says so (it polls until the reply
// lands) and offers no review panel, and a panel submitted from an older page adds
// no second request. Once nothing is pending the panel is back.
func TestReviewPanelWhileReplyPending(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := worklist.NewMemoryStore()
	store.Seed("u1", worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, Thread: []worklist.Message{
		{ID: "p1", Role: worklist.RoleUser, Content: independentReviewPrompt, Kind: worklist.KindReviewRequest, At: now.Add(-time.Minute)},
	}})
	pipe := &recordPipeline{}
	opts := []ModelOption{
		{Value: "c0|default-model", Label: "default-model", Endpoint: "https://default", Model: "default-model"},
		{Value: "c1|alt", Label: "alt", Endpoint: "https://second", Model: "alt"},
	}
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, pipe, nil, true, opts)
	h.now = func() time.Time { return now }
	page := func() string {
		rec := httptest.NewRecorder()
		h.Thread(rec, httptest.NewRequest(http.MethodGet, "/items/thread?id=pr1", nil))
		return rec.Body.String()
	}

	out := page()
	if !strings.Contains(out, "zz-pending") || !strings.Contains(out, `http-equiv="refresh"`) {
		t.Fatalf("a pending reply must show the working cue and poll, got:\n%s", out)
	}
	if strings.Contains(out, `action="/items/second-opinion"`) {
		t.Fatal("no review panel while a reply is pending")
	}

	req := httptest.NewRequest(http.MethodPost, "/items/second-opinion", strings.NewReader("id=pr1&choice=c1|alt"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.SecondOpinion(httptest.NewRecorder(), req)
	if len(pipe.secondOpinions) != 0 {
		t.Fatalf("a panel submitted while a reply is pending must not dispatch, got %v", pipe.secondOpinions)
	}
	if items, _ := store.List(context.Background(), "u1"); len(items[0].Thread) != 1 {
		t.Fatalf("a panel submitted while a reply is pending must add nothing, got %+v", items[0].Thread)
	}

	// The reply lands: no cue, and the panel is offered again.
	if err := worklist.ApplyReply(context.Background(), store, "u1", "pr1", "p1", worklist.Message{ID: "r1", Content: "review", At: now}); err != nil {
		t.Fatal(err)
	}
	out = page()
	if strings.Contains(out, "zz-pending") || !strings.Contains(out, `action="/items/second-opinion"`) {
		t.Fatalf("an answered thread shows the panel and no cue, got:\n%s", out)
	}
}

// The panel never compares a model with itself: once the alternative model has given
// the item's first review (the owner ran the panel first), the panel offers the other
// model, and a choice made on a page rendered before that review landed is resolved
// against the item as stored.
func TestReviewPanelPicksAModelOtherThanTheFirstReviewer(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := worklist.NewMemoryStore()
	store.Seed("u1", worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest})
	pipe := &recordPipeline{}
	opts := []ModelOption{
		{Value: "c0|default-model", Label: "default-model", Endpoint: "https://default", Model: "default-model"},
		{Value: "c1|alt", Label: "alt", Endpoint: "https://second", Model: "alt"},
	}
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, pipe, nil, true, opts)
	h.now = func() time.Time { return now }
	page := func() string {
		rec := httptest.NewRecorder()
		h.Thread(rec, httptest.NewRequest(http.MethodGet, "/items/thread?id=pr1", nil))
		return rec.Body.String()
	}
	panel := func(choice string) {
		req := httptest.NewRequest(http.MethodPost, "/items/second-opinion", strings.NewReader("id=pr1&choice="+choice))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		h.SecondOpinion(httptest.NewRecorder(), req)
	}

	picker := func() string {
		out := page()
		so := out[strings.Index(out, `id="zz-so-model"`):]
		return so[:strings.Index(so, "</select>")]
	}
	if so := picker(); !strings.Contains(so, "c1|alt") || strings.Contains(so, "c0|default-model") {
		t.Fatalf("with no review yet the panel offers the alternative model, got:\n%s", so)
	}
	panel("c1|alt")
	if len(pipe.soModels) != 1 || pipe.soModels[0] != "alt" {
		t.Fatalf("first panel: want alt, got %v", pipe.soModels)
	}
	// alt's blind review lands and is the item's first review.
	ref := pipe.turns[0]
	if err := worklist.ApplyReply(context.Background(), store, "u1", "pr1", ref.Turn, worklist.Message{ID: "r1", Content: "review", Model: "alt", At: now}); err != nil {
		t.Fatal(err)
	}
	if so := picker(); strings.Contains(so, "c1|alt") || !strings.Contains(so, "c0|default-model") {
		t.Fatalf("after alt's first review the panel offers only the default model, got:\n%s", picker())
	}
	// A stale page still posting alt is resolved against the stored item.
	panel("c1|alt")
	if len(pipe.soModels) != 2 || pipe.soModels[1] != "default-model" {
		t.Fatalf("second panel: want default-model (never the first reviewer's), got %v", pipe.soModels)
	}
}

// A consensus synthesis whose dispatch failed stays retryable after the owner writes
// again, by the default model, under its own key, and is not offered while a review it
// weighs is hidden.
func TestRetryStrandedSynthesis(t *testing.T) {
	now := retryNow
	old := now.Add(-time.Hour)
	store := worklist.NewMemoryStore()
	store.Seed("u1", worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, Thread: []worklist.Message{
		{ID: "q1", Role: worklist.RoleUser, Kind: worklist.KindReviewRequest, Content: reviewPrompt, At: old},
		{ID: "r1", ReplyTo: "q1", Role: worklist.RoleAgent, Content: "first review", Model: "default", At: old},
		{ID: "p1", Role: worklist.RoleUser, Kind: worklist.KindReviewRequest, Content: independentReviewPrompt, At: old},
		{ID: "r2", ReplyTo: "p1", Role: worklist.RoleAgent, Content: "blind review", Model: "alt", At: old},
		{ID: "s1", Role: worklist.RoleUser, Kind: worklist.KindSynthesisRequest, Content: worklist.SynthesisPrompt, Reviews: []string{"q1", "p1"}, At: old},
		{ID: "d1", Role: worklist.RoleUser, Content: "and the tests?", At: old},
		{ID: "d2", ReplyTo: "d1", Role: worklist.RoleAgent, Content: "they pass", At: old},
	}})
	pipe := &recordPipeline{}
	h := retryHandler(store, pipe, &now)
	page := func() string {
		rec := httptest.NewRecorder()
		h.Thread(rec, httptest.NewRequest(http.MethodGet, "/items/thread?id=pr1", nil))
		return rec.Body.String()
	}
	retry := func() {
		req := httptest.NewRequest(http.MethodPost, "/items/thread/retry", strings.NewReader("id=pr1&turn=s1&choice=c1|alt"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		h.ThreadRetry(httptest.NewRecorder(), req)
	}

	// Hidden review: no offer, and a forged retry does nothing.
	hide := func(hidden bool) {
		_ = store.Update(context.Background(), "u1", func(cur []worklist.WorkItem) ([]worklist.WorkItem, error) {
			cur[0].Thread[1].Hidden = hidden
			return cur, nil
		})
	}
	hide(true)
	if out := page(); strings.Contains(out, `name="turn" value="s1"`) {
		t.Fatal("no synthesis retry while a review it weighs is hidden")
	}
	retry()
	if len(pipe.turns) != 0 {
		t.Fatalf("a synthesis missing a review must not be dispatched, got %v", pipe.turns)
	}

	hide(false)
	out := page()
	if !strings.Contains(out, `name="turn" value="s1"`) || !strings.Contains(out, "consensus on the reviews") {
		t.Fatalf("the stranded synthesis must be offered for retry, got:\n%s", out)
	}
	retry()
	if len(pipe.turns) != 1 || pipe.turns[0].Turn != "s1" || pipe.converseModels[0] != "" || len(pipe.secondOpinions) != 0 {
		t.Fatalf("want one default-model converse for s1, got turns %v models %v so %v", pipe.turns, pipe.converseModels, pipe.secondOpinions)
	}
	it := storedItem(t, store, "pr1")
	if i, _ := it.TurnIndex("s1"); !it.Thread[i].At.Equal(now) || len(it.Thread) != 7 {
		t.Fatalf("the retry re-stamps s1 and adds no turn, got %+v", it.Thread)
	}
	retry() // a double submit
	if len(pipe.turns) != 1 {
		t.Fatalf("a second retry within the window dispatches nothing, got %v", pipe.turns)
	}
}

// A hidden first review does not count: with model A's review hidden and B's blind
// review the only visible one, the next panel offers A, for the thread picker and the
// bulk action alike.
func TestReviewPanelExcludesTheFirstVisibleReviewer(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	store := worklist.NewMemoryStore()
	store.Seed("u1", worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1, State: "open"}, Thread: []worklist.Message{
		{ID: "q1", Role: worklist.RoleUser, Kind: worklist.KindReviewRequest, Content: reviewPrompt, At: old},
		{ID: "r1", ReplyTo: "q1", Role: worklist.RoleAgent, Content: "A's review", Model: "a", Hidden: true, At: old},
		{ID: "p1", Role: worklist.RoleUser, Kind: worklist.KindReviewRequest, Content: independentReviewPrompt, At: old},
		{ID: "r2", ReplyTo: "p1", Role: worklist.RoleAgent, Content: "B's review", Model: "b", At: old},
	}})
	pipe := &recordPipeline{}
	opts := []ModelOption{
		{Value: "c0|a", Label: "a", Endpoint: "https://a", Model: "a"},
		{Value: "c1|b", Label: "b", Endpoint: "https://b", Model: "b"},
	}
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, pipe, nil, true, opts)
	h.now = func() time.Time { return now }
	rec := httptest.NewRecorder()
	h.Thread(rec, httptest.NewRequest(http.MethodGet, "/items/thread?id=pr1", nil))
	out := rec.Body.String()
	so := out[strings.Index(out, `id="zz-so-model"`):]
	so = so[:strings.Index(so, "</select>")]
	if strings.Contains(so, "c1|b") || !strings.Contains(so, "c0|a") {
		t.Fatalf("the panel must exclude b, the first visible reviewer, got:\n%s", so)
	}
	req := httptest.NewRequest(http.MethodPost, "/items/second-opinion", strings.NewReader("id=pr1&choice=c1|b"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.SecondOpinion(httptest.NewRecorder(), req)
	if len(pipe.soModels) != 1 || pipe.soModels[0] != "a" {
		t.Fatalf("panel: want model a, got %v", pipe.soModels)
	}
}

// Reviews that are each valid but too large to read together: the page offers no
// Retry, says a smaller comparison is needed, and a forged retry dispatches nothing.
func TestSynthesisTooLargeOffersNoRetry(t *testing.T) {
	now := retryNow
	old := now.Add(-time.Hour)
	big := strings.Repeat("r", 750<<10)
	th := []worklist.Message{}
	var keys []string
	for i := range 6 {
		q := fmt.Sprintf("q%d", i)
		th = append(th,
			worklist.Message{ID: q, Role: worklist.RoleUser, Kind: worklist.KindReviewRequest, Content: reviewPrompt, At: old},
			worklist.Message{ID: "r" + q, ReplyTo: q, Role: worklist.RoleAgent, Content: big, Model: fmt.Sprint(i), At: old})
		keys = append(keys, q)
	}
	th = append(th, worklist.Message{ID: "s1", Role: worklist.RoleUser, Kind: worklist.KindSynthesisRequest, Content: worklist.SynthesisPrompt, Reviews: keys, At: old})
	store := worklist.NewMemoryStore()
	store.Seed("u1", worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, Thread: th})
	pipe := &recordPipeline{}
	h := retryHandler(store, pipe, &now)
	rec := httptest.NewRecorder()
	h.Thread(rec, httptest.NewRequest(http.MethodGet, "/items/thread?id=pr1", nil))
	out := rec.Body.String()
	if strings.Contains(out, `action="/items/thread/retry"`) {
		t.Fatal("no Retry for a comparison that cannot fit")
	}
	if !strings.Contains(out, "too large to read together") {
		t.Fatal("the page must say a smaller comparison is needed")
	}
	postRetry(h, url.Values{"id": {"pr1"}, "turn": {"s1"}})
	if len(pipe.turns) != 0 {
		t.Fatalf("a comparison that cannot fit must not be dispatched, got %v", pipe.turns)
	}
}

// A retry that shares a run still in flight (the gate submitted nothing) keeps that
// run's attempt on the turn, and the new time, so the run's own notice still ends the
// wait; Review all's retries do the same.
func TestRetrySharingARunKeepsItsAttempt(t *testing.T) {
	now := retryNow
	old := now.Add(-time.Hour)
	seed := func(store *worklist.MemoryStore) {
		store.Seed("u1", worklist.WorkItem{ID: "pr1", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1, State: "open"}, Thread: []worklist.Message{
			{ID: "t1", Role: worklist.RoleUser, Kind: worklist.KindReviewRequest, Content: reviewPrompt, Attempt: "a0", At: old},
		}})
	}
	check := func(t *testing.T, store *worklist.MemoryStore, pipe *recordPipeline) {
		t.Helper()
		if len(pipe.turns) != 1 || pipe.turns[0].Attempt == "" || pipe.turns[0].Attempt == "a0" {
			t.Fatalf("the retry dispatches a new attempt, got %+v", pipe.turns)
		}
		m := storedItem(t, store, "pr1").Thread[0]
		if m.Attempt != "a0" || !m.At.Equal(now) {
			t.Fatalf("a shared run keeps the turn's attempt (a0) and the retry's time, got %+v", m)
		}
		// The shared run's notice (dispatched as a0) ends the wait.
		_ = worklist.ApplyReply(context.Background(), store, "u1", "pr1", "t1", worklist.Message{ID: "n1", Notice: worklist.NoticeReconnect, Content: worklist.ReconnectNotice, Attempt: "a0", At: now})
		if storedItem(t, store, "pr1").ReplyPending(now, reviewStaleAfter) {
			t.Fatal("the shared run's notice must end the wait")
		}
	}
	t.Run("thread retry", func(t *testing.T) {
		store := worklist.NewMemoryStore()
		seed(store)
		pipe := &recordPipeline{shared: true}
		h := retryHandler(store, pipe, &now)
		postRetry(h, url.Values{"id": {"pr1"}, "turn": {"t1"}})
		check(t, store, pipe)
	})
	t.Run("review all", func(t *testing.T) {
		store := worklist.NewMemoryStore()
		seed(store)
		pipe := &recordPipeline{shared: true}
		h := retryHandler(store, pipe, &now)
		req := httptest.NewRequest(http.MethodPost, "/items/review-all", strings.NewReader(""))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		h.ReviewAllPRs(httptest.NewRecorder(), req)
		check(t, store, pipe)
	})
}
