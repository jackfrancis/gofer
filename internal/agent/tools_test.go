package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackfrancis/gofer/internal/github"
)

// githubToolServer serves the minimal GitHub endpoints the toolbox calls.
func githubToolServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/contents/go.mod":
			enc := base64.StdEncoding.EncodeToString([]byte("module foo\n"))
			_, _ = w.Write([]byte(`{"type":"file","encoding":"base64","content":"` + enc + `"}`))
		case r.URL.Path == "/repos/o/r/pulls/7":
			_, _ = w.Write([]byte(`{"number":7,"state":"open","title":"t","base":{"ref":"main"},"head":{"ref":"f","sha":"s"},"html_url":"u"}`))
		case r.URL.Path == "/repos/o/r/issues/9":
			_, _ = w.Write([]byte(`{"number":9,"state":"open","title":"t","html_url":"u"}`))
		case r.URL.Path == "/search/issues":
			_, _ = w.Write([]byte(`{"items":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestGitHubToolBoxDefinitions(t *testing.T) {
	box := newGitHubToolBox(github.NewClient(http.DefaultClient, "https://example.test"), "tok", "o/r")
	defs := box.Definitions()
	got := make(map[string]bool)
	for _, d := range defs {
		got[d.Name] = true
		if len(d.Parameters) == 0 {
			t.Errorf("tool %q has empty parameters schema", d.Name)
		}
	}
	for _, want := range []string{"github_read_file", "github_get_pull_request", "github_get_issue", "github_search"} {
		if !got[want] {
			t.Errorf("missing tool %q", want)
		}
	}
}

func TestGitHubToolBoxInvokeDispatch(t *testing.T) {
	srv := githubToolServer(t)
	defer srv.Close()
	box := newGitHubToolBox(github.NewClient(srv.Client(), srv.URL), "tok", "o/r")
	ctx := context.Background()

	// repo omitted -> defaults to the item's repo (o/r).
	if got, err := box.Invoke(ctx, "github_read_file", json.RawMessage(`{"path":"go.mod"}`)); err != nil || !strings.Contains(got, "module foo") {
		t.Fatalf("github_read_file = %q, err=%v", got, err)
	}
	if got, err := box.Invoke(ctx, "github_get_pull_request", json.RawMessage(`{"number":7}`)); err != nil || !strings.Contains(got, "PR o/r#7") {
		t.Fatalf("github_get_pull_request = %q, err=%v", got, err)
	}
	if got, err := box.Invoke(ctx, "github_get_issue", json.RawMessage(`{"number":9}`)); err != nil || !strings.Contains(got, "issue o/r#9") {
		t.Fatalf("github_get_issue = %q, err=%v", got, err)
	}
	if _, err := box.Invoke(ctx, "github_search", json.RawMessage(`{"query":"otel"}`)); err != nil {
		t.Fatalf("github_search err=%v", err)
	}
}

func TestGitHubToolBoxInvokeValidation(t *testing.T) {
	box := newGitHubToolBox(github.NewClient(http.DefaultClient, "https://example.test"), "tok", "o/r")
	ctx := context.Background()
	cases := []struct {
		name string
		args string
		want string
	}{
		{"github_read_file", `{}`, "path is required"},
		{"github_get_pull_request", `{}`, "number is required"},
		{"github_get_issue", `{"repo":"x/y"}`, "number is required"},
		{"github_search", `{}`, "query is required"},
		{"github_read_file", `{bad json`, "bad arguments"},
		{"nope", `{}`, `unknown tool "nope"`},
	}
	for _, c := range cases {
		_, err := box.Invoke(ctx, c.name, json.RawMessage(c.args))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Invoke(%s, %s) err = %v, want containing %q", c.name, c.args, err, c.want)
		}
	}
}
