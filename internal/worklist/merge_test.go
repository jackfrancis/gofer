package worklist

import (
	"context"
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

// answers returns the substantive (non-notice) agent turns answering turn.
func answers(it WorkItem, turn string) []Message {
	var out []Message
	for _, m := range it.Thread {
		if m.Role == RoleAgent && m.Notice == "" && m.ReplyTo == turn {
			out = append(out, m)
		}
	}
	return out
}

// A user turn keeps one answer, whichever run stores it first; a notice is not an
// answer.
func TestAppendReplyOneAnswerPerTurn(t *testing.T) {
	base := func() WorkItem {
		return WorkItem{ID: "a", Thread: []Message{{ID: "q1", Role: RoleUser, Content: "q", At: t0}}}
	}

	t.Run("a second run answering the same turn is dropped", func(t *testing.T) {
		it := base()
		if changed, _ := AppendReply(&it, "q1", Message{ID: "r1", Content: "first", Model: "m", At: t1}); !changed {
			t.Fatal("want the first answer stored")
		}
		changed, err := AppendReply(&it, "q1", Message{ID: "r2", Content: "second", Model: "m", At: t2})
		if err != nil || changed {
			t.Fatalf("a second answer must be dropped, got %v %v", changed, err)
		}
		if got := answers(it, "q1"); len(got) != 1 || got[0].Content != "first" {
			t.Fatalf("want one answer, the first, got %+v", got)
		}
	})
	t.Run("a first run finishing late loses to the retry that answered", func(t *testing.T) {
		it := base()
		// The retry (dispatched after the first run looked stalled) answers first...
		if changed, _ := AppendReply(&it, "q1", Message{ID: "retry", Content: "retry", Model: "m", At: t2}); !changed {
			t.Fatal("want the retry's answer stored")
		}
		// ...then the original run, which started earlier, finally writes back.
		if changed, _ := AppendReply(&it, "q1", Message{ID: "late", Content: "late", Model: "m", At: t1}); changed {
			t.Fatal("the late answer must be dropped")
		}
		if got := answers(it, "q1"); len(got) != 1 || got[0].ID != "retry" {
			t.Fatalf("want only the retry's answer, got %+v", got)
		}
	})
	t.Run("a turn answered only by a notice still takes its answer", func(t *testing.T) {
		it := base()
		notice := Message{ID: "n1", Content: ReconnectNotice, Notice: NoticeReconnect, At: t1}
		if changed, _ := AppendReply(&it, "q1", notice); !changed {
			t.Fatal("want the notice stored")
		}
		if it.Answered("q1") {
			t.Fatal("a notice must not answer the turn")
		}
		if changed, _ := AppendReply(&it, "q1", Message{ID: "r1", Content: "answer", Model: "m", At: t2}); !changed {
			t.Fatal("the real answer must be stored after a notice")
		}
		if !it.Answered("q1") || len(answers(it, "q1")) != 1 {
			t.Fatalf("want q1 answered once, got %+v", it.Thread)
		}
	})
	t.Run("a notice after the answer is dropped", func(t *testing.T) {
		it := base()
		_, _ = AppendReply(&it, "q1", Message{ID: "r1", Content: "answer", Model: "m", At: t1})
		notice := Message{ID: "n1", Content: ReconnectNotice, Notice: NoticeReconnect, At: t2}
		if changed, _ := AppendReply(&it, "q1", notice); changed {
			t.Fatal("an answered turn takes nothing more, notices included")
		}
		if it.NeedsReconnect() {
			t.Fatal("a dropped notice must not ask the user to reconnect")
		}
	})
	t.Run("a hidden answer still answers the turn", func(t *testing.T) {
		it := base()
		_, _ = AppendReply(&it, "q1", Message{ID: "r1", Content: "answer", Model: "m", At: t1})
		it.Thread[1].Hidden = true
		if changed, _ := AppendReply(&it, "q1", Message{ID: "r2", Content: "again", Model: "m", At: t2}); changed {
			t.Fatal("hiding the answer must not reopen the turn")
		}
	})
	t.Run("a legacy positional answer counts", func(t *testing.T) {
		// Stored before turns had IDs: the agent turn answers the user turn before it.
		it := WorkItem{ID: "a", Thread: []Message{
			{Role: RoleUser, Content: "old", At: t0},
			{Role: RoleAgent, Content: "old answer", At: t1},
		}}
		if changed, _ := AppendReply(&it, "pos:0", Message{ID: "r2", Content: "new", Model: "m", At: t2}); changed {
			t.Fatalf("the legacy turn already has its answer, got %+v", it.Thread)
		}
	})
	t.Run("a second opinion is its own turn and takes its own answer", func(t *testing.T) {
		// Review-all and the second-opinion action each post a new user turn, so the
		// second model's review answers that turn, not the first review's.
		it := base()
		it.Thread[0].Kind = KindReviewRequest
		_, _ = AppendReply(&it, "q1", Message{ID: "r1", Content: "first review", Model: "m1", At: t1})
		it.Thread = append(it.Thread, Message{ID: "q2", Role: RoleUser, Content: "independent", Kind: KindReviewRequest, At: t2})
		if changed, _ := AppendReply(&it, "q2", Message{ID: "r2", Content: "second review", Model: "m2", At: t3}); !changed {
			t.Fatal("the second opinion must be stored")
		}
		if len(answers(it, "q1")) != 1 || len(answers(it, "q2")) != 1 {
			t.Fatalf("want one answer per turn, got %+v", it.Thread)
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
