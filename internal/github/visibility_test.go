package github

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// gateStub is a GitHub API stub whose repositories have set visibilities. A repository
// lookup (GET /repos/{o}/{r} or /repositories/{id}) answers from repos; every request
// is recorded, so a test can show that nothing past a refused gate was asked for.
type gateStub struct {
	t     *testing.T
	srv   *httptest.Server
	mu    sync.Mutex
	paths []string
	// repos maps a lower-cased owner/name to its metadata answer: "public", "private",
	// "404", "500", or "rename:<new owner/name>:<id>" (a 301 to /repositories/<id>).
	repos map[string]string
	// ids maps a repository ID to "public:<full name>" or "private:<full name>".
	ids map[string]string
	// other serves everything else.
	other http.HandlerFunc
}

func newGateStub(t *testing.T, repos map[string]string, other http.HandlerFunc) *gateStub {
	s := &gateStub{t: t, repos: repos, ids: map[string]string{}, other: other}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *gateStub) client() *Client { return NewClient(s.srv.Client(), s.srv.URL) }

func (s *gateStub) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.paths = append(s.paths, r.URL.Path)
	s.mu.Unlock()
	segs := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	switch {
	case len(segs) == 3 && segs[0] == "repos":
		name := segs[1] + "/" + segs[2]
		answer, ok := s.repos[strings.ToLower(name)]
		if !ok {
			answer = "404"
		}
		switch {
		case answer == "public" || answer == "private":
			fmt.Fprintf(w, `{"id":1,"full_name":%q,"private":%v}`, name, answer == "private")
		case answer == "404":
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		case answer == "500":
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
		case strings.HasPrefix(answer, "rename:"):
			parts := strings.Split(answer, ":")
			http.Redirect(w, r, "/repositories/"+parts[2], http.StatusMovedPermanently)
		}
	case len(segs) == 2 && segs[0] == "repositories":
		vis, full, ok := strings.Cut(s.ids[segs[1]], ":")
		if !ok {
			http.NotFound(w, r)
			return
		}
		if vis == "403" {
			http.Error(w, fmt.Sprintf(`{"message":"Access to %s is blocked"}`, full), http.StatusForbidden)
			return
		}
		fmt.Fprintf(w, `{"id":%s,"full_name":%q,"private":%v}`, segs[1], full, vis == "private")
	default:
		s.other(w, r)
	}
}

func (s *gateStub) requested(prefix string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, p := range s.paths {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	return out
}

// Every per-repository call refuses a private repository before it asks GitHub for
// anything from it, with the public-only message.
func TestGateRefusesPrivateRepository(t *testing.T) {
	s := newGateStub(t, map[string]string{"o/secret": "private"}, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request past the gate: %s", r.URL.Path)
		http.NotFound(w, r)
	})
	c := s.client()
	ctx := context.Background()
	var buf bytes.Buffer
	calls := map[string]func() error{
		"FileContents": func() error { _, err := c.FileContents(ctx, "tok", "o/secret", "go.mod", "", 0); return err },
		"IssueStatus":  func() error { _, err := c.IssueStatus(ctx, "tok", "o/secret", 1); return err },
		"PullRequest":  func() error { _, err := c.PullRequestStatus(ctx, "tok", "o/secret", 1); return err },
		"Discussion":   func() error { _, err := c.Discussion(ctx, "tok", "o/secret", 1, true); return err },
		"ItemActivity": func() error { _, err := c.ItemActivity(ctx, "tok", "o/secret", 1, "me"); return err },
		"CIStatus":     func() error { _, err := c.CIStatusForRef(ctx, "tok", "o/secret", "main"); return err },
		"CIStatusPR":   func() error { _, err := c.CIStatusForPR(ctx, "tok", "o/secret", 1); return err },
		"ActionsJob":   func() error { _, err := c.ActionsJob(ctx, "tok", "o/secret", 9); return err },
		"RunJobs":      func() error { _, _, err := c.ActionsRunJobs(ctx, "tok", "o/secret", 9, 0); return err },
		"JobLog":       func() error { _, err := c.ActionsJobLog(ctx, "tok", "o/secret", 9, &buf, 1<<20); return err },
		"StepLog":      func() error { _, err := c.ActionsStepLog(ctx, "tok", "o/secret", 9, 1, &buf, 1<<20); return err },
		"Tarball": func() error {
			rc, err := c.Tarball(ctx, "tok", "o/secret", "")
			if rc != nil {
				rc.Close()
			}
			return err
		},
	}
	for name, call := range calls {
		err := call()
		if !errors.Is(err, ErrNotPublic) || !strings.Contains(err.Error(), "public repositories only") {
			t.Errorf("%s: err = %v, want ErrNotPublic", name, err)
		}
	}
	// One lookup for the whole run: the verdict is cached.
	if got := s.requested("/repos/o/secret"); len(got) != 1 {
		t.Errorf("gate lookups = %v, want exactly one (cached per run)", got)
	}
}

// The gate's canonical name is what the call uses: a renamed repository (a 301 to
// /repositories/{id}) and a differently cased name both read from the current name.
func TestGateUsesCanonicalName(t *testing.T) {
	s := newGateStub(t, map[string]string{"old/name": "rename:new/name:42"}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/new/name/issues/3" {
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"number":3,"state":"open","title":"t","repository_url":"https://api.github.com/repos/new/name"}`))
	})
	s.ids["42"] = "public:new/name"
	got, err := s.client().IssueStatus(context.Background(), "tok", "old/name", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "new/name#3") {
		t.Errorf("IssueStatus = %q, want the canonical name", got)
	}
}

// A repository the gate cannot see is refused as not found; one whose check has no
// answer is refused as unconfirmed and asked again on the next call (not cached).
func TestGateNotFoundAndUnknown(t *testing.T) {
	s := newGateStub(t, map[string]string{"o/flaky": "500"}, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request past the gate: %s", r.URL.Path)
	})
	c := s.client()
	ctx := context.Background()
	if _, err := c.IssueStatus(ctx, "tok", "o/missing", 1); err == nil || !strings.Contains(err.Error(), "status 404") {
		t.Errorf("missing repo: err = %v, want not found", err)
	}
	for range 2 {
		if _, err := c.IssueStatus(ctx, "tok", "o/flaky", 1); err == nil || !strings.Contains(err.Error(), "could not confirm") {
			t.Errorf("flaky repo: err = %v, want could not confirm", err)
		}
	}
	if got := s.requested("/repos/o/flaky"); len(got) != 2 {
		t.Errorf("an unknown answer must not be cached; lookups = %v", got)
	}
	for _, bad := range []string{"o/..", "./x", "o/r/x"} {
		if _, err := c.FileContents(ctx, "tok", bad, "f", "", 0); err == nil {
			t.Errorf("FileContents(%q) passed the gate", bad)
		}
	}
}

// An API-host redirect (a transferred issue) is followed only when its destination
// repository passes the gate, whether the location names it by ID or by name.
func TestGateChecksRedirectDestination(t *testing.T) {
	s := newGateStub(t, map[string]string{"o/r": "public", "o/hidden": "private", "o/moved": "public"}, nil)
	s.ids["7"] = "private:o/hidden"
	s.ids["8"] = "public:o/moved"
	s.other = func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/issues/1":
			http.Redirect(w, r, "/repositories/7/issues/1", http.StatusMovedPermanently)
		case "/repos/o/r/issues/2":
			http.Redirect(w, r, "/repos/o/hidden/issues/9", http.StatusTemporaryRedirect)
		case "/repos/o/r/issues/3":
			http.Redirect(w, r, "/repositories/8/issues/4", http.StatusMovedPermanently)
		case "/repositories/8/issues/4":
			_, _ = w.Write([]byte(`{"number":4,"state":"open","title":"moved","repository_url":"https://api.github.com/repos/o/moved"}`))
		case "/repos/o/r/issues/5":
			http.Redirect(w, r, "https://elsewhere.example/x", http.StatusFound)
		case "/repos/o/r/issues/6":
			http.Redirect(w, r, "/user", http.StatusFound)
		default:
			t.Errorf("request past a refused redirect: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}
	c := s.client()
	ctx := context.Background()

	for _, n := range []int{1, 2, 6} {
		_, err := c.IssueStatus(ctx, "tok", "o/r", n)
		if !errors.Is(err, ErrNotPublic) {
			t.Errorf("issue %d: err = %v, want ErrNotPublic", n, err)
		}
		// The destination's name came from GitHub, not the model: it stays out of the
		// error the model reads.
		if err != nil && (strings.Contains(err.Error(), "hidden") || strings.Contains(err.Error(), "repositories/7")) {
			t.Errorf("issue %d: the refusal names the private destination: %v", n, err)
		}
	}
	if _, err := c.IssueStatus(ctx, "tok", "o/r", 5); err == nil || !strings.Contains(err.Error(), "off the API host") {
		t.Errorf("off-host redirect: err = %v", err)
	}
	got, err := c.IssueStatus(ctx, "tok", "o/r", 3)
	if err != nil || !strings.Contains(got, "moved") {
		t.Errorf("redirect to a public repository: %q, %v", got, err)
	}
}

// The body backstop: an issue or PR answered without a redirect, but naming another
// repository, is dropped unless that repository passes the gate.
func TestGateBodyBackstop(t *testing.T) {
	s := newGateStub(t, map[string]string{"o/r": "public", "o/hidden": "private", "o/pub": "public"}, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/issues/1":
			_, _ = w.Write([]byte(`{"number":1,"body":"secret plans","repository_url":"https://api.github.com/repos/o/hidden"}`))
		case "/repos/o/r/issues/2":
			_, _ = w.Write([]byte(`{"number":2,"body":"fine","repository_url":"https://api.github.com/repos/o/pub"}`))
		case "/repos/o/r/pulls/3":
			_, _ = w.Write([]byte(`{"number":3,"title":"secret","base":{"ref":"main","repo":{"full_name":"o/hidden"}}}`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	})
	c := s.client()
	ctx := context.Background()
	if _, err := c.IssueStatus(ctx, "tok", "o/r", 1); !errors.Is(err, ErrNotPublic) {
		t.Errorf("IssueStatus: err = %v, want ErrNotPublic", err)
	}
	if d, err := c.Discussion(ctx, "tok", "o/r", 1, false); !errors.Is(err, ErrNotPublic) || d.Body != "" {
		t.Errorf("Discussion: body %q, err = %v; want nothing and ErrNotPublic", d.Body, err)
	}
	if _, err := c.PullRequestStatus(ctx, "tok", "o/r", 3); !errors.Is(err, ErrNotPublic) {
		t.Errorf("PullRequestStatus: err = %v, want ErrNotPublic", err)
	}
	if d, err := c.Discussion(ctx, "tok", "o/r", 2, false); err != nil || d.Body != "fine" {
		t.Errorf("a body naming another public repository: %q, %v", d.Body, err)
	}
}

// An Actions log request redirected within the API host is followed only to a public
// repository.
func TestGateChecksLogRedirect(t *testing.T) {
	s := newGateStub(t, map[string]string{"o/r": "public"}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r/actions/jobs/9/logs" {
			http.Redirect(w, r, "/repositories/7/actions/jobs/9/logs", http.StatusFound)
			return
		}
		t.Errorf("request past a refused redirect: %s", r.URL.Path)
	})
	s.ids["7"] = "private:o/hidden"
	var buf bytes.Buffer
	if _, err := s.client().ActionsJobLog(context.Background(), "tok", "o/r", 9, &buf, 1<<20); !errors.Is(err, ErrNotPublic) {
		t.Errorf("err = %v, want ErrNotPublic", err)
	}
}

// Ingest searches public repositories only, with the involves: query split into
// issues and PRs, and writes only hits whose repository is confirmed public. A
// definite refusal (private, not found) drops the hit; no answer drops it AND makes
// the fetch incomplete, so the caller retires nothing.
func TestFetchWorklistPublicOnly(t *testing.T) {
	hit := func(repo string, n int) string {
		return fmt.Sprintf(`{"number":%d,"title":"t","html_url":"u","state":"open","repository_url":"https://api.github.com/repos/%s"}`, n, repo)
	}
	repos := map[string]string{"o/pub": "public", "o/hidden": "private"}
	var mu sync.Mutex
	var queries []string
	s := newGateStub(t, repos, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"login":"me"}`))
		case "/search/issues":
			q := r.URL.Query().Get("q")
			mu.Lock()
			queries = append(queries, q)
			mu.Unlock()
			if !strings.HasSuffix(q, " is:public") || !(strings.Contains(q, "is:issue") || strings.Contains(q, "is:pr")) {
				w.WriteHeader(http.StatusUnprocessableEntity) // as a GitHub App user token answers
				return
			}
			fmt.Fprintf(w, `{"items":[%s,%s,%s,%s]}`, hit("o/pub", 1), hit("o/hidden", 2), hit("o/gone", 3), hit("o/flaky", 4))
		default:
			http.NotFound(w, r)
		}
	})

	items, complete, err := s.client().FetchWorklist(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"is:issue is:open involves:@me archived:false is:public",
		"is:pr is:open involves:@me archived:false is:public",
		"is:pr is:open review-requested:@me archived:false is:public",
	}
	if strings.Join(queries, "|") != strings.Join(want, "|") {
		t.Errorf("queries = %q, want %q", queries, want)
	}
	if len(items) != 1 || items[0].GitHub.Repo != "o/pub" {
		t.Fatalf("items = %+v, want only the public repository's hit", items)
	}
	if !complete {
		t.Error("definite refusals alone must not make the fetch incomplete")
	}

	// Now one repository's check has no answer.
	repos["o/flaky"] = "500"
	items, complete, err = s.client().FetchWorklist(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].GitHub.Repo != "o/pub" {
		t.Fatalf("items = %+v, want only the public repository's hit", items)
	}
	if complete {
		t.Error("an unconfirmed repository must make the fetch incomplete")
	}
}

func TestHasTypeQualifier(t *testing.T) {
	for q, want := range map[string]bool{
		"repo:o/r otel":           false,
		"is:open author:x":        false,
		`"is:issue" in:title`:     false,
		"is:issue foo":            true,
		"IS:PR foo":               true,
		"foo is:pull-request":     true,
		"type:issue label:bug":    true,
		"type:pr review:none":     true,
		"-is:pr label:bug":        false,
		"repo:o/r is:issue OR x":  true,
		`"adding is:pr support"`:  false,
		`"adding is:pr" is:issue`: true,
		`label:"is:issue" foo`:    false,
	} {
		if got := hasTypeQualifier(q); got != want {
			t.Errorf("hasTypeQualifier(%q) = %v, want %v", q, got, want)
		}
	}
}

// A refusal names only what the caller asked for. A private repository reached by a
// rename, or named in a response body, is never named.
func TestGateRefusalNamesOnlyWhatWasAsked(t *testing.T) {
	s := newGateStub(t, map[string]string{"old/name": "rename:new/secret:42", "o/r": "public"}, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/issues/1":
			_, _ = w.Write([]byte(`{"number":1,"repository_url":"https://api.github.com/repos/new/secret"}`))
		default:
			t.Errorf("request past the gate: %s", r.URL.Path)
		}
	})
	s.ids["42"] = "private:new/secret"
	c := s.client()
	ctx := context.Background()
	_, err := c.IssueStatus(ctx, "tok", "old/name", 1)
	if !errors.Is(err, ErrNotPublic) || !strings.Contains(err.Error(), "old/name") || strings.Contains(err.Error(), "secret") {
		t.Errorf("renamed private repository: err = %v, want ErrNotPublic naming old/name only", err)
	}
	// The cached private verdict is not worded with the canonical name either.
	if _, err := c.IssueStatus(ctx, "tok", "new/secret", 1); err == nil || !strings.Contains(err.Error(), "new/secret is private") {
		t.Errorf("asked by its canonical name: err = %v", err)
	}
	_, err = c.IssueStatus(ctx, "tok", "o/r", 1)
	if !errors.Is(err, ErrNotPublic) || strings.Contains(err.Error(), "secret") {
		t.Errorf("body backstop: err = %v, want ErrNotPublic without the other repository's name", err)
	}
}

// No request leaves with a path a server could normalise onto another repository: a
// "." or ".." segment (also percent-encoded) is refused, a repository argument must
// be exactly owner/name, and a file path is escaped segment by segment.
func TestGateRefusesDotSegments(t *testing.T) {
	var got []string
	s := newGateStub(t, map[string]string{"o/r": "public"}, func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.EscapedPath())
		_, _ = w.Write([]byte(`{"type":"file","encoding":"base64","content":"aGk="}`))
	})
	c := s.client()
	ctx := context.Background()
	for _, path := range []string{"../../../evil/secret/contents/x", "a/./b", "a/../b", "a\\..\\b"} {
		if _, err := c.FileContents(ctx, "tok", "o/r", path, "", 0); err == nil {
			t.Errorf("FileContents(%q) was sent", path)
		}
	}
	for _, repo := range []string{"o/r/../../evil/secret", "o/r/x"} {
		if _, err := c.CIStatusForRef(ctx, "tok", repo, "main"); err == nil || !strings.Contains(err.Error(), "owner/name") {
			t.Errorf("CIStatusForRef(%q) err = %v", repo, err)
		}
		if _, err := c.ItemActivity(ctx, "tok", repo, 1, "me"); err == nil {
			t.Errorf("ItemActivity(%q) was sent", repo)
		}
	}
	for _, p := range []string{"/repos/o/r/contents/%2e%2e/x", "/repos/o/r/contents/..%2F..%2Fx", "/repos/o/r/./x"} {
		if _, err := c.get(ctx, "tok", p); err == nil {
			t.Errorf("get(%q) was sent", p)
		}
	}
	if len(got) != 0 {
		t.Fatalf("requests sent: %q", got)
	}
	// A legitimate path and a branch with a slash still work.
	if _, err := c.FileContents(ctx, "tok", "o/r", "/dir/a b.go", "release/1.2", 0); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "/repos/o/r/contents/dir/a%20b.go" {
		t.Errorf("sent %q", got)
	}
}

// CIStatusForPR reads nothing of a PR answer, its head SHA included, that names
// another repository that is not public.
func TestCIStatusForPRBackstop(t *testing.T) {
	s := newGateStub(t, map[string]string{"o/r": "public", "o/hidden": "private"}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r/pulls/5" {
			_, _ = w.Write([]byte(`{"head":{"ref":"secret-branch","sha":"abc"},"base":{"repo":{"full_name":"o/hidden"}}}`))
			return
		}
		t.Errorf("CI lookup after a refused PR answer: %s", r.URL.Path)
	})
	st, err := s.client().CIStatusForPR(context.Background(), "tok", "o/r", 5)
	if !errors.Is(err, ErrNotPublic) || strings.Contains(err.Error(), "hidden") || st.Ref != "" || st.SHA != "" {
		t.Errorf("CIStatusForPR = %+v, %v; want nothing and ErrNotPublic", st, err)
	}
}

// An Actions log redirected within the API host is followed with the token once its
// destination is public; only the off-host download link goes to the blob client.
func TestLogRedirectWithinAPIKeepsToken(t *testing.T) {
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("the blob host received the token")
		}
		_, _ = w.Write([]byte("log line\n"))
	}))
	defer blob.Close()
	s := newGateStub(t, map[string]string{"o/r": "public"}, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/actions/jobs/9/logs":
			http.Redirect(w, r, "/repositories/8/actions/jobs/9/logs", http.StatusFound)
		case "/repositories/8/actions/jobs/9/logs":
			if r.Header.Get("Authorization") != "Bearer tok" {
				http.Error(w, `{"message":"Must have admin rights to Repository."}`, http.StatusForbidden)
				return
			}
			http.Redirect(w, r, blob.URL+"/log?sig=presigned", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	})
	s.ids["8"] = "public:o/moved"
	var buf bytes.Buffer
	if _, err := s.client().ActionsJobLog(context.Background(), "tok", "o/r", 9, &buf, 1<<20); err != nil || buf.String() != "log line\n" {
		t.Fatalf("log = %q, err = %v", buf.String(), err)
	}
}

// Search does not send a request it knows the rate limit will refuse: when the reset
// is past the turn's deadline it says so.
func TestSearchRateLimitPastDeadline(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(10*time.Minute).Unix()))
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := NewClient(srv.Client(), srv.URL).Search(ctx, "tok", "otel")
	if err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("err = %v, want a rate-limit explanation", err)
	}
	if calls != 1 {
		t.Errorf("requests = %d, want 1 (the PR query must not be sent)", calls)
	}
}

// The visibility checks have their own, smaller reserve than pagination: a slow
// search that stopped paging for time still gets its hits checked and written,
// rather than every check being skipped and the backfill saving nothing.
func TestFetchWorklistChecksAfterSlowSearch(t *testing.T) {
	s := newGateStub(t, map[string]string{"o/pub": "public"}, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"login":"me"}`))
		case "/search/issues":
			// More pages are advertised, but the run has too little time to read them.
			w.Header().Set("Link", `<x?page=2>; rel="next"`)
			_, _ = w.Write([]byte(`{"items":[{"number":1,"title":"t","html_url":"u","state":"open","repository_url":"https://api.github.com/repos/o/pub"}]}`))
		default:
			http.NotFound(w, r)
		}
	})
	// Inside pagination's reserve (90s), outside the checks' (30s).
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	items, complete, err := s.client().FetchWorklist(ctx, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want the public hit kept", len(items))
	}
	if complete {
		t.Error("a walk stopped for time is not complete")
	}

	// Inside the checks' reserve too: nothing can be confirmed, nothing is complete.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel2()
	items, complete, err = s.client().FetchWorklist(ctx2, "tok")
	if err != nil || len(items) != 0 || complete {
		t.Errorf("items = %d, complete = %v, err = %v; want none, incomplete", len(items), complete, err)
	}
}

// A repository name may contain dots, even be "...": only a component that is "." or
// ".." is refused, by every entry point alike. Ingest keeps such a repository's
// public hits; a hit whose repository it cannot name at all is left out and makes
// the fetch incomplete, rather than counting as a definite refusal.
func TestDottedRepositoryNames(t *testing.T) {
	s := newGateStub(t, map[string]string{"manx/...": "public", "o/a..b": "public"}, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"login":"me"}`))
		case "/search/issues":
			_, _ = w.Write([]byte(`{"items":[` +
				`{"number":1,"title":"t","html_url":"u","state":"open","repository_url":"https://api.github.com/repos/manx/..."},` +
				`{"number":2,"title":"t","html_url":"u","state":"open","repository_url":"https://api.github.com/repos/o/a..b"}]}`))
		case "/repos/manx/.../issues/1":
			_, _ = w.Write([]byte(`{"number":1,"state":"open","title":"dots","repository_url":"https://api.github.com/repos/manx/..."}`))
		default:
			http.NotFound(w, r)
		}
	})
	c := s.client()
	ctx := context.Background()
	items, complete, err := c.FetchWorklist(ctx, "tok")
	if err != nil || len(items) != 2 || !complete {
		t.Fatalf("items = %d, complete = %v, err = %v; want both dotted repositories, complete", len(items), complete, err)
	}
	if got, err := c.IssueStatus(ctx, "tok", "manx/...", 1); err != nil || !strings.Contains(got, "dots") {
		t.Errorf("IssueStatus(manx/...) = %q, %v", got, err)
	}
	if _, err := ParseActionsURL("https://github.com/manx/.../actions/runs/7"); err != nil {
		t.Errorf("ParseActionsURL with a dotted repository: %v", err)
	}
	for _, bad := range []string{"o/..", "../r", "o/.", "./r"} {
		if validRepo(bad) {
			t.Errorf("validRepo(%q) = true", bad)
		}
	}

	// A hit whose repository_url names no owner/name cannot be checked.
	s2 := newGateStub(t, map[string]string{"o/pub": "public"}, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"login":"me"}`))
		case "/search/issues":
			_, _ = w.Write([]byte(`{"items":[` +
				`{"number":1,"title":"t","html_url":"u","state":"open","repository_url":"https://api.github.com/repos/o/pub"},` +
				`{"number":2,"title":"t","html_url":"u","state":"open","repository_url":"https://elsewhere.example/weird"}]}`))
		default:
			http.NotFound(w, r)
		}
	})
	items, complete, err = s2.client().FetchWorklist(ctx, "tok")
	if err != nil || len(items) != 1 || complete {
		t.Errorf("items = %d, complete = %v, err = %v; want the public hit only, incomplete", len(items), complete, err)
	}
}

// A metadata lookup that follows a rename and ends in a 403 is refused with the
// status alone: GitHub's message there could name the destination.
func TestGateRedirected403KeepsNoFreeText(t *testing.T) {
	s := newGateStub(t, map[string]string{"old/name": "rename:new/secret:42"}, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request past the gate: %s", r.URL.Path)
	})
	s.ids["42"] = "403:new/secret"
	_, err := s.client().IssueStatus(context.Background(), "tok", "old/name", 1)
	if err == nil || !strings.Contains(err.Error(), "status 403") || strings.Contains(err.Error(), "secret") {
		t.Errorf("err = %v, want a 403 refusal without the destination's name", err)
	}
}
