package worklist

import (
	"testing"
	"time"
)

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantVerdict string
		wantClean   string
	}{
		{"agreement", "AGREEMENT\nBoth reviews concur.", VerdictAgree, "Both reviews concur."},
		{"disagreement", "DISAGREEMENT\nThey differ on tests.", VerdictDisagree, "They differ on tests."},
		{"lowercase + markdown", "**agreement**\nYep.", VerdictAgree, "Yep."},
		{"leading blank lines", "\n\nDISAGREEMENT\nNope.", VerdictDisagree, "Nope."},
		{"no token", "Both reviews mostly agree, though.", "", "Both reviews mostly agree, though."},
		{"token mid-word not matched", "AGREEMENTS are common\nrest", "", "AGREEMENTS are common\nrest"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, clean := ParseVerdict(c.in)
			if v != c.wantVerdict || clean != c.wantClean {
				t.Fatalf("ParseVerdict(%q) = (%q, %q), want (%q, %q)", c.in, v, clean, c.wantVerdict, c.wantClean)
			}
		})
	}
}

// The review-state helpers classify a PR's thread deterministically off the Kind
// markers and verdict field, driving the toolbar counts and the radar badges.
func TestReviewStateHelpers(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	stale := 5 * time.Minute
	rr := func(at time.Time) Message { return Message{Role: RoleUser, Kind: KindReviewRequest, At: at} }
	sr := func(at time.Time) Message { return Message{Role: RoleUser, Kind: KindSynthesisRequest, At: at} }
	ag := func(at time.Time) Message { return Message{Role: RoleAgent, At: at} }
	pr := func(thread ...Message) WorkItem { return WorkItem{Type: TypePullRequest, Thread: thread} }

	// Unreviewed PR: needs a first review, not a second opinion.
	if u := pr(); !u.NeedsReview(now, stale) || u.HasReview() || u.NeedsSecondOpinion(now, stale) {
		t.Error("empty PR should need a first review only")
	}

	// A freshly pending review is in flight — not re-dispatched.
	if f := pr(rr(now.Add(-1 * time.Minute))); f.NeedsReview(now, stale) {
		t.Error("a freshly pending review should not be re-dispatched")
	}

	// A stale pending review is treated as failed — eligible for retry.
	if s := pr(rr(now.Add(-10 * time.Minute))); !s.NeedsReview(now, stale) {
		t.Error("a stale pending review should be eligible again")
	}

	// Reviewed once: eligible for a second opinion, no verdict yet.
	once := pr(rr(now.Add(-10*time.Minute)), ag(now.Add(-9*time.Minute)))
	if once.NeedsReview(now, stale) {
		t.Error("a reviewed PR should not need a first review")
	}
	if !once.HasReview() || !once.NeedsSecondOpinion(now, stale) {
		t.Error("a reviewed-once PR should be eligible for a second opinion")
	}
	if once.HasSecondOpinion() || once.ReviewVerdict() != "" {
		t.Error("a reviewed-once PR has no synthesis verdict yet")
	}

	// Synthesized (agree): reports its verdict, not offered again.
	syn := pr(rr(now.Add(-10*time.Minute)), ag(now.Add(-9*time.Minute)), sr(now.Add(-8*time.Minute)), Message{Role: RoleAgent, Verdict: VerdictAgree, At: now.Add(-7 * time.Minute)})
	if !syn.HasSecondOpinion() || syn.ReviewVerdict() != VerdictAgree {
		t.Error("a synthesized PR should report its verdict")
	}
	if syn.NeedsSecondOpinion(now, stale) {
		t.Error("a synthesized PR should not be offered a second opinion again")
	}

	// A discuss-only thread is not a review.
	chat := pr(Message{Role: RoleUser, Content: "hi", At: now.Add(-10 * time.Minute)}, ag(now.Add(-9*time.Minute)))
	if chat.HasReview() {
		t.Error("a discussion is not a review")
	}
	if !chat.NeedsReview(now, stale) {
		t.Error("a discussed-but-unreviewed PR still needs a review")
	}

	// Issues are never subject to PR review actions.
	iss := WorkItem{Type: TypeIssue, Thread: []Message{rr(now.Add(-10 * time.Minute)), ag(now.Add(-9 * time.Minute))}}
	if iss.NeedsReview(now, stale) || iss.NeedsSecondOpinion(now, stale) {
		t.Error("issues are not subject to PR review actions")
	}
}

// A reply that lands after a newer turn still answers the turn it names, so review
// state and pending detection follow the reply links, not adjacency.
func TestOutOfOrderReplies(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	stale := 5 * time.Minute
	w := WorkItem{Type: TypePullRequest, Thread: []Message{
		{ID: "r", Role: RoleUser, Kind: KindReviewRequest, Content: "review", At: now.Add(-2 * time.Minute)},
		{ID: "f", Role: RoleUser, Content: "follow-up", At: now.Add(-time.Minute)},
		{ID: "a", ReplyTo: "r", Role: RoleAgent, Content: "the review", Model: "x", At: now.Add(-30 * time.Second)},
	}}
	if !w.HasReview() || w.FirstReviewModel() != "x" {
		t.Fatalf("the review answered out of order must count: HasReview=%v FirstReviewModel=%q", w.HasReview(), w.FirstReviewModel())
	}
	if !w.Answered("r") || w.Answered("f") {
		t.Fatal("r is answered and f is not, whatever their positions")
	}
	if !w.ReplyPending(now, stale) {
		t.Fatal("the unanswered follow-up is still pending, though the thread ends on an agent turn")
	}
	if w.ReplyStalled(now, stale) {
		t.Fatal("a fresh follow-up is not stalled")
	}
	if !w.ReplyStalled(now.Add(10*time.Minute), stale) || w.ReplyPending(now.Add(10*time.Minute), stale) {
		t.Fatal("once old, the unanswered follow-up is stalled, not pending")
	}

	// Answering the follow-up clears both.
	w.Thread = append(w.Thread, Message{ID: "b", ReplyTo: "f", Role: RoleAgent, Content: "answer", At: now})
	if w.ReplyPending(now, stale) || w.ReplyStalled(now.Add(10*time.Minute), stale) {
		t.Fatal("every turn is answered")
	}
}

// Turns stored before turns had IDs or replies recorded their target keep their old
// meaning: a reply answers the user turn directly before it, and turns are told apart
// by position even when their timestamps are equal.
func TestLegacyTurns(t *testing.T) {
	w := WorkItem{Type: TypePullRequest, Thread: []Message{
		{Role: RoleUser, Kind: KindReviewRequest, Content: "review"},
		{Role: RoleAgent, Content: "the review", Model: "y"},
		{Role: RoleUser, Content: "follow-up"}, // same (zero) timestamp as the first turn
	}}
	if !w.HasReview() || w.FirstReviewModel() != "y" {
		t.Fatalf("a legacy adjacent reply must count: HasReview=%v FirstReviewModel=%q", w.HasReview(), w.FirstReviewModel())
	}
	if w.TurnKey(0) == w.TurnKey(2) {
		t.Fatal("legacy turns with equal timestamps must have distinct keys")
	}
	if !w.Answered(w.TurnKey(0)) || w.Answered(w.TurnKey(2)) {
		t.Fatal("the first turn is answered and the follow-up is not")
	}
	if i, ok := w.TurnIndex(w.TurnKey(2)); !ok || i != 2 {
		t.Fatalf("TurnIndex(TurnKey(2)) = %d, %v", i, ok)
	}
}

// A reconnect notice replies to a turn without answering it: review state does not
// count it, the turn is neither pending nor stalled, and a later real answer still
// counts. AppendReply keeps the notice marker, but no marker gofer does not know.
func TestReconnectNotice(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	it := WorkItem{ID: "a", Type: TypePullRequest, Thread: []Message{
		{ID: "q1", Role: RoleUser, Kind: KindReviewRequest, Content: "review", At: now.Add(-time.Minute)},
	}}
	notice := Message{ID: "n1", Role: RoleAgent, Notice: NoticeReconnect, Content: ReconnectNotice, At: now}
	if changed, err := AppendReply(&it, "q1", notice); err != nil || !changed {
		t.Fatalf("append notice: %v %v", changed, err)
	}
	if it.Thread[1].Notice != NoticeReconnect {
		t.Fatal("the notice marker was dropped")
	}
	if it.Answered("q1") || it.HasReview() {
		t.Error("a notice counted as an answer")
	}
	if it.ReplyPending(now, time.Hour) || it.ReplyStalled(now.Add(2*time.Hour), time.Hour) {
		t.Error("a noticed turn reads as pending or stalled")
	}
	if !it.NeedsReconnect() {
		t.Error("NeedsReconnect = false after a reconnect notice")
	}
	if !it.NeedsReview(now, time.Hour) {
		t.Error("a PR whose review got only a notice should still need a review")
	}
	if changed, _ := AppendReply(&it, "q1", Message{ID: "r1", Content: "the review", At: now.Add(time.Minute)}); !changed {
		t.Fatal("a real answer after a notice was dropped")
	}
	if !it.Answered("q1") || !it.HasReview() || it.NeedsReconnect() {
		t.Error("the real answer after the notice does not count")
	}

	forged := WorkItem{ID: "b", Thread: []Message{{ID: "q", Role: RoleUser, Content: "q", At: now}}}
	_, _ = AppendReply(&forged, "q", Message{ID: "x", Notice: "something-else", Content: "hi", At: now})
	if forged.Thread[1].Notice != "" || !forged.Answered("q") {
		t.Errorf("an unknown notice marker survived: %+v", forged.Thread[1])
	}
}

// UndoRetry restores a refused attempt's turn only while it still carries exactly
// that attempt's time and is unanswered: a later attempt, or an answer, wins.
func TestUndoRetry(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	prev := now.Add(-10 * time.Minute)
	stalled := func() WorkItem {
		return WorkItem{ID: "i", Thread: []Message{{ID: "t1", Role: RoleUser, Content: "q", At: prev}}}
	}

	it := stalled()
	key, ok := it.RetryStalled(now, 5*time.Minute)
	if !ok || key != "t1" || !it.Thread[0].At.Equal(now) {
		t.Fatalf("RetryStalled = %q %v, at %v", key, ok, it.Thread[0].At)
	}
	if !it.UndoRetry("t1", now, prev) || !it.Thread[0].At.Equal(prev) {
		t.Fatal("UndoRetry must restore this attempt's turn")
	}

	later := stalled()
	later.Thread[0].At = now.Add(time.Minute) // another attempt re-stamped it since
	if later.UndoRetry("t1", now, prev) {
		t.Fatal("UndoRetry must leave a later attempt alone")
	}

	answered := stalled()
	answered.Thread[0].At = now
	answered.Thread = append(answered.Thread, Message{ID: "a1", ReplyTo: "t1", Role: RoleAgent, Content: "a", At: now})
	if answered.UndoRetry("t1", now, prev) {
		t.Fatal("UndoRetry must leave an answered turn alone")
	}

	hidden := stalled()
	hidden.Thread[0].Hidden = true
	if _, ok := hidden.RetryStalled(now, 5*time.Minute); ok || !hidden.Thread[0].At.Equal(prev) {
		t.Fatal("RetryStalled must not retry a hidden turn")
	}
}
