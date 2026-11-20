package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// deadGitHub is a GitHub that answers 404 to everything: a turn runs with a vended
// token but gets no live context (the snapshot fetch is best-effort), the shape of
// these tests before runs told "GitHub unavailable" apart from "no context found".
func deadGitHub(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	return srv.URL
}

type fakeConverser struct {
	reply      string
	gotItem    worklist.WorkItem
	gotUser    string
	gotHistory []worklist.Message
	gotTools   worklist.ToolBox
}

func (f *fakeConverser) Reply(_ context.Context, item worklist.WorkItem, _ string, _ string, history []worklist.Message, userText string, tools worklist.ToolBox) (string, error) {
	f.gotItem = item
	f.gotUser = userText
	f.gotHistory = history
	f.gotTools = tools
	return f.reply, nil
}

func TestRunConverseAppendsReply(t *testing.T) {
	sink := &memSink{items: []worklist.WorkItem{{
		ID:     "github:o/r#1",
		Thread: []worklist.Message{{Role: worklist.RoleUser, Content: "review this?"}},
	}}}
	fc := &fakeConverser{reply: "Sure — here's a draft."}
	err := Run(context.Background(),
		Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: "github:o/r#1", Converser: fc},
		fakeVendor{token: "t"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	th := sink.items[0].Thread
	if len(th) != 2 || th[1].Role != worklist.RoleAgent || th[1].Content != "Sure — here's a draft." {
		t.Fatalf("expected an appended agent reply, got %+v", th)
	}
	if fc.gotUser != "review this?" {
		t.Fatalf("converser should get the last user turn, got %q", fc.gotUser)
	}
}

// A thread whose last turn is already the assistant's has nothing new to answer.
func TestRunConverseNothingPending(t *testing.T) {
	sink := &memSink{items: []worklist.WorkItem{{
		ID: "github:o/r#1",
		Thread: []worklist.Message{
			{Role: worklist.RoleUser, Content: "q"},
			{Role: worklist.RoleAgent, Content: "a"},
		},
	}}}
	fc := &fakeConverser{reply: "should not be called"}
	if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: "github:o/r#1", Converser: fc}, fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.items[0].Thread) != 2 {
		t.Fatalf("no reply should be appended, got %d turns", len(sink.items[0].Thread))
	}
}

func TestRunConverseNoConverser(t *testing.T) {
	if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: "x"}, fakeVendor{token: "t"}, &memSink{}); err == nil {
		t.Fatal("expected an error when no converser is configured")
	}
}

// A turn the user has hidden is withheld from the model: it never appears in the
// history handed to the converser for a later turn.
func TestRunConverseExcludesHiddenHistory(t *testing.T) {
	sink := &memSink{items: []worklist.WorkItem{{
		ID: "github:o/r#1",
		Thread: []worklist.Message{
			{Role: worklist.RoleUser, Content: "q1"},
			{Role: worklist.RoleAgent, Content: "a1"},
			{Role: worklist.RoleUser, Content: "q2", Hidden: true},
			{Role: worklist.RoleAgent, Content: "a2", Hidden: true},
			{Role: worklist.RoleUser, Content: "q3"},
		},
	}}}
	fc := &fakeConverser{reply: "ok"}
	if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: "github:o/r#1", Converser: fc}, fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	if fc.gotUser != "q3" {
		t.Fatalf("answered turn = %q, want q3", fc.gotUser)
	}
	for _, m := range fc.gotHistory {
		if m.Hidden {
			t.Fatalf("a hidden turn leaked into the model history: %+v", m)
		}
	}
	if len(fc.gotHistory) != 2 {
		t.Fatalf("expected 2 visible history turns (q1, a1), got %d: %+v", len(fc.gotHistory), fc.gotHistory)
	}
}

// A reply to a synthesis request has its leading verdict token lifted into the
// structured Verdict field and stripped from the displayed content.
func TestRunConverseSynthesisVerdict(t *testing.T) {
	sink := &memSink{items: []worklist.WorkItem{{
		ID:     "github:o/r#1",
		Thread: []worklist.Message{{Role: worklist.RoleUser, Content: "synthesize", Kind: worklist.KindSynthesisRequest}},
	}}}
	fc := &fakeConverser{reply: "AGREEMENT\nBoth reviews say it's ready."}
	if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: "github:o/r#1", Converser: fc}, fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	th := sink.items[0].Thread
	if len(th) != 2 || th[1].Verdict != worklist.VerdictAgree {
		t.Fatalf("expected the agent reply tagged with the agree verdict, got %+v", th)
	}
	if th[1].Content != "Both reviews say it's ready." {
		t.Fatalf("verdict token should be stripped from content, got %q", th[1].Content)
	}
}

// sideEffectConverser runs during() while the "model" is answering, to stand in for
// what the owner does in the web tier meanwhile.
type sideEffectConverser struct {
	reply  string
	during func()
}

func (f *sideEffectConverser) Reply(context.Context, worklist.WorkItem, string, string, []worklist.Message, string, worklist.ToolBox) (string, error) {
	f.during()
	return f.reply, nil
}

// The owner posts a follow-up while the run is answering. The reply lands, and the
// follow-up is not overwritten by the run's stale copy of the thread — including the
// write-back of the research signal that follows the reply.
func TestRunConverseKeepsTurnsPostedMeanwhile(t *testing.T) {
	t0 := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	sink := &memSink{items: []worklist.WorkItem{{
		ID:     "github:o/r#1",
		Thread: []worklist.Message{{Role: worklist.RoleUser, Content: "first question", At: t0}},
	}}}
	fc := &sideEffectConverser{reply: "answer to the first", during: func() {
		sink.items[0].Thread = append(sink.items[0].Thread, worklist.Message{Role: worklist.RoleUser, Content: "follow-up", At: t0.Add(time.Minute)})
	}}
	err := Run(context.Background(),
		Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: "github:o/r#1", Converser: fc, Researcher: &fakeResearcher{}},
		fakeVendor{token: "t"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	th := sink.items[0].Thread
	if len(th) != 3 || th[1].Content != "follow-up" || th[2].Content != "answer to the first" || th[2].Role != worklist.RoleAgent {
		t.Fatalf("want [question, follow-up, reply], got %+v", th)
	}
	if sink.items[0].Signals.Research == nil {
		t.Fatal("the research signal should still be written")
	}
}

// The owner clears the conversation while the run is answering: the reply to the
// cleared question is dropped rather than resurrecting a one-sided thread.
func TestRunConverseDropsReplyToClearedThread(t *testing.T) {
	sink := &memSink{items: []worklist.WorkItem{{
		ID:     "github:o/r#1",
		Thread: []worklist.Message{{Role: worklist.RoleUser, Content: "q", At: time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)}},
	}}}
	fc := &sideEffectConverser{reply: "too late", during: func() { sink.items[0].Thread = nil }}
	if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: "github:o/r#1", Converser: fc}, fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	if n := len(sink.items[0].Thread); n != 0 {
		t.Fatalf("a reply to a cleared thread must be dropped, got %d turns", n)
	}
}

// recordingConverser answers with a fixed reply and records the turns it was asked.
type recordingConverser struct {
	reply string
	asked []string
}

func (f *recordingConverser) Reply(_ context.Context, _ worklist.WorkItem, _ string, _ string, _ []worklist.Message, userText string, _ worklist.ToolBox) (string, error) {
	f.asked = append(f.asked, userText)
	return f.reply, nil
}

// The review's reported scenario, through the real store sink: run A answers a review
// request; the owner posts a follow-up and run B is dispatched for it; A's reply lands
// before B starts. B must still answer the follow-up, and the review must still count.
func TestConverseFollowUpAfterOutOfOrderReply(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	store := worklist.NewMemoryStore()
	store.Seed("u1", worklist.WorkItem{ID: "pr", Type: worklist.TypePullRequest, Thread: []worklist.Message{
		{ID: "r", Role: worklist.RoleUser, Kind: worklist.KindReviewRequest, Content: "review this", At: t0},
	}})
	sink := NewStoreSink(store, "u1")
	postFollowUp := func() {
		_ = store.Update(ctx, "u1", func(cur []worklist.WorkItem) ([]worklist.WorkItem, error) {
			cur[0].Thread = append(cur[0].Thread, worklist.Message{ID: "f", Role: worklist.RoleUser, Content: "follow-up", At: t0.Add(time.Minute)})
			return cur, nil
		})
	}

	// Run A: the follow-up is posted while the model answers the review.
	a := &sideEffectConverser{reply: "the review", during: postFollowUp}
	if err := Run(ctx, Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: "pr", Turn: "r", Model: "x", Converser: a}, fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	// Run B starts only now, with the thread ending on A's reply.
	b := &recordingConverser{reply: "the answer"}
	if err := Run(ctx, Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: "pr", Turn: "f", Model: "y", Converser: b}, fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	if len(b.asked) != 1 || b.asked[0] != "follow-up" {
		t.Fatalf("run B must answer the follow-up, was asked %q", b.asked)
	}
	items, _ := store.List(ctx, "u1")
	it := items[0]
	if len(it.Thread) != 4 || it.Thread[2].ReplyTo != "r" || it.Thread[3].ReplyTo != "f" {
		t.Fatalf("want [review, follow-up, reply->r, reply->f], got %+v", it.Thread)
	}
	if !it.HasReview() || it.FirstReviewModel() != "x" {
		t.Fatalf("the review answered out of order must count: HasReview=%v FirstReviewModel=%q", it.HasReview(), it.FirstReviewModel())
	}
	if it.ReplyPending(t0.Add(2*time.Minute), 5*time.Minute) {
		t.Fatal("nothing is pending once both turns are answered")
	}
}

// A run answers the turn it was dispatched for, not the newest one: run A, dispatched
// for the review, starts only after the follow-up is posted.
func TestConverseAnswersTheDispatchedTurn(t *testing.T) {
	sink := &memSink{items: []worklist.WorkItem{{ID: "pr", Thread: []worklist.Message{
		{ID: "r", Role: worklist.RoleUser, Content: "review this"},
		{ID: "f", Role: worklist.RoleUser, Content: "follow-up"},
	}}}}
	a := &recordingConverser{reply: "the review"}
	if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: "pr", Turn: "r", Converser: a}, fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	if len(a.asked) != 1 || a.asked[0] != "review this" {
		t.Fatalf("run A must answer the review, was asked %q", a.asked)
	}
	if last := sink.items[0].Thread[2]; last.ReplyTo != "r" {
		t.Fatalf("the reply must name the review turn, got %+v", last)
	}
}

// A run whose turn is already answered (a duplicate dispatch) or gone does nothing.
func TestConverseSkipsAnsweredOrMissingTurn(t *testing.T) {
	for name, turn := range map[string]string{"answered": "r", "missing": "nope"} {
		t.Run(name, func(t *testing.T) {
			sink := &memSink{items: []worklist.WorkItem{{ID: "pr", Thread: []worklist.Message{
				{ID: "r", Role: worklist.RoleUser, Content: "q"},
				{ID: "a", ReplyTo: "r", Role: worklist.RoleAgent, Content: "done"},
			}}}}
			c := &recordingConverser{reply: "again"}
			if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: "pr", Turn: turn, Converser: c}, fakeVendor{token: "t"}, sink); err != nil {
				t.Fatal(err)
			}
			if len(c.asked) != 0 || len(sink.items[0].Thread) != 2 {
				t.Fatalf("nothing should be answered, asked=%q thread=%d", c.asked, len(sink.items[0].Thread))
			}
		})
	}
}

// A review that finishes after a Refresh leaves the refreshed title and state in
// place: the post-reply research write-back carries only the adjustment, not the item
// the run read when it started.
func TestRunConverseAfterRefreshKeepsGitHubFields(t *testing.T) {
	t0 := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	sink := &memSink{items: []worklist.WorkItem{{
		ID:     "github:o/r#1",
		GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1, Title: "Old title", State: "open"},
		Thread: []worklist.Message{{ID: "q1", Role: worklist.RoleUser, Content: "Can you review this PR?", Kind: worklist.KindReviewRequest, At: t0}},
	}}}
	fc := &sideEffectConverser{reply: "Looks good.", during: refreshDuring(sink)}
	fr := &fakeResearcher{adj: worklist.ResearchAdjustment{Relevance: 1.5, Impact: 1, Engagement: 1, Urgency: 1}}
	err := Run(context.Background(),
		Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: "github:o/r#1", Converser: fc, Researcher: fr},
		fakeVendor{token: "t"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	assertRefreshKept(t, sink)
	if th := sink.items[0].Thread; len(th) != 2 || th[1].Content != "Looks good." {
		t.Fatalf("the review reply must land, got %+v", th)
	}
	if sink.ingests != 0 {
		t.Fatalf("converse must not write back the whole item, got %d ingest calls", sink.ingests)
	}
}

// turnsResearcher records the turns it read, as the model-backed researcher does.
type turnsResearcher struct{ rationale string }

func (r turnsResearcher) Research(_ context.Context, item worklist.WorkItem) (worklist.ResearchAdjustment, error) {
	adj := worklist.ResearchAdjustment{Relevance: 1.5, Impact: 1, Engagement: 1, Urgency: 1, Rationale: r.rationale}
	for i := range item.Thread {
		adj.Turns = append(adj.Turns, item.TurnKey(i))
	}
	return adj, nil
}

// The owner resets the conversation and starts a new one while a review is being
// answered. The late reply is dropped, and so is the research the run derived from
// the old thread, even though it is stamped after the new thread's research.
func TestRunConverseDropsResearchOnClearedThread(t *testing.T) {
	t0 := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	current := worklist.ResearchAdjustment{Relevance: 0.5, Impact: 1, Engagement: 1, Urgency: 1, Rationale: "new thread", AppliedAt: t0, Turns: []string{"n1"}}
	sink := &memSink{items: []worklist.WorkItem{{
		ID:     "github:o/r#1",
		Thread: []worklist.Message{{ID: "o1", Role: worklist.RoleUser, Content: "old question", At: t0}},
	}}}
	fc := &sideEffectConverser{reply: "answer to the old question", during: func() {
		sink.items[0].Thread = []worklist.Message{{ID: "n1", Role: worklist.RoleUser, Content: "new question", At: t0.Add(time.Minute)}}
		sink.items[0].Signals.Research = &current
	}}
	err := Run(context.Background(),
		Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: "github:o/r#1", Converser: fc, Researcher: turnsResearcher{rationale: "old thread"}},
		fakeVendor{token: "t"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if th := sink.items[0].Thread; len(th) != 1 || th[0].ID != "n1" {
		t.Fatalf("the reply to the cleared question must be dropped, got %+v", th)
	}
	if r := sink.items[0].Signals.Research; r == nil || r.Rationale != "new thread" {
		t.Fatalf("research on the cleared thread replaced the current thread's: %+v", r)
	}
}
