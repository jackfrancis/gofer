package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/github/githubtest"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// runEnrich fetches per-item timeline signals and updates the item's Signals.
func TestRunEnrichUpdatesSignals(t *testing.T) {
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"me"}`))
		case strings.Contains(r.URL.Path, "/timeline"):
			_, _ = w.Write([]byte(`[{"event":"review_requested","created_at":"2026-01-02T00:00:00Z","requested_reviewer":{"login":"me"},"actor":{"login":"bob"}}]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	sink := &memSink{items: []worklist.WorkItem{{
		ID: "github:o/r#1", Source: "github",
		GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1},
	}}}
	err := Run(context.Background(),
		Params{JobType: JobEnrich, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client()},
		fakeVendor{token: "t"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	s := sink.items[0].Signals
	if s.AwaitingMeSince.IsZero() {
		t.Fatal("expected AwaitingMeSince set after enrich")
	}
	if s.ReviewRequestedBy != "bob" {
		t.Fatalf("expected ReviewRequestedBy=bob, got %q", s.ReviewRequestedBy)
	}
}

// enrich settles what a union membership query cannot: the timeline proves whether
// the user actually spoke, so the honest generic "involved" reason is upgraded to
// "commented". A stronger, already-proven relationship is left alone.
func TestRunEnrichUpgradesInvolvedReason(t *testing.T) {
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"me"}`))
		case strings.Contains(r.URL.Path, "/timeline"):
			_, _ = w.Write([]byte(`[{"event":"commented","created_at":"2026-01-02T00:00:00Z","actor":{"login":"me"}}]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	sink := &memSink{items: []worklist.WorkItem{
		{ID: "involved", Source: "github", GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1},
			Signals: worklist.Signals{Reasons: []worklist.Reason{worklist.ReasonInvolved}}},
		{ID: "authored", Source: "github", GitHub: worklist.GitHubRef{Repo: "o/r", Number: 2},
			Signals: worklist.Signals{Reasons: []worklist.Reason{worklist.ReasonAuthor}}},
	}}
	if err := Run(context.Background(),
		Params{JobType: JobEnrich, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client()},
		fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	got := map[string][]worklist.Reason{}
	for _, it := range sink.items {
		got[it.ID] = it.Signals.Reasons
	}
	if len(got["involved"]) != 1 || got["involved"][0] != worklist.ReasonCommented {
		t.Errorf("involved item should be upgraded to commented, got %v", got["involved"])
	}
	if len(got["authored"]) != 1 || got["authored"][0] != worklist.ReasonAuthor {
		t.Errorf("a proven relationship must be left alone, got %v", got["authored"])
	}
}

// enrich covers EVERY item, not a capped prefix: it feeds the deterministic baseline
// score, so a skipped item is scored from missing signals and can never rise. It
// reads the worklist a bounded page at a time and pages through all of it, and its
// write-back is batched, so a large worklist never crosses in one oversized body.
func TestRunEnrichCoversEveryItemInBatches(t *testing.T) {
	var (
		mu        sync.Mutex
		timelines int
	)
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"me"}`))
		case strings.Contains(r.URL.Path, "/timeline"):
			mu.Lock()
			timelines++
			mu.Unlock()
			_, _ = w.Write([]byte(`[{"event":"review_requested","created_at":"2026-01-02T00:00:00Z","requested_reviewer":{"login":"me"},"actor":{"login":"bob"}}]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	const total = 60 // comfortably past the old hard-coded 50
	sink := &memSink{}
	for i := 1; i <= total; i++ {
		sink.items = append(sink.items, worklist.WorkItem{
			ID: "github:o/r#" + strconv.Itoa(i), Source: "github",
			GitHub: worklist.GitHubRef{Repo: "o/r", Number: i},
		})
	}

	const pageSize = 7
	err := Run(context.Background(),
		Params{JobType: JobEnrich, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client(), ReadPageSize: pageSize},
		fakeVendor{token: "t"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if want := (total + pageSize - 1) / pageSize; len(sink.selects) != want {
		t.Fatalf("enrich read %d pages, want %d pages of %d", len(sink.selects), want, pageSize)
	}
	for _, sel := range sink.selects {
		if sel.Limit != pageSize || sel.Top != 0 || !sel.Open {
			t.Fatalf("enrich read %+v, want open pages of %d", sel, pageSize)
		}
	}
	if timelines != total {
		t.Fatalf("enriched %d items, want all %d", timelines, total)
	}
	for _, it := range sink.items {
		if it.Signals.ReviewRequestedBy != "bob" {
			t.Fatalf("%s was not enriched", it.ID)
		}
	}
	if want := (total + writeBackChunk - 1) / writeBackChunk; sink.ingests != want {
		t.Fatalf("write-back made %d calls, want %d batches of %d", sink.ingests, want, writeBackChunk)
	}
}

// When the rank cap does bite, it spends the model budget on the items the
// deterministic baseline rates highest — not on an arbitrary slice of the store.
func TestRunRankPicksHighestBaseline(t *testing.T) {
	// A pending review request outranks a bare "commented" relationship on the baseline.
	hot := worklist.WorkItem{ID: "hot", Source: "github", Signals: worklist.Signals{
		Reasons: []worklist.Reason{worklist.ReasonReviewRequested},
	}}
	cold := worklist.WorkItem{ID: "cold", Source: "github", Signals: worklist.Signals{
		Reasons: []worklist.Reason{worklist.ReasonCommented},
	}}
	// Store order deliberately puts the cold item first, so a prefix cap would pick it.
	sink := &memSink{items: []worklist.WorkItem{cold, hot}}

	if err := Run(context.Background(), Params{JobType: JobRank, RankLimit: 1}, fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	var ranked []string
	for _, it := range sink.items {
		if it.Signals.Proposed != nil {
			ranked = append(ranked, it.ID)
		}
	}
	if len(ranked) != 1 || ranked[0] != "hot" {
		t.Fatalf("rank should spend its one slot on the highest-baseline item, got %v", ranked)
	}
}

// Timeline reads end at the enrichment deadline (the run deadline less
// enrichReserve), not the run deadline: a slow, multi-page item cannot hold the
// batch past the time kept for writing back what the other items found.
func TestRunEnrichTimelineReadsKeepTheReserve(t *testing.T) {
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"me"}`))
		case strings.Contains(r.URL.Path, "/issues/2/timeline"):
			select { // hang until the client gives up
			case <-r.Context().Done():
			case <-time.After(20 * time.Second):
			}
		case strings.Contains(r.URL.Path, "/timeline"):
			_, _ = w.Write([]byte(`[{"event":"review_requested","created_at":"2026-01-02T00:00:00Z","requested_reviewer":{"login":"me"},"actor":{"login":"bob"}}]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	sink := &memSink{items: []worklist.WorkItem{
		{ID: "github:o/r#1", Source: "github", GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1}},
		{ID: "github:o/r#2", Source: "github", GitHub: worklist.GitHubRef{Repo: "o/r", Number: 2}},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), enrichReserve+time.Second)
	defer cancel()
	start := time.Now()
	err := Run(ctx, Params{JobType: JobEnrich, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client()},
		fakeVendor{token: "t"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("enrich took %v; the hung timeline read was not cut at the enrichment deadline", elapsed)
	}
	if got := itemByID(t, sink.items, "github:o/r#1").Signals.ReviewRequestedBy; got != "bob" {
		t.Fatalf("item 1 ReviewRequestedBy = %q, want its result written back", got)
	}
}

// enrich summarizes the CI on each open pull request's head commit. A pull request
// whose CI cannot be read keeps the stored summary, and an issue has no CI to read.
func TestRunEnrichSummarizesPullRequestCI(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"login":"me"}`))
		case "/repos/o/r/pulls/1":
			_, _ = w.Write([]byte(`{"head":{"ref":"b","sha":"sha1"}}`))
		case "/repos/o/r/commits/sha1/status":
			_, _ = w.Write([]byte(`{"sha":"sha1","total_count":2,"statuses":[
				{"context":"pull-unit","state":"success"},{"context":"pull-e2e","state":"failure","target_url":"https://prow.k8s.io/view/gs/b/pull-e2e/1"}]}`))
		case "/repos/o/r/commits/sha1/check-runs":
			_, _ = w.Write([]byte(`{"total_count":2,"check_runs":[
				{"name":"lint","status":"in_progress"},{"name":"build","status":"completed","conclusion":"failure","details_url":"https://github.com/o/r/actions/runs/5"}]}`))
		case "/repos/o/r/pulls/2":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	kept := &worklist.CISummary{SHA: "old", Passed: 4}
	sink := &memSink{items: []worklist.WorkItem{
		{ID: "pr1", Source: "github", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1}},
		{ID: "pr2", Source: "github", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: "o/r", Number: 2, CI: kept}},
		{ID: "issue3", Source: "github", Type: worklist.TypeIssue, GitHub: worklist.GitHubRef{Repo: "o/r", Number: 3}},
	}}
	if err := Run(context.Background(),
		Params{JobType: JobEnrich, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client()},
		fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	got := map[string]*worklist.CISummary{}
	for _, it := range sink.items {
		got[it.ID] = it.GitHub.CI
	}
	want := &worklist.CISummary{SHA: "sha1", Passed: 1, Pending: 1, Failed: []string{"build", "pull-e2e"},
		FailedURLs: []string{"https://github.com/o/r/actions/runs/5", "https://prow.k8s.io/view/gs/b/pull-e2e/1"}}
	if !got["pr1"].Equal(want) {
		t.Errorf("pr1 CI = %+v, want %+v", got["pr1"], want)
	}
	if !got["pr2"].Equal(kept) {
		t.Errorf("an unreadable CI must keep the stored summary, got %+v", got["pr2"])
	}
	if got["issue3"] != nil {
		t.Errorf("an issue has no CI, got %+v", got["issue3"])
	}
	for _, p := range paths {
		if strings.Contains(p, "/pulls/3") {
			t.Errorf("enrich read CI for an issue: %s", p)
		}
	}
}

// A refresh's search hits carry no CI, and the run's read of an oversized item is a
// projection whose failed-job list is cut: the refresh must not copy that cut list,
// and the write-back keeps the stored summary whole (worklist.KeepCI).
func TestRefreshKeepsWholeStoredCI(t *testing.T) {
	ci := &worklist.CISummary{SHA: "s", Passed: 2}
	for i := range 40 {
		ci.Failed = append(ci.Failed, "job-"+strconv.Itoa(i))
	}
	stored := []worklist.WorkItem{
		{ID: "pr", Type: worklist.TypePullRequest,
			GitHub: worklist.GitHubRef{Title: strings.Repeat("t", worklist.MaxBriefBytes), CI: ci}},
		{ID: "was-pr", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{CI: ci}},
	}
	read := []worklist.WorkItem{worklist.Project(stored[0]), stored[1]}
	if len(read[0].GitHub.CI.Failed) >= 40 {
		t.Fatal("fixture must be projected")
	}
	fetched := []worklist.WorkItem{
		{ID: "pr", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Title: "fresh"}},
		{ID: "was-pr", Type: worklist.TypeIssue},
	}
	out, _ := reconcileFetched(fetched, read, true)
	merged := worklist.MergeRunItems(stored, out)
	if !merged[0].GitHub.CI.Equal(ci) {
		t.Errorf("refresh must keep the stored CI whole, got %d names, %d omitted",
			len(merged[0].GitHub.CI.Failed), merged[0].GitHub.CI.FailedOmitted)
	}
	if merged[1].GitHub.CI != nil {
		t.Errorf("only a pull request carries CI, got %+v", merged[1].GitHub.CI)
	}
}

// The timeline feeds the score and CI is display-only, so CI reads that hang until
// the enrich cutoff on every pull request must not keep an issue's timeline from
// being read.
func TestRunEnrichSlowCIDoesNotStarveTimelines(t *testing.T) {
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"me"}`))
		case strings.Contains(r.URL.Path, "/timeline"):
			_, _ = w.Write([]byte(`[{"event":"commented","created_at":"2026-01-02T00:00:00Z","actor":{"login":"bob"}}]`))
		case strings.Contains(r.URL.Path, "/pulls/"):
			<-r.Context().Done() // a CI outage: the read never answers
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	var items []worklist.WorkItem
	for n := 1; n <= enrichConcurrency; n++ {
		items = append(items, worklist.WorkItem{ID: "pr" + strconv.Itoa(n), Source: "github",
			Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: "o/r", Number: n}})
	}
	items = append(items, worklist.WorkItem{ID: "z-issue", Source: "github", Type: worklist.TypeIssue, // sorts after every PR
		GitHub: worklist.GitHubRef{Repo: "o/r", Number: 99}})
	sink := &memSink{items: items}
	ctx, cancel := context.WithTimeout(context.Background(), enrichReserve+2*time.Second)
	defer cancel()
	if err := Run(ctx, Params{JobType: JobEnrich, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client()},
		fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	for _, it := range sink.items {
		if it.Signals.Participants != 1 {
			t.Errorf("%s: participants = %d, want 1 (timeline starved by CI reads)", it.ID, it.Signals.Participants)
		}
	}
}

// Rank reads CI with the item but never fetches it: an enrich that lands while the
// model works must not be undone by rank's write-back of the copy it read.
func TestRunRankKeepsNewerCI(t *testing.T) {
	read := &worklist.CISummary{SHA: "s", Pending: 1}
	newer := &worklist.CISummary{SHA: "s", Failed: []string{"build"}}
	sink := &memSink{items: []worklist.WorkItem{{ID: "pr", Source: "github", Type: worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{CI: read}}}}
	sink.beforeIngest = func() { sink.items[0].GitHub.CI = newer }
	if err := Run(context.Background(), Params{JobType: JobRank}, fakeVendor{}, sink); err != nil {
		t.Fatal(err)
	}
	if sink.items[0].Signals.Proposed == nil {
		t.Fatal("fixture: rank must write the item back")
	}
	if !sink.items[0].GitHub.CI.Equal(newer) {
		t.Fatalf("rank overwrote the newer CI with %+v", sink.items[0].GitHub.CI)
	}
}

// Retiring an item sends no CI either, so the stored summary stands.
func TestRetireMissingSendsNoCI(t *testing.T) {
	existing := []worklist.WorkItem{{ID: "pr", Source: "github", Type: worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{CI: &worklist.CISummary{Pending: 1}}}}
	retired := retireMissing(nil, existing, time.Now())
	if len(retired) != 1 || retired[0].GitHub.CI != nil {
		t.Fatalf("retired = %+v", retired)
	}
}
