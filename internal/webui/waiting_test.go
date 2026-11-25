package webui

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/worklist"
)

func waitingPR(id string, since time.Time) worklist.WorkItem {
	return worklist.WorkItem{
		ID: id, Type: worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Repo: "o/r", Number: 9, Title: "Waiting PR", Author: "RadekCap"},
		Signals: worklist.Signals{
			AwaitingOthersSince: since,
			LastWord:            worklist.NewLastWord("review:1", since, true, []string{"RadekCap"}, "@RadekCap are you willing to downscope?"),
		},
	}
}

// The radar lists items waiting on someone else in their own section after the rest,
// each saying whom it waits on.
func TestRadarListsWaitingAfterTheRest(t *testing.T) {
	now := time.Now()
	store := worklist.NewMemoryStore()
	store.Seed("u1",
		worklist.WorkItem{ID: "mine", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1, Title: "My turn PR"}},
		waitingPR("waiting", now.Add(-4*24*time.Hour)),
	)
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, &recordPipeline{}, reconnectProviders{}, true, nil)
	h.now = func() time.Time { return now }
	rec := httptest.NewRecorder()
	h.Index(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	out := rec.Body.String()

	mine, section, waiting := strings.Index(out, "My turn PR"), strings.Index(out, "Waiting on others"), strings.Index(out, "Waiting PR")
	if mine < 0 || section < 0 || waiting < 0 || !(mine < section && section < waiting) {
		t.Fatalf("want the item that needs me, then the Waiting section, then the waiting item:\n%s", out)
	}
	if !strings.Contains(out, "Waiting on @RadekCap · 4d") || !strings.Contains(out, "You reviewed it on") {
		t.Fatalf("waiting badge missing:\n%s", out[section:])
	}
	if !strings.Contains(out, "1 item on your radar") {
		t.Fatal("the radar count should cover only the items that need the user")
	}
}

// Review all PRs never takes a PR that is waiting on someone else.
func TestReviewAllSkipsWaiting(t *testing.T) {
	now := time.Now()
	store := worklist.NewMemoryStore()
	store.Seed("u1",
		worklist.WorkItem{ID: "mine", Type: worklist.TypePullRequest},
		waitingPR("waiting", now.Add(-time.Hour)),
	)
	pipe := &recordPipeline{}
	h := &Handler{sessions: fakeSessions{&session.User{ID: "u1"}}, store: store, pipeline: pipe, convEnabled: true, now: func() time.Time { return now }}
	h.ReviewAllPRs(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/items/review-all", nil))
	if len(pipe.reviewed) != 1 || pipe.reviewed[0] != "mine" {
		t.Fatalf("reviewed %v, want only the PR that is the user's turn", pipe.reviewed)
	}
}

// A selected project whose only item is waiting does not read as empty.
func TestRadarOnlyWaitingInTheSelectedProject(t *testing.T) {
	store := worklist.NewMemoryStore()
	store.Seed("u1", waitingPR("wait", time.Now()), worklist.WorkItem{ID: "active", GitHub: worklist.GitHubRef{Repo: "else/where"}})
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, &recordPipeline{}, reconnectProviders{}, true, nil)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: projectsCookie, Value: "u=u1&p=o%2Fr"})
	rec := httptest.NewRecorder()
	h.Index(rec, r)
	out := rec.Body.String()
	if !strings.Contains(out, "Waiting PR") {
		t.Fatal("fixture: the waiting PR should be shown")
	}
	if strings.Contains(out, "No items in the selected projects.") || !strings.Contains(out, "Nothing in the selected projects needs you right now.") {
		t.Fatalf("empty state wrong:\n%s", out)
	}
}

// A waiting item is on the radar, so it can be selected for a bulk comment (a nudge
// to whoever it waits on), and the comment reaches it.
func TestWaitingItemTakesABulkComment(t *testing.T) {
	store := worklist.NewMemoryStore()
	wait := waitingPR("github:o/r#9", time.Now())
	wait.Source = "github"
	store.Seed("github:42", wait)
	h := New(fakeSessions{&session.User{ID: "github:42", Provider: "github", Login: "octocat"}}, store, &recordPipeline{}, reconnectProviders{false}, false, nil)
	h.UseCommenter(&fakeCommenter{enabled: true, connected: true})
	rec := httptest.NewRecorder()
	h.Index(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	page := rec.Body.String()
	section := page[strings.Index(page, "zz-waiting-head"):]
	if !strings.Contains(section, `value="`+template.HTMLEscapeString(selectionValue(wait))+`" form="zz-bulk-comment"`) {
		t.Fatal("the waiting item has no bulk-comment checkbox")
	}
	targets, skipped, err := h.commentTargets(context.Background(), "github:42", []string{wait.ID})
	if err != nil || len(targets) != 1 || len(skipped) != 0 {
		t.Errorf("targets %v, skipped %v, err %v", targets, skipped, err)
	}
}
