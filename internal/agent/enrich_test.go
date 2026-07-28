package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// runEnrich fetches per-item timeline signals and updates the item's Signals.
func TestRunEnrichUpdatesSignals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
// score, so a skipped item is scored from missing signals and can never rise. Its
// write-back is batched, so a large worklist never posts one oversized body.
func TestRunEnrichCoversEveryItemInBatches(t *testing.T) {
	var (
		mu        sync.Mutex
		timelines int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	err := Run(context.Background(),
		Params{JobType: JobEnrich, Provider: "github", GitHubBaseURL: srv.URL, Client: srv.Client()},
		fakeVendor{token: "t"}, sink)
	if err != nil {
		t.Fatal(err)
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
