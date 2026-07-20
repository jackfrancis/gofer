package agent

import (
	"context"
	"testing"

	"github.com/jackfrancis/gofer/internal/worklist"
)

type fakeConverser struct {
	reply   string
	gotUser string
}

func (f *fakeConverser) Reply(_ context.Context, _ worklist.WorkItem, _ string, _ string, _ []worklist.Message, userText string, _ worklist.ToolBox) (string, error) {
	f.gotUser = userText
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
