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
	if rec := l.get("github:1"); rec.RunID != "run-1" || rec.RunGen != 1 || rec.Done != 0 || rec.Pending {
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
	if rec := l.get("github:1"); rec.RunID != "run-2" || rec.RunGen != 2 || rec.Done != 1 {
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
	_ = l.PutIngestRun(ctx, "github:1", IngestRecord{Gen: 4, Done: 3, RunGen: 4, RunID: "old", RunAt: time.Now().Add(-retryAfter - time.Minute)})
	d := &gateDispatcher{phase: "Running"}
	ig := restart(t, d, l)
	if done, active := ig.LocalRefreshState("github:1"); done != 4 || active {
		t.Fatalf("stale run: done %d active %v, want 4, false", done, active)
	}
	if ok, _ := ig.ScheduledRefresh(ctx, "github:1"); !ok || l.get("github:1").RunGen != 5 {
		t.Fatalf("a refresh after a stale run must dispatch generation 5: %+v", l.get("github:1"))
	}
}

// A restored run the backend no longer knows (its status read fails) blocks the owner
// only until its deadline.
func TestLedgerUnknownRunBlocksUntilItsDeadline(t *testing.T) {
	ctx := context.Background()
	l := newMemLedger()
	_ = l.PutIngestRun(ctx, "github:1", IngestRecord{Gen: 1, RunGen: 1, RunID: "gone", RunAt: time.Now()})
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
		return "", fmt.Errorf("aei: overloaded: %w", ErrNotAccepted)
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

// lostDispatcher accepts each run but loses the answer: Submit fails without saying
// the backend refused, as when the response never reaches gofer.
type lostDispatcher struct {
	gateDispatcher
	lost bool
}

func (d *lostDispatcher) Submit(ctx context.Context, spec RunSpec) (string, error) {
	id, err := d.gateDispatcher.Submit(ctx, spec)
	if err == nil && d.lost {
		return "", errors.New("aei: dispatch github-ingest (acceptance unknown): connection reset")
	}
	return id, err
}

// A dispatch whose answer was lost may have started a run, so the owner stays held
// until the reservation's deadline: in this process and, from the ledger, after a
// restart. Then it counts as ended, and the next refresh dispatches.
func TestLostDispatchAnswerHoldsTheOwnerUntilItsDeadline(t *testing.T) {
	ctx := context.Background()
	d := &lostDispatcher{gateDispatcher: gateDispatcher{phase: "Running"}, lost: true}
	l := newMemLedger()
	ig := restart(t, d, l)
	if ok, err := ig.ScheduledRefresh(ctx, "github:1"); ok || err == nil {
		t.Fatalf("a lost answer: ok=%v err=%v, want the error", ok, err)
	}
	d.lost = false
	if ok, _ := ig.ScheduledRefresh(ctx, "github:1"); ok {
		t.Fatal("a second ingest started while the first may be running")
	}
	_ = ig.Refresh(ctx, "github:1")
	_ = ig.EnsureBackfill(ctx, "github:1")
	if d.count() != 1 {
		t.Fatalf("the backend received %d runs, want 1", d.count())
	}
	if a, _ := ig.Active(ctx, "github:1"); !a {
		t.Fatal("a run of unknown acceptance counts as in flight")
	}
	if rec := l.get("github:1"); !rec.Pending || rec.RunGen != 1 || rec.RunID != "" {
		t.Fatalf("recorded %+v, want a pending reservation at generation 1", rec)
	}

	ig2 := restart(t, d, l)
	if ok, _ := ig2.ScheduledRefresh(ctx, "github:1"); ok {
		t.Fatal("after a restart, a second ingest started while the first may be running")
	}
	if done, active := ig2.LocalRefreshState("github:1"); done != 0 || !active {
		t.Fatalf("restored: done %d active %v", done, active)
	}

	ig2.mu.Lock()
	ig2.runs[ingestKey("github:1")].until = time.Now()
	ig2.mu.Unlock()
	if done, active := ig2.LocalRefreshState("github:1"); done != 1 || active {
		t.Fatalf("past its deadline: done %d active %v, want 1, false", done, active)
	}
	if ok, _ := ig2.ScheduledRefresh(ctx, "github:1"); !ok {
		t.Fatal("past the deadline the owner must be refreshed again")
	}
}

// A definite refusal releases the reservation at once, and the record goes back to
// what it was.
func TestRefusedDispatchReleasesTheReservation(t *testing.T) {
	ctx := context.Background()
	d := &gateDispatcher{phase: "Succeeded", failNext: true} // a refusal (ErrNotAccepted)
	l := newMemLedger()
	ig := restart(t, d, l)
	if ok, err := ig.ScheduledRefresh(ctx, "github:1"); ok || !errors.Is(err, ErrNotAccepted) {
		t.Fatalf("refused: ok=%v err=%v", ok, err)
	}
	if rec := l.get("github:1"); rec.Pending || rec.RunGen != 0 {
		t.Fatalf("after a refusal the record holds no run: %+v", rec)
	}
	if ok, _ := ig.ScheduledRefresh(ctx, "github:1"); !ok {
		t.Fatal("after a refusal the next refresh dispatches")
	}
}

// peekDispatcher reads the ledger when Submit is called.
type peekDispatcher struct {
	gateDispatcher
	l    *memLedger
	seen IngestRecord
}

func (d *peekDispatcher) Submit(ctx context.Context, spec RunSpec) (string, error) {
	d.seen = d.l.get(spec.Subject)
	return d.gateDispatcher.Submit(ctx, spec)
}

// failingLedger fails every write.
type failingLedger struct{ memLedger }

func (*failingLedger) PutIngestRun(context.Context, string, IngestRecord) error {
	return errors.New("database is locked")
}

// The reservation is recorded before the backend sees the run; a reservation that
// cannot be recorded is not dispatched.
func TestReservationRecordedBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	l := newMemLedger()
	d := &peekDispatcher{gateDispatcher: gateDispatcher{phase: "Running"}, l: l}
	ig := restart(t, d, l)
	if ok, err := ig.ScheduledRefresh(ctx, "github:1"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if !d.seen.Pending || d.seen.RunGen != 1 {
		t.Fatalf("at dispatch the ledger held %+v, want the pending reservation", d.seen)
	}

	f := &failingLedger{memLedger: *newMemLedger()}
	d2 := &gateDispatcher{phase: "Running"}
	ig2 := restart(t, d2, f)
	if ok, err := ig2.ScheduledRefresh(ctx, "github:1"); ok || err == nil || d2.count() != 0 {
		t.Fatalf("unrecorded reservation: ok=%v err=%v runs=%d, want nothing dispatched", ok, err, d2.count())
	}
	if a, _ := ig2.Active(ctx, "github:1"); a {
		t.Fatal("an unrecorded reservation must not hold the owner")
	}
}

// A page that slept through a run and first polls after its deadline still learns
// it ended: observing the expiry raises the version, and records it.
func TestExpiryObservedByAPollRaisesTheVersion(t *testing.T) {
	ctx := context.Background()
	d := &gateDispatcher{phase: "Running"}
	l := newMemLedger()
	ig := restart(t, d, l)
	_, _ = ig.ScheduledRefresh(ctx, "github:1")
	d.set("Succeeded") // nobody asks before the deadline
	ig.mu.Lock()
	ig.runs[ingestKey("github:1")].at = time.Now().Add(-retryAfter)
	ig.mu.Unlock()
	if done, active, err := ig.RefreshState(ctx, "github:1"); done != 1 || active || err != nil {
		t.Fatalf("poll after the deadline: done %d active %v err %v, want 1", done, active, err)
	}
	if rec := l.get("github:1"); rec.Done != 1 {
		t.Fatalf("the expiry was not recorded: %+v", rec)
	}
}

// failingRead fails to load.
type failingRead struct{ memLedger }

func (*failingRead) IngestRuns(context.Context) (map[string]IngestRecord, error) {
	return nil, errors.New("database is locked")
}

// A ledger that cannot be read is an error, and is not attached.
func TestUseLedgerReadFailure(t *testing.T) {
	ig := New(&gateDispatcher{}, "", "", nil, "", "")
	if err := ig.UseLedger(context.Background(), &failingRead{}); err == nil {
		t.Fatal("want the read error")
	}
	if ig.ledger != nil {
		t.Fatal("a ledger that could not be read must not be attached")
	}
}

// An uncertain run is held until the backend's own deadline for it could have
// passed: the backend starts that clock when it accepts, which can be as late as the
// end of the dispatch, so the hold runs from when Submit returned (plus a grace for a
// backend still finishing the request), not from the reservation. A restart honours
// the recorded expiry, even when the reservation itself is older than a deadline.
func TestUncertainRunHeldThroughTheBackendsDeadline(t *testing.T) {
	ctx := context.Background()
	d := &lostDispatcher{gateDispatcher: gateDispatcher{phase: "Running"}, lost: true}
	l := newMemLedger()
	ig := restart(t, d, l)
	before := time.Now()
	_, _ = ig.ScheduledRefresh(ctx, "github:1")
	returned := time.Now()
	rec := l.get("github:1")
	// Submit returned (and any acceptance happened) between before and returned.
	if !rec.Pending || rec.RunUntil.Before(before.Add(holdUncertain)) || rec.RunUntil.After(returned.Add(holdUncertain)) {
		t.Fatalf("recorded expiry %v, want Submit's return (%v..%v) + %v", rec.RunUntil, before, returned, holdUncertain)
	}
	if rec.RunAt.Before(before) || rec.RunAt.After(returned) {
		t.Fatalf("recorded reservation time %v", rec.RunAt)
	}

	// The reservation is older than a run deadline, but the backend may have
	// accepted it late: the recorded expiry still holds the owner after a restart.
	_ = l.PutIngestRun(ctx, "github:1", IngestRecord{Gen: 1, RunGen: 1, Pending: true,
		RunAt: time.Now().Add(-ingestDeadline - time.Second), RunUntil: time.Now().Add(time.Minute)})
	d.lost = false
	if ok, _ := restart(t, d, l).ScheduledRefresh(ctx, "github:1"); ok {
		t.Fatal("dispatched before the uncertain run's recorded expiry")
	}

	// A pending record from before expiries were recorded assumes the longest hold.
	_ = l.PutIngestRun(ctx, "github:1", IngestRecord{Gen: 1, RunGen: 1, Pending: true,
		RunAt: time.Now().Add(-ingestDeadline - time.Second)})
	if ok, _ := restart(t, d, l).ScheduledRefresh(ctx, "github:1"); ok {
		t.Fatal("dispatched within holdPending of a reservation with no recorded expiry")
	}
}

// ctxDispatcher records the context Submit was called with.
type ctxDispatcher struct {
	gateDispatcher
	err      error
	deadline time.Time
}

func (d *ctxDispatcher) Submit(ctx context.Context, spec RunSpec) (string, error) {
	d.err = ctx.Err()
	d.deadline, _ = ctx.Deadline()
	return d.gateDispatcher.Submit(ctx, spec)
}

// An ingest dispatch is bounded by submitTimeout and does not end with the request
// that asked for it, so a browser going away cannot leave acceptance unknown; and
// while gofer has not heard back, the record holds the owner through the whole
// dispatch window.
func TestIngestSubmitIsDetachedAndBounded(t *testing.T) {
	cctx, cancel := context.WithCancel(context.Background())
	d := &ctxDispatcher{gateDispatcher: gateDispatcher{phase: "Running"}}
	ig := restart(t, d, newMemLedger())
	cancel()
	start := time.Now()
	if err := ig.Refresh(cctx, "github:1"); err != nil {
		t.Fatal(err)
	}
	if d.err != nil || d.deadline.IsZero() || d.deadline.After(start.Add(submitTimeout+time.Second)) {
		t.Fatalf("Submit's context: err %v, deadline %v; want live and bounded by %v", d.err, d.deadline, submitTimeout)
	}

	l := newMemLedger()
	pd := &peekDispatcher{gateDispatcher: gateDispatcher{phase: "Running"}, l: l}
	_, _ = restart(t, pd, l).ScheduledRefresh(context.Background(), "github:2")
	if want := pd.seen.RunAt.Add(holdPending); !pd.seen.Pending || !pd.seen.RunUntil.Equal(want) {
		t.Fatalf("at dispatch the record held %+v, want expiry %v", pd.seen, want)
	}
}
