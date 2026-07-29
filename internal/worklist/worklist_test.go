package worklist

import (
	"context"
	"testing"
	"time"
)

func TestScoreReviewRequested(t *testing.T) {
	item := WorkItem{
		Signals: Signals{
			Reasons:         []Reason{ReasonReviewRequested},
			AwaitingMeSince: time.Now().Add(-24 * time.Hour),
		},
	}
	m := Score(item, time.Now())
	if m.Relevance < 0.99 {
		t.Fatalf("review_requested should give relevance ~1.0, got %v", m.Relevance)
	}
	if m.Urgency < 0.6 {
		t.Fatalf("expected urgency >= 0.6, got %v", m.Urgency)
	}
	if m.Rank <= 0 {
		t.Fatalf("expected rank > 0, got %v", m.Rank)
	}
	if m.Priority == PriorityNone {
		t.Fatal("expected a priority band")
	}
}

// An LLM proposal is authoritative for the four axes it returns.
func TestScoreProposalAuthoritative(t *testing.T) {
	item := WorkItem{Signals: Signals{Proposed: &AxisProposal{
		Relevance: 0.5, Impact: 0.5, Engagement: 0.5, Urgency: 0.5, Confidence: 1,
	}}}
	m := Score(item, time.Now())
	if m.Relevance != 0.5 || m.Impact != 0.5 || m.Engagement != 0.5 || m.Urgency != 0.5 {
		t.Fatalf("proposal should drive the axes, got %+v", m)
	}
}

type fakeIngestor struct{ called bool }

func (f *fakeIngestor) EnsureBackfill(context.Context, string) error { f.called = true; return nil }

// failingIngestor also implements BackfillProber and reports a failed backfill run.
type failingIngestor struct{ msg string }

func (f *failingIngestor) EnsureBackfill(context.Context, string) error { return nil }
func (f *failingIngestor) BackfillFailure(context.Context, string) (bool, string, error) {
	return true, f.msg, nil
}

// completedIngestor implements BackfillCompleter and reports a backfill that finished
// successfully — the case where the user genuinely has no open work.
type completedIngestor struct{}

func (c *completedIngestor) EnsureBackfill(context.Context, string) error { return nil }
func (c *completedIngestor) BackfillSucceeded(context.Context, string) (bool, error) {
	return true, nil
}

// A backfill that completed and still produced nothing means the radar is genuinely
// empty, so the read model reports ready-with-no-items instead of spinning on
// "processing" forever.
func TestResolveCompletedEmptyBackfillIsReady(t *testing.T) {
	res, err := Resolve(context.Background(), NewMemoryStore(), &completedIngestor{}, time.Now(), "u1", DefaultSort, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusReady {
		t.Fatalf("status = %q, want %q (a completed backfill that found nothing)", res.Status, StatusReady)
	}
	if len(res.Items) != 0 {
		t.Fatalf("want no items, got %d", len(res.Items))
	}
}

func TestResolveEmptyTriggersBackfill(t *testing.T) {
	store := NewMemoryStore()
	ing := &fakeIngestor{}
	res, err := Resolve(context.Background(), store, ing, time.Now(), "u1", DefaultSort, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusProcessing {
		t.Fatalf("want processing, got %q", res.Status)
	}
	if len(res.Items) != 0 {
		t.Fatalf("want no items, got %d", len(res.Items))
	}
	if !ing.called {
		t.Fatal("expected EnsureBackfill to be called on an empty worklist")
	}
}

// A backfill that keeps failing surfaces as StatusFailed (with the run's message)
// instead of an endless StatusProcessing.
func TestResolveEmptyBackfillFailedSurfaces(t *testing.T) {
	store := NewMemoryStore()
	ing := &failingIngestor{msg: `agent-sandbox: create sandbox: no matches for kind "Sandbox"`}
	res, err := Resolve(context.Background(), store, ing, time.Now(), "u1", DefaultSort, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusFailed {
		t.Fatalf("want failed, got %q", res.Status)
	}
	if res.Message != ing.msg {
		t.Fatalf("want the run's failure message, got %q", res.Message)
	}
}

func TestResolvePopulatedReadyAndSorted(t *testing.T) {
	store := NewMemoryStore()
	store.Seed("u1",
		WorkItem{ID: "a", Signals: Signals{Reasons: []Reason{ReasonCommented}}},
		WorkItem{ID: "b", Signals: Signals{Reasons: []Reason{ReasonReviewRequested}}},
	)
	ing := &fakeIngestor{}
	res, err := Resolve(context.Background(), store, ing, time.Now(), "u1", SortRank, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusReady {
		t.Fatalf("want ready, got %q", res.Status)
	}
	if len(res.Items) != 2 {
		t.Fatalf("want 2 items, got %d", len(res.Items))
	}
	if res.Items[0].ID != "b" {
		t.Fatalf("expected higher-ranked 'b' first, got %q", res.Items[0].ID)
	}
	if ing.called {
		t.Fatal("a populated worklist must not trigger backfill")
	}
}

// An item with an unread agent reply floats to the top regardless of rank.
func TestSortUnreadFloatsFirst(t *testing.T) {
	now := time.Now()
	items := []WorkItem{
		{ID: "high", Meta: Metadata{Rank: 0.9}},
		{ID: "low", Meta: Metadata{Rank: 0.1}, Thread: []Message{{Role: RoleAgent, Content: "hi", At: now}}},
	}
	if err := Sort(items, SortRank, true); err != nil {
		t.Fatal(err)
	}
	if items[0].ID != "low" {
		t.Fatalf("an unread reply should float first, got %q", items[0].ID)
	}
}
