package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/webui"
	"github.com/jackfrancis/gofer/internal/worklist"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// A handler behind the logging middleware can flush to the real connection: the
// recorder unwraps to the server's writer.
func TestLogRequestsPassesFlushThrough(t *testing.T) {
	flushed := make(chan error, 1)
	srv := httptest.NewServer(logRequests(discard, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "page")
		flushed <- http.NewResponseController(w).Flush()
	})))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if err := <-flushed; err != nil {
		t.Fatalf("flush through logRequests: %v", err)
	}
}

type oneUser struct{ user *session.User }

func (o oneUser) CurrentUser(*http.Request) *session.User { return o.user }

// Over a real connection, through the logging middleware, opening a thread delivers
// the page and records the read receipt.
func TestThreadReceiptOverRealConnection(t *testing.T) {
	item := worklist.WorkItem{
		ID: "github:o/r#1", Type: worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Number: 1, Repo: "o/r", Title: "A PR"},
		Thread: []worklist.Message{
			{ID: "u1", Role: worklist.RoleUser, Content: "review?", At: time.Now().Add(-time.Hour)},
			{ID: "r1", ReplyTo: "u1", Role: worklist.RoleAgent, Content: "Looks good.", At: time.Now().Add(-time.Minute)},
		},
	}
	store := worklist.NewMemoryStore()
	store.Seed("u1", item)
	web := webui.New(oneUser{&session.User{ID: "u1"}}, store, nil, nil, true, nil)
	srv := httptest.NewServer(logRequests(discard, http.HandlerFunc(web.Thread)))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/items/thread?id=" + url.QueryEscape(item.ID))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "Looks good.") {
		t.Fatalf("thread page not delivered: %s", body)
	}
	items, err := store.List(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if items[0].ThreadReadThrough != "r1" || items[0].HasUnreadReply() {
		t.Fatalf("a delivered thread must be marked read, cursor = %q", items[0].ThreadReadThrough)
	}
}

// The CSP lets the page load the GitHub avatar and still no other third-party image.
func TestSecurityHeadersAllowAvatarHost(t *testing.T) {
	rec := httptest.NewRecorder()
	securityHeaders(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "img-src 'self' data: https://avatars.githubusercontent.com;") {
		t.Fatalf("CSP img-src does not allow exactly the avatar host: %q", csp)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q, want no-referrer", got)
	}
}
