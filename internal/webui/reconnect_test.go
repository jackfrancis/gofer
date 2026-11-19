package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// reconnectProviders is the auth handler's view the page uses: its providers, and
// whether the user must sign in again.
type reconnectProviders struct{ required bool }

func (reconnectProviders) Providers() []string { return []string{"github"} }
func (p reconnectProviders) SignInRequired(context.Context, string, string) bool {
	return p.required
}

// The landing page and the thread ask the user to sign in again when gofer holds no
// usable credential for them, and only then.
func TestPageAsksToSignInAgain(t *testing.T) {
	store := worklist.NewMemoryStore()
	store.Seed("github:42", worklist.WorkItem{ID: "i1", Source: "github", Type: worklist.TypeIssue,
		GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1, Title: "An issue"}})
	user := &session.User{ID: "github:42", Provider: "github", Name: "Octo"}
	for _, required := range []bool{true, false} {
		h := New(fakeSessions{user}, store, &recordPipeline{}, reconnectProviders{required}, true, nil)
		for _, target := range []string{"/", "/items/thread?id=i1"} {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, target, nil)
			if target == "/" {
				h.Index(rec, req)
			} else {
				h.Thread(rec, req)
			}
			got := strings.Contains(rec.Body.String(), `href="/auth/github/login">Sign in again</a> to reconnect`)
			if got != required {
				t.Errorf("%s with sign-in required=%v: prompt shown=%v", target, required, got)
			}
		}
	}
}

// A reconnect notice renders as a system notice with a sign-in link, not as the
// assistant's answer.
func TestThreadRendersReconnectNotice(t *testing.T) {
	it := worklist.WorkItem{ID: "i1", Type: worklist.TypeIssue, GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1, Title: "An issue"}}
	it.Thread = []worklist.Message{
		{ID: "q", Role: worklist.RoleUser, Content: "why?"},
		{ID: "n", ReplyTo: "q", Role: worklist.RoleAgent, Notice: worklist.NoticeReconnect, Content: worklist.ReconnectNotice},
	}
	out := renderPage(t, pageData{View: "thread", User: &session.User{Provider: "github"}, Item: it, ConvEnabled: true})
	if !strings.Contains(out, `zz-notice--reconnect`) || !strings.Contains(out, `<a href="/auth/github/login">Sign in again</a>`) {
		t.Fatalf("no reconnect notice:\n%s", out)
	}
	if strings.Contains(out, `zz-msg zz-msg--agent`) {
		t.Error("the notice rendered as an assistant reply")
	}
}
