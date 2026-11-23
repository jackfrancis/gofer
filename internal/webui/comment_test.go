package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/comment"
	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/worklist"
)

type fakeCommenter struct {
	enabled, connected bool
	postErr            error

	mu       sync.Mutex
	posts    [][]comment.Target
	bodies   []string
	mentions []string // the repos the last Mentions call named
}

func (f *fakeCommenter) State(context.Context, string) (bool, bool) { return f.enabled, f.connected }

func (f *fakeCommenter) Post(_ context.Context, _ string, targets []comment.Target, body string) ([]comment.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.postErr != nil {
		return nil, f.postErr
	}
	f.posts = append(f.posts, targets)
	f.bodies = append(f.bodies, body)
	out := make([]comment.Result, len(targets))
	for i, t := range targets {
		out[i] = comment.Result{Target: t, Posted: true, CommentURL: t.URL + "#c"}
	}
	return out, nil
}

func (f *fakeCommenter) Mentions(_ context.Context, _ string, repos []string) (comment.Mentions, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mentions = repos
	return comment.Mentions{Users: []comment.Mention{{Login: "tallaxes", Repos: len(repos)}}, Repos: len(repos)}, nil
}

func (f *fakeCommenter) postCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

// commentFixture is a radar with two open items in different repos, plus a hidden
// one and a closed one, for a GitHub user.
func commentFixture(t *testing.T, c *fakeCommenter) *Handler {
	t.Helper()
	store := worklist.NewMemoryStore()
	item := func(repo string, n int) worklist.WorkItem {
		return worklist.WorkItem{
			ID: fmt.Sprintf("github:%s#%d", repo, n), Source: "github", Type: worklist.TypePullRequest,
			GitHub: worklist.GitHubRef{Repo: repo, Number: n, Title: fmt.Sprintf("title-%d", n), URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, n), State: "open"},
			Meta:   worklist.Metadata{Origin: worklist.OriginUser, Rank: 0.5},
		}
	}
	hidden := item("a/one", 3)
	hidden.Meta.HiddenAt = time.Now()
	closed := item("b/two", 4)
	closed.GitHub.State = "closed"
	closed.Meta.CompletedAt = time.Now()
	store.Seed("github:42", item("a/one", 1), item("b/two", 2), hidden, closed)
	h := New(fakeSessions{&session.User{ID: "github:42", Provider: "github", Login: "octocat"}}, store, &recordPipeline{}, reconnectProviders{false}, false, nil)
	if c != nil {
		h.UseCommenter(c)
	}
	return h
}

func postComment(h *Handler, form url.Values, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/items/comment", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.Comment(rec, req)
	return rec
}

func commentForm(body string, ids ...string) url.Values {
	return url.Values{"content": {body}, "item": ids}
}

// previewNonce extracts the nonce the preview rendered.
func previewNonce(t *testing.T, page string) string {
	t.Helper()
	const marker = `name="nonce" value="`
	i := strings.Index(page, marker)
	if i < 0 {
		t.Fatalf("no nonce in the preview:\n%s", page)
	}
	rest := page[i+len(marker):]
	return rest[:strings.Index(rest, `"`)]
}

// The radar offers the bulk comment only to a user who connected the write grant,
// and a connect link to one who has not.
func TestRadarCommentControls(t *testing.T) {
	page := func(c *fakeCommenter) string {
		rec := httptest.NewRecorder()
		commentFixture(t, c).Index(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		return rec.Body.String()
	}
	connected := page(&fakeCommenter{enabled: true, connected: true})
	for _, want := range []string{
		`id="zz-bulk-comment"`, `action="/items/comment"`, `form="zz-bulk-comment"`,
		`data-repo="a/one"`, `/static/comment.js`, `action="/auth/github-write/disconnect"`,
	} {
		if !strings.Contains(connected, want) {
			t.Errorf("connected radar lacks %s", want)
		}
	}
	notConnected := page(&fakeCommenter{enabled: true})
	if !strings.Contains(notConnected, `href="/auth/github-write/login"`) || strings.Contains(notConnected, `data-comment-item`) {
		t.Error("an unconnected user should get the connect link and no checkboxes")
	}
	for name, out := range map[string]string{"disabled": page(&fakeCommenter{}), "no commenter": page(nil)} {
		if strings.Contains(out, "github-write") || strings.Contains(out, "data-comment-item") || strings.Contains(out, "comment.js") {
			t.Errorf("%s: the radar shows commenting", name)
		}
	}
}

// The preview shows the comment and its targets, skips items off the radar, and
// posts nothing.
func TestCommentPreviewPostsNothing(t *testing.T) {
	c := &fakeCommenter{enabled: true, connected: true}
	h := commentFixture(t, c)
	rec := postComment(h, commentForm("/assign @tallaxes", "github:a/one#1", "github:b/two#2", "github:a/one#3", "github:b/two#4", "github:x/y#9"))
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d", rec.Code)
	}
	page := rec.Body.String()
	for _, want := range []string{"Post this comment?", "/assign @tallaxes", "a/one #1", "b/two #2", "Post to 2 items", "hidden", "closed", "gone"} {
		if !strings.Contains(page, want) {
			t.Errorf("preview lacks %q", want)
		}
	}
	if strings.Contains(page, `name="item" value="github:a/one#3"`) {
		t.Error("the confirm form carries a hidden item")
	}
	if c.postCount() != 0 {
		t.Error("the preview posted")
	}
}

// Confirm posts once; resubmitting the same confirmation shows its results again
// and posts nothing more.
func TestCommentConfirmPostsOnce(t *testing.T) {
	c := &fakeCommenter{enabled: true, connected: true}
	h := commentFixture(t, c)
	nonce := previewNonce(t, postComment(h, commentForm("/lgtm", "github:a/one#1", "github:b/two#2")).Body.String())

	form := commentForm("/lgtm", "github:a/one#1", "github:b/two#2")
	form.Set("confirm", "1")
	form.Set("nonce", nonce)
	rec := postComment(h, form, "Sec-Fetch-Site", "same-origin", "Origin", "http://example.com")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Posted 2 of 2") {
		t.Fatalf("confirm = %d\n%s", rec.Code, rec.Body)
	}
	if c.postCount() != 1 || len(c.posts[0]) != 2 || c.bodies[0] != "/lgtm" {
		t.Fatalf("posts = %+v bodies = %v", c.posts, c.bodies)
	}
	again := postComment(h, form, "Sec-Fetch-Site", "same-origin")
	if !strings.Contains(again.Body.String(), "Already posted") {
		t.Errorf("resubmit page:\n%s", again.Body)
	}
	if c.postCount() != 1 {
		t.Errorf("a resubmit posted again: %d posts", c.postCount())
	}
}

// A confirm keeps the comment's line breaks through the preview's form.
func TestCommentKeepsLineBreaks(t *testing.T) {
	c := &fakeCommenter{enabled: true, connected: true}
	h := commentFixture(t, c)
	body := "\n/assign @a\r\n/cc @b"
	page := postComment(h, commentForm(body, "github:a/one#1")).Body.String()
	// The preview's hidden textarea starts with a newline the parser drops, then the
	// comment as normalised.
	if !strings.Contains(page, "<textarea name=\"content\" hidden>\n\n/assign @a\n/cc @b</textarea>") {
		t.Errorf("preview textarea does not carry the comment intact:\n%s", page)
	}
}

// A cross-site confirm is refused before anything is posted.
func TestCommentConfirmRefusesCrossSite(t *testing.T) {
	c := &fakeCommenter{enabled: true, connected: true}
	h := commentFixture(t, c)
	form := commentForm("/lgtm", "github:a/one#1")
	form.Set("confirm", "1")
	form.Set("nonce", newNonce())
	for _, hdr := range [][]string{
		{"Sec-Fetch-Site", "cross-site"},
		{"Sec-Fetch-Site", "same-site"},
		{"Origin", "https://evil.example"},
	} {
		if rec := postComment(h, form, hdr...); rec.Code != http.StatusForbidden {
			t.Errorf("%v: status %d, want 403", hdr, rec.Code)
		}
	}
	if c.postCount() != 0 {
		t.Error("a cross-site confirm posted")
	}
}

// A form gofer cannot accept posts nothing.
func TestCommentRefusesBadForms(t *testing.T) {
	c := &fakeCommenter{enabled: true, connected: true}
	h := commentFixture(t, c)
	many := make([]string, comment.MaxTargets+1)
	for i := range many {
		many[i] = fmt.Sprintf("github:a/one#%d", i+100)
	}
	confirm := func(f url.Values) url.Values { f.Set("confirm", "1"); f.Set("nonce", newNonce()); return f }
	cases := map[string]url.Values{
		"empty comment": confirm(commentForm("  \n ", "github:a/one#1")),
		"no items":      confirm(commentForm("/lgtm")),
		"too many":      confirm(commentForm("/lgtm", many...)),
		"too long":      confirm(commentForm(strings.Repeat("x", maxMessageBytes+1), "github:a/one#1")),
		"bad nonce":     {"content": {"/lgtm"}, "item": {"github:a/one#1"}, "confirm": {"1"}, "nonce": {"nope"}},
	}
	for name, form := range cases {
		if rec := postComment(h, form); rec.Code < 400 {
			t.Errorf("%s: status %d", name, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/items/comment", strings.NewReader("--x--"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	rec := httptest.NewRecorder()
	h.Comment(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("multipart: status %d", rec.Code)
	}
	if c.postCount() != 0 {
		t.Errorf("posted %d times", c.postCount())
	}
}

// A user who has not connected the write grant is sent to connect it.
func TestCommentNeedsConnection(t *testing.T) {
	h := commentFixture(t, &fakeCommenter{enabled: true})
	rec := postComment(h, commentForm("/lgtm", "github:a/one#1"))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?commenting=connect" {
		t.Fatalf("status %d location %q", rec.Code, rec.Header().Get("Location"))
	}
}

// A batch that posted nothing (no grant) frees its confirmation, so the same
// confirmation works once the user reconnects.
func TestCommentNotConnectedFreesNonce(t *testing.T) {
	c := &fakeCommenter{enabled: true, connected: true, postErr: comment.ErrNotConnected}
	h := commentFixture(t, c)
	form := commentForm("/lgtm", "github:a/one#1")
	form.Set("confirm", "1")
	form.Set("nonce", newNonce())
	rec := postComment(h, form)
	if !strings.Contains(rec.Body.String(), "Connect commenting") {
		t.Errorf("page:\n%s", rec.Body)
	}
	c.mu.Lock()
	c.postErr = nil
	c.mu.Unlock()
	rec = postComment(h, form)
	if c.postCount() != 1 || strings.Contains(rec.Body.String(), "Already posted") {
		t.Errorf("retry after reconnecting: %d posts\n%s", c.postCount(), rec.Body)
	}
}

// Suggestions are asked only for well-formed repository names.
func TestMentionsEndpoint(t *testing.T) {
	c := &fakeCommenter{enabled: true, connected: true}
	h := commentFixture(t, c)
	req := httptest.NewRequest(http.MethodGet, "/items/mentions?repo=a/one&repo=b/two&repo=../x&repo=bad", nil)
	rec := httptest.NewRecorder()
	h.Mentions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var m comment.Mentions
	if err := json.NewDecoder(rec.Body).Decode(&m); err != nil || len(m.Users) != 1 || m.Users[0].Login != "tallaxes" {
		t.Fatalf("answer %+v, %v", m, err)
	}
	if len(c.mentions) != 2 || c.mentions[0] != "a/one" || c.mentions[1] != "b/two" {
		t.Errorf("asked for %v", c.mentions)
	}
	off := httptest.NewRecorder()
	commentFixture(t, &fakeCommenter{}).Mentions(off, httptest.NewRequest(http.MethodGet, "/items/mentions?repo=a/one", nil))
	if off.Code != http.StatusNotFound {
		t.Errorf("mentions with commenting off: %d", off.Code)
	}
}

// The refresh watcher never reloads away a selection.
func TestReloadKeepsSelection(t *testing.T) {
	b, err := staticFS.ReadFile("static/reload.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "[data-comment-item]:checked") {
		t.Error("reload.js does not wait while items are checked")
	}
}
