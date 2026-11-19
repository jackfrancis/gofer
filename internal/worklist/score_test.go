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
	q := Message{ID: "q", Role: RoleUser}
	a := Message{ID: "a", ReplyTo: "q", Role: RoleAgent}
	hiddenA := a
	hiddenA.Hidden = true
	n := Message{ID: "n", Role: RoleUser}
	legacy := Message{Role: RoleUser} // key "pos:0"
	read := func(keys ...string) *ResearchAdjustment {
		return &ResearchAdjustment{Relevance: 1, Impact: 1, Engagement: 1, Urgency: 1, Turns: keys}
	}
	for name, tc := range map[string]struct {
		research *ResearchAdjustment
		thread   []Message
		want     bool
	}{
		"no research":                     {nil, []Message{q}, false},
		"read turns all visible":          {read("q", "a"), []Message{q, a, n}, true},
		"a read turn is hidden":           {read("q", "a"), []Message{q, hiddenA}, false},
		"a later turn is hidden":          {read("q", "a"), []Message{q, a, func() Message { m := n; m.Hidden = true; return m }()}, true},
		"thread reset":                    {read("q", "a"), nil, false},
		"reset, then a new turn":          {read("q", "a"), []Message{n}, false},
		"read a turn past the given ones": {read("q", "a"), []Message{q}, false},
		"recorded no turns":               {read(), []Message{q}, false},
		"legacy positional key":           {read("pos:0"), []Message{legacy}, true},
		"positional key after reset":      {read("pos:0"), []Message{n}, false},
	} {
		item := WorkItem{Signals: Signals{Research: tc.research}, Thread: tc.thread}
		if got := ResearchReflectsVisibleThread(item); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

func TestResearchMultipliersAreClamped(t *testing.T) {
	r := ResearchAdjustment{Relevance: -1, Impact: 0.5, Engagement: 1, Urgency: 4}
	if rel, imp, eng, urg := r.Multipliers(); rel != 0 || imp != 0.5 || eng != 1 || urg != 2 {
		t.Fatalf("Multipliers() = %v %v %v %v, want 0 0.5 1 2", rel, imp, eng, urg)
	}
}
