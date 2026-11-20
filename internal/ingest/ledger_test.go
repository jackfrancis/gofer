package ingest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// memLedger is an in-memory RunLedger: what a database keeps across a restart.
type memLedger struct {
	mu   sync.Mutex
	recs map[string]IngestRecord
}

func newMemLedger() *memLedger { return &memLedger{recs: map[string]IngestRecord{}} }

func (l *memLedger) IngestRuns(context.Context) (map[string]IngestRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]IngestRecord, len(l.recs))
	for k, v := range l.recs {
		out[k] = v
	}
	return out, nil
}

func (l *memLedger) PutIngestRun(_ context.Context, owner string, rec IngestRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recs[owner] = rec
	return nil
}

func (l *memLedger) get(owner string) IngestRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.recs[owner]
}

// restart builds a fresh Ingestor over the same backend and ledger, as a new web pod.
func restart(t *testing.T, d Dispatcher, l RunLedger) *Ingestor {
	t.Helper()
	ig := New(d, "", "", nil, "", "")
	if err := ig.UseLedger(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	return ig
}

// A run still executing when the web tier restarts keeps gating the owner in the new
// process: no tick, Refresh or empty-worklist poll starts a second ingest until the
// backend reports it ended. The refresh version carries over too.
func TestLedgerKeepsARunningIngestAcrossRestart(t *testing.T) {
	ctx := context.Background()
	d := &gateDispatcher{phase: "Running"}
	l := newMemLedger()
	ig := restart(t, d, l)
	if ok, err := ig.ScheduledRefresh(ctx, "github:1"); !ok || err != nil {
		t.Fatalf("first refresh: %v, %v", ok, err)
	}
	if rec := l.get("github:1"); rec.RunID != "run-1" || rec.Gen != 1 || rec.Done != 0 {
		t.Fatalf("recorded %+v", rec)
	}

	ig2 := restart(t, d, l)
	if ok, _ := ig2.ScheduledRefresh(ctx, "github:1"); ok {
		t.Fatal("the restarted web tier started a second ingest while the first was running")
	}
	_ = ig2.Refresh(ctx, "github:1")
	_ = ig2.EnsureBackfill(ctx, "github:1")
	if d.count() != 1 {
		t.Fatalf("dispatched %d runs across the restart, want 1", d.count())
	}
	if done, active := ig2.LocalRefreshState("github:1"); done != 0 || !active {
		t.Fatalf("restored state: done %d active %v", done, active)
	}

	d.set("Succeeded")
	if done, active, err := ig2.RefreshState(ctx, "github:1"); done != 1 || active || err != nil {
		t.Fatalf("after the run ended: done %d active %v err %v", done, active, err)
	}
	if rec := l.get("github:1"); rec.Done != 1 {
		t.Fatalf("the end of the run was not recorded: %+v", rec)
	}
	if ok, _ := ig2.ScheduledRefresh(ctx, "github:1"); !ok {
		t.Fatal("a refresh after the restored run ended must dispatch")
	}
	if rec := l.get("github:1"); rec.RunID != "run-2" || rec.Gen != 2 || rec.Done != 1 {
		t.Fatalf("recorded %+v, want run-2 at generation 2", rec)
	}

	// A third process sees both: the version never goes back.
	if done, _ := restart(t, d, l).LocalRefreshState("github:1"); done != 1 {
		t.Fatalf("done after a second restart = %d, want 1", done)
	}
}

// A recorded run past its deadline counts as ended after a restart, so it neither
// blocks the owner nor holds the refresh version back.
func TestLedgerStaleRunCountsAsEnded(t *testing.T) {
	ctx := context.Background()
	l := newMemLedger()
	_ = l.PutIngestRun(ctx, "github:1", IngestRecord{RunID: "old", Gen: 4, Done: 3, AcceptedAt: time.Now().Add(-retryAfter - time.Minute)})
	d := &gateDispatcher{phase: "Running"}
	ig := restart(t, d, l)
	if done, active := ig.LocalRefreshState("github:1"); done != 4 || active {
		t.Fatalf("stale run: done %d active %v, want 4, false", done, active)
	}
	if ok, _ := ig.ScheduledRefresh(ctx, "github:1"); !ok || l.get("github:1").Gen != 5 {
		t.Fatalf("a refresh after a stale run must dispatch generation 5: %+v", l.get("github:1"))
	}
}

// A restored run the backend no longer knows (its status read fails) blocks the owner
// only until its deadline.
func TestLedgerUnknownRunBlocksUntilItsDeadline(t *testing.T) {
	ctx := context.Background()
	l := newMemLedger()
	_ = l.PutIngestRun(ctx, "github:1", IngestRecord{RunID: "gone", Gen: 1, AcceptedAt: time.Now()})
	d := &errStatusDispatcher{}
	ig := restart(t, d, l)
	if ok, _ := ig.ScheduledRefresh(ctx, "github:1"); ok {
		t.Fatal("dispatched while the restored run's status is unknown")
	}
	ig.mu.Lock()
	ig.runs[ingestKey("github:1")].at = time.Now().Add(-retryAfter)
	ig.mu.Unlock()
	if ok, _ := ig.ScheduledRefresh(ctx, "github:1"); !ok {
		t.Fatal("past the deadline the owner must be refreshed again")
	}
}

// The refresh version moves when a run ends, also when the next run has already
// started: a page that polls only while B runs still learns that A ended.
func TestRefreshVersionMovesWhileTheNextRunRuns(t *testing.T) {
	ctx := context.Background()
	d := &gateDispatcher{phase: "Running"}
	ig := New(d, "", "", nil, "", "")
	_, _ = ig.ScheduledRefresh(ctx, "github:1") // A, generation 1
	if done, active := ig.LocalRefreshState("github:1"); done != 0 || !active {
		t.Fatalf("A running: done %d active %v", done, active)
	}
	d.set("Succeeded")
	_, _ = ig.ScheduledRefresh(ctx, "github:1") // B starts: the gate sees A ended
	d.set("Running")
	done, active, err := ig.RefreshState(ctx, "github:1")
	if done != 1 || !active || err != nil {
		t.Fatalf("A ended, B running: done %d active %v err %v; want 1, true", done, active, err)
	}
	if d.count() != 2 {
		t.Fatalf("dispatched %d runs, want 2", d.count())
	}
}

// panicStatus fails the test if anything asks the backend.
type panicStatus struct{ gateDispatcher }

func (p *panicStatus) Status(context.Context, string) (RunStatus, error) {
	panic("LocalRefreshState must not call the backend")
}

func TestLocalRefreshStateNeverAsksTheBackend(t *testing.T) {
	ctx := context.Background()
	d := &panicStatus{}
	ig := New(d, "", "", nil, "", "")
	if _, a := ig.LocalRefreshState("github:1"); a {
		t.Fatal("active before any run")
	}
	_ = ig.Refresh(ctx, "github:1")
	if done, a := ig.LocalRefreshState("github:1"); done != 0 || !a {
		t.Fatalf("a run gofer has not seen end counts as in flight: done %d active %v", done, a)
	}
}

// capacityDispatcher accepts at most room submissions per pass, like a full AEI
// dispatch queue, and reports every run succeeded.
type capacityDispatcher struct {
	mu     sync.Mutex
	room   int
	left   int
	counts map[string]int
	n      int
}

func (d *capacityDispatcher) Submit(_ context.Context, spec RunSpec) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.left == 0 {
		return "", errors.New("aei: overloaded")
	}
	d.left--
	d.n++
	d.counts[spec.Subject]++
	return fmt.Sprintf("run-%d", d.n), nil
}

func (d *capacityDispatcher) Status(context.Context, string) (RunStatus, error) {
	return RunStatus{Phase: "Succeeded"}, nil
}

// When the runtime's queue fills, the owners it turned away go first on the next
// pass, so the owners share the capacity evenly instead of the first ones taking
// every slot (12 slots over 6 passes: 4 each).
func TestRefresherGivesDeferredOwnersTheNextTurn(t *testing.T) {
	ctx := context.Background()
	d := &capacityDispatcher{room: 2, counts: map[string]int{}}
	r := NewRefresher(seeded("github:1", "github:2", "github:3"), New(d, "", "", nil, "", ""), nil, time.Hour, nil)
	for range 6 {
		d.mu.Lock()
		d.left = d.room
		d.mu.Unlock()
		r.RefreshOnce(ctx)
	}
	lo, hi := 1<<30, 0
	for _, o := range []string{"github:1", "github:2", "github:3"} {
		c := d.counts[o]
		lo, hi = min(lo, c), max(hi, c)
	}
	if lo != 4 || hi != 4 {
		t.Fatalf("refreshes per owner %v over 6 passes with room for 2: an owner was starved", d.counts)
	}
}
