package worklist

import (
	"context"
	"testing"
	"time"
)

// The policy classifies the requesting account against the bot list, ignoring case
// and replacing whatever the run sent, and stamps every item agent-derived.
func TestRunPolicyApply(t *testing.T) {
	p := NewRunPolicy([]string{"k8s-ci-robot", " Dependabot ", ""})
	items := []WorkItem{
		{ID: "bot", Signals: Signals{ReviewRequestedBy: "k8s-ci-robot"}},
		{ID: "bot-case", Signals: Signals{ReviewRequestedBy: "K8s-CI-Robot"}},
		{ID: "bot-trimmed", Signals: Signals{ReviewRequestedBy: "dependabot"}},
		// A run cannot mark a human's request as automated...
		{ID: "human", Signals: Signals{ReviewRequestedBy: "alice", ReviewRequestedByBot: true}},
		// ...nor flag an item with no pending request.
		{ID: "none", Signals: Signals{ReviewRequestedByBot: true}},
		// ...nor claim the human override.
		{ID: "claims-user", Meta: Metadata{Origin: OriginUser}},
	}
	p.Apply(items)

	wantBot := map[string]bool{"bot": true, "bot-case": true, "bot-trimmed": true}
	for _, it := range items {
		if got := it.Signals.ReviewRequestedByBot; got != wantBot[it.ID] {
			t.Errorf("%s: ReviewRequestedByBot = %v, want %v", it.ID, got, wantBot[it.ID])
		}
		if it.Meta.Origin != OriginAgent {
			t.Errorf("%s: Origin = %q, want %q", it.ID, it.Meta.Origin, OriginAgent)
		}
	}
	if NewRunPolicy(nil).IsBot("") || p.IsBot("") {
		t.Error("an empty login is never a bot")
	}
}

// A workload write cannot claim the human override that skips rescoring: once the
// policy is applied at write-back, Resolve rescores the item instead of serving the
// metadata the run sent verbatim.
func TestRunPolicyWorkloadCannotClaimOverride(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	forged := WorkItem{
		ID:      "github:o/r#1",
		OwnerID: "u1",
		GitHub:  GitHubRef{Number: 1, Repo: "o/r", UpdatedAt: now},
		Meta:    Metadata{Origin: OriginUser, Rank: 0.99, Urgency: 0.99},
	}
	resolveRank := func(items []WorkItem) Metadata {
		t.Helper()
		store := NewMemoryStore()
		if err := store.Update(ctx, "u1", func(current []WorkItem) ([]WorkItem, error) {
			return MergeRunItems(current, items), nil
		}); err != nil {
			t.Fatal(err)
		}
		res, err := Resolve(ctx, store, nil, now, "u1", SortRank, true)
		if err != nil || len(res.Items) != 1 {
			t.Fatalf("Resolve = (%+v, %v)", res, err)
		}
		return res.Items[0].Meta
	}

	// Without the policy, the forged origin would be served verbatim: this is the
	// override the policy must keep out of a run's reach.
	if got := resolveRank([]WorkItem{forged}); got.Rank != 0.99 {
		t.Fatalf("precondition: an OriginUser item is not rescored, got rank %v", got.Rank)
	}

	stamped := []WorkItem{forged}
	NewRunPolicy(nil).Apply(stamped)
	got := resolveRank(stamped)
	want := Score(stamped[0], now)
	if got.Origin != OriginAgent || got.Rank != want.Rank || got.Rank == 0.99 {
		t.Fatalf("Resolve kept the run's claimed metadata: origin %q rank %v, want agent rank %v", got.Origin, got.Rank, want.Rank)
	}
}

// The deterministic scorer (used with AI disabled, by the stub ranker, and to choose
// which items get a model call) discounts a bot's review request: no awaiting-me
// pressure and a low floor, while a human's request keeps its urgency and other
// signals such as a deadline or an assignment still count.
func TestScoreDiscountsBotReviewRequest(t *testing.T) {
	now := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	review := func(bot bool, extra ...Reason) WorkItem {
		return WorkItem{Signals: Signals{
			Reasons:              append([]Reason{ReasonReviewRequested}, extra...),
			ReviewRequestedBy:    map[bool]string{true: "k8s-ci-robot", false: "alice"}[bot],
			ReviewRequestedByBot: bot,
			AwaitingMeSince:      now.Add(-5 * 24 * time.Hour),
		}}
	}
	stub := &StubRanker{now: func() time.Time { return now }}
	urgency := func(it WorkItem) (score, stubbed float64) {
		t.Helper()
		p, err := stub.Propose(context.Background(), it)
		if err != nil {
			t.Fatal(err)
		}
		return Score(it, now).Urgency, p.Urgency
	}

	if s, p := urgency(review(false)); s != 1 || p != 1 {
		t.Errorf("human request, 5 days: urgency score=%v stub=%v, want 1", s, p)
	}
	if s, p := urgency(review(true)); s != botReviewUrgency || p != botReviewUrgency {
		t.Errorf("bot request, 5 days: urgency score=%v stub=%v, want %v", s, p, botReviewUrgency)
	}
	if s, _ := urgency(review(true, ReasonAssignee)); s != 0.6 {
		t.Errorf("bot request on an assignee: urgency %v, want the explicit floor 0.6", s)
	}
	withDeadline := review(true)
	withDeadline.Signals.DeadlineAt = now.Add(-time.Hour)
	if s, _ := urgency(withDeadline); s != 1 {
		t.Errorf("bot request past a deadline: urgency %v, want the deadline's 1", s)
	}

	if h, b := Score(review(false), now).Relevance, Score(review(true), now).Relevance; b >= h || b != botReviewRelevance {
		t.Errorf("relevance: human %v, bot %v; want bot = %v, below human", h, b, botReviewRelevance)
	}
	if r := Score(review(true, ReasonAssignee), now).Relevance; r < 0.9 {
		t.Errorf("bot request on an assignee: relevance %v, want the assignment's weight", r)
	}
	if Score(review(false), now).Rank <= Score(review(true), now).Rank {
		t.Error("a bot's review request ranks at or above a human's")
	}
}

// Classify re-derives the flag only; it leaves the origin alone, so the read path
// can use it without touching a stored override.
func TestRunPolicyClassifyLeavesOrigin(t *testing.T) {
	items := []WorkItem{{Signals: Signals{ReviewRequestedBy: "k8s-ci-robot"}, Meta: Metadata{Origin: OriginUser}}}
	NewRunPolicy([]string{"k8s-ci-robot"}).Classify(items)
	if !items[0].Signals.ReviewRequestedByBot || items[0].Meta.Origin != OriginUser {
		t.Fatalf("Classify: bot=%v origin=%q", items[0].Signals.ReviewRequestedByBot, items[0].Meta.Origin)
	}
}

// The user's reads classify by the current bot list before Resolve rescores, so the
// radar shows the rank Discuss explains (the agent plane classifies the same way)
// for an item stored before the policy, with a stale flag and no proposal.
func TestClassifyReadsRadarMatchesAgentPlane(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	store.Seed("u1", WorkItem{
		ID: "github:o/r#1", OwnerID: "u1", GitHub: GitHubRef{Number: 1, Repo: "o/r", UpdatedAt: now},
		Signals: Signals{
			Reasons:           []Reason{ReasonReviewRequested},
			ReviewRequestedBy: "k8s-ci-robot", // stored flag: false (predates the policy)
			AwaitingMeSince:   now.Add(-5 * 24 * time.Hour),
		},
		Meta: Metadata{Origin: OriginAgent},
	})
	policy := NewRunPolicy([]string{"k8s-ci-robot"})

	res, err := Resolve(ctx, policy.ClassifyReads(store), nil, now, "u1", SortRank, true)
	if err != nil || len(res.Items) != 1 {
		t.Fatalf("Resolve = (%+v, %v)", res, err)
	}
	radar := res.Items[0].Meta

	agentView, _ := store.List(ctx, "u1")
	policy.Classify(agentView) // what GET /agent/worklist serves Discuss
	discuss := Score(agentView[0], now)
	if radar.Rank != discuss.Rank || radar.Urgency != botReviewUrgency {
		t.Fatalf("radar rank %v (urgency %v), Discuss rank %v; want equal and bot-discounted", radar.Rank, radar.Urgency, discuss.Rank)
	}

	// Reads are classified; the stored item is not rewritten.
	stored, _ := store.List(ctx, "u1")
	if stored[0].Signals.ReviewRequestedByBot {
		t.Error("ClassifyReads wrote the store")
	}
	if err := policy.ClassifyReads(store).Update(ctx, "u1", func(cur []WorkItem) ([]WorkItem, error) { return nil, nil }); err != nil {
		t.Errorf("Update through the wrapper: %v", err)
	}
}
