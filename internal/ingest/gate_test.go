package ingest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// gateDispatcher is a concurrency-safe test backend: it records submitted runs, can
// hold each Submit until released, fail the next Submit, and reports every run in the
// phase it is set to.
type gateDispatcher struct {
	mu       sync.Mutex
	specs    []RunSpec
	phase    string
	failNext bool
	// hold, when set, blocks each Submit until it is closed; arrived gets a value as
	// each Submit starts waiting.
	hold    chan struct{}
	arrived chan struct{}
}

func (d *gateDispatcher) Submit(_ context.Context, spec RunSpec) (string, error) {
	if d.arrived != nil {
		d.arrived <- struct{}{}
	}
	if d.hold != nil {
		<-d.hold
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failNext {
		d.failNext = false
		return "", errors.New("backend unavailable")
	}
	d.specs = append(d.specs, spec)
	return fmt.Sprintf("run-%d", len(d.specs)), nil
}

func (d *gateDispatcher) Status(context.Context, string) (RunStatus, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return RunStatus{Phase: d.phase}, nil
}

func (d *gateDispatcher) set(phase string) {
	d.mu.Lock()
	d.phase = phase
	d.mu.Unlock()
}

func (d *gateDispatcher) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.specs)
}

var ref = worklist.TurnRef{Item: "item-7", Turn: "t1"}

// Identical requests while the first run is in flight produce one run, whether they
// arrive in turn (a double submit, Review-all retrying a turn still being answered) or
// at once.
func TestConverseCollapsesIdenticalRequests(t *testing.T) {
	ctx := context.Background()
	t.Run("in turn", func(t *testing.T) {
		d := &gateDispatcher{phase: "Running"}
		ig := New(d, "", "", nil, "", "")
		_ = ig.Converse(ctx, "github:1", ref, "", "")
		_ = ig.Converse(ctx, "github:1", ref, "", "")
		_ = ig.ReviewAll(ctx, "github:1", []worklist.TurnRef{ref}, "", "")
		_ = ig.Converse(ctx, "github:1", ref, "", "other-model") // the model is not part of the key
		if got := d.count(); got != 1 {
			t.Fatalf("dispatched %d runs, want 1", got)
		}
	})
	t.Run("at once", func(t *testing.T) {
		d := &gateDispatcher{phase: "Running"}
		ig := New(d, "", "", nil, "", "")
		var wg sync.WaitGroup
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = ig.Converse(ctx, "github:1", ref, "", "")
			}()
		}
		wg.Wait()
		if got := d.count(); got != 1 {
			t.Fatalf("dispatched %d runs, want 1", got)
		}
	})
}

// Requests for different items, different turns, or different owners are not
// duplicates: they run concurrently.
func TestConverseDifferentKeysRunConcurrently(t *testing.T) {
	refs := []worklist.TurnRef{
		{Item: "item-1", Turn: "t1"},
		{Item: "item-2", Turn: "t1"},
		{Item: "item-1", Turn: "t2"},
	}
	d := &gateDispatcher{phase: "Running", hold: make(chan struct{}), arrived: make(chan struct{}, 8)}
	ig := New(d, "", "", nil, "", "")
	done := make(chan error, 2)
	go func() { done <- ig.ReviewAll(context.Background(), "github:1", refs, "", "") }()
	go func() { done <- ig.Converse(context.Background(), "github:2", refs[0], "", "") }()
	// Every run must reach the backend while the others are still being submitted.
	for n := range len(refs) + 1 {
		select {
		case <-d.arrived:
		case <-time.After(5 * time.Second):
			close(d.hold)
			t.Fatalf("only %d of %d runs reached the backend concurrently", n, len(refs)+1)
		}
	}
	close(d.hold)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if got := d.count(); got != len(refs)+1 {
		t.Fatalf("dispatched %d runs, want %d", got, len(refs)+1)
	}
}

// Once the backend reports the run terminal — failed, or succeeded with only a notice
// — the turn can be retried; the store keeps it to one answer.
func TestConverseRedispatchesAfterTheRunEnds(t *testing.T) {
	for _, phase := range []string{"Failed", "Succeeded"} {
		t.Run(phase, func(t *testing.T) {
			d := &gateDispatcher{phase: "Running"}
			ig := New(d, "", "", nil, "", "")
			_ = ig.Converse(context.Background(), "github:1", ref, "", "")
			d.set(phase)
			_ = ig.Converse(context.Background(), "github:1", ref, "", "")
			if got := d.count(); got != 2 {
				t.Fatalf("dispatched %d runs, want 2", got)
			}
		})
	}
}

// A run past its deadline no longer blocks the turn, even if the backend still calls
// it running or cannot say (NoopDispatcher reports no run id).
func TestConverseRedispatchesAfterTheDeadline(t *testing.T) {
	d := &gateDispatcher{phase: "Running"}
	ig := New(d, "", "", nil, "", "")
	key := runKey{owner: "github:1", task: taskConverse, item: ref.Item, turn: ref.Turn}
	_ = ig.Converse(context.Background(), "github:1", ref, "", "")
	ig.mu.Lock()
	ig.runs[key].at = time.Now().Add(-converseDeadline)
	ig.mu.Unlock()
	_ = ig.Converse(context.Background(), "github:1", ref, "", "")
	if got := d.count(); got != 2 {
		t.Fatalf("dispatched %d runs, want 2", got)
	}

	noop := New(NoopDispatcher{}, "", "", nil, "", "")
	_ = noop.Converse(context.Background(), "github:1", ref, "", "")
	if _, ok := noop.reserve(context.Background(), key, converseGate, false); ok {
		t.Fatal("a run with no id must gate the turn until its deadline")
	}
}

// A Submit that fails leaves nothing in flight, so the next request dispatches.
func TestConverseSubmitErrorFreesTheTurn(t *testing.T) {
	d := &gateDispatcher{phase: "Running", failNext: true}
	ig := New(d, "", "", nil, "", "")
	if err := ig.Converse(context.Background(), "github:1", ref, "", ""); err == nil {
		t.Fatal("want the Submit error")
	}
	if err := ig.Converse(context.Background(), "github:1", ref, "", ""); err != nil {
		t.Fatal(err)
	}
	if got := d.count(); got != 1 {
		t.Fatalf("dispatched %d runs, want 1", got)
	}
}

// Finished converse runs are dropped from the tracker; the owner's ingest run is kept.
func TestGatePrunesFinishedConverseRuns(t *testing.T) {
	d := &gateDispatcher{phase: "Succeeded"}
	ig := New(d, "", "", nil, "", "")
	_ = ig.EnsureBackfill(context.Background(), "github:1")
	_ = ig.Converse(context.Background(), "github:1", ref, "", "")
	if done, _ := ig.BackfillSucceeded(context.Background(), "github:1"); !done {
		t.Fatal("want the backfill observed succeeded")
	}
	_, _, _, _ = ig.probeRun(context.Background(), runKey{owner: "github:1", task: taskConverse, item: ref.Item, turn: ref.Turn})
	_ = ig.Converse(context.Background(), "github:1", worklist.TurnRef{Item: "item-8", Turn: "t9"}, "", "")
	ig.mu.Lock()
	defer ig.mu.Unlock()
	if len(ig.runs) != 2 {
		t.Fatalf("want the ingest run and the newest converse run, got %d entries", len(ig.runs))
	}
	if _, ok := ig.runs[ingestKey("github:1")]; !ok {
		t.Fatal("the ingest run must be kept")
	}
}

// Concurrent empty-worklist polls start one backfill between them.
func TestEnsureBackfillConcurrentPollsStartOneRun(t *testing.T) {
	d := &gateDispatcher{phase: "Running"}
	ig := New(d, "", "", nil, "", "")
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = ig.EnsureBackfill(context.Background(), "github:1")
		}()
	}
	wg.Wait()
	if got := d.count(); got != 1 {
		t.Fatalf("dispatched %d runs, want 1", got)
	}
}

// A Refresh whose Submit fails leaves the previous run tracked, so the read model still
// knows how the last backfill ended.
func TestRefreshSubmitErrorKeepsThePreviousRun(t *testing.T) {
	d := &gateDispatcher{phase: "Succeeded"}
	ig := New(d, "", "", nil, "", "")
	_ = ig.EnsureBackfill(context.Background(), "github:1")
	d.mu.Lock()
	d.failNext = true
	d.mu.Unlock()
	if err := ig.Refresh(context.Background(), "github:1"); err == nil {
		t.Fatal("want the Submit error")
	}
	if done, _ := ig.BackfillSucceeded(context.Background(), "github:1"); !done {
		t.Fatal("the previous backfill must still be tracked")
	}
}

// A backfill that succeeded and found nothing keeps the owner gated until retryAfter:
// every empty-worklist read calls EnsureBackfill before asking how the run ended, so
// freeing the key on success would start a new ingest on every reload and send the
// radar back to "Discovering…".
func TestEnsureBackfillStaysGatedAfterSuccess(t *testing.T) {
	ctx := context.Background()
	d := &gateDispatcher{phase: "Succeeded"}
	ig := New(d, "", "", nil, "", "")
	_ = ig.EnsureBackfill(ctx, "github:1")
	for range 3 {
		if done, _ := ig.BackfillSucceeded(ctx, "github:1"); !done {
			t.Fatal("want the backfill reported succeeded")
		}
		_ = ig.EnsureBackfill(ctx, "github:1")
	}
	if got := d.count(); got != 1 {
		t.Fatalf("dispatched %d runs, want 1", got)
	}
	ig.mu.Lock()
	ig.runs[ingestKey("github:1")].at = time.Now().Add(-retryAfter)
	ig.mu.Unlock()
	_ = ig.EnsureBackfill(ctx, "github:1")
	if got := d.count(); got != 2 {
		t.Fatalf("dispatched %d runs after retryAfter, want 2", got)
	}
}

// step scripts one Submit's result.
type step struct {
	id  string
	err error
}

// scriptedDispatcher hands each Submit, in arrival order, a channel the test answers
// it on, so a test can decide which of two concurrent submissions returns first and
// how.
type scriptedDispatcher struct {
	calls chan chan step
	phase string
}

func (d *scriptedDispatcher) Submit(context.Context, RunSpec) (string, error) {
	ch := make(chan step)
	d.calls <- ch
	s := <-ch
	return s.id, s.err
}

func (d *scriptedDispatcher) Status(context.Context, string) (RunStatus, error) {
	return RunStatus{Phase: d.phase}, nil
}

// A Refresh forced while a backfill is still being submitted must not lose the
// backfill's outcome: when the backfill's Submit succeeds and the Refresh's fails, the
// backfill run is tracked again, with its id, so its result can be reported.
func TestRefreshDuringBackfillSubmitKeepsTheBackfillOutcome(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name          string
		backfillFirst bool
	}{{"backfill returns first", true}, {"refresh returns first", false}} {
		t.Run(tc.name, func(t *testing.T) {
			d := &scriptedDispatcher{calls: make(chan chan step), phase: "Succeeded"}
			ig := New(d, "", "", nil, "", "")
			backfillDone, refreshDone := make(chan error, 1), make(chan error, 1)
			go func() { backfillDone <- ig.EnsureBackfill(ctx, "github:1") }()
			backfill := <-d.calls
			go func() { refreshDone <- ig.Refresh(ctx, "github:1") }()
			refresh := <-d.calls
			if tc.backfillFirst {
				backfill <- step{id: "run-backfill"}
				<-backfillDone
				refresh <- step{err: errors.New("backend unavailable")}
				<-refreshDone
			} else {
				refresh <- step{err: errors.New("backend unavailable")}
				<-refreshDone
				backfill <- step{id: "run-backfill"}
				<-backfillDone
			}
			ig.mu.Lock()
			cur := ig.runs[ingestKey("github:1")]
			id, pending := cur.id, cur.pending
			ig.mu.Unlock()
			if id != "run-backfill" || pending {
				t.Fatalf("want the backfill run tracked with its id, got id=%q pending=%v", id, pending)
			}
			if done, _ := ig.BackfillSucceeded(ctx, "github:1"); !done {
				t.Fatal("the backfill's outcome must be reportable")
			}
		})
	}
}

// When both a backfill and the Refresh that replaced it fail to submit, the key goes
// back to the run before them.
func TestRefreshRollbackSkipsFailedSubmissions(t *testing.T) {
	ctx := context.Background()
	d := &scriptedDispatcher{calls: make(chan chan step), phase: "Succeeded"}
	ig := New(d, "", "", nil, "", "")
	go func() { _ = ig.EnsureBackfill(ctx, "github:1") }()
	(<-d.calls) <- step{id: "run-first"}
	for ig.tracked("github:1") != "run-first" {
		time.Sleep(time.Millisecond)
	}
	ig.mu.Lock()
	ig.runs[ingestKey("github:1")].failed = true // so the next poll may reserve
	ig.mu.Unlock()
	backfillDone, refreshDone := make(chan error, 1), make(chan error, 1)
	go func() { backfillDone <- ig.EnsureBackfill(ctx, "github:1") }()
	backfill := <-d.calls
	go func() { refreshDone <- ig.Refresh(ctx, "github:1") }()
	refresh := <-d.calls
	backfill <- step{err: errors.New("down")}
	<-backfillDone
	refresh <- step{err: errors.New("down")}
	<-refreshDone
	if got := ig.tracked("github:1"); got != "run-first" {
		t.Fatalf("want the first run tracked again, got %q", got)
	}
}

// tracked returns the id of ownerID's tracked ingest run.
func (i *Ingestor) tracked(ownerID string) string {
	i.mu.Lock()
	defer i.mu.Unlock()
	if r := i.runs[ingestKey(ownerID)]; r != nil {
		return r.id
	}
	return ""
}
