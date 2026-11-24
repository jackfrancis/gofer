package ingest

import (
	"context"
	"errors"
	"testing"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// The review panel's synthesis is a converse run answering the chained turn by the
// DEFAULT model, reading the whole thread (not independent), even when the context
// it is handed is already cancelled: the agent plane detaches it from the reply's
// request.
func TestSynthesizeDispatchesDefaultModelConverse(t *testing.T) {
	d := &recordDispatcher{}
	ig := New(d, "gofer-agent", "http://gofer/sink", nil, "https://api.example/chat", "model-x")
	ig.Synthesize(context.WithoutCancel(context.Background()), "github:1", worklist.TurnRef{Item: "item-7", Turn: "s1"})
	if len(d.specs) != 1 {
		t.Fatalf("dispatched %d runs, want 1", len(d.specs))
	}
	spec := d.specs[0]
	p := spec.Parameters
	if spec.TaskRef != "github-converse" || p["item"] != "item-7" || p["turn"] != "s1" || spec.Subject != "github:1" {
		t.Fatalf("synthesis run intent = %+v", spec)
	}
	if p["ai_endpoint"] != "https://api.example/chat" || p["ai_model"] != "model-x" {
		t.Fatalf("the synthesis must use the default model, got %+v", p)
	}
	if _, blind := p["independent"]; blind {
		t.Fatalf("the synthesis must read the thread, got %+v", p)
	}
}

// runningDispatcher records submissions and reports every run still running.
type runningDispatcher struct{ recordDispatcher }

func (d *runningDispatcher) Status(context.Context, string) (RunStatus, error) {
	return RunStatus{Phase: "Running"}, nil
}

// A retry's attempt rides to the run as a parameter, and a dispatch that shares a run
// already in flight says so (ErrRunShared, naming the turn), so the web tier keeps
// that run's attempt on the turn.
func TestConverseAttemptAndSharedRun(t *testing.T) {
	d := &runningDispatcher{}
	ig := New(d, "gofer-agent", "http://gofer/sink", nil, "https://api.example/chat", "model-x")
	first := worklist.TurnRef{Item: "item-7", Turn: "t1"}
	if err := ig.Converse(context.Background(), "github:1", first, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.specs[0].Parameters["attempt"]; ok {
		t.Fatalf("a first attempt carries no attempt parameter, got %+v", d.specs[0].Parameters)
	}
	retry := worklist.TurnRef{Item: "item-7", Turn: "t1", Attempt: "a2"}
	err := ig.Converse(context.Background(), "github:1", retry, "", "")
	if !errors.Is(err, ErrRunShared) || len(d.specs) != 1 {
		t.Fatalf("a retry while the run is in flight must share it and say so, got %v and %d submissions", err, len(d.specs))
	}
	if got := SharedRuns(errors.Join(errors.New("other"), err)); len(got) != 1 || got[0] != retry {
		t.Fatalf("SharedRuns = %+v, want the retried turn", got)
	}
	if err := ig.Converse(context.Background(), "github:1", worklist.TurnRef{Item: "item-8", Turn: "t9", Attempt: "a2"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if got := d.specs[1].Parameters["attempt"]; got != "a2" {
		t.Fatalf("attempt parameter = %q, want a2", got)
	}
}
