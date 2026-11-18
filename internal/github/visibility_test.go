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
		if _, err := c.IssueStatus(ctx, "tok", "o/r", n); !errors.Is(err, ErrNotPublic) {
			t.Errorf("issue %d: err = %v, want ErrNotPublic", n, err)
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
		"repo:o/r otel":          false,
		"is:open author:x":       false,
		`"is:issue" in:title`:    false,
		"is:issue foo":           true,
		"IS:PR foo":              true,
		"foo is:pull-request":    true,
		"type:issue label:bug":   true,
		"type:pr review:none":    true,
		"-is:pr label:bug":       false,
		"repo:o/r is:issue OR x": true,
	} {
		if got := hasTypeQualifier(q); got != want {
			t.Errorf("hasTypeQualifier(%q) = %v, want %v", q, got, want)
		}
	}
}
