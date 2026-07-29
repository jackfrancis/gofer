package ingest

import (
	"context"
	"testing"
)

// recordDispatcher is a test Dispatcher standing in for an agent-runtime backend: it
// records the RunSpecs gofer submits, so a test can assert the run INTENT gofer builds
// even though the default dispatcher is a no-op.
type recordDispatcher struct{ specs []RunSpec }

func (d *recordDispatcher) Submit(_ context.Context, spec RunSpec) (string, error) {
	d.specs = append(d.specs, spec)
	return "run-" + spec.TaskRef, nil
}
func (d *recordDispatcher) Status(context.Context, string) (RunStatus, error) {
	return RunStatus{Phase: "Succeeded"}, nil
}

func TestNoopDispatcherAlwaysSucceeds(t *testing.T) {
	id, err := NoopDispatcher{}.Submit(context.Background(), RunSpec{TaskRef: "github-ingest"})
	if err != nil || id != "" {
		t.Fatalf("Submit = (%q, %v), want (\"\", nil)", id, err)
	}
	st, err := NoopDispatcher{}.Status(context.Background(), "anything")
	if err != nil || st.Phase != "Succeeded" {
		t.Fatalf("Status = (%+v, %v), want Succeeded, nil", st, err)
	}
}

func TestIngestorBuildsRunIntentAndSucceeds(t *testing.T) {
	d := &recordDispatcher{}
	ig := New(d, "gofer-agent", "http://gofer/sink", nil, "https://api.example/chat", "model-x")

	if err := ig.EnsureBackfill(context.Background(), "github:1"); err != nil {
		t.Fatalf("EnsureBackfill: %v", err)
	}
	if err := ig.Converse(context.Background(), "github:1", "item-7", "", ""); err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if got := len(d.specs); got != 2 {
		t.Fatalf("dispatched %d runs, want 2", got)
	}
	ingest, converse := d.specs[0], d.specs[1]
	if ingest.TaskRef != "github-ingest" || ingest.Parameters["owner"] != "github:1" {
		t.Fatalf("ingest run intent = %+v", ingest)
	}
	if converse.TaskRef != "github-converse" || converse.Parameters["item"] != "item-7" {
		t.Fatalf("converse run intent = %+v", converse)
	}
	// The default connection's coordinates ride to every run; the token never does.
	if ingest.Parameters["ai_endpoint"] != "https://api.example/chat" || ingest.Parameters["ai_model"] != "model-x" {
		t.Fatalf("model coordinates missing from run intent: %+v", ingest.Parameters)
	}
}

func TestBackfillNeverFailsWithoutRuntime(t *testing.T) {
	ig := New(NoopDispatcher{}, "", "", nil, "", "")
	failed, msg, err := ig.BackfillFailure(context.Background(), "github:1")
	if failed || msg != "" || err != nil {
		t.Fatalf("BackfillFailure = (%v, %q, %v), want (false, \"\", nil)", failed, msg, err)
	}
}
