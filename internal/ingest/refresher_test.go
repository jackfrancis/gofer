package ingest

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// seeded returns a store holding one item for each owner.
func seeded(owners ...string) *worklist.MemoryStore {
	s := worklist.NewMemoryStore()
	for _, o := range owners {
		s.Seed(o, worklist.WorkItem{ID: "github:o/r#1"})
	}
	return s
}

// subjects returns the owners of every submitted run, sorted.
func (d *gateDispatcher) subjects() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.specs))
	for _, s := range d.specs {
		out = append(out, s.Subject)
	}
	sort.Strings(out)
	return out
}

type signIn map[string]bool // owner -> sign-in required

func (s signIn) SignInRequired(_ context.Context, userID, provider string) bool {
	return provider == "github" && s[userID]
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Overlapping ticks produce one run per owner: ticks that start together while the
// first submissions are still in flight, and ticks that arrive while those runs are
// still running, add nothing.
func TestRefresherOverlappingTicksOneRunPerOwner(t *testing.T) {
	owners := []string{"github:1", "github:2", "github:3"}
	d := &gateDispatcher{phase: "Running", hold: make(chan struct{}), arrived: make(chan struct{}, 16)}
	ig := New(d, "", "", nil, "", "")
	r := NewRefresher(seeded(owners...), ig, nil, time.Hour, nil)
	ctx := context.Background()

	const ticks = 5
	var wg sync.WaitGroup
	for range ticks {
		wg.Add(1)
		go func() { defer wg.Done(); r.RefreshOnce(ctx) }()
	}
	// Every owner's first Submit is now blocked; let the other ticks pass the gate.
	for range owners {
		<-d.arrived
	}
	time.Sleep(20 * time.Millisecond)
	close(d.hold)
	wg.Wait()
	if got := d.subjects(); !equal(got, owners) {
		t.Fatalf("concurrent ticks dispatched %v, want one run per owner %v", got, owners)
	}

	// A later tick while the runs are still running adds nothing.
	if n := r.RefreshOnce(ctx); n != 0 || d.count() != len(owners) {
		t.Fatalf("a tick during running refreshes dispatched %d (total %d), want 0 (total %d)", n, d.count(), len(owners))
	}

	// Once they finish, the next tick refreshes every owner again: the schedule does
	// not wait out the run's deadline after a success.
	d.set("Succeeded")
	if n := r.RefreshOnce(ctx); n != len(owners) {
		t.Fatalf("a tick after the runs finished dispatched %d, want %d", n, len(owners))
	}
}

// A tick during a backfill or a Refresh the user clicked shares that run.
func TestRefresherTickDuringOtherIngestRuns(t *testing.T) {
	ctx := context.Background()
	for name, start := range map[string]func(*Ingestor) error{
		"backfill": func(ig *Ingestor) error { return ig.EnsureBackfill(ctx, "github:1") },
		"refresh":  func(ig *Ingestor) error { return ig.Refresh(ctx, "github:1") },
	} {
		t.Run(name, func(t *testing.T) {
			d := &gateDispatcher{phase: "Running"}
			ig := New(d, "", "", nil, "", "")
			if err := start(ig); err != nil {
				t.Fatal(err)
			}
			r := NewRefresher(seeded("github:1"), ig, nil, time.Hour, nil)
			if n := r.RefreshOnce(ctx); n != 0 || d.count() != 1 {
				t.Fatalf("tick dispatched %d (total %d), want 0 (total 1)", n, d.count())
			}
		})
	}
}

// A tick that finds no run in flight dispatches a github-ingest run for the owner,
// shaped like a backfill's.
func TestRefresherDispatchesAnIngestRun(t *testing.T) {
	d := &gateDispatcher{phase: "Running"}
	ig := New(d, "gofer-agent", "http://sink", nil, "", "")
	r := NewRefresher(seeded("github:1"), ig, nil, time.Hour, nil)
	if n := r.RefreshOnce(context.Background()); n != 1 {
		t.Fatalf("dispatched %d, want 1", n)
	}
	spec := d.specs[0]
	if spec.TaskRef != taskIngest || spec.Subject != "github:1" || spec.Parameters["owner"] != "github:1" ||
		spec.Audience != "gofer-agent" || spec.Deadline != ingestDeadline {
		t.Fatalf("unexpected run spec %+v", spec)
	}
}

// Only owners with stored work, who sign in with GitHub and hold a credential gofer
// could vend, are refreshed.
func TestRefresherSkipsOwnersItCannotRefresh(t *testing.T) {
	store := seeded("github:1", "github:2", "google:3")
	store.Seed("github:4") // an owner whose items are all gone
	d := &gateDispatcher{phase: "Running"}
	r := NewRefresher(store, New(d, "", "", nil, "", ""), signIn{"github:2": true}, time.Hour, nil)
	r.RefreshOnce(context.Background())
	if got := d.subjects(); !equal(got, []string{"github:1"}) {
		t.Fatalf("refreshed %v, want only github:1", got)
	}
}

// A refresh that could not be submitted, or whose run failed, is retried on the next
// tick rather than blocking the owner until the run's deadline.
func TestRefresherRetriesAfterAFailure(t *testing.T) {
	ctx := context.Background()
	d := &gateDispatcher{phase: "Running", failNext: true}
	r := NewRefresher(seeded("github:1"), New(d, "", "", nil, "", ""), nil, time.Hour, nil)
	if n := r.RefreshOnce(ctx); n != 0 {
		t.Fatalf("a failed submit counted %d runs", n)
	}
	if n := r.RefreshOnce(ctx); n != 1 {
		t.Fatalf("the tick after a failed submit dispatched %d, want 1", n)
	}
	d.set("Failed")
	if n := r.RefreshOnce(ctx); n != 1 || d.count() != 2 {
		t.Fatalf("the tick after a failed run dispatched %d (total %d), want 1 (total 2)", n, d.count())
	}
}

type failingOwners struct{}

func (failingOwners) Owners(context.Context) ([]string, error) { return nil, errors.New("db down") }

func TestRefresherOwnerListErrorDispatchesNothing(t *testing.T) {
	d := &gateDispatcher{phase: "Running"}
	r := NewRefresher(failingOwners{}, New(d, "", "", nil, "", ""), nil, time.Hour, nil)
	if n := r.RefreshOnce(context.Background()); n != 0 || d.count() != 0 {
		t.Fatalf("dispatched %d runs after a failed owner list", d.count())
	}
}

// Start ticks on the interval; Stop ends the loop and may be called twice.
func TestRefresherStartStop(t *testing.T) {
	d := &gateDispatcher{phase: "Succeeded"}
	r := NewRefresher(seeded("github:1"), New(d, "", "", nil, "", ""), nil, 5*time.Millisecond, nil)
	r.Start()
	deadline := time.Now().Add(5 * time.Second)
	for d.count() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the loop dispatched %d runs in 5s, want at least 2", d.count())
		}
		time.Sleep(time.Millisecond)
	}
	r.Stop()
	n := d.count()
	time.Sleep(30 * time.Millisecond)
	if d.count() != n {
		t.Fatalf("the loop kept dispatching after Stop (%d -> %d)", n, d.count())
	}
	r.Stop()
}

// Active reports an ingest run in flight — submitted, or still running — and not one
// that has ended, gone stale, or that the backend gave no id to probe.
func TestActive(t *testing.T) {
	ctx := context.Background()
	active := func(ig *Ingestor) bool {
		t.Helper()
		a, err := ig.Active(ctx, "github:1")
		if err != nil {
			t.Fatal(err)
		}
		return a
	}

	d := &gateDispatcher{phase: "Running"}
	ig := New(d, "", "", nil, "", "")
	if active(ig) {
		t.Fatal("active before any run")
	}
	_ = ig.EnsureBackfill(ctx, "github:1")
	if !active(ig) {
		t.Fatal("not active while the run is running")
	}
	d.set("Succeeded")
	if active(ig) {
		t.Fatal("active after the run succeeded")
	}
	_, _ = ig.ScheduledRefresh(ctx, "github:1")
	d.set("Failed")
	if active(ig) {
		t.Fatal("active after the run failed")
	}

	// Being submitted counts.
	held := &gateDispatcher{phase: "Running", hold: make(chan struct{}), arrived: make(chan struct{}, 1)}
	ig = New(held, "", "", nil, "", "")
	done := make(chan error)
	go func() { done <- ig.Refresh(ctx, "github:1") }()
	<-held.arrived
	if !active(ig) {
		t.Fatal("not active while the run is being submitted")
	}
	close(held.hold)
	<-done

	// A run past its deadline no longer counts.
	ig.mu.Lock()
	ig.runs[ingestKey("github:1")].at = time.Now().Add(-retryAfter)
	ig.mu.Unlock()
	if active(ig) {
		t.Fatal("active past the run's deadline")
	}

	// NoopDispatcher gives no run id: nothing to wait for.
	ig = New(NoopDispatcher{}, "", "", nil, "", "")
	_ = ig.Refresh(ctx, "github:1")
	if active(ig) {
		t.Fatal("active with a backend that runs nothing")
	}
}

// The empty-worklist poll still waits out a successful ingest run (a schedule's
// probing gate must not loosen it), while a scheduled refresh may follow a
// successful backfill.
func TestScheduledRefreshFollowsASuccessfulBackfill(t *testing.T) {
	ctx := context.Background()
	d := &gateDispatcher{phase: "Running"}
	ig := New(d, "", "", nil, "", "")
	_ = ig.EnsureBackfill(ctx, "github:1")
	d.set("Succeeded")
	if ok, err := ig.ScheduledRefresh(ctx, "github:1"); err != nil || !ok {
		t.Fatalf("ScheduledRefresh after a successful backfill = %v, %v; want a run", ok, err)
	}
	// The refresh's run is seen to succeed (the page probes it), and every item was
	// retired, so the worklist is empty again: the poll still must not dispatch.
	if a, err := ig.Active(ctx, "github:1"); err != nil || a {
		t.Fatalf("Active = %v, %v after the refresh succeeded", a, err)
	}
	if err := ig.EnsureBackfill(ctx, "github:1"); err != nil {
		t.Fatal(err)
	}
	if d.count() != 2 {
		t.Fatalf("EnsureBackfill dispatched after a successful refresh: %d runs, want 2", d.count())
	}
}
