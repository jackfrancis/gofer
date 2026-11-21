package ingest

import (
	"context"
	"sync"
	"testing"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// lockedDispatcher records RunSpecs from concurrent dispatches (the bulk actions).
type lockedDispatcher struct {
	mu    sync.Mutex
	specs []RunSpec
}

func (d *lockedDispatcher) Submit(_ context.Context, spec RunSpec) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.specs = append(d.specs, spec)
	return "run", nil
}
func (d *lockedDispatcher) Status(context.Context, string) (RunStatus, error) {
	return RunStatus{Phase: "Succeeded"}, nil
}

// In the model-gateway profile every run says so (ai_auth=gateway), including a
// review routed to another model, so no run tries to vend a model token the web tier
// does not hold. The endpoint and model still travel per run: the gateway routes by
// them.
func TestModelGatewayMarksEveryRun(t *testing.T) {
	const gw = "http://gw/v1/chat/completions"
	d := &lockedDispatcher{}
	ig := New(d, "gofer-agent", "http://gofer/sink", nil, gw, "model-a")
	ig.UseModelGateway()
	ctx := context.Background()
	refs := []worklist.TurnRef{{Item: "i1", Turn: "t1"}, {Item: "i2", Turn: "t2"}}

	if err := ig.Refresh(ctx, "github:1"); err != nil {
		t.Fatal(err)
	}
	if err := ig.ReviewAll(ctx, "github:1", refs, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := ig.SecondOpinionAll(ctx, "github:1", refs, "http://gw/v1/responses", "model-b"); err != nil {
		t.Fatal(err)
	}
	if len(d.specs) != 5 {
		t.Fatalf("dispatched %d runs, want 5", len(d.specs))
	}
	models := map[string]int{}
	for _, s := range d.specs {
		if s.Parameters["ai_auth"] != AIAuthGateway {
			t.Errorf("%s run: ai_auth = %q, want %q", s.TaskRef, s.Parameters["ai_auth"], AIAuthGateway)
		}
		models[s.Parameters["ai_endpoint"]+" "+s.Parameters["ai_model"]]++
	}
	if models[gw+" model-a"] != 3 || models["http://gw/v1/responses model-b"] != 2 {
		t.Fatalf("model routing per run = %v, want 3 default and 2 second-opinion runs", models)
	}
}

func TestNoModelGatewayByDefault(t *testing.T) {
	d := &lockedDispatcher{}
	ig := New(d, "gofer-agent", "http://gofer/sink", nil, "https://api.example/chat", "model-x")
	if err := ig.Refresh(context.Background(), "github:1"); err != nil {
		t.Fatal(err)
	}
	if v, ok := d.specs[0].Parameters["ai_auth"]; ok {
		t.Fatalf("ai_auth = %q without the gateway, want unset (the run vends the model token)", v)
	}
}
