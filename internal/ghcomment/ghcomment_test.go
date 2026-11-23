package ghcomment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

const token = "gho_secret_token"

// fakeAPI serves the three calls the client makes, against a small set of repos.
type fakeAPI struct {
	srv        *httptest.Server
	posted     atomic.Int32
	postedTo   atomic.Value // string: the path of the last comment POST
	postedBody atomic.Value
	assignees  int // how many assignees each public repo has
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{assignees: 3}
	f.postedTo.Store("")
	f.postedBody.Store("")
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		switch {
		case r.URL.Path == "/repos/Org/Public" || r.URL.Path == "/repos/org/public":
			_, _ = w.Write([]byte(`{"full_name":"Org/Public","private":false}`))
		case r.URL.Path == "/repos/org/private":
			_, _ = w.Write([]byte(`{"full_name":"org/private","private":true}`))
		case r.URL.Path == "/repos/org/moved":
			w.Header().Set("Location", f.srv.URL+"/repositories/1")
			w.WriteHeader(http.StatusMovedPermanently)
		case r.URL.Path == "/repos/org/locked":
			_, _ = w.Write([]byte(`{"full_name":"org/locked","private":false}`))
		case r.URL.Path == "/repos/org/locked/issues/1/comments":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Unable to create comment because issue is locked."}`))
		case r.URL.Path == "/repos/Org/Public/issues/7/comments" && r.Method == http.MethodPost:
			var b struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			f.posted.Add(1)
			f.postedTo.Store(r.URL.Path)
			f.postedBody.Store(b.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"html_url":"https://github.com/Org/Public/issues/7#issuecomment-1"}`))
		case r.URL.Path == "/repos/Org/Public/assignees":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
			var out []map[string]string
			for i := (page - 1) * per; i < page*per && i < f.assignees; i++ {
				out = append(out, map[string]string{"login": fmt.Sprintf("user%d", i)})
			}
			if out == nil {
				out = []map[string]string{}
			}
			_ = json.NewEncoder(w).Encode(out)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) client() *Client { return New(f.srv.URL, f.srv.Client().Transport) }

func noToken(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), token) {
		t.Errorf("error carries the token: %v", err)
	}
}

// A comment goes to the canonical name the public check returned, with the body as
// given, and the answer is the new comment's link.
func TestCommentPostsToCanonicalRepo(t *testing.T) {
	f := newFakeAPI(t)
	link, err := f.client().Comment(context.Background(), token, "org/public", 7, "/assign @someone")
	if err != nil {
		t.Fatal(err)
	}
	if link != "https://github.com/Org/Public/issues/7#issuecomment-1" {
		t.Errorf("link = %q", link)
	}
	if got := f.postedTo.Load(); got != "/repos/Org/Public/issues/7/comments" {
		t.Errorf("posted to %q", got)
	}
	if got := f.postedBody.Load(); got != "/assign @someone" {
		t.Errorf("posted body %q", got)
	}
}

// Nothing is posted to a repository that is private, missing, redirected or misnamed.
func TestCommentRefusesAnythingButPublic(t *testing.T) {
	f := newFakeAPI(t)
	c := f.client()
	for repo, want := range map[string]error{
		"org/private": ErrNotPublic,
		"org/missing": ErrNotPublic,
		"org/../x":    ErrInvalidRepo,
		"org":         ErrInvalidRepo,
	} {
		_, err := c.Comment(context.Background(), token, repo, 1, "hi")
		if !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", repo, err, want)
		}
		noToken(t, err)
	}
	_, err := c.Comment(context.Background(), token, "org/moved", 1, "hi")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusMovedPermanently {
		t.Errorf("redirect: err = %v, want a 301 StatusError (not followed)", err)
	}
	if strings.Contains(fmt.Sprint(err), "/repositories/1") {
		t.Errorf("the redirect's Location reached the error: %v", err)
	}
	if f.posted.Load() != 0 {
		t.Errorf("posted %d comments", f.posted.Load())
	}
}

// GitHub's 401 is ErrUnauthorized; a refusal keeps GitHub's message.
func TestCommentErrors(t *testing.T) {
	f := newFakeAPI(t)
	c := f.client()
	_, err := c.Comment(context.Background(), "gho_dead", "org/public", 7, "hi")
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("dead token: %v", err)
	}
	_, err = c.Comment(context.Background(), token, "org/locked", 1, "hi")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusForbidden || !strings.Contains(se.Message, "locked") {
		t.Errorf("locked: %v", err)
	}
	noToken(t, err)
	if _, err := c.Comment(context.Background(), token, "org/public", 7, ""); err == nil {
		t.Error("posted an empty comment")
	}
	if _, err := c.Comment(context.Background(), token, "org/public", 0, "hi"); err == nil {
		t.Error("posted without an issue number")
	}
}

// Assignees are paged until a short page, and cut (complete=false) at the bound.
func TestAssignees(t *testing.T) {
	f := newFakeAPI(t)
	c := f.client()
	logins, complete, err := c.Assignees(context.Background(), token, "org/public")
	if err != nil || !complete || len(logins) != 3 {
		t.Fatalf("assignees = %v, %v, %v", logins, complete, err)
	}
	f.assignees = 150
	logins, complete, err = c.Assignees(context.Background(), token, "org/public")
	if err != nil || !complete || len(logins) != 150 {
		t.Fatalf("two pages: %d, %v, %v", len(logins), complete, err)
	}
	f.assignees = 100*MaxAssigneePages + 1
	logins, complete, err = c.Assignees(context.Background(), token, "org/public")
	if err != nil || complete || len(logins) != 100*MaxAssigneePages {
		t.Fatalf("over the bound: %d, %v, %v", len(logins), complete, err)
	}
	if _, _, err := c.Assignees(context.Background(), token, "org/private"); !errors.Is(err, ErrNotPublic) {
		t.Errorf("private repo assignees: %v", err)
	}
}
