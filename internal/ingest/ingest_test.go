package ingest

import (
	"context"
	"sort"
	"sync"
	"testing"

	"github.com/jackfrancis/agent-execution-interface/aei"
	"github.com/jackfrancis/agent-execution-interface/sdks/go/aeiapp"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// recordDispatcher records every submitted run spec, safely under concurrency.
type recordDispatcher struct {
	mu    sync.Mutex
	specs []aei.RunSpec
}

func (d *recordDispatcher) Submit(_ context.Context, spec aei.RunSpec) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.specs = append(d.specs, spec)
	return "run-" + spec.Parameters["item"], nil
}

func (d *recordDispatcher) Status(context.Context, string) (aeiapp.Result, error) {
	return aeiapp.Result{}, nil
}

// ReviewAll dispatches exactly one github-converse run per item, each carrying its
// own item id and the owner's agent-plane identity. Run with -race to also exercise
// the concurrent launch for data races.
func TestReviewAllDispatchesPerItem(t *testing.T) {
	d := &recordDispatcher{}
	ing := New(d, "gofer-agent", "http://gofer.svc:8080", nil, "", "")

	ids := []string{"github:o/r#1", "github:o/r#2", "github:o/r#3"}
	if err := ing.ReviewAll(context.Background(), "u1", ids, "https://ep", "m1"); err != nil {
		t.Fatal(err)
	}
	if len(d.specs) != len(ids) {
		t.Fatalf("expected %d dispatches, got %d", len(ids), len(d.specs))
	}
	var gotItems []string
	for _, s := range d.specs {
		if s.TaskRef != "github-converse" {
			t.Errorf("taskRef = %q, want github-converse", s.TaskRef)
		}
		if s.Identity.Subject != "u1" || s.Identity.Audience != "gofer-agent" {
			t.Errorf("identity = %+v, want subject u1 / audience gofer-agent", s.Identity)
		}
		if s.Parameters["ai_endpoint"] != "https://ep" || s.Parameters["ai_model"] != "m1" {
			t.Errorf("review dispatch model = %q@%q, want m1@https://ep", s.Parameters["ai_model"], s.Parameters["ai_endpoint"])
		}
		gotItems = append(gotItems, s.Parameters["item"])
	}
	sort.Strings(gotItems)
	for i := range ids {
		if gotItems[i] != ids[i] {
			t.Fatalf("dispatched items = %v, want %v", gotItems, ids)
		}
	}
}

// Refresh forces a github-ingest run even while a recent backfill is still tracked,
// where EnsureBackfill would no-op — so a user can pull in newly created work on
// demand.
func TestRefreshForcesIngest(t *testing.T) {
	d := &recordDispatcher{}
	ing := New(d, "gofer-agent", "", nil, "", "")

	if err := ing.EnsureBackfill(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	// A second EnsureBackfill is gated out (a recent run is in flight)...
	if err := ing.EnsureBackfill(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	// ...but Refresh dispatches regardless.
	if err := ing.Refresh(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}

	ingests := 0
	for _, s := range d.specs {
		if s.TaskRef == "github-ingest" {
			ingests++
		}
	}
	if ingests != 2 {
		t.Fatalf("github-ingest dispatches = %d, want 2 (one gated backfill + one forced refresh)", ingests)
	}
}

// SecondOpinion dispatches an INDEPENDENT (blind) github-converse run that overrides
// the endpoint and model (routing to the chosen connection; the token is shared) and
// carries independent=true, so the runtime answers blind with the chosen model. It
// errors when no model is given.
func TestSecondOpinionDispatchesChosenModel(t *testing.T) {
	d := &recordDispatcher{}
	ing := New(d, "gofer-agent", "http://gofer.svc:8080", nil, "https://default", "default-model")

	if err := ing.SecondOpinion(context.Background(), "u1", "github:o/r#1", "https://second", "second-model"); err != nil {
		t.Fatal(err)
	}
	if len(d.specs) != 1 {
		t.Fatalf("expected 1 dispatch, got %d", len(d.specs))
	}
	s := d.specs[0]
	if s.TaskRef != "github-converse" {
		t.Errorf("taskRef = %q, want github-converse", s.TaskRef)
	}
	// The chosen connection's endpoint + model override the ingestor's default.
	if s.Parameters["ai_endpoint"] != "https://second" || s.Parameters["ai_model"] != "second-model" {
		t.Errorf("second-opinion params wrong (want chosen endpoint + model): %+v", s.Parameters)
	}
	// The review-panel review is blind: the runtime must skip the prior thread.
	if s.Parameters["independent"] != "true" {
		t.Errorf("independent param = %q, want true (blind review): %+v", s.Parameters["independent"], s.Parameters)
	}
	if s.Parameters["item"] != "github:o/r#1" {
		t.Errorf("item = %q, want github:o/r#1", s.Parameters["item"])
	}

	// No model given -> error, no dispatch.
	d2 := &recordDispatcher{}
	ing2 := New(d2, "gofer-agent", "", nil, "https://default", "default-model")
	if err := ing2.SecondOpinion(context.Background(), "u1", "x", "", ""); err == nil {
		t.Fatal("expected an error when no model is given")
	}
	if len(d2.specs) != 0 {
		t.Fatal("no dispatch expected when no model is given")
	}
}

// fakeAppender records the thread appends the ingestor makes when chaining a
// review-panel synthesis turn.
type fakeAppender struct {
	appends []struct{ owner, item, content, kind string }
}

func (f *fakeAppender) AppendUserTurn(_ context.Context, owner, item, content, kind string) (bool, error) {
	f.appends = append(f.appends, struct{ owner, item, content, kind string }{owner, item, content, kind})
	return true, nil
}

// The review panel is a two-stage flow: SecondOpinion dispatches the blind review by
// the alternative model, and when that review writes back the ingestor chains a
// consensus synthesis by the DEFAULT model onto the same thread. The chained dispatch
// is run inline here (spawn override) for determinism.
func TestReviewPanelChainsSynthesis(t *testing.T) {
	d := &recordDispatcher{}
	ing := New(d, "gofer-agent", "http://gofer.svc:8080", nil, "https://default", "default-model")
	app := &fakeAppender{}
	ing.SetThreadAppender(app)
	ing.spawn = func(f func()) { f() } // run the chained synthesis inline

	// Click "Run review panel": one blind review by the alternative connection.
	if err := ing.SecondOpinion(context.Background(), "u1", "item1", "https://alt", "alt-model"); err != nil {
		t.Fatal(err)
	}
	if len(d.specs) != 1 {
		t.Fatalf("expected 1 independent dispatch, got %d", len(d.specs))
	}
	if iv := d.specs[0]; iv.Parameters["independent"] != "true" || iv.Parameters["ai_endpoint"] != "https://alt" || iv.Parameters["ai_model"] != "alt-model" {
		t.Fatalf("independent review params wrong: %+v", iv.Parameters)
	}

	// The independent review writes back -> the synthesis is chained.
	ing.NoteWriteback("run-item1")
	if len(d.specs) != 2 {
		t.Fatalf("expected the chained synthesis dispatch, got %d specs", len(d.specs))
	}
	sy := d.specs[1]
	if _, ok := sy.Parameters["independent"]; ok {
		t.Errorf("synthesis must not be independent (it must see every review): %+v", sy.Parameters)
	}
	// Synthesis routes to the DEFAULT connection (no per-turn override).
	if sy.Parameters["ai_endpoint"] != "https://default" || sy.Parameters["ai_model"] != "default-model" {
		t.Errorf("synthesis should use the default connection: %+v", sy.Parameters)
	}
	if len(app.appends) != 1 || app.appends[0].content != synthesisPrompt || app.appends[0].item != "item1" {
		t.Fatalf("synthesis prompt not appended to the item: %+v", app.appends)
	}
	if app.appends[0].kind != worklist.KindSynthesisRequest {
		t.Errorf("synthesis turn kind = %q, want %q", app.appends[0].kind, worklist.KindSynthesisRequest)
	}

	// A re-delivered write-back for the same run must not chain a second synthesis.
	ing.NoteWriteback("run-item1")
	if len(d.specs) != 2 {
		t.Fatal("synthesis must not double-fire on a repeated write-back")
	}
}

// SecondOpinionAll dispatches an independent (blind) review for each item, routed to
// the given alternative connection, so the bulk "Get 2nd Opinion" acts like clicking
// the per-item panel on each PR. Run with -race to exercise the concurrent launch.
func TestSecondOpinionAllDispatchesPerItem(t *testing.T) {
	d := &recordDispatcher{}
	ing := New(d, "gofer-agent", "http://gofer.svc:8080", nil, "https://default", "default-model")
	ing.SetThreadAppender(&fakeAppender{})

	ids := []string{"item1", "item2", "item3"}
	if err := ing.SecondOpinionAll(context.Background(), "u1", ids, "https://alt", "alt-model"); err != nil {
		t.Fatal(err)
	}
	if len(d.specs) != len(ids) {
		t.Fatalf("expected %d independent dispatches, got %d", len(ids), len(d.specs))
	}
	for _, s := range d.specs {
		if s.Parameters["independent"] != "true" || s.Parameters["ai_endpoint"] != "https://alt" || s.Parameters["ai_model"] != "alt-model" {
			t.Errorf("bulk 2nd-opinion dispatch params wrong: %+v", s.Parameters)
		}
	}

	// No model given -> error, no dispatch.
	d2 := &recordDispatcher{}
	ing2 := New(d2, "gofer-agent", "", nil, "https://default", "default-model")
	if err := ing2.SecondOpinionAll(context.Background(), "u1", ids, "", ""); err == nil {
		t.Fatal("expected an error when no model is given")
	}
	if len(d2.specs) != 0 {
		t.Fatal("no dispatch expected when no model is given")
	}
}
