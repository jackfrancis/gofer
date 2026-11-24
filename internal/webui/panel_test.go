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
