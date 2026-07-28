package agent

import (
	"context"
	"testing"

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
