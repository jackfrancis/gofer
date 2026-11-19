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
