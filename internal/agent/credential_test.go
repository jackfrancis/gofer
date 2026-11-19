package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackfrancis/gofer/internal/github"
	"github.com/jackfrancis/gofer/internal/github/githubtest"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// fakePlane serves GET /agent/credential the way gofer's broker does: a first vend,
// and a scripted answer to rejected=<version>.
type fakePlane struct {
	srv *httptest.Server

	mu       sync.Mutex
	first    planeAnswer
	onReject planeAnswer
	rejected []string // the rejected= values received
	vends    int
}

type planeAnswer struct {
	status  int // 0 = 200
	token   string
	version string
}

func newFakePlane(t *testing.T, first, onReject planeAnswer) *fakePlane {
	t.Helper()
	f := &fakePlane{first: first, onReject: onReject}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/credential" || r.Header.Get("Authorization") != "Bearer run-cred" {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		f.vends++
		a := f.first
		if rej := r.URL.Query().Get("rejected"); rej != "" {
			f.rejected = append(f.rejected, rej)
			a = f.onReject
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch a.status {
		case 0:
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": a.token, "token_type": "bearer", "credential_version": a.version})
		case signInRequiredStatus:
			w.WriteHeader(a.status)
			_, _ = w.Write([]byte(`{"error":"sign-in required","code":"sign_in_required"}`))
		default:
			w.WriteHeader(a.status)
			_, _ = w.Write([]byte(`{"error":"could not load credential"}`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePlane) vendor() *PlaneClient {
	return NewPlaneClient(f.srv.URL, "run-cred", f.srv.Client())
}

func (f *fakePlane) rejections() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.rejected...)
}

// fakeAPI is a GitHub API that accepts only the tokens in good, and counts calls.
type fakeAPI struct {
	srv   *httptest.Server
	good  map[string]bool
	calls atomic.Int32
	// unauthorized paths answer 401 whatever the token (a revocation that lands
	// mid-turn).
	unauthorized func(path string) bool
	auth         []string
	mu           sync.Mutex
}

func newFakeAPI(t *testing.T, good ...string) *fakeAPI {
	t.Helper()
	f := &fakeAPI{good: map[string]bool{}}
	for _, g := range good {
		f.good[g] = true
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		f.auth = append(f.auth, tok)
		f.mu.Unlock()
		if !f.good[tok] || (f.unauthorized != nil && f.unauthorized(r.URL.Path)) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"octocat"}`))
		case r.URL.Path == "/repos/o/r":
			_, _ = w.Write([]byte(`{"id":1,"full_name":"o/r","private":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// A 401 re-vends once, naming the rejected version, and the request is retried with
// the new token; the caller's stale token string is swapped on later calls too.
func TestHolderReVendsOnceOn401(t *testing.T) {
	plane := newFakePlane(t, planeAnswer{token: "tok-old", version: "v1"}, planeAnswer{token: "tok-new", version: "v2"})
	api := newFakeAPI(t, "tok-new")
	h, err := newTokenHolder(context.Background(), plane.vendor(), "github", api.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	gh := github.NewClient(h.client(nil), api.srv.URL)
	stale := h.Token()
	if login, err := gh.Login(context.Background(), stale); err != nil || login != "octocat" {
		t.Fatalf("login = %q, %v", login, err)
	}
	if got := plane.rejections(); len(got) != 1 || got[0] != "v1" {
		t.Fatalf("rejections = %v, want [v1]", got)
	}
	// The caller still passes the token it was first given; the holder swaps it.
	if _, err := gh.Login(context.Background(), stale); err != nil {
		t.Fatalf("second call with the stale token string: %v", err)
	}
	if h.Token() != "tok-new" || len(plane.rejections()) != 1 {
		t.Errorf("token %q, rejections %v", h.Token(), plane.rejections())
	}

	// The new token is rejected as well: no second re-vend in this run.
	delete(api.good, "tok-new")
	if _, err := gh.Login(context.Background(), stale); err == nil {
		t.Fatal("a 401 after the one re-vend should stand")
	}
	if len(plane.rejections()) != 1 {
		t.Errorf("re-vended again: %v", plane.rejections())
	}
}

// When the broker confirms the token is valid it returns the same token, and the 401
// stands without a retry.
func TestHolderSameTokenStands(t *testing.T) {
	plane := newFakePlane(t, planeAnswer{token: "tok", version: "v1"}, planeAnswer{token: "tok", version: "v1"})
	api := newFakeAPI(t) // rejects everything
	h, _ := newTokenHolder(context.Background(), plane.vendor(), "github", api.srv.URL)
	gh := github.NewClient(h.client(nil), api.srv.URL)
	if _, err := gh.Login(context.Background(), h.Token()); err == nil {
		t.Fatal("want the 401 to stand")
	}
	if api.calls.Load() != 1 || len(plane.rejections()) != 1 || h.Reconnect() != nil {
		t.Errorf("calls=%d rejections=%v reconnect=%v", api.calls.Load(), plane.rejections(), h.Reconnect())
	}
}

// "Sign-in required" on the re-vend is a ReconnectError, and every later request
// through the holder fails with it without reaching GitHub.
func TestHolderSignInRequiredIsSticky(t *testing.T) {
	plane := newFakePlane(t, planeAnswer{token: "tok", version: "v1"}, planeAnswer{status: signInRequiredStatus})
	api := newFakeAPI(t)
	h, _ := newTokenHolder(context.Background(), plane.vendor(), "github", api.srv.URL)
	gh := github.NewClient(h.client(nil), api.srv.URL)
	_, err := gh.Login(context.Background(), h.Token())
	var re *ReconnectError
	if !errors.As(err, &re) || !errors.Is(err, worklist.ErrSignInRequired) {
		t.Fatalf("err = %v, want a ReconnectError", err)
	}
	if !isSignInRequired(h.Reconnect()) {
		t.Fatal("holder did not record the reconnect")
	}
	before := api.calls.Load()
	if _, err := gh.Login(context.Background(), h.Token()); !isSignInRequired(err) {
		t.Fatalf("later call: %v", err)
	}
	if api.calls.Load() != before {
		t.Error("a request reached GitHub after sign-in was required")
	}
}

// The holder touches only requests that carry one of its tokens to the API host: it
// never adds a credential to anything else.
func TestHolderLeavesOtherRequestsAlone(t *testing.T) {
	plane := newFakePlane(t, planeAnswer{token: "tok", version: "v1"}, planeAnswer{})
	api := newFakeAPI(t, "tok")
	other := newFakeAPI(t, "tok")
	h, _ := newTokenHolder(context.Background(), plane.vendor(), "github", api.srv.URL)
	c := h.client(nil)
	for _, target := range []string{api.srv.URL + "/user", other.srv.URL + "/user"} {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	for _, f := range []*fakeAPI{api, other} {
		if len(f.auth) != 1 || f.auth[0] != "" {
			t.Errorf("a credential was added: %q", f.auth)
		}
	}
	if len(plane.rejections()) != 0 {
		t.Error("an unauthenticated 401 caused a re-vend")
	}
}

// The plane's "sign-in required" at the first vend is a ReconnectError; any other
// failure is not.
func TestPlaneVendSignInRequired(t *testing.T) {
	plane := newFakePlane(t, planeAnswer{status: signInRequiredStatus}, planeAnswer{})
	if _, err := plane.vendor().Vend(context.Background(), "github"); !isSignInRequired(err) {
		t.Fatalf("err = %v", err)
	}
	down := newFakePlane(t, planeAnswer{status: http.StatusBadGateway}, planeAnswer{})
	if _, err := down.vendor().Vend(context.Background(), "github"); err == nil || isSignInRequired(err) {
		t.Fatalf("502 err = %v", err)
	}
}

// converseItem is one item with one unanswered user turn.
func converseSink() *memSink {
	return &memSink{items: []worklist.WorkItem{{
		ID: "github:o/r#1", Type: worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1},
		Thread: []worklist.Message{{ID: "q", Role: worklist.RoleUser, Content: "why did CI fail?"}},
	}}}
}

func lastTurn(t *testing.T, s *memSink) worklist.Message {
	t.Helper()
	th := s.items[0].Thread
	if len(th) != 2 {
		t.Fatalf("thread = %+v, want the question and one agent turn", th)
	}
	return th[1]
}

func assertReconnectNotice(t *testing.T, s *memSink) {
	t.Helper()
	m := lastTurn(t, s)
	if m.Notice != worklist.NoticeReconnect || m.Content != worklist.ReconnectNotice || m.ReplyTo != "q" {
		t.Fatalf("agent turn = %+v, want a reconnect notice", m)
	}
}

// "Sign-in required" at the vend: no model call, a reconnect notice.
func TestConverseReconnectAtVend(t *testing.T) {
	plane := newFakePlane(t, planeAnswer{status: signInRequiredStatus}, planeAnswer{})
	sink := converseSink()
	fc := &fakeConverser{reply: "a confident answer"}
	if err := Run(context.Background(), Params{JobType: JobConverse, ItemID: "github:o/r#1", Converser: fc, GitHubBaseURL: deadGitHub(t)}, plane.vendor(), sink); err != nil {
		t.Fatal(err)
	}
	if fc.gotUser != "" {
		t.Error("the model was called")
	}
	assertReconnectNotice(t, sink)
}

// The vended token is unexpired but revoked at GitHub: the snapshot fetch's 401
// re-vends, the broker answers "sign-in required", and the turn gets the notice
// without a model call.
func TestConverseReconnectInSnapshot(t *testing.T) {
	plane := newFakePlane(t, planeAnswer{token: "tok", version: "v1"}, planeAnswer{status: signInRequiredStatus})
	api := newFakeAPI(t) // rejects the token
	sink := converseSink()
	fc := &fakeConverser{reply: "a confident answer"}
	if err := Run(context.Background(), Params{JobType: JobConverse, ItemID: "github:o/r#1", Converser: fc, GitHubBaseURL: api.srv.URL}, plane.vendor(), sink); err != nil {
		t.Fatal(err)
	}
	if fc.gotUser != "" {
		t.Error("the model was called")
	}
	assertReconnectNotice(t, sink)
}

// toolConverser calls one tool, then answers whatever the tool said.
type toolConverser struct {
	tool, args string
	toolErr    error
	called     bool
}

func (c *toolConverser) Reply(ctx context.Context, _ worklist.WorkItem, _, _ string, _ []worklist.Message, _ string, tools worklist.ToolBox) (string, error) {
	c.called = true
	if tools == nil {
		return "no tools", nil
	}
	_, err := tools.Invoke(ctx, c.tool, json.RawMessage(c.args))
	c.toolErr = err
	// A converser that (wrongly) swallows the error still must not get its answer
	// saved: runConverse checks the holder too.
	return "partial answer built without the evidence", nil
}

// A tool call that finds the access gone ends the turn with the notice: the tool
// returns the typed error, and whatever the model said is discarded.
func TestConverseReconnectFromToolCall(t *testing.T) {
	plane := newFakePlane(t, planeAnswer{token: "tok", version: "v1"}, planeAnswer{status: signInRequiredStatus})
	api := newFakeAPI(t, "tok")
	api.unauthorized = func(path string) bool { return strings.Contains(path, "/contents/") }
	sink := converseSink()
	tc := &toolConverser{tool: "github_read_file", args: `{"repo":"o/r","path":"README.md"}`}
	if err := Run(context.Background(), Params{JobType: JobConverse, ItemID: "github:o/r#1", Converser: tc, GitHubBaseURL: api.srv.URL}, plane.vendor(), sink); err != nil {
		t.Fatal(err)
	}
	if !tc.called || !isSignInRequired(tc.toolErr) {
		t.Fatalf("tool error = %v, want the sign-in error", tc.toolErr)
	}
	assertReconnectNotice(t, sink)
}

// Any other vend failure: the model runs, told GitHub is unavailable, and the saved
// reply says it was answered from stored context only.
func TestConverseWithoutGitHubAnnotatesReply(t *testing.T) {
	plane := newFakePlane(t, planeAnswer{status: http.StatusBadGateway}, planeAnswer{})
	sink := converseSink()
	fc := &fakeConverser{reply: "From what gofer stored, probably a flake."}
	if err := Run(context.Background(), Params{JobType: JobConverse, ItemID: "github:o/r#1", Converser: fc, GitHubBaseURL: deadGitHub(t)}, plane.vendor(), sink); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(fc.gotUser, "why did CI fail?") || !strings.Contains(fc.gotUser, "GitHub is unavailable for this turn") {
		t.Errorf("model was asked %q", fc.gotUser)
	}
	m := lastTurn(t, sink)
	if m.Notice != "" || !strings.HasPrefix(m.Content, fc.reply) || !strings.Contains(m.Content, "Answered from stored context only") {
		t.Errorf("reply = %+v", m)
	}
	if sink.items[0].Thread[0].Content != "why did CI fail?" {
		t.Error("the stored user turn was changed")
	}
}

// Ingest stops with the sign-in error, and writes nothing, when the access is gone.
func TestIngestReconnect(t *testing.T) {
	plane := newFakePlane(t, planeAnswer{token: "tok", version: "v1"}, planeAnswer{status: signInRequiredStatus})
	api := newFakeAPI(t)
	sink := &memSink{}
	err := Run(context.Background(), Params{JobType: JobIngest, GitHubBaseURL: api.srv.URL}, plane.vendor(), sink)
	if !isSignInRequired(err) {
		t.Fatalf("err = %v, want sign-in required", err)
	}
	if sink.ingests != 0 {
		t.Error("ingest wrote back")
	}
}

// The Actions log download makes its API hop with the transport directly (not
// http.Client.Do), so pin that the holder covers it too: a 401 on the log re-vends
// and the log is read with the new token.
func TestHolderCoversActionsLogHop(t *testing.T) {
	plane := newFakePlane(t, planeAnswer{token: "tok-old", version: "v1"}, planeAnswer{token: "tok-new", version: "v2"})
	api := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/actions/jobs/7/logs" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok-new" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		_, _ = w.Write([]byte("the job log"))
	}))
	t.Cleanup(api.Close)
	h, err := newTokenHolder(context.Background(), plane.vendor(), "github", api.URL)
	if err != nil {
		t.Fatal(err)
	}
	gh := github.NewClient(h.client(nil), api.URL)
	var buf strings.Builder
	if _, err := gh.ActionsJobLog(context.Background(), h.Token(), "o/r", 7, &buf, 1<<20); err != nil {
		t.Fatalf("log: %v", err)
	}
	if buf.String() != "the job log" || len(plane.rejections()) != 1 {
		t.Fatalf("log %q, rejections %v", buf.String(), plane.rejections())
	}
}
