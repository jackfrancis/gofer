package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

type fakeResearcher struct{ adj worklist.ResearchAdjustment }

func (f *fakeResearcher) Research(context.Context, worklist.WorkItem) (worklist.ResearchAdjustment, error) {
	return f.adj, nil
}

func TestRunResearchSetsAdjustment(t *testing.T) {
	sink := &memSink{items: []worklist.WorkItem{{
		ID:     "github:o/r#1",
		Thread: []worklist.Message{{Role: worklist.RoleUser, Content: "q"}},
	}}}
	fr := &fakeResearcher{adj: worklist.ResearchAdjustment{Relevance: 1.5, Impact: 1, Engagement: 1, Urgency: 1}}
	if err := Run(context.Background(), Params{JobType: JobResearch, ItemID: "github:o/r#1", Researcher: fr}, fakeVendor{}, sink); err != nil {
		t.Fatal(err)
	}
	r := sink.items[0].Signals.Research
	if r == nil || r.Relevance != 1.5 {
		t.Fatalf("expected research adjustment set, got %+v", r)
	}
}

func TestRunResearchNoResearcher(t *testing.T) {
	if err := Run(context.Background(), Params{JobType: JobResearch, ItemID: "x"}, fakeVendor{}, &memSink{}); err == nil {
		t.Fatal("expected an error when no researcher is configured")
	}
}

// sideEffectResearcher runs during() while the "model" is researching, to stand in
// for a Refresh landing in the web tier meanwhile.
type sideEffectResearcher struct {
	adj    worklist.ResearchAdjustment
	during func()
}

func (f *sideEffectResearcher) Research(context.Context, worklist.WorkItem) (worklist.ResearchAdjustment, error) {
	f.during()
	return f.adj, nil
}

// refreshDuring returns a func that applies a Refresh to sink's first item: a new
// title and state, as the web tier would store them while a run works.
func refreshDuring(sink *memSink) func() {
	return func() {
		sink.items[0].GitHub.Title = "Refreshed title"
		sink.items[0].GitHub.State = "closed"
		sink.items[0].Signals.Comments = 9
	}
}

// A research pass that finishes after a Refresh writes back only its adjustment: the
// refreshed title, state and signals stay in place.
func TestRunResearchAfterRefreshKeepsGitHubFields(t *testing.T) {
	sink := &memSink{items: []worklist.WorkItem{{
		ID:     "github:o/r#1",
		GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1, Title: "Old title", State: "open"},
		Thread: []worklist.Message{{Role: worklist.RoleUser, Content: "q"}},
	}}}
	fr := &sideEffectResearcher{adj: worklist.ResearchAdjustment{Relevance: 1.5, Impact: 1, Engagement: 1, Urgency: 1}, during: refreshDuring(sink)}
	if err := Run(context.Background(), Params{JobType: JobResearch, ItemID: "github:o/r#1", Researcher: fr}, fakeVendor{}, sink); err != nil {
		t.Fatal(err)
	}
	assertRefreshKept(t, sink)
	if sink.ingests != 0 {
		t.Fatalf("research must not write back the whole item, got %d ingest calls", sink.ingests)
	}
}

func assertRefreshKept(t *testing.T, sink *memSink) {
	t.Helper()
	got := sink.items[0]
	if got.GitHub.Title != "Refreshed title" || got.GitHub.State != "closed" || got.Signals.Comments != 9 {
		t.Fatalf("a late write-back undid the refresh: title %q state %q comments %d", got.GitHub.Title, got.GitHub.State, got.Signals.Comments)
	}
	if r := got.Signals.Research; r == nil || r.Relevance != 1.5 || r.AppliedAt.IsZero() {
		t.Fatalf("the research adjustment must still land, stamped, got %+v", r)
	}
}

// errResearcher fails every research call, as a model outage after a Discuss reply.
type errResearcher struct{}

func (errResearcher) Research(context.Context, worklist.WorkItem) (worklist.ResearchAdjustment, error) {
	return worklist.ResearchAdjustment{}, errors.New("model unavailable")
}

// capturingResearcher records the item it was asked to research.
type capturingResearcher struct {
	got worklist.WorkItem
	adj worklist.ResearchAdjustment
}

func (c *capturingResearcher) Research(_ context.Context, item worklist.WorkItem) (worklist.ResearchAdjustment, error) {
	c.got = item
	return c.adj, nil
}

// A post-reply adjustment that fails leaves the item stale, and the research run the
// schedule then dispatches sets the adjustment without adding a reply: the thread
// keeps exactly one answer.
func TestFailedPostReplyResearchIsRetriedWithoutASecondReply(t *testing.T) {
	sink := &memSink{items: []worklist.WorkItem{{
		ID:     "github:o/r#1",
		Thread: []worklist.Message{{ID: "u1", Role: worklist.RoleUser, Content: "why is this urgent?", At: time.Now().UTC()}},
	}}}
	fc := &fakeConverser{reply: "Because the release is blocked."}
	if err := Run(context.Background(),
		Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: "github:o/r#1", Turn: "u1", Converser: fc, Researcher: errResearcher{}},
		fakeVendor{token: "t"}, sink); err != nil {
		t.Fatalf("a failed post-reply research must not fail the run: %v", err)
	}
	if sink.replies != 1 || sink.items[0].Signals.Research != nil {
		t.Fatalf("want one reply and no research, got %d replies, research %+v", sink.replies, sink.items[0].Signals.Research)
	}
	if !worklist.ResearchStale(sink.items[0]) {
		t.Fatal("an item whose post-reply research failed must be stale")
	}

	if err := Run(context.Background(), Params{JobType: JobResearch, ItemID: "github:o/r#1", Researcher: turnsResearcher{}}, fakeVendor{}, sink); err != nil {
		t.Fatal(err)
	}
	got := sink.items[0]
	if sink.replies != 1 || len(got.Thread) != 2 {
		t.Fatalf("the retry added a turn: %d replies, thread %+v", sink.replies, got.Thread)
	}
	if got.Signals.Research == nil || got.Signals.Research.Relevance != 1.5 {
		t.Fatalf("research not set by the retry: %+v", got.Signals.Research)
	}
	if worklist.ResearchStale(got) {
		t.Fatal("the item is still stale after a successful retry")
	}
}

// A research run never hands the researcher a turn the owner hid.
func TestRunResearchWithholdsHiddenTurns(t *testing.T) {
	sink := &memSink{items: []worklist.WorkItem{{
		ID: "github:o/r#1",
		Thread: []worklist.Message{
			{ID: "u1", Role: worklist.RoleUser, Content: "visible"},
			{ID: "u2", Role: worklist.RoleUser, Content: "secret", Hidden: true},
		},
	}}}
	cr := &capturingResearcher{adj: worklist.ResearchAdjustment{Relevance: 1, Impact: 1, Engagement: 1, Urgency: 1}}
	if err := Run(context.Background(), Params{JobType: JobResearch, ItemID: "github:o/r#1", Researcher: cr}, fakeVendor{}, sink); err != nil {
		t.Fatal(err)
	}
	for _, m := range cr.got.Thread {
		if m.Hidden || m.Content == "secret" {
			t.Fatalf("hidden turn reached the researcher: %+v", cr.got.Thread)
		}
	}
	if len(cr.got.Thread) != 1 {
		t.Fatalf("want the one visible turn, got %+v", cr.got.Thread)
	}
}

// Research run with a turn hidden is current; unhiding that turn makes it stale, so
// the restored evidence is reconsidered without any other activity.
func TestResearchIsStaleOnceAHiddenTurnIsUnhidden(t *testing.T) {
	now := time.Now().UTC()
	sink := &memSink{items: []worklist.WorkItem{{
		ID:     "github:o/r#1",
		GitHub: worklist.GitHubRef{UpdatedAt: now.Add(-time.Hour)},
		Thread: []worklist.Message{
			{ID: "u1", Role: worklist.RoleUser, Content: "q", At: now.Add(-3 * time.Minute)},
			{ID: "u2", Role: worklist.RoleUser, Content: "withdrawn", At: now.Add(-2 * time.Minute), Hidden: true},
			{ID: "a1", Role: worklist.RoleAgent, Content: "a", ReplyTo: "u1", At: now.Add(-time.Minute)},
		},
	}}}
	if err := Run(context.Background(), Params{JobType: JobResearch, ItemID: "github:o/r#1", Researcher: turnsResearcher{}}, fakeVendor{}, sink); err != nil {
		t.Fatal(err)
	}
	if worklist.ResearchStale(sink.items[0]) {
		t.Fatalf("fresh research is stale: %+v", sink.items[0].Signals.Research)
	}
	sink.items[0].Thread[1].Hidden = false // the owner unhides u2
	if !worklist.ResearchStale(sink.items[0]) {
		t.Fatal("research that never read an unhidden turn must be stale")
	}
}

// A research run records the GitHub version it read, so a refresh that lands while
// the model works (an update older than the research's AppliedAt) still makes it
// stale.
func TestResearchIsStaleAfterARefreshLandsDuringIt(t *testing.T) {
	read := time.Now().UTC().Add(-time.Hour)
	githubChange := time.Now().UTC().Add(-time.Minute) // before the research is applied
	sink := &memSink{items: []worklist.WorkItem{{
		ID:     "github:o/r#1",
		GitHub: worklist.GitHubRef{UpdatedAt: read},
		Thread: []worklist.Message{{ID: "u1", Role: worklist.RoleUser, Content: "q", At: read}},
	}}}
	fr := &sideEffectResearcher{
		adj:    worklist.ResearchAdjustment{Relevance: 1, Impact: 1, Engagement: 1, Urgency: 1, Turns: []string{"u1"}},
		during: func() { sink.items[0].GitHub.UpdatedAt = githubChange },
	}
	if err := Run(context.Background(), Params{JobType: JobResearch, ItemID: "github:o/r#1", Researcher: fr}, fakeVendor{}, sink); err != nil {
		t.Fatal(err)
	}
	r := sink.items[0].Signals.Research
	if r == nil || !r.GitHubUpdatedAt.Equal(read) || !r.AppliedAt.After(githubChange) {
		t.Fatalf("want research applied after the change, recording the version it read; got %+v", r)
	}
	if !worklist.ResearchStale(sink.items[0]) {
		t.Fatal("research that never saw the GitHub change must be stale")
	}
}
