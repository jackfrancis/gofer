package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// recordingRanker records the items the rank job hands the model.
type recordingRanker struct {
	mu   sync.Mutex
	seen map[string]worklist.WorkItem
}

func (r *recordingRanker) Propose(_ context.Context, item worklist.WorkItem) (worklist.AxisProposal, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen[item.ID] = item
	return worklist.AxisProposal{Rationale: "ranked"}, nil
}

// A review a bot requested reaches the ranker as automated, and a human's request as
// a human ask: enrich writes the requesting login, the write-back classifies it, and
// the rank job ranks the stored items. The in-process sink applies the same policy as
// the agent plane, and leaves the caller's items untouched.
func TestRankSeesBotReviewPolicy(t *testing.T) {
	ctx := context.Background()
	store := worklist.NewMemoryStore()
	sink := NewStoreSink(store, "u1").WithPolicy(worklist.NewRunPolicy([]string{"k8s-ci-robot"}))
	now := time.Now().UTC()

	written := []worklist.WorkItem{
		{ID: "bot", OwnerID: "u1", GitHub: worklist.GitHubRef{UpdatedAt: now},
			Signals: worklist.Signals{ReviewRequestedBy: "k8s-ci-robot"},
			Meta:    worklist.Metadata{Origin: worklist.OriginUser}},
		{ID: "human", OwnerID: "u1", GitHub: worklist.GitHubRef{UpdatedAt: now},
			Signals: worklist.Signals{ReviewRequestedBy: "alice", ReviewRequestedByBot: true}},
	}
	if err := sink.Ingest(ctx, written); err != nil {
		t.Fatal(err)
	}
	if written[0].Meta.Origin != worklist.OriginUser || !written[1].Signals.ReviewRequestedByBot {
		t.Fatal("Ingest modified the caller's items")
	}

	ranker := &recordingRanker{seen: map[string]worklist.WorkItem{}}
	if err := runRank(ctx, Params{Ranker: ranker}, sink); err != nil {
		t.Fatal(err)
	}
	if !ranker.seen["bot"].Signals.ReviewRequestedByBot {
		t.Error("the ranker did not see the bot's review request as automated")
	}
	if h, ok := ranker.seen["human"]; !ok || h.Signals.ReviewRequestedByBot {
		t.Errorf("the ranker saw the human's review request as automated (seen=%v)", ok)
	}

	// The rank job's own write-back is stamped too.
	items, _ := store.List(ctx, "u1")
	for _, it := range items {
		if it.Meta.Origin != worklist.OriginAgent {
			t.Errorf("%s: Origin = %q after rank, want %q", it.ID, it.Meta.Origin, worklist.OriginAgent)
		}
		if it.Signals.ReviewRequestedByBot != (it.ID == "bot") {
			t.Errorf("%s: ReviewRequestedByBot = %v after rank", it.ID, it.Signals.ReviewRequestedByBot)
		}
	}
}

// A standalone rank ranks stored items by the current bot list: an item stored with
// a stale flag (before an upgrade, or before BOT_REVIEWERS changed) reaches the
// ranker with the classification its write-back stores.
func TestRankClassifiesStoredItemsByCurrentPolicy(t *testing.T) {
	ctx := context.Background()
	store := worklist.NewMemoryStore()
	store.Seed("u1", worklist.WorkItem{ID: "bot", OwnerID: "u1",
		Signals: worklist.Signals{ReviewRequestedBy: "k8s-ci-robot"}}) // stored flag: false
	sink := NewStoreSink(store, "u1").WithPolicy(worklist.NewRunPolicy([]string{"k8s-ci-robot"}))

	ranker := &recordingRanker{seen: map[string]worklist.WorkItem{}}
	if err := runRank(ctx, Params{Ranker: ranker}, sink); err != nil {
		t.Fatal(err)
	}
	if !ranker.seen["bot"].Signals.ReviewRequestedByBot {
		t.Fatal("the ranker saw the stale stored flag, not the current classification")
	}
	items, _ := store.List(ctx, "u1")
	if len(items) != 1 || !items[0].Signals.ReviewRequestedByBot {
		t.Fatalf("stored after rank: %+v", items)
	}
}
