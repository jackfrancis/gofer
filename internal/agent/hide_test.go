package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/github/githubtest"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// "Hide this item until GitHub updates it", end to end through a re-ingest and the
// plane's merge: an unchanged PR stays hidden across refreshes, and new GitHub
// activity brings it back with its thread intact.
func TestRefreshKeepsHiddenUntilGitHubUpdates(t *testing.T) {
	hiddenAt := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	updated := hiddenAt.Add(-time.Hour) // GitHub's updated_at, as the fake serves it
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_, _ = w.Write([]byte(`{"login":"me"}`))
			return
		}
		if strings.Contains(r.URL.Query().Get("q"), "review-requested:@me") {
			fmt.Fprintf(w, `{"items":[{"number":7,"title":"Fresh title","html_url":"u","state":"open","updated_at":%q,"repository_url":"https://api.github.com/repos/o/r","pull_request":{"url":"x"}}]}`, updated.Format(time.RFC3339))
			return
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()

	const id = "github:o/r#7"
	sink := &memSink{items: []worklist.WorkItem{{
		ID: id, Source: "github", Type: worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Repo: "o/r", Number: 7, Title: "Old title", UpdatedAt: updated},
		Thread: []worklist.Message{
			{ID: "q", Role: worklist.RoleUser, Content: "Can you review this PR?", Kind: worklist.KindReviewRequest},
			{ID: "a", Role: worklist.RoleAgent, Content: "Looks good.", ReplyTo: "q"},
		},
		Meta: worklist.Metadata{HiddenAt: hiddenAt},
	}}}
	refresh := func() worklist.WorkItem {
		t.Helper()
		if err := Run(context.Background(),
			Params{JobType: JobIngest, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client()},
			fakeVendor{token: "tok"}, sink); err != nil {
			t.Fatal(err)
		}
		for _, it := range sink.items {
			if it.ID == id {
				return it
			}
		}
		t.Fatal("item #7 missing after refresh")
		return worklist.WorkItem{}
	}

	for i := 1; i <= 2; i++ {
		got := refresh()
		if !got.Meta.HiddenAt.Equal(hiddenAt) {
			t.Fatalf("refresh %d: an unchanged PR must stay hidden, HiddenAt = %v", i, got.Meta.HiddenAt)
		}
		if got.GitHub.Title != "Fresh title" {
			t.Fatalf("refresh %d: a hidden item still takes fresh GitHub fields, got %q", i, got.GitHub.Title)
		}
	}

	updated = hiddenAt.Add(time.Minute) // someone commented on the PR after the hide
	got := refresh()
	if !got.Meta.HiddenAt.IsZero() || !got.OnRadar() {
		t.Fatalf("new GitHub activity must resurface the item, HiddenAt = %v", got.Meta.HiddenAt)
	}
	if len(got.Thread) != 2 || got.Thread[1].Content != "Looks good." {
		t.Fatalf("a resurfaced item keeps its thread, got %+v", got.Thread)
	}
}
