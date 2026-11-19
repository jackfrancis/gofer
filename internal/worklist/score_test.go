package worklist

import (
	"testing"
	"time"
)

// Rescore re-derives an agent-scored item's Meta at read time but keeps what is not
// scoring (the write time, hidden and completed marks), and returns a human
// override verbatim.
func TestRescore(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	written := now.Add(-time.Hour)
	item := WorkItem{
		Signals: Signals{Reasons: []Reason{ReasonReviewRequested}},
		Meta: Metadata{
			Rank: 0.01, Priority: PriorityLow, Rationale: "stale", Origin: OriginAgent,
			UpdatedAt: written, HiddenAt: written, CompletedAt: written,
		},
	}
	got := Rescore(item, now)
	want := Score(item, now)
	if got.Rank != want.Rank || got.Priority != want.Priority || got.Rationale != want.Rationale || !got.ScoredAt.Equal(now) {
		t.Fatalf("Rescore = %+v, want the Score result %+v", got, want)
	}
	if !got.UpdatedAt.Equal(written) || !got.HiddenAt.Equal(written) || !got.CompletedAt.Equal(written) {
		t.Fatalf("Rescore dropped non-scoring fields: %+v", got)
	}

	item.Meta.Origin = OriginUser
	if got := Rescore(item, now); got.Rank != 0.01 || got.Rationale != "stale" {
		t.Fatalf("a human override must be returned verbatim, got %+v", got)
	}
}
