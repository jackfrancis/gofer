package ingest

import (
	"context"
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
