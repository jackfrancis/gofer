package agent

import (
	"context"
	"testing"

	"github.com/jackfrancis/gofer/internal/worklist"
)

type fakeResearcher struct{ adj worklist.ResearchAdjustment }

func (f *fakeResearcher) Research(context.Context, worklist.WorkItem) (worklist.ResearchAdjustment, error) {
	return f.adj, nil
}

func TestRunResearchSetsAdjustment(t *testing.T) {
	sink := &memSink{items: []worklist.WorkItem{{
		ID:     "github:o/r#1",
		Thread: []worklist.Message{{Role: worklist.RoleUser, Content: "q"}},
	}}}
	fr := &fakeResearcher{adj: worklist.ResearchAdjustment{Relevance: 1.5, Impact: 1, Engagement: 1, Urgency: 1}}
	if err := Run(context.Background(), Params{JobType: JobResearch, ItemID: "github:o/r#1", Researcher: fr}, fakeVendor{}, sink); err != nil {
		t.Fatal(err)
	}
	r := sink.items[0].Signals.Research
	if r == nil || r.Relevance != 1.5 {
		t.Fatalf("expected research adjustment set, got %+v", r)
	}
}

func TestRunResearchNoResearcher(t *testing.T) {
	if err := Run(context.Background(), Params{JobType: JobResearch, ItemID: "x"}, fakeVendor{}, &memSink{}); err == nil {
		t.Fatal("expected an error when no researcher is configured")
	}
}

// sideEffectResearcher runs during() while the "model" is researching, to stand in
// for a Refresh landing in the web tier meanwhile.
type sideEffectResearcher struct {
	adj    worklist.ResearchAdjustment
	during func()
}

func (f *sideEffectResearcher) Research(context.Context, worklist.WorkItem) (worklist.ResearchAdjustment, error) {
	f.during()
	return f.adj, nil
}

// refreshDuring returns a func that applies a Refresh to sink's first item: a new
// title and state, as the web tier would store them while a run works.
func refreshDuring(sink *memSink) func() {
	return func() {
		sink.items[0].GitHub.Title = "Refreshed title"
		sink.items[0].GitHub.State = "closed"
		sink.items[0].Signals.Comments = 9
	}
}

// A research pass that finishes after a Refresh writes back only its adjustment: the
// refreshed title, state and signals stay in place.
func TestRunResearchAfterRefreshKeepsGitHubFields(t *testing.T) {
	sink := &memSink{items: []worklist.WorkItem{{
		ID:     "github:o/r#1",
		GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1, Title: "Old title", State: "open"},
		Thread: []worklist.Message{{Role: worklist.RoleUser, Content: "q"}},
	}}}
	fr := &sideEffectResearcher{adj: worklist.ResearchAdjustment{Relevance: 1.5, Impact: 1, Engagement: 1, Urgency: 1}, during: refreshDuring(sink)}
	if err := Run(context.Background(), Params{JobType: JobResearch, ItemID: "github:o/r#1", Researcher: fr}, fakeVendor{}, sink); err != nil {
		t.Fatal(err)
	}
	assertRefreshKept(t, sink)
	if sink.ingests != 0 {
		t.Fatalf("research must not write back the whole item, got %d ingest calls", sink.ingests)
	}
}

func assertRefreshKept(t *testing.T, sink *memSink) {
	t.Helper()
	got := sink.items[0]
	if got.GitHub.Title != "Refreshed title" || got.GitHub.State != "closed" || got.Signals.Comments != 9 {
		t.Fatalf("a late write-back undid the refresh: title %q state %q comments %d", got.GitHub.Title, got.GitHub.State, got.Signals.Comments)
	}
	if r := got.Signals.Research; r == nil || r.Relevance != 1.5 {
		t.Fatalf("the research adjustment must still land, got %+v", r)
	}
}
