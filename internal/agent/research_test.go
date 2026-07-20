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
