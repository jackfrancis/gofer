package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/github/githubtest"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// A hidden item, end to end through re-ingests and the plane's merge: it stays
// hidden however GitHub changes, takes fresh GitHub fields but no signals or score
// (it is neither enriched nor ranked), and keeps its thread. Unhidden, it comes back
// empty and the next refresh fills it in.
func TestRefreshKeepsHiddenItemHiddenAndUnscored(t *testing.T) {
	hiddenAt := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	updated := hiddenAt.Add(-time.Hour) // GitHub's updated_at, as the fake serves it
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_, _ = w.Write([]byte(`{"login":"me"}`))
			return
		}
		if strings.Contains(r.URL.Query().Get("q"), "review-requested:@me") {
			fmt.Fprintf(w, `{"items":[{"number":7,"title":"Fresh title","html_url":"u","state":"open","updated_at":%q,"repository_url":"https://api.github.com/repos/o/r","pull_request":{"url":"x"},"labels":[{"name":"bug","color":"d73a4a"}]}]}`, updated.Format(time.RFC3339))
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

	for i, at := range []time.Time{updated, hiddenAt.Add(time.Minute)} { // unchanged, then new activity
		updated = at
		got := refresh()
		if !got.Meta.HiddenAt.Equal(hiddenAt) {
			t.Fatalf("refresh %d: a hidden PR must stay hidden, HiddenAt = %v", i, got.Meta.HiddenAt)
		}
		if got.GitHub.Title != "Fresh title" || len(got.Signals.Labels) != 1 || got.GitHub.LabelColor("bug") == "" {
			t.Fatalf("refresh %d: a hidden item still takes fresh GitHub fields and labels, got %+v", i, got)
		}
		if len(got.Signals.Reasons) != 0 || got.Signals.Proposed != nil || got.Meta.Rank != 0 || got.Meta.Rationale != "" || !got.Signals.ObservedAt.IsZero() {
			t.Fatalf("refresh %d: a hidden item carries no signals or score, got %+v %+v", i, got.Signals, got.Meta)
		}
		if len(got.Thread) != 2 || got.Thread[1].Content != "Looks good." {
			t.Fatalf("refresh %d: a hidden item keeps its thread, got %+v", i, got.Thread)
		}
	}

	// Unhide lands after this run read the (hidden) item and before its write-back:
	// the run must still have sent the fresh signals, which the store's merge keeps
	// now that the item is visible.
	sink.beforeIngest = func() {
		for i := range sink.items {
			if sink.items[i].ID == id {
				worklist.UnhideItem(&sink.items[i])
			}
		}
	}
	got := refresh()
	if got.IsHidden() || got.Meta.AwaitingRefresh || !got.OnRadar() {
		t.Fatalf("an unhidden item is back on the radar once refreshed, got %+v", got.Meta)
	}
	if !slices.Contains(got.Signals.Reasons, worklist.ReasonReviewRequested) || got.Signals.ObservedAt.IsZero() || got.Signals.Proposed == nil {
		t.Fatalf("the refresh fills an unhidden item in (fresh signals, then a ranking), got %+v", got.Signals)
	}
}

// A ranking pass leaves an item awaiting its post-unhide refresh alone: it holds no
// signals (a partial fetch may have missed it), and a proposal made from nothing
// would outlive the refresh that fills it in if that pass's ranking failed.
func TestRankSkipsItemAwaitingRefresh(t *testing.T) {
	sink := &memSink{items: []worklist.WorkItem{
		{ID: "github:o/r#1", Source: "github", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1},
			Meta: worklist.Metadata{AwaitingRefresh: true}},
		{ID: "github:o/r#2", Source: "github", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: "o/r", Number: 2},
			Signals: worklist.Signals{Reasons: []worklist.Reason{worklist.ReasonReviewRequested}}},
	}}
	if err := Run(context.Background(), Params{JobType: JobRank}, fakeVendor{token: "tok"}, sink); err != nil {
		t.Fatal(err)
	}
	for _, it := range sink.items {
		switch {
		case it.ID == "github:o/r#1" && (it.Signals.Proposed != nil || !it.Meta.AwaitingRefresh):
			t.Fatalf("the item awaiting refresh must not be ranked, got %+v %+v", it.Signals.Proposed, it.Meta)
		case it.ID == "github:o/r#2" && it.Signals.Proposed == nil:
			t.Fatal("test premise: the other item is ranked")
		}
	}
}

// An unhidden PR that the next complete fetch finds closed is retired: it awaits no
// refresh any more (none revisits it), and keeps its CI as the last known result.
func TestRetiringAnUnhiddenItemClearsTheWait(t *testing.T) {
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_, _ = w.Write([]byte(`{"login":"me"}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[]}`)) // a complete fetch: it is no longer open
	}))
	defer srv.Close()
	const id = "github:o/r#7"
	it := worklist.WorkItem{ID: id, Source: "github", Type: worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Repo: "o/r", Number: 7, CI: &worklist.CISummary{SHA: "abc", Passed: 2}},
		Meta:   worklist.Metadata{HiddenAt: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}}
	worklist.UnhideItem(&it)
	sink := &memSink{items: []worklist.WorkItem{it}}
	if err := Run(context.Background(),
		Params{JobType: JobIngest, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client()},
		fakeVendor{token: "tok"}, sink); err != nil {
		t.Fatal(err)
	}
	got := sink.items[0]
	if got.Meta.CompletedAt.IsZero() {
		t.Fatal("test premise: the complete fetch retires the item")
	}
	if got.Meta.AwaitingRefresh || got.GitHub.CI == nil || got.GitHub.CI.Passed != 2 {
		t.Fatalf("a retired item awaits nothing and keeps its last known CI, got %+v %+v", got.Meta, got.GitHub.CI)
	}
}
