package github

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient points a Client at a test server.
func newTestClient(srv *httptest.Server) *Client {
	return NewClient(srv.Client(), srv.URL)
}

func TestFileContents(t *testing.T) {
	const body = "module foo\n\ngo 1.26.0\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/contents/go.mod" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("ref") != "main" {
			t.Errorf("ref = %q, want main", r.URL.Query().Get("ref"))
		}
		enc := base64.StdEncoding.EncodeToString([]byte(body))
		_, _ = w.Write([]byte(`{"type":"file","encoding":"base64","content":"` + enc + `"}`))
	}))
	defer srv.Close()

	got, err := newTestClient(srv).FileContents(context.Background(), "tok", "o/r", "go.mod", "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != body {
		t.Fatalf("FileContents = %q, want %q", got, body)
	}
}

func TestFileContentsDirectoryAndBinary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/contents/dir":
			_, _ = w.Write([]byte(`[{"name":"a"},{"name":"b"}]`)) // a directory is a JSON array
		case "/repos/o/r/contents/blob.bin":
			enc := base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe, 0x00})
			_, _ = w.Write([]byte(`{"type":"file","encoding":"base64","content":"` + enc + `"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(srv)
	if got, _ := c.FileContents(context.Background(), "tok", "o/r", "dir", "", 0); !strings.Contains(got, "directory") {
		t.Fatalf("directory case = %q", got)
	}
	if got, _ := c.FileContents(context.Background(), "tok", "o/r", "blob.bin", "", 0); !strings.Contains(got, "binary") {
		t.Fatalf("binary case = %q", got)
	}
}

func TestPullRequestStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/pulls/7" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"number":7,"state":"closed","merged":true,"merged_at":"2026-01-02T03:04:05Z","title":"Bump dep","base":{"ref":"main"},"head":{"ref":"feature","sha":"abc123"},"html_url":"https://github.com/o/r/pull/7"}`))
	}))
	defer srv.Close()

	got, err := newTestClient(srv).PullRequestStatus(context.Background(), "tok", "o/r", 7)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PR o/r#7", "state=closed", "merged", "base=main", "head=feature", "head commit=abc123"} {
		if !strings.Contains(got, want) {
			t.Fatalf("PullRequestStatus missing %q in:\n%s", want, got)
		}
	}
}

func TestIssueStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/issues/9" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"number":9,"state":"open","title":"Flaky test","html_url":"https://github.com/o/r/issues/9"}`))
	}))
	defer srv.Close()

	got, err := newTestClient(srv).IssueStatus(context.Background(), "tok", "o/r", 9)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "issue o/r#9") || !strings.Contains(got, "state=open") {
		t.Fatalf("IssueStatus = %q", got)
	}
}

func TestSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/issues" {
			http.NotFound(w, r)
			return
		}
		if q := r.URL.Query().Get("q"); q != "repo:o/r otel" {
			t.Errorf("q = %q", q)
		}
		_, _ = w.Write([]byte(`{"items":[` +
			`{"number":7,"title":"Add otel","state":"open","html_url":"https://github.com/o/r/pull/7","repository_url":"https://api.github.com/repos/o/r","pull_request":{"url":"x"}},` +
			`{"number":8,"title":"otel bug","state":"closed","html_url":"https://github.com/o/r/issues/8","repository_url":"https://api.github.com/repos/o/r"}` +
			`]}`))
	}))
	defer srv.Close()

	got, err := newTestClient(srv).Search(context.Background(), "tok", "repo:o/r otel")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"2 result(s)", "PR o/r#7", "issue o/r#8", "[open]", "[closed]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Search missing %q in:\n%s", want, got)
		}
	}
}

func TestDiscussionPullRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/issues/7":
			_, _ = w.Write([]byte(`{"body":"Please review this change."}`))
		case "/repos/o/r/issues/7/comments":
			_, _ = w.Write([]byte(`[{"body":"looks good","user":{"login":"bob"},"created_at":"2026-01-01T00:00:00Z"}]`))
		case "/repos/o/r/pulls/7":
			_, _ = w.Write([]byte(`{"head":{"sha":"deadbeef"}}`))
		case "/repos/o/r/pulls/7/reviews":
			_, _ = w.Write([]byte(`[{"body":"needs work","state":"CHANGES_REQUESTED","user":{"login":"carol"},"submitted_at":"2026-01-01T01:00:00Z"},{"body":"","state":"COMMENTED","user":{"login":"dan"}}]`))
		case "/repos/o/r/pulls/7/comments":
			_, _ = w.Write([]byte(`[{"body":"nit","path":"main.go","user":{"login":"carol"},"created_at":"2026-01-01T02:00:00Z"}]`))
		case "/repos/o/r/pulls/7/files":
			_, _ = w.Write([]byte(`[{"filename":"main.go"},{"filename":"go.mod"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	d, err := newTestClient(srv).Discussion(context.Background(), "tok", "o/r", 7, true)
	if err != nil {
		t.Fatal(err)
	}
	if d.Body != "Please review this change." {
		t.Fatalf("body = %q", d.Body)
	}
	if len(d.Comments) != 1 || d.Comments[0].Author != "bob" {
		t.Fatalf("comments = %+v", d.Comments)
	}
	if d.HeadSHA != "deadbeef" {
		t.Fatalf("head sha = %q", d.HeadSHA)
	}
	// The empty "commented" review is dropped; only the substantive one remains.
	if len(d.Reviews) != 1 || d.Reviews[0].State != "changes_requested" {
		t.Fatalf("reviews = %+v", d.Reviews)
	}
	if len(d.ReviewComments) != 1 || !strings.HasPrefix(d.ReviewComments[0].Body, "[main.go]") {
		t.Fatalf("review comments = %+v", d.ReviewComments)
	}
	if len(d.ChangedFiles) != 2 || d.ChangedFiles[0] != "main.go" {
		t.Fatalf("changed files = %+v", d.ChangedFiles)
	}
}

func TestDiscussionIssueSkipsPRFetches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/pulls/") {
			t.Errorf("issue discussion should not hit %s", r.URL.Path)
		}
		switch r.URL.Path {
		case "/repos/o/r/issues/3":
			_, _ = w.Write([]byte(`{"body":"an issue"}`))
		case "/repos/o/r/issues/3/comments":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	d, err := newTestClient(srv).Discussion(context.Background(), "tok", "o/r", 3, false)
	if err != nil {
		t.Fatal(err)
	}
	if d.Body != "an issue" || d.HeadSHA != "" || len(d.ChangedFiles) != 0 {
		t.Fatalf("unexpected issue discussion: %+v", d)
	}
}
