package agent

import (
	"context"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

type fakeConverser struct {
	reply      string
	gotUser    string
	gotHistory []worklist.Message
}

func (f *fakeConverser) Reply(_ context.Context, _ worklist.WorkItem, _ string, _ string, history []worklist.Message, userText string, _ worklist.ToolBox) (string, error) {
	f.gotUser = userText
	f.gotHistory = history
	return f.reply, nil
}

func TestRunConverseAppendsReply(t *testing.T) {
	sink := &memSink{items: []worklist.WorkItem{{
		ID:     "github:o/r#1",
		Thread: []worklist.Message{{Role: worklist.RoleUser, Content: "review this?"}},
	}}}
	fc := &fakeConverser{reply: "Sure — here's a draft."}
	err := Run(context.Background(),
		Params{JobType: JobConverse, ItemID: "github:o/r#1", Converser: fc},
		fakeVendor{}, sink)
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
	if err := Run(context.Background(), Params{JobType: JobConverse, ItemID: "github:o/r#1", Converser: fc}, fakeVendor{}, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.items[0].Thread) != 2 {
		t.Fatalf("no reply should be appended, got %d turns", len(sink.items[0].Thread))
	}
}

func TestRunConverseNoConverser(t *testing.T) {
	if err := Run(context.Background(), Params{JobType: JobConverse, ItemID: "x"}, fakeVendor{}, &memSink{}); err == nil {
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
	if err := Run(context.Background(), Params{JobType: JobConverse, ItemID: "github:o/r#1", Converser: fc}, fakeVendor{}, sink); err != nil {
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
	if err := Run(context.Background(), Params{JobType: JobConverse, ItemID: "github:o/r#1", Converser: fc}, fakeVendor{}, sink); err != nil {
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
		Params{JobType: JobConverse, ItemID: "github:o/r#1", Converser: fc, Researcher: &fakeResearcher{}},
		fakeVendor{}, sink)
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
	if err := Run(context.Background(), Params{JobType: JobConverse, ItemID: "github:o/r#1", Converser: fc}, fakeVendor{}, sink); err != nil {
		t.Fatal(err)
	}
	if n := len(sink.items[0].Thread); n != 0 {
		t.Fatalf("a reply to a cleared thread must be dropped, got %d turns", n)
	}
}
