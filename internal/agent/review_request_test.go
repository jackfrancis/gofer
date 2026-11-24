package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackfrancis/gofer/internal/github/githubtest"
	"github.com/jackfrancis/gofer/internal/worklist"
)

const (
	botRequest = `{"event":"review_requested","created_at":"2026-11-01T09:00:00Z","requested_reviewer":{"login":"me"},"actor":{"login":"review-bot"}}`
	botRemoved = `{"event":"review_request_removed","created_at":"2026-11-24T09:00:00Z","requested_reviewer":{"login":"me"},"actor":{"login":"review-bot"}}`
)

// requestRun runs one ingest (with its chained enrich) against a single public PR,
// #8, which the involves: search serves updated at 12:<involvesMinute> and the
// review-request search, when review is set, at 12:09. timeline is the PR's timeline
// ("" answers 404). It returns the stored item and the number of timeline reads.
func requestRun(t *testing.T, store *worklist.MemoryStore, sink Sink, involvesMinute int, review, partialReview bool, timeline string) (worklist.WorkItem, int32) {
	t.Helper()
	var reads atomic.Int32
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		switch {
		case r.URL.Path == "/user":
			fmt.Fprint(w, `{"login":"me"}`)
		case strings.HasSuffix(r.URL.Path, "/timeline"):
			reads.Add(1)
			if timeline == "" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, timeline)
		case r.URL.Path == "/search/issues" && strings.Contains(q, "is:pr") && strings.Contains(q, "involves:@me"):
			fmt.Fprintf(w, `{"total_count":1,"items":[%s]}`, lifecycleHit(8, false, involvesMinute))
		case r.URL.Path == "/search/issues" && strings.Contains(q, "review-requested:@me"):
			if review {
				fmt.Fprintf(w, `{"total_count":1,"items":[%s]}`, lifecycleHit(8, false, 9))
				return
			}
			fmt.Fprintf(w, `{"total_count":0,"items":[],"incomplete_results":%t}`, partialReview)
		case r.URL.Path == "/search/issues":
			fmt.Fprint(w, `{"total_count":0,"items":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	if err := Run(ctx, Params{JobType: JobIngest, GitHubBaseURL: srv.URL, Client: srv.Client(), Ranker: failRanker{}}, fakeVendor{token: "tok"}, sink); err != nil {
		t.Fatal(err)
	}
	it, ok, err := store.Get(ctx, "owner", "github:o/r#8")
	if err != nil || !ok {
		t.Fatalf("missing item: %v", err)
	}
	return it, reads.Load()
}

// A withdrawn review request stops affecting the worklist whether the hit is fresh
// or older than the stored state (keepNewerState). A complete review-request search
// confirms the removal on its own, clearing the request's waiting date too
// (settlePins), even with the timeline unreadable. A partial one cannot, so enrich
// must read the timeline, even when the ingest itself had nothing to write.
func TestRunIngestWithdrawnRequestStopsAffectingWorklist(t *testing.T) {
	for _, stale := range []bool{false, true} {
		for _, partial := range []bool{false, true} {
			t.Run(fmt.Sprintf("stale=%t/partial=%t", stale, partial), func(t *testing.T) {
				store := worklist.NewMemoryStore()
				sink := NewStoreSink(store, "owner").WithPolicy(worklist.NewRunPolicy([]string{"review-bot"}))
				first, _ := requestRun(t, store, sink, 0, true, false, "["+botRequest+"]")
				if !slices.Contains(first.Signals.Reasons, worklist.ReasonReviewRequested) || !first.Signals.ReviewRequestedByBot ||
					first.Signals.AwaitingMeSince.IsZero() || first.GitHub.UpdatedAt.Minute() != 9 {
					t.Fatalf("setup: %+v", first.Signals)
				}
				timeline := ""
				if partial {
					timeline = "[" + botRequest + "," + botRemoved + "]"
				}
				minute := 9
				if stale {
					minute = 0
				}
				got, reads := requestRun(t, store, sink, minute, false, partial, timeline)
				if got.GitHub.UpdatedAt.Minute() != 9 || !got.OnRadar() {
					t.Fatal("the newer 12:09 state was lost")
				}
				if slices.Contains(got.Signals.Reasons, worklist.ReasonReviewRequested) || !got.Signals.AwaitingMeSince.IsZero() {
					t.Fatalf("the withdrawn request still counts: reasons %v, awaiting me since %v, timeline reads %d",
						got.Signals.Reasons, got.Signals.AwaitingMeSince, reads)
				}
			})
		}
	}
}

// A partial review-request search that misses a stale hit's PR proves nothing: with
// the timeline unreadable, the stored request and its waiting date stand.
func TestRunIngestPartialSearchKeepsRequestDate(t *testing.T) {
	store := worklist.NewMemoryStore()
	sink := NewStoreSink(store, "owner").WithPolicy(worklist.NewRunPolicy([]string{"review-bot"}))
	first, _ := requestRun(t, store, sink, 0, true, false, "["+botRequest+"]")
	got, reads := requestRun(t, store, sink, 0, false, true, "")
	if reads == 0 {
		t.Error("enrich did not try the timeline of the request kept by the partial search")
	}
	if !slices.Contains(got.Signals.Reasons, worklist.ReasonReviewRequested) || !got.Signals.AwaitingMeSince.Equal(first.Signals.AwaitingMeSince) {
		t.Fatalf("the kept request lost its state: reasons %v, awaiting me since %v (was %v)",
			got.Signals.Reasons, got.Signals.AwaitingMeSince, first.Signals.AwaitingMeSince)
	}
}
