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
