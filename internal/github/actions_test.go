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
	if _, err := g.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"Authorization", "Cookie", "Proxy-Authorization"} {
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
