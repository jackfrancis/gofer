package ingest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// A research run is a github-research task for one item, carrying the default model,
// with no turn: it re-weights the ranking and answers nothing.
func TestScheduledResearchBuildsRunIntent(t *testing.T) {
	d := &recordDispatcher{}
	ig := New(d, "gofer-agent", "http://gofer", nil, "https://model", "m1")
	if !ig.CanResearch() {
		t.Fatal("CanResearch must be true with a default model")
	}
	ok, err := ig.ScheduledResearch(context.Background(), "github:1", "github:o/r#1")
	if err != nil || !ok {
		t.Fatalf("ScheduledResearch = %v, %v", ok, err)
	}
	if len(d.specs) != 1 {
		t.Fatalf("want one run, got %d", len(d.specs))
	}
	s := d.specs[0]
	if s.TaskRef != "github-research" || s.Subject != "github:1" || s.Audience != "gofer-agent" || s.Deadline != researchDeadline {
		t.Fatalf("unexpected run spec %+v", s)
	}
	p := s.Parameters
	if p["item"] != "github:o/r#1" || p["owner"] != "github:1" || p["ai_model"] != "m1" || p["gofer_url"] != "http://gofer" {
		t.Fatalf("unexpected parameters %v", p)
	}
	if _, has := p["turn"]; has {
		t.Fatalf("a research run must name no turn: %v", p)
	}
	if New(d, "", "", nil, "", "").CanResearch() {
		t.Fatal("CanResearch must be false without a default model")
	}
}

// One research run per item while it is in flight; another item runs concurrently;
// once the run has ended the item may be researched again.
func TestScheduledResearchGate(t *testing.T) {
	d := &gateDispatcher{phase: "Running"}
	ig := New(d, "", "", nil, "e", "m")
	ctx := context.Background()
	for range 3 {
		if _, err := ig.ScheduledResearch(ctx, "github:1", "item-a"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ig.ScheduledResearch(ctx, "github:1", "item-b"); err != nil {
		t.Fatal(err)
	}
	if d.count() != 2 {
		t.Fatalf("want one run per item (2), got %d", d.count())
	}
	d.set("Failed")
	if ok, _ := ig.ScheduledResearch(ctx, "github:1", "item-a"); !ok || d.count() != 3 {
		t.Fatalf("a failed research run must free the item: ok=%v runs=%d", ok, d.count())
	}
}

// While a Discuss run for the item is in flight, the schedule leaves research to it:
// that run researches the thread once its reply lands.
func TestScheduledResearchWaitsForDiscuss(t *testing.T) {
	d := &gateDispatcher{phase: "Running"}
	ig := New(d, "", "", nil, "e", "m")
	ctx := context.Background()
	if err := ig.Converse(ctx, "github:1", worklist.TurnRef{Item: "item-a", Turn: "u1"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if ok, err := ig.ScheduledResearch(ctx, "github:1", "item-a"); ok || err != nil {
		t.Fatalf("research dispatched during a Discuss run: ok=%v err=%v", ok, err)
	}
	if ok, _ := ig.ScheduledResearch(ctx, "github:1", "item-b"); !ok {
		t.Fatal("a Discuss run on another item must not hold this one")
	}
}

// researchStore returns a store holding items for one owner.
func researchStore(owner string, items ...worklist.WorkItem) *worklist.MemoryStore {
	s := worklist.NewMemoryStore()
	s.Seed(owner, items...)
	return s
}

func discussed(id string, at time.Time, research *worklist.ResearchAdjustment) worklist.WorkItem {
	return worklist.WorkItem{
		ID:     id,
		GitHub: worklist.GitHubRef{UpdatedAt: at.Add(-time.Hour)},
		Thread: []worklist.Message{
			{ID: id + "/u1", Role: worklist.RoleUser, Content: "q", At: at},
			{ID: id + "/a1", Role: worklist.RoleAgent, Content: "a", ReplyTo: id + "/u1", At: at.Add(time.Minute)},
		},
		Signals: worklist.Signals{Research: research},
	}
}

func researchedAtTime(at time.Time, turns ...string) *worklist.ResearchAdjustment {
	return &worklist.ResearchAdjustment{Relevance: 1, Impact: 1, Engagement: 1, Urgency: 1, AppliedAt: at, Turns: turns}
}

func researchItems(d *gateDispatcher) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, s := range d.specs {
		if s.TaskRef == taskResearch {
			out = append(out, s.Parameters["item"])
		}
	}
	return out
}

// The pass dispatches only stale items: one with current research, one with no
// discussion, and one whose only turns are hidden are left alone.
func TestResearchOnceSkipsCurrentAndUndiscussed(t *testing.T) {
	const owner = "github:1"
	t0 := time.Now().UTC().Add(-time.Hour)
	current := discussed("current", t0, researchedAtTime(t0.Add(5*time.Minute), "current/u1", "current/a1"))
	stale := discussed("stale", t0, nil)
	silent := worklist.WorkItem{ID: "silent", GitHub: worklist.GitHubRef{UpdatedAt: t0}}
	hidden := worklist.WorkItem{ID: "hidden", Thread: []worklist.Message{{ID: "h1", Role: worklist.RoleUser, Content: "x", At: t0, Hidden: true}}}
	store := researchStore(owner, current, stale, silent, hidden)

	d := &gateDispatcher{phase: "Running"}
	ig := New(d, "", "", nil, "e", "m")
	r := NewRefresher(store, ig, nil, time.Hour, nil).WithResearch(store, ig)
	if n := r.ResearchOnce(context.Background()); n != 1 {
		t.Fatalf("ResearchOnce submitted %d runs, want 1", n)
	}
	if got := researchItems(d); len(got) != 1 || got[0] != "stale" {
		t.Fatalf("researched %v, want [stale]", got)
	}
}

// A GitHub update after an item's research triggers one re-rank, however many ticks
// overlap it, and none once the research has caught up.
func TestResearchOnceLaterGitHubUpdateReranksOnce(t *testing.T) {
	const owner = "github:1"
	t0 := time.Now().UTC().Add(-time.Hour)
	item := discussed("pr", t0, researchedAtTime(t0.Add(5*time.Minute), "pr/u1", "pr/a1"))
	store := researchStore(owner, item)
	d := &gateDispatcher{phase: "Running"}
	ig := New(d, "", "", nil, "e", "m")
	r := NewRefresher(store, ig, nil, time.Hour, nil).WithResearch(store, ig)
	ctx := context.Background()

	if n := r.ResearchOnce(ctx); n != 0 {
		t.Fatalf("current research re-ranked: %d runs", n)
	}

	// A refresh stores a newer GitHub update.
	updated := t0.Add(30 * time.Minute)
	if err := store.Update(ctx, owner, func(cur []worklist.WorkItem) ([]worklist.WorkItem, error) {
		cur[0].GitHub.UpdatedAt = updated
		return cur, nil
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); r.ResearchOnce(ctx) }()
	}
	wg.Wait()
	r.ResearchOnce(ctx) // a later tick while the run is still going
	if got := researchItems(d); len(got) != 1 || got[0] != "pr" {
		t.Fatalf("researched %v, want one run for pr", got)
	}

	// The run writes back its adjustment and ends: nothing more to do.
	if err := worklist.ApplyResearch(ctx, store, owner, "pr", *researchedAtTime(time.Now().UTC(), "pr/u1", "pr/a1")); err != nil {
		t.Fatal(err)
	}
	d.set("Succeeded")
	if n := r.ResearchOnce(ctx); n != 0 {
		t.Fatalf("re-ranked again after the research caught up: %d runs", n)
	}
	if got := researchItems(d); len(got) != 1 {
		t.Fatalf("want exactly one re-rank in all, got %v", got)
	}
}

// The pass serves only owners the Eligibility confirms, and none but GitHub's.
func TestResearchOnceHonoursEligibility(t *testing.T) {
	t0 := time.Now().UTC().Add(-time.Hour)
	store := worklist.NewMemoryStore()
	store.Seed("github:1", discussed("a", t0, nil))
	store.Seed("github:2", discussed("b", t0, nil))
	store.Seed("gitlab:3", discussed("c", t0, nil))
	d := &gateDispatcher{phase: "Running"}
	ig := New(d, "", "", nil, "e", "m")
	r := NewRefresher(store, ig, eligibility{"github:2": nil}, time.Hour, nil).WithResearch(store, ig)
	r.ResearchOnce(context.Background())
	if got := researchItems(d); len(got) != 1 || got[0] != "a" {
		t.Fatalf("researched %v, want [a]", got)
	}
}

// A dispatch error ends the pass; the next tick carries on with what is left.
func TestResearchOnceStopsAtADispatchError(t *testing.T) {
	const owner = "github:1"
	t0 := time.Now().UTC().Add(-time.Hour)
	store := researchStore(owner, discussed("a", t0, nil), discussed("b", t0, nil))
	d := &gateDispatcher{phase: "Running", failNext: true}
	ig := New(d, "", "", nil, "e", "m")
	r := NewRefresher(store, ig, nil, time.Hour, nil).WithResearch(store, ig)
	ctx := context.Background()
	if n := r.ResearchOnce(ctx); n != 0 || d.count() != 0 {
		t.Fatalf("pass went on after a dispatch error: %d submitted, %d runs", n, d.count())
	}
	if n := r.ResearchOnce(ctx); n != 2 {
		t.Fatalf("next tick submitted %d, want 2", n)
	}
}

// Without WithResearch the pass does nothing.
func TestResearchOnceOffWithoutResearch(t *testing.T) {
	t0 := time.Now().UTC().Add(-time.Hour)
	store := researchStore("github:1", discussed("a", t0, nil))
	d := &gateDispatcher{phase: "Running"}
	r := NewRefresher(store, New(d, "", "", nil, "e", "m"), nil, time.Hour, nil)
	if n := r.ResearchOnce(context.Background()); n != 0 || d.count() != 0 {
		t.Fatalf("research ran without WithResearch: %d", n)
	}
}
