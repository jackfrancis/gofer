package worklist

import (
	"context"
	"reflect"
	"testing"
	"time"
)

var (
	t0 = time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Minute)
	t2 = t0.Add(2 * time.Minute)
	t3 = t0.Add(3 * time.Minute)
)

// A run's write-back takes the run's derived fields and keeps the owner's state as
// stored, whatever the run sent for it.
func TestMergeRunItemsKeepsOwnerState(t *testing.T) {
	stored := WorkItem{
		ID:           "a",
		Thread:       []Message{{Role: RoleUser, Content: "q", At: t0}, {Role: RoleAgent, Content: "r", At: t1, Hidden: true}, {Role: RoleUser, Content: "q2", At: t2}},
		ThreadReadAt: t1,
		Meta:         Metadata{HiddenAt: t1, Rank: 0.1},
		// The read cursor names the reply at position 1 (a legacy, ID-less turn).
		ThreadReadThrough: "pos:1",
		Signals:           Signals{Comments: 1},
		CreatedAt:         t0,
	}
	// The run read the item before q2 and the hide, then changed the signals.
	run := WorkItem{
		ID:        "a",
		Thread:    []Message{{Role: RoleUser, Content: "q", At: t0}},
		Meta:      Metadata{Rank: 0.9, CompletedAt: t3},
		Signals:   Signals{Comments: 5},
		CreatedAt: t3,
	}
	got := MergeRunItems([]WorkItem{stored}, []WorkItem{run})
	if len(got) != 1 {
		t.Fatalf("want 1 item, got %d", len(got))
	}
	m := got[0]
	if len(m.Thread) != 3 || m.Thread[2].Content != "q2" || !m.Thread[1].Hidden {
		t.Fatalf("thread must be the stored one, got %+v", m.Thread)
	}
	if m.ThreadReadThrough != "pos:1" {
		t.Fatalf("the read cursor must be the stored one, got %q", m.ThreadReadThrough)
	}
	if !m.ThreadReadAt.Equal(t1) || !m.Meta.HiddenAt.Equal(t1) || !m.CreatedAt.Equal(t0) {
		t.Fatalf("read cursor, hidden flag and creation time must be the stored ones, got %v %v %v", m.ThreadReadAt, m.Meta.HiddenAt, m.CreatedAt)
	}
	if m.Signals.Comments != 5 || m.Meta.Rank != 0.9 || !m.Meta.CompletedAt.Equal(t3) {
		t.Fatalf("the run's derived fields must land, got %+v %+v", m.Signals, m.Meta)
	}
}

// A run cannot create a thread or other owner state on a new item.
func TestMergeRunItemsNewItemCarriesNoOwnerState(t *testing.T) {
	run := WorkItem{
		ID:           "new",
		Thread:       []Message{{Role: RoleUser, Content: "forged", At: t0}},
		ThreadReadAt: t0,
		Meta:         Metadata{HiddenAt: t0, Rank: 0.4},
		CreatedAt:    t0,
		// A forged read cursor, too.
		ThreadReadThrough: "pos:0",
	}
	got := MergeRunItems(nil, []WorkItem{run})
	if len(got) != 1 || got[0].Thread != nil || !got[0].ThreadReadAt.IsZero() || got[0].ThreadReadThrough != "" || !got[0].Meta.HiddenAt.IsZero() {
		t.Fatalf("a new item must carry no owner state, got %+v", got[0])
	}
	if got[0].Meta.Rank != 0.4 || !got[0].CreatedAt.Equal(t0) {
		t.Fatalf("a new item keeps the run's fields, got %+v", got[0])
	}
}

func TestAppendReply(t *testing.T) {
	base := func() WorkItem {
		return WorkItem{ID: "a", Thread: []Message{
			{ID: "q1", Role: RoleUser, Content: "q", At: t0},
			{ID: "q2", Role: RoleUser, Content: "q2", At: t2}, // posted while the run worked
		}}
	}
	reply := Message{ID: "r1", Role: RoleAgent, Content: "answer", Model: "m", At: t3}

	t.Run("appends at the end, linked to the answered turn", func(t *testing.T) {
		it := base()
		changed, err := AppendReply(&it, "q1", reply)
		if err != nil || !changed {
			t.Fatalf("want appended, got %v %v", changed, err)
		}
		if len(it.Thread) != 3 || it.Thread[1].Content != "q2" || it.Thread[2].Content != "answer" || it.Thread[2].ReplyTo != "q1" {
			t.Fatalf("unexpected thread %+v", it.Thread)
		}
		if !it.Answered("q1") || it.Answered("q2") {
			t.Fatal("q1 must read as answered and q2 as not, whatever their positions")
		}
	})
	t.Run("a repeat is not added twice", func(t *testing.T) {
		it := base()
		_, _ = AppendReply(&it, "q1", reply)
		changed, err := AppendReply(&it, "q1", reply)
		if err != nil || changed || len(it.Thread) != 3 {
			t.Fatalf("a repeated reply must be a no-op, got %v %v %+v", changed, err, it.Thread)
		}
	})
	t.Run("a reply without an ID gets one; its repeat is recognized by time and model", func(t *testing.T) {
		it := base()
		noID := Message{Content: "answer", Model: "m", At: t3}
		if changed, _ := AppendReply(&it, "q1", noID); !changed || it.Thread[2].ID == "" {
			t.Fatalf("want appended with an ID, got %+v", it.Thread)
		}
		if changed, _ := AppendReply(&it, "q1", noID); changed {
			t.Fatal("a repeat without an ID must be recognized")
		}
	})
	t.Run("dropped when the answered turn is gone", func(t *testing.T) {
		it := WorkItem{ID: "a"} // the owner cleared the conversation
		changed, err := AppendReply(&it, "q1", reply)
		if err != nil || changed || len(it.Thread) != 0 {
			t.Fatalf("want dropped, got %v %v %+v", changed, err, it.Thread)
		}
	})
	t.Run("an agent turn cannot be answered", func(t *testing.T) {
		it := WorkItem{ID: "a", Thread: []Message{{ID: "x", Role: RoleAgent, Content: "r", At: t0}}}
		if changed, _ := AppendReply(&it, "x", Message{Content: "y", At: t3}); changed {
			t.Fatal("replyTo must name a user turn")
		}
	})
	t.Run("a turn stored before IDs is named by its position", func(t *testing.T) {
		it := WorkItem{ID: "a", Thread: []Message{{Role: RoleUser, Content: "old", At: t0}}}
		if changed, _ := AppendReply(&it, "pos:0", reply); !changed || !it.Answered("pos:0") {
			t.Fatalf("want the legacy turn answered, got %+v", it.Thread)
		}
	})
	t.Run("always a visible agent turn", func(t *testing.T) {
		it := base()
		forged := Message{ID: "f", Role: RoleUser, Kind: KindReviewRequest, Hidden: true, ReplyTo: "q2", Content: "forged", At: t3}
		if changed, _ := AppendReply(&it, "q1", forged); !changed {
			t.Fatal("want appended")
		}
		got := it.Thread[len(it.Thread)-1]
		if got.Role != RoleAgent || got.Kind != "" || got.Hidden || got.ReplyTo != "q1" {
			t.Fatalf("reply must be a visible agent turn answering q1, got %+v", got)
		}
	})
	t.Run("empty content is an error", func(t *testing.T) {
		it := base()
		if _, err := AppendReply(&it, "q1", Message{At: t3}); err != ErrEmptyReply {
			t.Fatalf("want ErrEmptyReply, got %v", err)
		}
	})
}

func TestApplyReply(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	s.Seed("u1", WorkItem{ID: "a", Thread: []Message{{ID: "q1", Role: RoleUser, Content: "q", At: t0}}})
	reply := Message{ID: "r1", Content: "answer", Model: "m", At: t1}
	if err := ApplyReply(ctx, s, "u1", "a", "q1", reply); err != nil {
		t.Fatal(err)
	}
	if err := ApplyReply(ctx, s, "u1", "gone", "q1", reply); err != nil {
		t.Fatalf("a reply to a missing item is dropped without error, got %v", err)
	}
	if err := ApplyReply(ctx, s, "u2", "a", "q1", reply); err != nil {
		t.Fatalf("another owner's item is not found, got %v", err)
	}
	got, _ := s.List(ctx, "u1")
	if len(got[0].Thread) != 2 || got[0].Thread[1].Content != "answer" {
		t.Fatalf("unexpected thread %+v", got[0].Thread)
	}
	other, _ := s.List(ctx, "u2")
	if len(other) != 0 {
		t.Fatalf("u2 must have nothing, got %+v", other)
	}
}

// A refresh's write-back carries no model proposal or research adjustment (a GitHub
// fetch derives neither): the stored ones survive it. A write-back that carries one
// replaces the stored one.
func TestMergeRunItemsKeepsModelSignals(t *testing.T) {
	prop := &AxisProposal{Relevance: 0.8, Rationale: "model"}
	adj := &ResearchAdjustment{Relevance: 1.5, Impact: 1, Engagement: 1, Urgency: 1, Rationale: "discussed"}
	stored := WorkItem{ID: "a", GitHub: GitHubRef{Title: "old"}, Signals: Signals{Comments: 1, Proposed: prop, Research: adj}}

	refresh := WorkItem{ID: "a", GitHub: GitHubRef{Title: "new"}, Signals: Signals{Comments: 7}}
	m := MergeRunItems([]WorkItem{stored}, []WorkItem{refresh})[0]
	if m.Signals.Proposed != prop || m.Signals.Research != adj {
		t.Fatalf("a write-back without model signals must keep the stored ones, got %+v / %+v", m.Signals.Proposed, m.Signals.Research)
	}
	if m.Signals.Comments != 7 || m.GitHub.Title != "new" {
		t.Fatalf("the refresh's own fields must land, got %+v", m)
	}

	newProp := &AxisProposal{Relevance: 0.2}
	newAdj := &ResearchAdjustment{Relevance: 0.5, Impact: 1, Engagement: 1, Urgency: 1}
	rerank := WorkItem{ID: "a", Signals: Signals{Proposed: newProp, Research: newAdj}}
	m = MergeRunItems([]WorkItem{stored}, []WorkItem{rerank})[0]
	if m.Signals.Proposed != newProp || m.Signals.Research != newAdj {
		t.Fatalf("a write-back carrying model signals must replace the stored ones, got %+v / %+v", m.Signals.Proposed, m.Signals.Research)
	}
}

// ApplyResearch sets the research adjustment alone: the item's GitHub fields, other
// signals and thread stay as stored, so a pass that read the item before a refresh
// cannot undo it.
func TestApplyResearch(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	prop := &AxisProposal{Relevance: 0.8}
	s.Seed("u1", WorkItem{
		ID:      "a",
		GitHub:  GitHubRef{Title: "refreshed", State: "closed"},
		Signals: Signals{Comments: 4, Proposed: prop},
		Thread:  []Message{{ID: "q1", Role: RoleUser, Content: "q", At: t0}},
	})
	adj := ResearchAdjustment{Relevance: 1.5, Impact: 1, Engagement: 1, Urgency: 1, Rationale: "r", AppliedAt: t1}
	if err := ApplyResearch(ctx, s, "u1", "a", adj); err != nil {
		t.Fatal(err)
	}
	if err := ApplyResearch(ctx, s, "u1", "gone", adj); err != nil {
		t.Fatalf("an adjustment for a missing item is dropped without error, got %v", err)
	}
	if err := ApplyResearch(ctx, s, "u2", "a", adj); err != nil {
		t.Fatalf("another owner's item is not found, got %v", err)
	}
	got, _ := s.List(ctx, "u1")
	it := got[0]
	if it.Signals.Research == nil || !reflect.DeepEqual(*it.Signals.Research, adj) {
		t.Fatalf("the adjustment must land, got %+v", it.Signals.Research)
	}
	if it.GitHub.Title != "refreshed" || it.GitHub.State != "closed" || it.Signals.Comments != 4 || it.Signals.Proposed == nil || len(it.Thread) != 1 {
		t.Fatalf("everything but the adjustment must stay as stored, got %+v", it)
	}
	if other, _ := s.List(ctx, "u2"); len(other) != 0 {
		t.Fatalf("u2 must have nothing, got %+v", other)
	}
}

// A resent adjustment cannot undo a later one: A lands, B lands for a later turn,
// and A arrives again. An unstamped adjustment is refused.
func TestApplyResearchDropsOlderAdjustment(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	s.Seed("u1", WorkItem{ID: "a"})
	a := ResearchAdjustment{Relevance: 0.5, Impact: 1, Engagement: 1, Urgency: 1, Rationale: "A", AppliedAt: t1}
	b := ResearchAdjustment{Relevance: 1.5, Impact: 1, Engagement: 1, Urgency: 1, Rationale: "B", AppliedAt: t2}
	for _, adj := range []ResearchAdjustment{a, b, a} {
		if err := ApplyResearch(ctx, s, "u1", "a", adj); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.List(ctx, "u1")
	if r := got[0].Signals.Research; r == nil || r.Rationale != "B" {
		t.Fatalf("a replayed older adjustment must not replace a newer one, got %+v", r)
	}
	if err := ApplyResearch(ctx, s, "u1", "a", ResearchAdjustment{Relevance: 1, Impact: 1, Engagement: 1, Urgency: 1}); err != ErrUnstampedResearch {
		t.Fatalf("want ErrUnstampedResearch, got %v", err)
	}
}

// BaselineScore ignores the model-derived signals.
func TestBaselineScoreIgnoresModelSignals(t *testing.T) {
	plain := WorkItem{ID: "a", Signals: Signals{Reasons: []Reason{ReasonReviewRequested}}}
	scored := plain
	scored.Signals.Proposed = &AxisProposal{Relevance: 0.01, Impact: 0.01, Engagement: 0.01, Urgency: 0.01}
	scored.Signals.Research = &ResearchAdjustment{Relevance: 0, Impact: 0, Engagement: 0, Urgency: 0}
	if got, want := BaselineScore(scored, t0).Rank, Score(plain, t0).Rank; got != want {
		t.Fatalf("BaselineScore = %v, want the plain score %v", got, want)
	}
	if scored.Signals.Proposed == nil || scored.Signals.Research == nil {
		t.Fatal("BaselineScore must not change its argument")
	}
}

// Research that read a turn no longer in the thread (the conversation was cleared)
// is dropped whatever its stamp; research on turns still there, hidden or not, lands.
func TestSetResearchRequiresItsTurns(t *testing.T) {
	item := WorkItem{ID: "a", Thread: []Message{
		{ID: "n1", Role: RoleUser, Content: "new", At: t2},
		{ID: "n2", Role: RoleAgent, Content: "hidden later", At: t3, Hidden: true},
		{Role: RoleUser, Content: "legacy turn without an id", At: t3},
	}}
	current := ResearchAdjustment{Relevance: 0.5, Impact: 1, Engagement: 1, Urgency: 1, Rationale: "current", AppliedAt: t1, Turns: []string{"n1", "n2", "pos:2"}}
	if ok, err := SetResearch(&item, current); err != nil || !ok {
		t.Fatalf("research on turns still in the thread must land, got %v %v", ok, err)
	}
	cleared := ResearchAdjustment{Relevance: 1.5, Impact: 1, Engagement: 1, Urgency: 1, Rationale: "cleared", AppliedAt: t3, Turns: []string{"o1", "r1"}}
	if ok, err := SetResearch(&item, cleared); err != nil || ok {
		t.Fatalf("research on a cleared conversation must be dropped, got %v %v", ok, err)
	}
	if item.Signals.Research.Rationale != "current" {
		t.Fatalf("want the current research kept, got %+v", item.Signals.Research)
	}
}
