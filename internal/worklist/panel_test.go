package worklist

import (
	"context"
	"testing"
)

func panelThread() []Message {
	return []Message{
		{ID: "q1", Role: RoleUser, Content: "Can you review this PR?", Kind: KindReviewRequest, At: t0},
		{ID: "r1", Role: RoleAgent, ReplyTo: "q1", Content: "first review", At: t0},
		{ID: "p1", Role: RoleUser, Content: IndependentReviewPrompt, Kind: KindReviewRequest, At: t1},
		{ID: "r2", Role: RoleAgent, ReplyTo: "p1", Content: "blind review", At: t2},
	}
}

func TestChainSynthesis(t *testing.T) {
	cases := map[string]struct {
		edit     func(*WorkItem)
		answered string
		want     bool
	}{
		"panel review answered after an earlier review": {func(*WorkItem) {}, "p1", true},
		"an ordinary review":                            {func(*WorkItem) {}, "q1", false},
		"panel review with no earlier review": {func(w *WorkItem) {
			w.Thread = w.Thread[2:]
		}, "p1", false},
		"earlier review hidden": {func(w *WorkItem) { w.Thread[1].Hidden = true }, "p1", false},
		"earlier review unanswered": {func(w *WorkItem) {
			w.Thread = append(w.Thread[:1], w.Thread[2:]...)
		}, "p1", false},
		"panel answered only by a notice": {func(w *WorkItem) {
			w.Thread[3].Notice = NoticeReconnect
		}, "p1", false},
		"panel review hidden": {func(w *WorkItem) { w.Thread[2].Hidden = true }, "p1", false},
		"item hidden":         {func(w *WorkItem) { w.Meta.HiddenAt = t2 }, "p1", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w := WorkItem{ID: "a", Type: TypePullRequest, Thread: panelThread()}
			tc.edit(&w)
			n := len(w.Thread)
			key, ok := ChainSynthesis(&w, tc.answered, t2)
			if ok != tc.want {
				t.Fatalf("chained = %v, want %v", ok, tc.want)
			}
			if !ok {
				if len(w.Thread) != n || key != "" {
					t.Fatalf("nothing chained, so nothing appended; got %q and %+v", key, w.Thread)
				}
				return
			}
			last := w.Thread[len(w.Thread)-1]
			if len(w.Thread) != n+1 || last.ID != key || last.Kind != KindSynthesisRequest || last.Role != RoleUser || last.Content != SynthesisPrompt || !last.At.Equal(t2) {
				t.Fatalf("want one synthesis request appended under %q, got %+v", key, w.Thread)
			}
			if !w.ReplyPending(t2, 5*t1.Sub(t0)) {
				t.Fatal("the chained synthesis turn awaits its answer")
			}
		})
	}
}

// ApplyReplyAndChain stores the answer and its synthesis turn together, and only for
// a turn's first answer: a resent or late second answer chains nothing.
func TestApplyReplyAndChainOnce(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	th := panelThread()[:3]
	s.Seed("u1", WorkItem{ID: "a", Type: TypePullRequest, Thread: th})
	key, err := ApplyReplyAndChain(ctx, s, "u1", "a", "p1", Message{ID: "r2", Content: "blind review", At: t2}, t2)
	if err != nil || key == "" {
		t.Fatalf("want a synthesis turn, got %q, %v", key, err)
	}
	for _, again := range []Message{
		{ID: "r2", Content: "blind review", At: t2},          // a resend
		{ID: "r3", Content: "a late retry's answer", At: t2}, // a second answer
	} {
		if k, err := ApplyReplyAndChain(ctx, s, "u1", "a", "p1", again, t2); err != nil || k != "" {
			t.Fatalf("a second delivery must chain nothing, got %q, %v", k, err)
		}
	}
	got, _ := s.List(ctx, "u1")
	if n := len(got[0].Thread); n != 5 {
		t.Fatalf("want the answer and one synthesis turn, got %d turns: %+v", n, got[0].Thread)
	}
	// ApplyReply never chains.
	s.Seed("u1", WorkItem{ID: "b", Type: TypePullRequest, Thread: th})
	if err := ApplyReply(ctx, s, "u1", "b", "p1", Message{ID: "r2", Content: "blind review", At: t2}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.List(ctx, "u1")
	for _, it := range got {
		if it.ID == "b" && len(it.Thread) != 4 {
			t.Fatalf("ApplyReply must not chain, got %+v", it.Thread)
		}
	}
}
