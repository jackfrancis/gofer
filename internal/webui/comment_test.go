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
	return comment.Mentions{Users: []comment.Mention{{Login: "tallaxes", Repos: len(repos)}}, Selected: len(repos), Checked: len(repos)}, nil
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

// commentForm is the radar's comment form, with a key the radar would mint now.
func commentForm(h *Handler, body string, ids ...string) url.Values {
	return url.Values{"key": {h.newCommentKey(h.now())}, "content": {body}, "item": ids}
}

// sameSite is the fetch metadata a browser sends with the radar's own form.
var sameSite = []string{"Sec-Fetch-Site", "same-origin"}

func post(h *Handler, form url.Values) *httptest.ResponseRecorder {
	return postComment(h, form, sameSite...)
}

// radar renders the radar at target (the redirect a post answers with).
func radar(t *testing.T, h *Handler, target string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Index(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("radar %s = %d", target, rec.Code)
	}
	return rec.Body.String()
}

// posted posts form and returns the radar it is sent back to.
func posted(t *testing.T, h *Handler, form url.Values) string {
	t.Helper()
	rec := post(h, form)
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/?comment=") {
		t.Fatalf("post = %d %q\n%s", rec.Code, loc, rec.Body)
	}
	return radar(t, h, loc)
}

// formKey extracts the key the radar's comment form carries.
func formKey(t *testing.T, page string) string {
	t.Helper()
	const marker = `name="key" value="`
	i := strings.Index(page, marker)
	if i < 0 {
		t.Fatalf("no comment key on the radar:\n%s", page)
	}
	rest := page[i+len(marker):]
	return rest[:strings.Index(rest, `"`)]
}

// The radar offers the bulk comment only to a user who connected the write grant,
// and a connect link to one who has not. Its form posts in one step.
func TestRadarCommentControls(t *testing.T) {
	page := func(c *fakeCommenter) string {
		rec := httptest.NewRecorder()
		commentFixture(t, c).Index(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		return rec.Body.String()
	}
	connected := page(&fakeCommenter{enabled: true, connected: true})
	for _, want := range []string{
		`id="zz-bulk-comment"`, `action="/items/comment"`, `form="zz-bulk-comment"`, `name="key"`,
		`data-repo="a/one"`, `/static/comment.js`, `action="/auth/github-write/disconnect"`, `>Post</button>`,
	} {
		if !strings.Contains(connected, want) {
			t.Errorf("connected radar lacks %s", want)
		}
	}
	if strings.Contains(connected, "Preview") {
		t.Error("the radar still offers a preview step")
	}
	notConnected := page(&fakeCommenter{enabled: true})
	if !strings.Contains(notConnected, `href="/auth/github-write/login"`) || strings.Contains(notConnected, `data-comment-item`) || strings.Contains(notConnected, `name="key"`) {
		t.Error("an unconnected user should get the connect link and no checkboxes")
	}
	for name, out := range map[string]string{"disabled": page(&fakeCommenter{}), "no commenter": page(nil)} {
		if strings.Contains(out, "github-write") || strings.Contains(out, "data-comment-item") || strings.Contains(out, "comment.js") {
			t.Errorf("%s: the radar shows commenting", name)
		}
	}
}

// Post posts to the selected items on the radar at once, skipping the rest, and the
// radar it returns to shows the results.
func TestCommentPostsAndShowsResultsOnRadar(t *testing.T) {
	c := &fakeCommenter{enabled: true, connected: true}
	h := commentFixture(t, c)
	page := posted(t, h, commentForm(h, "/assign @tallaxes", "github:a/one#1", "github:b/two#2", "github:a/one#3", "github:b/two#4", "github:x/y#9"))
	if c.postCount() != 1 || len(c.posts[0]) != 2 || c.posts[0][0].ID != "github:a/one#1" || c.posts[0][1].ID != "github:b/two#2" {
		t.Fatalf("posts = %+v", c.posts)
	}
	for _, want := range []string{"Posted 2 of 2", "/assign @tallaxes", "view comment", "hidden", "closed", "gone", `id="zz-bulk-comment"`, "Dismiss"} {
		if !strings.Contains(page, want) {
			t.Errorf("radar lacks %q", want)
		}
	}
	if strings.Contains(page, " checked aria-label") {
		t.Error("a posted comment's items are checked again")
	}
}

// A resubmitted form shows its results again and posts nothing more, even with
// another comment or items in it.
func TestCommentFormPostsOnce(t *testing.T) {
	c := &fakeCommenter{enabled: true, connected: true}
	h := commentFixture(t, c)
	form := commentForm(h, "/lgtm", "github:a/one#1", "github:b/two#2")
	posted(t, h, form)
	form.Set("content", "/close")
	form.Add("item", "github:a/one#3")
	rec := post(h, form)
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || !strings.HasSuffix(loc, "&again=1") {
		t.Fatalf("resubmit = %d %q", rec.Code, loc)
	}
	page := radar(t, h, rec.Header().Get("Location"))
	if !strings.Contains(page, "Already posted: 2 of 2") || !strings.Contains(page, "/lgtm") {
		t.Errorf("resubmit radar:\n%s", page)
	}
	if c.postCount() != 1 {
		t.Errorf("a resubmit posted again: %d posts", c.postCount())
	}
	// Each radar render mints a fresh key, so the next comment posts.
	if k1, k2 := formKey(t, radar(t, h, "/")), formKey(t, radar(t, h, "/")); k1 == k2 || !validKey(h, k1) {
		t.Errorf("radar keys %q %q", k1, k2)
	}
}

func validKey(h *Handler, key string) bool {
	_, ok := h.commentKeyIssued(key, h.now())
	return ok
}

// Concurrent submissions of one form (a double click) post once.
func TestCommentDoubleSubmitPostsOnce(t *testing.T) {
	c := &fakeCommenter{enabled: true, connected: true}
	h := commentFixture(t, c)
	form := commentForm(h, "/lgtm", "github:a/one#1")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); post(h, form) }()
	}
	wg.Wait()
	if c.postCount() != 1 {
		t.Errorf("posts = %d", c.postCount())
	}
}

// A key gofer refuses (from before a restart, expired, forged, or missing, as from
// an old preview page's confirm form) posts nothing, says gofer cannot tell what an
// earlier submission did, and offers the comment again.
func TestCommentStaleFormPostsNothing(t *testing.T) {
	c := &fakeCommenter{enabled: true, connected: true}
	h := commentFixture(t, c)
	form := commentForm(h, "/lgtm", "github:a/one#1")

	restarted := commentFixture(t, c) // a new process: another epoch
	expired := commentFixture(t, c)
	expired.epoch = h.epoch
	expired.now = func() time.Time { return time.Now().Add(commentKeyTTL + time.Minute) }
	future := url.Values{"key": {h.newCommentKey(time.Now().Add(time.Hour))}, "content": {"/lgtm"}, "item": {"github:a/one#1"}}
	old := url.Values{"confirm": {"1"}, "nonce": {strings.Repeat("ab", 16)}}
	for name, tc := range map[string]struct {
		h    *Handler
		form url.Values
	}{
		"restart": {restarted, form}, "expired": {expired, form}, "future": {h, future}, "old confirm": {h, old},
		"malformed": {h, url.Values{"key": {"nope"}, "content": {"/lgtm"}, "item": {"github:a/one#1"}}},
	} {
		page := posted(t, tc.h, tc.form)
		if !strings.Contains(page, "cannot tell whether an earlier submission") || !strings.Contains(page, "check the items on GitHub") {
			t.Errorf("%s: radar:\n%s", name, page)
		}
	}
	if c.postCount() != 0 {
		t.Errorf("posted %d times", c.postCount())
	}
	page := posted(t, restarted, form)
	if !strings.Contains(page, "<details class=\"zz-compose\" data-compose open>") || !strings.Contains(page, ">/lgtm</textarea>") ||
		!strings.Contains(page, `value="github:a/one#1" form="zz-bulk-comment" data-comment-item data-repo="a/one" checked`) {
		t.Errorf("the refused comment is not offered again:\n%s", page)
	}
}

// A batch's results are its owner's.
func TestCommentResultsAreTheOwners(t *testing.T) {
	c := &fakeCommenter{enabled: true, connected: true}
	h := commentFixture(t, c)
	form := commentForm(h, "/lgtm", "github:a/one#1")
	rec := post(h, form)
	h.sessions = fakeSessions{&session.User{ID: "github:7", Provider: "github"}}
	if page := radar(t, h, rec.Header().Get("Location")); strings.Contains(page, "/lgtm") || strings.Contains(page, "Posted 1") {
		t.Error("another user saw the results")
	}
	// The same key from another user is that user's own, unused form: its batch is
	// their own (none of their items match), never the first user's results.
	if rec := post(h, form); strings.HasSuffix(rec.Header().Get("Location"), "&again=1") {
		t.Error("another user's submission was taken for a resubmit")
	}
	if bt := h.batches.lookup("github:7", form.Get("key"), h.now()); bt == nil || !strings.Contains(bt.view.Error, "None of the selected items") {
		t.Errorf("another user's batch = %+v", bt)
	}
	if c.postCount() != 1 {
		t.Errorf("posts = %d", c.postCount())
	}
}

// The comment's line breaks survive to the post.
func TestCommentKeepsLineBreaks(t *testing.T) {
	c := &fakeCommenter{enabled: true, connected: true}
	h := commentFixture(t, c)
	post(h, commentForm(h, "\n/assign @a\r\n/cc @b", "github:a/one#1"))
	if c.postCount() != 1 || c.bodies[0] != "\n/assign @a\n/cc @b" {
		t.Errorf("posted %q", c.bodies)
	}
}

// Review P1: Post works under gofer's referrer policy, which makes a form POST send
// "Origin: null"; the radar relaxes that policy for itself when it carries the form.
func TestCommentPostUnderNoReferrer(t *testing.T) {
	c := &fakeCommenter{enabled: true, connected: true}
	h := commentFixture(t, c)
	rec := httptest.NewRecorder()
	h.Index(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rec.Header().Get("Referrer-Policy"); got != "same-origin" {
		t.Errorf("radar Referrer-Policy = %q, want same-origin", got)
	}
	if rec := postComment(h, commentForm(h, "/lgtm", "github:a/one#1"), "Sec-Fetch-Site", "same-origin", "Origin", "null"); rec.Code != http.StatusSeeOther {
		t.Fatalf("same-origin post with Origin: null = %d", rec.Code)
	}
	if c.postCount() != 1 {
		t.Errorf("posts = %d", c.postCount())
	}
}

// A cross-site post is refused before anything is posted.
func TestCommentRefusesCrossSite(t *testing.T) {
	c := &fakeCommenter{enabled: true, connected: true}
	h := commentFixture(t, c)
	for _, hdr := range [][]string{
		{"Sec-Fetch-Site", "cross-site"},
		{"Sec-Fetch-Site", "same-site"},
		{"Sec-Fetch-Site", "none"},
		{"Origin", "https://evil.example"},
		{"Origin", "null"}, // without fetch metadata, an opaque origin proves nothing
	} {
		if rec := postComment(h, commentForm(h, "/lgtm", "github:a/one#1"), hdr...); rec.Code != http.StatusForbidden {
			t.Errorf("%v: status %d, want 403", hdr, rec.Code)
		}
	}
	if c.postCount() != 0 {
		t.Error("a cross-site post posted")
	}
}

// A form gofer cannot accept posts nothing; the radar says why and offers the
// comment and selection again.
func TestCommentRefusesBadForms(t *testing.T) {
	c := &fakeCommenter{enabled: true, connected: true}
	h := commentFixture(t, c)
	many := make([]string, comment.MaxTargets+1)
	for i := range many {
		many[i] = fmt.Sprintf("github:a/one#%d", i+100)
	}
	cases := map[string]struct {
		form url.Values
		want string
	}{
		"empty comment": {commentForm(h, "  \n ", "github:a/one#1"), "Write a comment first."},
		"no items":      {commentForm(h, "/lgtm"), "Select at least one item"},
		"too many":      {commentForm(h, "/lgtm", many...), "at most 50 items"},
		"too long":      {commentForm(h, strings.Repeat("x", maxMessageBytes+1), "github:a/one#1"), "too long"},
		"none eligible": {commentForm(h, "/lgtm", "github:a/one#3"), "None of the selected items"},
	}
	for name, tc := range cases {
		page := posted(t, h, tc.form)
		if !strings.Contains(page, tc.want) {
			t.Errorf("%s: radar lacks %q", name, tc.want)
		}
	}
	page := posted(t, h, commentForm(h, "/lgtm", "github:a/one#3", "github:a/one#1x"))
	if !strings.Contains(page, ">/lgtm</textarea>") || !strings.Contains(page, "data-compose open") {
		t.Errorf("the refused comment is not offered again:\n%s", page)
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

// A refused attempt keeps the selection checked on the radar it returns to.
func TestCommentRefusalKeepsSelection(t *testing.T) {
	c := &fakeCommenter{enabled: true, connected: true, postErr: comment.ErrNotConnected}
	h := commentFixture(t, c)
	page := posted(t, h, commentForm(h, "/lgtm", "github:a/one#1"))
	if !strings.Contains(page, "Connect commenting") {
		t.Errorf("radar:\n%s", page)
	}
	if !strings.Contains(page, `data-repo="a/one" checked`) || strings.Contains(page, `data-repo="b/two" checked`) {
		t.Error("the selection is not kept as it was")
	}
}

// Held batches are bounded: one that posted nothing is dropped first, and gofer
// refuses to post rather than forget a used key.
func TestCommentBatchesAreBounded(t *testing.T) {
	var b batches
	now := time.Now()
	until := now.Add(time.Hour)
	failed, mine, _ := b.claim("u", "failed", until, now)
	failed.view = commentView{Error: "x"}
	close(failed.done)
	if !mine {
		t.Fatal("first claim not mine")
	}
	for i := 1; i < maxRecords; i++ {
		bt, _, ok := b.claim("u", fmt.Sprint("k", i), until, now.Add(time.Duration(i)))
		if !ok {
			t.Fatalf("claim %d refused", i)
		}
		bt.view.Posted = 1
		close(bt.done)
	}
	if _, _, ok := b.claim("u", "next", until, now.Add(time.Hour/2)); !ok {
		t.Fatal("the batch that posted nothing was not dropped for a new one")
	}
	if b.lookup("u", "failed", now) != nil || b.lookup("u", "k1", now) == nil {
		t.Error("dropped the wrong batch")
	}
	if _, _, ok := b.claim("u", "more", until, now.Add(time.Hour/2)); ok {
		t.Error("a used key was forgotten to make room")
	}
	if _, _, ok := b.claim("u", "later", until, until.Add(time.Second)); !ok {
		t.Error("expired batches were not dropped")
	}
}

// A user who has not connected the write grant is sent to connect it.
func TestCommentNeedsConnection(t *testing.T) {
	h := commentFixture(t, &fakeCommenter{enabled: true})
	rec := post(h, commentForm(h, "/lgtm", "github:a/one#1"))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?commenting=connect" {
		t.Fatalf("status %d location %q", rec.Code, rec.Header().Get("Location"))
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

// A result whose answer was lost is shown as unknown, not as failed or posted.
func TestCommentResultsShowUnknown(t *testing.T) {
	tgt := comment.Target{Repo: "a/one", Number: 1, Title: "t", URL: "https://github.com/a/one/pull/1"}
	out := renderPage(t, pageData{View: "worklist", CommentEnabled: true, CommentConnected: true, Comment: &commentView{
		Body: "/lgtm", Targets: []comment.Target{tgt, tgt}, Posted: 1, Unknown: 1,
		Results: []comment.Result{
			{Target: tgt, Posted: true},
			{Target: tgt, Unknown: true, Error: "Unknown: GitHub's answer was lost"},
		},
	}})
	if !strings.Contains(out, "Posted 1 of 2 (1 unknown)") || !strings.Contains(out, "zz-target-unknown") {
		t.Errorf("results:\n%s", out)
	}
}

// A batch still posting when the radar is reached says so.
func TestCommentResultsWhileRunning(t *testing.T) {
	h := commentFixture(t, &fakeCommenter{enabled: true, connected: true})
	key := h.newCommentKey(h.now())
	if _, _, ok := h.batches.claim("github:42", key, h.now().Add(time.Hour), h.now()); !ok {
		t.Fatal("claim refused")
	}
	if page := radar(t, h, "/?comment="+key); !strings.Contains(page, "still posting") {
		t.Errorf("radar:\n%s", page)
	}
	if page := radar(t, h, "/?comment="+h.newCommentKey(h.now())); !strings.Contains(page, "no longer holds the results") {
		t.Errorf("radar for unknown results:\n%s", page)
	}
}

// Review P2 (third round): the owner sees a used form's results without a live
// grant; only starting a batch needs one, and a refused start uses nothing.
func TestCommentReceiptNeedsNoGrant(t *testing.T) {
	c := &fakeCommenter{enabled: true, connected: true}
	h := commentFixture(t, c)
	done := commentForm(h, "/lgtm", "github:a/one#1")
	pending := commentForm(h, "/hold", "github:b/two#2")
	posted(t, h, done)

	c.connected = false // the grant died, or was disconnected
	rec := post(h, done)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/?comment=") {
		t.Fatalf("resubmit without a grant = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = post(h, pending)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?commenting=connect" {
		t.Fatalf("new batch without a grant = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	c.connected = true
	post(h, pending)
	if c.postCount() != 2 {
		t.Errorf("posts = %d; the refused start should have left its form to post", c.postCount())
	}
}
