package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackfrancis/gofer/internal/worklist"
)

func TestFetchWorklistMapsAndDedups(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/issues" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query().Get("q")
		// The same PR surfaces under the author and review-requested queries, so
		// the two reasons must merge onto one deduped item.
		if strings.Contains(q, "author:@me") || strings.Contains(q, "review-requested:@me") {
			_, _ = w.Write([]byte(`{"items":[{"number":42,"title":"Fix bug","html_url":"https://github.com/o/r/pull/42","state":"open","comments":3,"repository_url":"https://api.github.com/repos/o/r","pull_request":{"url":"x"},"labels":[{"name":"kind/bug"}]}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL)
	items, err := c.FetchWorklist(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 deduped item, got %d", len(items))
	}
	it := items[0]
	if it.ID != "github:o/r#42" {
		t.Fatalf("bad id %q", it.ID)
	}
	if it.Type != worklist.TypePullRequest {
		t.Fatalf("want PR type, got %q", it.Type)
	}
	if it.GitHub.Repo != "o/r" || it.GitHub.Number != 42 {
		t.Fatalf("bad ref %+v", it.GitHub)
	}
	if it.Signals.Comments != 3 {
		t.Fatalf("want 3 comments, got %d", it.Signals.Comments)
	}
	if len(it.Signals.Reasons) != 2 {
		t.Fatalf("want author+review_requested reasons merged, got %v", it.Signals.Reasons)
	}
	if it.Meta.Origin != worklist.OriginAgent {
		t.Fatalf("want agent origin, got %q", it.Meta.Origin)
	}
}
