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

func TestResearchReflectsVisibleThread(t *testing.T) {
	ran := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	before, after := ran.Add(-time.Hour), ran.Add(time.Hour)
	turn := func(at time.Time, hidden bool) Message { return Message{Role: RoleUser, At: at, Hidden: hidden} }
	research := &ResearchAdjustment{Relevance: 1, Impact: 1, Engagement: 1, Urgency: 1, AppliedAt: ran}
	for name, tc := range map[string]struct {
		research *ResearchAdjustment
		thread   []Message
		want     bool
	}{
		"no research":                   {nil, []Message{turn(before, false)}, false},
		"read turns all visible":        {research, []Message{turn(before, false), turn(after, false)}, true},
		"a read turn is hidden":         {research, []Message{turn(before, false), turn(before, true)}, false},
		"hidden only after it ran":      {research, []Message{turn(before, false), turn(after, true)}, true},
		"thread reset":                  {research, nil, false},
		"only turns posted after":       {research, []Message{turn(after, false)}, false},
		"no AppliedAt, visible":         {&ResearchAdjustment{}, []Message{turn(after, false)}, true},
		"no AppliedAt, any turn hidden": {&ResearchAdjustment{}, []Message{turn(before, false), turn(after, true)}, false},
	} {
		item := WorkItem{Signals: Signals{Research: tc.research}, Thread: tc.thread}
		if got := ResearchReflectsVisibleThread(item); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}
