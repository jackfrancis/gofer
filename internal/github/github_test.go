package github

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/github/githubtest"
	"github.com/jackfrancis/gofer/internal/worklist"
)

func TestFetchWorklistMapsAndDedups(t *testing.T) {
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_, _ = w.Write([]byte(`{"login":"me"}`))
			return
		}
		if r.URL.Path != "/search/issues" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query().Get("q")
		// The same PR surfaces under the involves and review-requested queries, so the
		// two reasons must merge onto one deduped item.
		if strings.Contains(q, "involves:@me") || strings.Contains(q, "review-requested:@me") {
			_, _ = w.Write([]byte(`{"items":[{"number":42,"title":"Fix bug","html_url":"https://github.com/o/r/pull/42","state":"open","comments":3,"repository_url":"https://api.github.com/repos/o/r","user":{"login":"me"},"pull_request":{"url":"x"},"labels":[{"name":"kind/bug","color":"D73A4A"},{"name":"odd","color":"red}*{x"}]}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL)
	items, _, err := c.FetchWorklist(context.Background(), "tok")
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
	// Every label is kept; only a well-formed color, normalized, comes with it.
	if !slices.Equal(it.Signals.Labels, []string{"kind/bug", "odd"}) {
		t.Fatalf("labels %v", it.Signals.Labels)
	}
	if !maps.Equal(it.GitHub.LabelColors, map[string]string{"kind/bug": "d73a4a"}) {
		t.Fatalf("label colors %v", it.GitHub.LabelColors)
	}
	if len(it.Signals.Reasons) != 2 {
		t.Fatalf("want author+review_requested reasons merged, got %v", it.Signals.Reasons)
	}
	if it.Meta.Origin != worklist.OriginAgent {
		t.Fatalf("want agent origin, got %q", it.Meta.Origin)
	}
}

// Membership comes from one union query, so the specific relationship is derived from
// each item's payload: authored and assigned are provable, everything else stays the
// honest generic reason until enrich can prove participation from the timeline.
// review-requested comes from its own query and needs no derivation.
func TestFetchWorklistDerivesReasons(t *testing.T) {
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_, _ = w.Write([]byte(`{"login":"me"}`))
			return
		}
		q := r.URL.Query().Get("q")
		switch {
		case strings.Contains(q, "involves:@me"):
			_, _ = w.Write([]byte(`{"items":[
				{"number":1,"repository_url":"https://api.github.com/repos/o/r","state":"open","user":{"login":"me"}},
				{"number":2,"repository_url":"https://api.github.com/repos/o/r","state":"open","user":{"login":"someone"},"assignees":[{"login":"me"}]},
				{"number":3,"repository_url":"https://api.github.com/repos/o/r","state":"open","user":{"login":"someone"}}]}`))
		case strings.Contains(q, "review-requested:@me"):
			_, _ = w.Write([]byte(`{"items":[{"number":4,"repository_url":"https://api.github.com/repos/o/r","state":"open","user":{"login":"someone"},"pull_request":{"url":"x"}},
				{"number":5,"repository_url":"https://api.github.com/repos/o/r","state":"open","user":{"login":"someone"},"assignees":[{"login":"Me"}],"pull_request":{"url":"x"}}]}`))
		default:
			t.Errorf("unexpected query %q", q)
			_, _ = w.Write([]byte(`{"items":[]}`))
		}
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL)
	items, _, err := c.FetchWorklist(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	got := map[int][]worklist.Reason{}
	for _, it := range items {
		got[it.GitHub.Number] = it.Signals.Reasons
	}
	want := map[int]worklist.Reason{
		1: worklist.ReasonAuthor,          // proven by user.login
		2: worklist.ReasonAssignee,        // proven by assignees[]
		3: worklist.ReasonInvolved,        // mentioned or commented — the union cannot say
		4: worklist.ReasonReviewRequested, // its own query already knows
	}
	// #5 came only from the review search (the union search missed it, as when it is
	// partial); the assignment its payload proves is kept beside the request.
	if r := got[5]; len(r) != 2 || r[0] != worklist.ReasonReviewRequested || r[1] != worklist.ReasonAssignee {
		t.Errorf("item #5 reasons = %v, want [review_requested assignee]", r)
	}
	delete(got, 5)
	if len(got) != len(want) {
		t.Fatalf("want %d items, got %v", len(want), got)
	}
	for n, reason := range want {
		if len(got[n]) != 1 || got[n][0] != reason {
			t.Errorf("item #%d reasons = %v, want [%s]", n, got[n], reason)
		}
	}
}

// Dedup runs through a map, so FetchWorklist sorts before returning: without it the
// order is Go's randomized map iteration and every downstream "first N" picks a
// different arbitrary subset on each run. Most recently updated first.
func TestFetchWorklistOrdersByRecency(t *testing.T) {
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_, _ = w.Write([]byte(`{"login":"me"}`))
			return
		}
		if !strings.Contains(r.URL.Query().Get("q"), "involves:@me") {
			_, _ = w.Write([]byte(`{"items":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[
			{"number":1,"updated_at":"2026-01-01T00:00:00Z","repository_url":"https://api.github.com/repos/o/r","state":"open"},
			{"number":2,"updated_at":"2026-07-01T00:00:00Z","repository_url":"https://api.github.com/repos/o/r","state":"open"},
			{"number":3,"updated_at":"2026-03-01T00:00:00Z","repository_url":"https://api.github.com/repos/o/r","state":"open"}]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL)
	for attempt := 0; attempt < 3; attempt++ { // map order varies per iteration; the result must not
		items, _, err := c.FetchWorklist(context.Background(), "tok")
		if err != nil {
			t.Fatal(err)
		}
		var got []int
		for _, it := range items {
			got = append(got, it.GitHub.Number)
		}
		if len(got) != 3 || got[0] != 2 || got[1] != 3 || got[2] != 1 {
			t.Fatalf("want newest-first [2 3 1], got %v", got)
		}
	}
}

// A query larger than one page is walked to the end: the client follows the Link
// header's rel="next" until the API stops offering one, and merges every page. A
// single-page sample would silently drop work off the radar.
func TestSearchIssuesPaginates(t *testing.T) {
	var pages []string
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want the API maximum 100", got)
		}
		if got := r.URL.Query().Get("sort"); got != "updated" {
			t.Errorf("sort = %q, want updated (so a capped query keeps the freshest results)", got)
		}
		switch page {
		case "1":
			// Advertise a next page exactly as GitHub does.
			w.Header().Set("Link", `<`+r.URL.String()+`&page=2>; rel="next", <x>; rel="last"`)
			_, _ = w.Write([]byte(`{"items":[{"number":1,"repository_url":"https://api.github.com/repos/o/r","state":"open","pull_request":{"url":"x"}}]}`))
		case "2":
			// No Link header: this is the last page.
			_, _ = w.Write([]byte(`{"items":[{"number":2,"repository_url":"https://api.github.com/repos/o/r","state":"open","pull_request":{"url":"x"}}]}`))
		default:
			t.Errorf("unexpected page %q — pagination should have stopped", page)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL)
	items, whole, err := c.searchIssues(context.Background(), "tok", "is:open commenter:@me")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("want both pages merged (2 items), got %d", len(items))
	}
	if !whole {
		t.Error("a walk that reached the last page should report the set as whole")
	}
	if len(pages) != 2 || pages[0] != "1" || pages[1] != "2" {
		t.Fatalf("want pages 1 then 2, got %v", pages)
	}
}

// The search budget pauses BEFORE a request when the last response said the window
// is exhausted — but never into the run's own deadline: pagination that sleeps past
// its budget kills the run, which persists nothing, so it stops and keeps what it has.
func TestSearchBudgetWait(t *testing.T) {
	var b searchBudget
	const reserve = 90 * time.Second

	// Nothing known yet: no pause.
	if !b.wait(context.Background(), reserve) {
		t.Fatal("an unknown budget should not pause")
	}

	// Budget remaining: no pause.
	h := http.Header{}
	h.Set("X-RateLimit-Remaining", "17")
	h.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10))
	b.note(h)
	if !b.wait(context.Background(), reserve) {
		t.Fatal("a budget with headroom should not pause")
	}

	// Exhausted with a reset in the past: nothing to wait for.
	h.Set("X-RateLimit-Remaining", "0")
	h.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10))
	b.note(h)
	if !b.wait(context.Background(), reserve) {
		t.Fatal("an elapsed window should not pause")
	}

	// Exhausted with a future reset, and a run that cannot afford the pause: stop
	// paginating rather than sleep into the deadline.
	h.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10))
	b.note(h)
	tight, cancelTight := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelTight()
	if b.wait(tight, reserve) {
		t.Fatal("a pause that would outlive the run's budget must not be taken")
	}

	// Same state, but cancelled: also stops.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if b.wait(ctx, reserve) {
		t.Fatal("a cancelled run must stop paginating")
	}
}

// Pagination yields to the run's clock: with no time left for another page it returns
// the pages already fetched instead of spending budget the rest of the backfill needs.
func TestSearchIssuesStopsOnBudget(t *testing.T) {
	var calls int
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// Always advertise another page, so only the budget can stop the walk.
		w.Header().Set("Link", `<`+r.URL.String()+`>; rel="next"`)
		_, _ = w.Write([]byte(`{"items":[{"number":1,"repository_url":"https://api.github.com/repos/o/r","state":"open"}]}`))
	}))
	defer srv.Close()

	// A deadline below searchTimeReserve: page 1 is fetched, nothing after it.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c := NewClient(srv.Client(), srv.URL)
	items, whole, err := c.searchIssues(ctx, "tok", "is:open commenter:@me")
	if err != nil {
		t.Fatal(err)
	}
	if whole {
		t.Error("a walk the budget cut short must not report the set as whole")
	}
	if calls != 1 {
		t.Fatalf("want a single page when out of budget, got %d requests", calls)
	}
	if len(items) != 1 {
		t.Fatalf("want the fetched page returned, got %d items", len(items))
	}
}

// GitHub's own incomplete_results flag (a server-side search timeout) means the page
// is not the full match set, even when it is the last page.
func TestSearchIssuesHonoursIncompleteResults(t *testing.T) {
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"incomplete_results":true,"items":[{"number":1,"repository_url":"https://api.github.com/repos/o/r","state":"open"}]}`))
	}))
	defer srv.Close()
	c := NewClient(srv.Client(), srv.URL)
	items, whole, err := c.searchIssues(context.Background(), "tok", "is:open involves:@me")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || whole {
		t.Fatalf("items=%d whole=%v; want 1 item and whole=false", len(items), whole)
	}
}

// The 1,000-result ceiling ends the walk with pages still advertised: partial.
func TestSearchIssuesResultCeilingIsPartial(t *testing.T) {
	var calls int
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Link", `<`+r.URL.String()+`>; rel="next"`)
		_, _ = w.Write([]byte(`{"items":[{"number":1,"repository_url":"https://api.github.com/repos/o/r","state":"open"}]}`))
	}))
	defer srv.Close()
	c := NewClient(srv.Client(), srv.URL)
	_, whole, err := c.searchIssues(context.Background(), "tok", "is:open involves:@me")
	if err != nil {
		t.Fatal(err)
	}
	if calls != searchResultCap/searchPerPage {
		t.Fatalf("calls = %d, want %d (the ceiling)", calls, searchResultCap/searchPerPage)
	}
	if whole {
		t.Error("a walk ended by the result ceiling must not report the set as whole")
	}
}

// Another contributor's draft is left out of the fetch without making it partial: the
// walk still saw the whole open set, so a stored copy of the draft (a pull request
// converted back to draft) is retired, and it resurfaces once marked ready.
func TestFetchWorklistSkipsOthersDraftsAndStaysComplete(t *testing.T) {
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_, _ = w.Write([]byte(`{"login":"me"}`))
			return
		}
		q := r.URL.Query().Get("q")
		switch {
		case strings.Contains(q, "is:pr") && strings.Contains(q, "involves:@me"):
			_, _ = w.Write([]byte(`{"total_count":3,"items":[
				{"number":1,"repository_url":"https://api.github.com/repos/o/r","state":"open","draft":true,"user":{"login":"me"},"pull_request":{"url":"x"}},
				{"number":2,"repository_url":"https://api.github.com/repos/o/r","state":"open","draft":true,"user":{"login":"someone"},"pull_request":{"url":"x"}},
				{"number":3,"repository_url":"https://api.github.com/repos/o/r","state":"open","user":{"login":"someone"},"pull_request":{"url":"x"}}]}`))
		case strings.Contains(q, "review-requested:@me"):
			_, _ = w.Write([]byte(`{"total_count":1,"items":[
				{"number":4,"repository_url":"https://api.github.com/repos/o/r","state":"open","draft":true,"user":{"login":"someone"},"pull_request":{"url":"x"}}]}`))
		default:
			_, _ = w.Write([]byte(`{"total_count":0,"items":[]}`))
		}
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL)
	f, err := c.FetchWorklistDetail(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, it := range f.Items {
		got = append(got, it.GitHub.Number)
	}
	slices.Sort(got)
	if !slices.Equal(got, []int{1, 3}) {
		t.Fatalf("items %v, want [1 3] (others' drafts #2 and #4 left out)", got)
	}
	if !f.Complete || !f.ReviewRequestsComplete {
		t.Fatalf("complete=%v reviewsComplete=%v, want both true", f.Complete, f.ReviewRequestsComplete)
	}
	if want := map[string]time.Time{"github:o/r#2": {}, "github:o/r#4": {}}; !maps.Equal(f.OthersDrafts, want) {
		t.Fatalf("OthersDrafts %v, want %v", f.OthersDrafts, want)
	}
}

// The same pull request can come back from both searches, walked at different
// moments, in different states. The copy GitHub updated last is its state: marked
// draft after the involves: walk, it is left out (and reported as a draft); marked
// ready after it, it is kept, with the reasons of both copies.
func TestFetchWorklistNewestCopyDecidesDraft(t *testing.T) {
	const (
		ready = `{"number":7,"repository_url":"https://api.github.com/repos/o/r","state":"open","updated_at":%q,"user":{"login":"someone"},"pull_request":{"url":"x"}}`
		draft = `{"number":7,"repository_url":"https://api.github.com/repos/o/r","state":"open","draft":true,"updated_at":%q,"user":{"login":"someone"},"pull_request":{"url":"x"}}`
	)
	older, newer := "2026-11-01T00:00:00Z", "2026-11-02T00:00:00Z"
	cases := []struct {
		name, involves, reviews string
		kept                    bool
	}{
		{"marked draft between walks", fmt.Sprintf(ready, older), fmt.Sprintf(draft, newer), false},
		{"marked ready between walks", fmt.Sprintf(draft, older), fmt.Sprintf(ready, newer), true},
		{"newer copy first", fmt.Sprintf(draft, newer), fmt.Sprintf(ready, older), false},
		// GitHub reports timestamps to the second, so two states can tie: the draft
		// wins whichever search returned it.
		{"tie, ready first", fmt.Sprintf(ready, newer), fmt.Sprintf(draft, newer), false},
		{"tie, draft first", fmt.Sprintf(draft, newer), fmt.Sprintf(ready, newer), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/user" {
					_, _ = w.Write([]byte(`{"login":"me"}`))
					return
				}
				q := r.URL.Query().Get("q")
				switch {
				case strings.Contains(q, "is:pr") && strings.Contains(q, "involves:@me"):
					fmt.Fprintf(w, `{"total_count":1,"items":[%s]}`, tc.involves)
				case strings.Contains(q, "review-requested:@me"):
					fmt.Fprintf(w, `{"total_count":1,"items":[%s]}`, tc.reviews)
				default:
					_, _ = w.Write([]byte(`{"total_count":0,"items":[]}`))
				}
			}))
			defer srv.Close()
			f, err := NewClient(srv.Client(), srv.URL).FetchWorklistDetail(context.Background(), "tok")
			if err != nil {
				t.Fatal(err)
			}
			if !tc.kept {
				want := map[string]time.Time{"github:o/r#7": time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}
				if len(f.Items) != 0 || !maps.EqualFunc(f.OthersDrafts, want, time.Time.Equal) {
					t.Fatalf("items %d, OthersDrafts %v; want the draft left out and reported", len(f.Items), f.OthersDrafts)
				}
				return
			}
			if len(f.Items) != 1 || len(f.OthersDrafts) != 0 {
				t.Fatalf("items %d, OthersDrafts %v; want the ready PR kept", len(f.Items), f.OthersDrafts)
			}
			it := f.Items[0]
			if it.GitHub.Draft || !it.GitHub.UpdatedAt.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)) {
				t.Errorf("kept the older copy: draft=%v updated=%v", it.GitHub.Draft, it.GitHub.UpdatedAt)
			}
			if !slices.Contains(it.Signals.Reasons, worklist.ReasonReviewRequested) || !slices.Contains(it.Signals.Reasons, worklist.ReasonInvolved) {
				t.Errorf("reasons %v, want both copies' reasons", it.Signals.Reasons)
			}
		})
	}
}

// A draft observation passes the public-only gate like a hit: one from a repository
// confirmed private is not reported, and one whose check has no answer is not
// reported and makes the fetch incomplete.
func TestFetchWorklistGatesDraftObservations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		lookup   func(w http.ResponseWriter, name string)
		complete bool
	}{
		{"public", func(w http.ResponseWriter, name string) {
			fmt.Fprintf(w, `{"id":1,"full_name":%q,"private":false}`, name)
		}, true},
		{"private", func(w http.ResponseWriter, name string) {
			fmt.Fprintf(w, `{"id":1,"full_name":%q,"private":true}`, name)
		}, true},
		{"no answer", func(w http.ResponseWriter, _ string) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if name, ok := githubtest.RepoLookup(r); ok {
					tc.lookup(w, name)
					return
				}
				q := r.URL.Query().Get("q")
				switch {
				case r.URL.Path == "/user":
					_, _ = w.Write([]byte(`{"login":"me"}`))
				case strings.Contains(q, "is:pr") && strings.Contains(q, "involves:@me"):
					_, _ = w.Write([]byte(`{"total_count":1,"items":[{"number":2,"repository_url":"https://api.github.com/repos/o/r","state":"open","draft":true,"user":{"login":"someone"},"pull_request":{"url":"x"}}]}`))
				default:
					_, _ = w.Write([]byte(`{"total_count":0,"items":[]}`))
				}
			}))
			defer srv.Close()
			f, err := NewClient(srv.Client(), srv.URL).FetchWorklistDetail(context.Background(), "tok")
			if err != nil {
				t.Fatal(err)
			}
			_, reported := f.OthersDrafts["github:o/r#2"]
			if reported != (tc.name == "public") {
				t.Errorf("draft reported = %v", reported)
			}
			if f.Complete != tc.complete {
				t.Errorf("complete = %v, want %v", f.Complete, tc.complete)
			}
		})
	}
}
