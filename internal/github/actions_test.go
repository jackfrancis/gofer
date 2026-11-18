package github

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/httpretry"
)

func TestParseActionsURL(t *testing.T) {
	cases := []struct {
		in   string
		want ActionsRef
		bad  bool
	}{
		{in: "https://github.com/o/r/actions/runs/123", want: ActionsRef{Repo: "o/r", RunID: 123}},
		{in: "https://github.com/o/r/actions/runs/123/", want: ActionsRef{Repo: "o/r", RunID: 123}},
		{in: "https://github.com/o/r/actions/runs/123/job/456", want: ActionsRef{Repo: "o/r", RunID: 123, JobID: 456}},
		{in: "https://github.com/o/r/actions/runs/123/job/456?pr=9#step:5:12", want: ActionsRef{Repo: "o/r", RunID: 123, JobID: 456, Step: 5}},
		{in: "https://github.com/o/r/actions/runs/123/attempts/2", want: ActionsRef{Repo: "o/r", RunID: 123, Attempt: 2}},
		{in: "https://github.com/o/r/actions/runs/123/attempts/2/job/7", want: ActionsRef{Repo: "o/r", RunID: 123, Attempt: 2, JobID: 7}},
		{in: "https://github.com/o/r/runs/456", want: ActionsRef{Repo: "o/r", JobID: 456}},
		{in: "https://github.com/o/r/commit/abc123/checks/456", want: ActionsRef{Repo: "o/r", JobID: 456}},
		{in: "  https://www.github.com/kubernetes-sigs/x.y/actions/runs/1/job/2  ", want: ActionsRef{Repo: "kubernetes-sigs/x.y", RunID: 1, JobID: 2}},
		{in: "https://example.com/o/r/actions/runs/1", bad: true},
		{in: "https://github.com.evil.test/o/r/actions/runs/1", bad: true},
		{in: "ftp://github.com/o/r/actions/runs/1", bad: true},
		{in: "https://github.com/o/r/pull/5", bad: true},
		{in: "https://github.com/o/r/actions/runs/abc", bad: true},
		{in: "https://github.com/o/r/actions/runs/1/job/-2", bad: true},
		{in: "https://github.com/o/r/runs/0", bad: true},
		{in: "https://github.com/../r/actions/runs/1", bad: true},
		{in: "https://prow.k8s.io/view/gs/b/pr-logs/pull/o_r/1/j/2", bad: true},
	}
	for _, c := range cases {
		got, err := ParseActionsURL(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("ParseActionsURL(%q) = %+v, want an error", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("ParseActionsURL(%q) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
}

func TestActionsRunJobsPaginates(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing token on %s", r.URL.Path)
		}
		if r.URL.Path != "/repos/o/r/actions/runs/7/jobs" {
			http.NotFound(w, r)
			return
		}
		page := r.URL.Query().Get("page")
		var jobs []string
		switch page {
		case "1":
			for i := range actionsJobsPerPage {
				jobs = append(jobs, fmt.Sprintf(`{"id":%d,"name":"j%d","status":"completed","conclusion":"success"}`, i+1, i+1))
			}
		case "2":
			jobs = append(jobs, `{"id":1000,"name":"last","status":"completed","conclusion":"failure","steps":[{"number":1,"name":"s","conclusion":"failure"}]}`)
		}
		fmt.Fprintf(w, `{"total_count":%d,"jobs":[%s]}`, actionsJobsPerPage+1, strings.Join(jobs, ","))
	}))
	defer srv.Close()
	c := NewClient(srv.Client(), srv.URL)
	jobs, complete, err := c.ActionsRunJobs(context.Background(), "tok", "o/r", 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !complete || len(jobs) != actionsJobsPerPage+1 {
		t.Fatalf("got %d jobs, complete=%v", len(jobs), complete)
	}
	last := jobs[len(jobs)-1]
	if last.ID != 1000 || !last.Failing() || len(last.Steps) != 1 {
		t.Errorf("last job = %+v", last)
	}
	if len(paths) != 2 || !strings.Contains(paths[0], "filter=latest") || !strings.Contains(paths[0], "per_page=100") {
		t.Errorf("requests = %v", paths)
	}

	// An attempt has its own endpoint.
	paths = nil
	if _, _, err := c.ActionsRunJobs(context.Background(), "tok", "o/r", 7, 2); err == nil {
		t.Error("attempt endpoint served by the stub's 404: want an error")
	}
	if len(paths) != 1 || !strings.HasPrefix(paths[0], "/repos/o/r/actions/runs/7/attempts/2/jobs?per_page=") {
		t.Errorf("attempt request = %v", paths)
	}
}

func TestActionsJobNotFound(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c := NewClient(srv.Client(), srv.URL)
	_, err := c.ActionsJob(context.Background(), "tok", "o/r", 5)
	if err == nil || !strings.Contains(err.Error(), "not visible with this token") {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.ActionsJob(context.Background(), "tok", "o/../r", 5); err == nil {
		t.Error("bad repo accepted")
	}
}

// logServers is an API stub that redirects a job's log to a separate blob host.
type logServers struct {
	api, blob    *httptest.Server
	apiLogHits   atomic.Int32
	blobHits     atomic.Int32
	blobSawCreds atomic.Bool
	blobBody     func(w http.ResponseWriter)
}

func newLogServers(t *testing.T, body string) *logServers {
	t.Helper()
	ls := &logServers{blobBody: func(w http.ResponseWriter) { _, _ = w.Write([]byte(body)) }}
	ls.blob = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ls.blobHits.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			ls.blobSawCreds.Store(true)
		}
		if r.URL.Query().Get("sig") != "s3cret-sig" {
			http.Error(w, "bad signature", http.StatusForbidden)
			return
		}
		ls.blobBody(w)
	}))
	ls.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("API request without the token: %s", r.URL.Path)
		}
		switch r.URL.Path {
		case "/repos/o/r/actions/jobs/42/logs":
			ls.apiLogHits.Add(1)
			http.Redirect(w, r, ls.blob.URL+"/logs/42?sig=s3cret-sig", http.StatusFound)
		case "/repos/o/r/actions/jobs/43/logs":
			http.Error(w, "gone", http.StatusGone)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ls.api.Close)
	t.Cleanup(ls.blob.Close)
	return ls
}

func TestActionsJobLogFollowsRedirectWithoutToken(t *testing.T) {
	ls := newLogServers(t, "line 1\nline 2\n")
	c := NewClient(ls.api.Client(), ls.api.URL)
	var buf bytes.Buffer
	n, err := c.ActionsJobLog(context.Background(), "tok", "o/r", 42, &buf, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if buf.String() != "line 1\nline 2\n" || n != int64(buf.Len()) {
		t.Errorf("log = %q (%d)", buf.String(), n)
	}
	if ls.blobSawCreds.Load() {
		t.Error("the blob host received a credential header")
	}
	if ls.apiLogHits.Load() != 1 || ls.blobHits.Load() != 1 {
		t.Errorf("api hits %d, blob hits %d", ls.apiLogHits.Load(), ls.blobHits.Load())
	}
}

// A shared client whose transport adds the token (as an OAuth transport would) must
// still never reach the blob host with it: the blob client is not built on it.
type addAuth struct{ base http.RoundTripper }

func (a addAuth) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer tok")
	return a.base.RoundTrip(r)
}

func TestActionsJobLogCredentialTransportStaysOnAPIHost(t *testing.T) {
	ls := newLogServers(t, "x\n")
	hc := &http.Client{Transport: addAuth{http.DefaultTransport}}
	c := NewClient(hc, ls.api.URL)
	if _, err := c.ActionsJobLog(context.Background(), "tok", "o/r", 42, &bytes.Buffer{}, 1<<20); err != nil {
		t.Fatal(err)
	}
	if ls.blobSawCreds.Load() {
		t.Error("the blob host received the token through the shared client's transport")
	}
}

func TestActionsJobLogSizeBound(t *testing.T) {
	ls := newLogServers(t, strings.Repeat("a", 2000))
	c := NewClient(ls.api.Client(), ls.api.URL)
	var buf bytes.Buffer
	_, err := c.ActionsJobLog(context.Background(), "tok", "o/r", 42, &buf, 1000)
	if !errors.Is(err, ErrLogTooLarge) {
		t.Fatalf("err = %v, want ErrLogTooLarge", err)
	}
	if buf.Len() > 1001 {
		t.Errorf("wrote %d bytes past a 1000-byte bound", buf.Len())
	}

	// Without a Content-Length (chunked), the bound is enforced while copying.
	ls.blobBody = func(w http.ResponseWriter) {
		for range 20 {
			_, _ = w.Write([]byte(strings.Repeat("b", 100)))
			w.(http.Flusher).Flush()
		}
	}
	buf.Reset()
	_, err = c.ActionsJobLog(context.Background(), "tok", "o/r", 42, &buf, 1000)
	if !errors.Is(err, ErrLogTooLarge) || buf.Len() > 1001 {
		t.Fatalf("chunked: err = %v, wrote %d", err, buf.Len())
	}
}

func TestActionsJobLogMissingAndExpired(t *testing.T) {
	ls := newLogServers(t, "")
	c := NewClient(ls.api.Client(), ls.api.URL)
	_, err := c.ActionsJobLog(context.Background(), "tok", "o/r", 41, &bytes.Buffer{}, 1<<20)
	if !errors.Is(err, ErrLogNotFound) || !strings.Contains(err.Error(), "404") {
		t.Errorf("404: err = %v", err)
	}
	_, err = c.ActionsJobLog(context.Background(), "tok", "o/r", 43, &bytes.Buffer{}, 1<<20)
	if !errors.Is(err, ErrLogNotFound) || !strings.Contains(err.Error(), "expired") {
		t.Errorf("410: err = %v", err)
	}
}

func TestActionsJobLogErrorsOmitBlobURL(t *testing.T) {
	ls := newLogServers(t, "x")
	c := NewClient(ls.api.Client(), ls.api.URL)
	ls.blob.Close() // the redirect now points at a closed port
	_, err := c.ActionsJobLog(context.Background(), "tok", "o/r", 42, &bytes.Buffer{}, 1<<20)
	if err == nil {
		t.Fatal("download from a closed host succeeded")
	}
	if strings.Contains(err.Error(), "s3cret-sig") || strings.Contains(err.Error(), ls.blob.URL) {
		t.Errorf("error leaks the pre-signed URL: %v", err)
	}
}

type recordRT struct{ got *http.Request }

func (r *recordRT) RoundTrip(req *http.Request) (*http.Response, error) {
	r.got = req
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

func TestBlobGuard(t *testing.T) {
	rec := &recordRT{}
	g := &blobGuard{base: rec}
	req, _ := http.NewRequest(http.MethodGet, "https://blob.example.test/x?sig=1", nil)
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Cookie", "a=b")
	req.Header.Set("Proxy-Authorization", "Basic x")
	req.Header.Set("Referer", "https://blob.example.test/prev?sig=2")
	if _, err := g.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"Authorization", "Cookie", "Proxy-Authorization", "Referer"} {
		if rec.got.Header.Get(h) != "" {
			t.Errorf("%s reached the blob host", h)
		}
	}
	if req.Header.Get("Authorization") == "" {
		t.Error("the caller's request was modified in place")
	}

	plain, _ := http.NewRequest(http.MethodGet, "http://blob.example.test/x", nil)
	if _, err := g.RoundTrip(plain); err == nil {
		t.Error("plain http allowed against an https API")
	}
	withUser, _ := http.NewRequest(http.MethodGet, "https://u:p@blob.example.test/x", nil)
	if _, err := g.RoundTrip(withUser); err == nil {
		t.Error("location with user info allowed")
	}
	if _, err := (&blobGuard{base: rec, allowHTTP: true}).RoundTrip(plain); err != nil {
		t.Errorf("plain http refused against a plain-http (test) API: %v", err)
	}
}

// On a redirect, net/http sets Referer to the previous URL — the pre-signed one. The
// guard drops it on every blob hop.
func TestActionsJobLogNoRefererOnChainedRedirect(t *testing.T) {
	var sawReferer atomic.Value
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawReferer.Store(r.Header.Get("Referer"))
		_, _ = w.Write([]byte("log\n"))
	}))
	defer final.Close()
	ls := newLogServers(t, "")
	ls.blobBody = func(w http.ResponseWriter) {
		w.Header().Set("Location", final.URL+"/moved")
		w.WriteHeader(http.StatusFound)
	}
	c := NewClient(ls.api.Client(), ls.api.URL)
	var buf bytes.Buffer
	if _, err := c.ActionsJobLog(context.Background(), "tok", "o/r", 42, &buf, 1<<20); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "log\n" {
		t.Fatalf("log = %q", buf.String())
	}
	if ref, _ := sawReferer.Load().(string); ref != "" {
		t.Errorf("the second blob host received Referer %q", ref)
	}
}

// A redirect Location net/http cannot parse is quoted inside its error; none of it
// may reach the caller.
func TestActionsJobLogRedactsBadLocation(t *testing.T) {
	ls := newLogServers(t, "")
	ls.blobBody = func(w http.ResponseWriter) {
		w.Header().Set("Location", "http://[::1%zz/next?sig=s3cret-sig-2")
		w.WriteHeader(http.StatusFound)
	}
	c := NewClient(ls.api.Client(), ls.api.URL)
	_, err := c.ActionsJobLog(context.Background(), "tok", "o/r", 42, &bytes.Buffer{}, 1<<20)
	if err == nil {
		t.Fatal("a malformed redirect succeeded")
	}
	for _, leak := range []string{"s3cret", "::1%zz", ls.blob.URL} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaks %q: %v", leak, err)
		}
	}
}

func TestActionsJobLogKeepsCancellation(t *testing.T) {
	ls := newLogServers(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	ls.blobBody = func(w http.ResponseWriter) {
		cancel()
		w.(http.Flusher).Flush()
		time.Sleep(50 * time.Millisecond)
	}
	c := NewClient(ls.api.Client(), ls.api.URL)
	_, err := c.ActionsJobLog(ctx, "tok", "o/r", 42, &bytes.Buffer{}, 1<<20)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if strings.Contains(err.Error(), "s3cret-sig") {
		t.Errorf("error leaks the pre-signed URL: %v", err)
	}
}

// A link the blob host refuses (it expired before use) is replaced once.
func TestActionsJobLogRetriesRefusedLink(t *testing.T) {
	ls := newLogServers(t, "")
	var refusals atomic.Int32
	refusals.Store(1)
	ls.blobBody = func(w http.ResponseWriter) {
		if refusals.Add(-1) >= 0 {
			http.Error(w, "AuthenticationFailed", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte("fresh\n"))
	}
	c := NewClient(ls.api.Client(), ls.api.URL)
	var buf bytes.Buffer
	if _, err := c.ActionsJobLog(context.Background(), "tok", "o/r", 42, &buf, 1<<20); err != nil || buf.String() != "fresh\n" {
		t.Fatalf("after one refusal: %q, %v", buf.String(), err)
	}
	if ls.apiLogHits.Load() != 2 {
		t.Errorf("asked the API for %d links, want 2", ls.apiLogHits.Load())
	}

	// Refused twice: give up, saying so.
	refusals.Store(2)
	_, err := c.ActionsJobLog(context.Background(), "tok", "o/r", 42, &bytes.Buffer{}, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "status 403, again with a fresh download link") {
		t.Fatalf("refused twice: err = %v", err)
	}
	if ls.apiLogHits.Load() != 4 {
		t.Errorf("asked the API for %d links in all, want 4", ls.apiLogHits.Load())
	}
}

func TestActionsStepLog(t *testing.T) {
	var path string
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("the blob host received the token")
		}
		_, _ = w.Write([]byte("step\n"))
	}))
	defer blob.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		http.Redirect(w, r, blob.URL+"/s?sig=x", http.StatusFound)
	}))
	defer api.Close()
	c := NewClient(api.Client(), api.URL)
	var buf bytes.Buffer
	if _, err := c.ActionsStepLog(context.Background(), "tok", "o/r", 42, 3, &buf, 1<<20); err != nil || buf.String() != "step\n" {
		t.Fatalf("step log = %q, %v", buf.String(), err)
	}
	if path != "/repos/o/r/actions/jobs/42/steps/3/logs" {
		t.Errorf("requested %s", path)
	}
	if _, err := c.ActionsStepLog(context.Background(), "tok", "o/r", 42, 0, &buf, 1<<20); err == nil {
		t.Error("step 0 accepted")
	}
}

// The API's own redirect can carry a malformed Location too; net/http's client would
// quote it in an error before CheckRedirect ran.
func TestActionsJobLogRedactsBadAPILocation(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://[::1%zz/logs?sig=s3cret-api-sig")
		w.WriteHeader(http.StatusFound)
	}))
	defer api.Close()
	c := NewClient(api.Client(), api.URL)
	_, err := c.ActionsJobLog(context.Background(), "tok", "o/r", 42, &bytes.Buffer{}, 1<<20)
	if err == nil {
		t.Fatal("a malformed API redirect succeeded")
	}
	for _, leak := range []string{"s3cret", "::1%zz"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaks %q: %v", leak, err)
		}
	}
}

// The API hop still goes through the client's transport, so a credential-adding or
// retrying transport is honoured there (and only there).
func TestActionsJobLogAPIHopUsesClientTransport(t *testing.T) {
	ls := newLogServers(t, "x\n")
	var used atomic.Bool
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		used.Store(true)
		return http.DefaultTransport.RoundTrip(r)
	})}
	c := NewClient(hc, ls.api.URL)
	if _, err := c.ActionsJobLog(context.Background(), "tok", "o/r", 42, &bytes.Buffer{}, 1<<20); err != nil {
		t.Fatal(err)
	}
	if !used.Load() {
		t.Error("the API hop bypassed the client's transport")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A refused log request carries GitHub's own reason and the scope headers, which is
// what tells a missing scope from an organisation's OAuth App access restriction.
func TestActionsJobLogForbiddenCarriesReason(t *testing.T) {
	type resp struct {
		status  int
		headers map[string]string
		body    string
	}
	cases := []struct {
		name    string
		r       resp
		is      error
		want    []string
		notWant []string
	}{
		{
			name: "org restriction",
			r: resp{403, map[string]string{"X-OAuth-Scopes": "read:user, user:email", "X-Accepted-OAuth-Scopes": ""},
				`{"message":"Although you appear to have the correct authorization credentials, the ` + "`example`" + ` organization has enabled OAuth App access restrictions."}`},
			is: ErrLogForbidden,
			want: []string{"job log: access denied (status 403)", "GitHub refused this token access to the job log",
				`GitHub says "Although you appear to have the correct authorization credentials`,
				"the token's scopes: read:user, user:email", "scopes this endpoint accepts: (none)",
				"not anything the job itself printed"},
		},
		{
			name: "missing scope",
			r:    resp{403, map[string]string{"X-OAuth-Scopes": "read:user", "X-Accepted-OAuth-Scopes": "repo"}, `{"message":"Must have admin rights to Repository."}`},
			is:   ErrLogForbidden,
			want: []string{`GitHub says "Must have admin rights to Repository."`, "scopes this endpoint accepts: repo"},
		},
		{
			name:    "rate limit",
			r:       resp{403, map[string]string{"X-RateLimit-Remaining": "0"}, `{"message":"API rate limit exceeded for user."}`},
			is:      ErrLogRateLimited,
			want:    []string{"GitHub rate limit (status 403)", `GitHub says "API rate limit exceeded for user."`},
			notWant: []string{"refused this token access"},
		},
		{
			name: "sso",
			r:    resp{403, map[string]string{"X-GitHub-SSO": "required; url=https://github.com/orgs/example/sso?authorization_request=abc"}, `{"message":"Resource protected by organization SAML enforcement."}`},
			is:   ErrLogForbidden,
			want: []string{"requires SAML SSO authorisation"}, notWant: []string{"authorization_request"},
		},
		{
			name: "not json, long",
			r:    resp{403, nil, strings.Repeat("x", 20000)},
			is:   ErrLogForbidden,
			want: []string{"GitHub refused this token access"}, notWant: []string{"xxxx", "GitHub says"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range c.r.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(c.r.status)
				_, _ = w.Write([]byte(c.r.body))
			}))
			defer api.Close()
			cl := NewClient(api.Client(), api.URL)
			_, err := cl.ActionsJobLog(context.Background(), "tok", "o/r", 42, &bytes.Buffer{}, 1<<20)
			if !errors.Is(err, c.is) {
				t.Fatalf("err = %v, want %v", err, c.is)
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error lacks %q: %v", w, err)
				}
			}
			for _, w := range c.notWant {
				if strings.Contains(err.Error(), w) {
					t.Errorf("error contains %q: %v", w, err)
				}
			}
		})
	}
}

// A long GitHub message is clipped.
func TestReasonClipsMessage(t *testing.T) {
	r := reason{message: clipReason(strings.Repeat("é", 400))}
	if len(r.message) > 304 || !strings.HasSuffix(r.message, "…") {
		t.Errorf("message not clipped: %d bytes", len(r.message))
	}
}

// With the production retry transport, a 429 whose Retry-After outlasts the request
// deadline still reaches the caller as a rate limit, with GitHub's reason, instead
// of a bare deadline error.
func TestActionsJobLogRateLimitThroughRetryTransport(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit."}`))
	}))
	defer api.Close()
	c := NewClient(httpretry.Wrap(&http.Client{Timeout: 2 * time.Second}), api.URL)
	start := time.Now()
	_, err := c.ActionsJobLog(context.Background(), "tok", "o/r", 42, &bytes.Buffer{}, 1<<20)
	if !errors.Is(err, ErrLogRateLimited) || !strings.Contains(err.Error(), "status 429") ||
		!strings.Contains(err.Error(), `GitHub says "You have exceeded a secondary rate limit."`) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("took %s: it waited out the deadline", time.Since(start))
	}
}
