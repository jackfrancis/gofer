package ingest

import (
	"context"
	"testing"
	"time"
)

// fastFollowUps builds an Ingestor over d whose follow-up worker retries every few
// milliseconds and gives up after giveUp.
func fastFollowUps(d Dispatcher, giveUp time.Duration) *Ingestor {
	ig := New(d, "", "", nil, "", "")
	ig.followUpPoll, ig.followUpMaxWait, ig.followUpGiveUp = 5*time.Millisecond, 20*time.Millisecond, giveUp
	return ig
}

// waitCount waits for d to have recorded want submissions.
func waitCount(t *testing.T, d *gateDispatcher, want int) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); d.count() < want; {
		if time.Now().After(deadline) {
			t.Fatalf("submitted %d runs, want %d", d.count(), want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// owed reports whether ig still owes ownerID a follow-up run.
func owed(ig *Ingestor, ownerID string) bool {
	ig.mu.Lock()
	defer ig.mu.Unlock()
	return ig.followUps[ownerID] != nil
}

// stays asserts d's submissions stay at want for a while (many worker polls).
func stays(t *testing.T, d *gateDispatcher, want int, why string) {
	t.Helper()
	time.Sleep(60 * time.Millisecond)
	if got := d.count(); got != want {
		t.Fatalf("%s: submitted %d runs, want %d", why, got, want)
	}
}

// With no ingest run in flight, RefreshFollowUp dispatches at once, like Refresh, and
// the accepted run pays the debt: nothing more follows.
func TestRefreshFollowUpDispatchesWhenIdle(t *testing.T) {
	d := &gateDispatcher{phase: "Running"}
	ig := fastFollowUps(d, time.Minute)
	if err := ig.RefreshFollowUp(context.Background(), "github:1"); err != nil {
		t.Fatal(err)
	}
	if d.count() != 1 || owed(ig, "github:1") {
		t.Fatalf("want one run and no debt, got %d runs, owed %v", d.count(), owed(ig, "github:1"))
	}
	d.set("Succeeded")
	stays(t, d, 1, "nothing was owed")
}

// A follow-up requested while an ingest run is in flight starts once that run ends,
// exactly once, with nobody polling: the worker asks the backend itself (an unhide
// from the thread page, with the scheduled refresh off). It never overlaps the run.
func TestRefreshFollowUpRunsAfterTheRunInFlightUnwatched(t *testing.T) {
	ctx := context.Background()
	d := &gateDispatcher{phase: "Running"}
	ig := fastFollowUps(d, time.Minute)
	if err := ig.Refresh(ctx, "github:1"); err != nil {
		t.Fatal(err)
	}
	if err := ig.RefreshFollowUp(ctx, "github:1"); err != nil {
		t.Fatal(err)
	}
	if err := ig.RefreshFollowUp(ctx, "github:1"); err != nil { // a second unhide owes no second run
		t.Fatal(err)
	}
	stays(t, d, 1, "a follow-up must not overlap the run in flight")
	if !owed(ig, "github:1") {
		t.Fatal("the run in flight may have read the store first: a run is owed")
	}
	d.set("Succeeded") // no Active, no page: only the worker can notice
	waitCount(t, d, 2)
	stays(t, d, 2, "the follow-up runs once")
	if owed(ig, "github:1") {
		t.Fatal("the accepted follow-up paid the debt")
	}
}

// A late answer about an older run, landing after a newer run started and a
// follow-up was queued behind it, neither pays the debt nor starts anything while
// the newer run is in flight; the follow-up starts once that run ends.
func TestRefreshFollowUpIgnoresAnOlderRunsEnd(t *testing.T) {
	ctx := context.Background()
	d := &gateDispatcher{phase: "Running"}
	ig := fastFollowUps(d, time.Minute)
	if err := ig.Refresh(ctx, "github:1"); err != nil { // run A
		t.Fatal(err)
	}
	ig.mu.Lock()
	a := ig.runs[ingestKey("github:1")]
	a.succeeded = true // A ended
	ig.mu.Unlock()
	if err := ig.Refresh(ctx, "github:1"); err != nil { // run B
		t.Fatal(err)
	}
	if err := ig.RefreshFollowUp(ctx, "github:1"); err != nil { // queued behind B
		t.Fatal(err)
	}
	ig.mu.Lock()
	ig.markDoneLocked(ingestKey("github:1"), a) // A's delayed status lands
	ig.mu.Unlock()
	stays(t, d, 2, "B is still running")
	if !owed(ig, "github:1") {
		t.Fatal("an older run's end must not consume the follow-up")
	}
	d.set("Succeeded")
	waitCount(t, d, 3)
}

// A definite refusal (ErrNotAccepted) keeps the debt: the worker retries, backing
// off, until the backend accepts the run.
func TestRefreshFollowUpRetriesARefusal(t *testing.T) {
	ctx := context.Background()
	d := &gateDispatcher{phase: "Running"}
	ig := fastFollowUps(d, time.Minute)
	if err := ig.Refresh(ctx, "github:1"); err != nil {
		t.Fatal(err)
	}
	if err := ig.RefreshFollowUp(ctx, "github:1"); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.failNext = true // the backend refuses the follow-up's first submission
	d.phase = "Succeeded"
	d.mu.Unlock()
	waitCount(t, d, 2)
	if owed(ig, "github:1") {
		t.Fatal("the accepted retry paid the debt")
	}
}

// A follow-up queued while the original submission is still pending survives that
// submission being refused: no run exists to end, and the worker dispatches.
func TestRefreshFollowUpSurvivesARefusedPendingRun(t *testing.T) {
	ctx := context.Background()
	d := &gateDispatcher{phase: "Running", hold: make(chan struct{}), arrived: make(chan struct{}, 4), failNext: true}
	ig := fastFollowUps(d, time.Minute)
	first := make(chan error, 1)
	go func() { first <- ig.Refresh(ctx, "github:1") }()
	<-d.arrived // the original Submit is outstanding
	if err := ig.RefreshFollowUp(ctx, "github:1"); err != nil {
		t.Fatal(err)
	}
	close(d.hold) // it returns refused
	if err := <-first; err == nil {
		t.Fatal("test premise: the original submission is refused")
	}
	waitCount(t, d, 1) // the follow-up's own run
	if owed(ig, "github:1") {
		t.Fatal("the accepted follow-up paid the debt")
	}
}

// An ingest run reserved after the follow-up was owed and accepted (a tick, a
// click) reads the store after the change, so it pays the debt.
func TestRefreshFollowUpPaidByALaterRun(t *testing.T) {
	ctx := context.Background()
	d := &gateDispatcher{phase: "Running"}
	ig := fastFollowUps(d, time.Minute)
	ig.followUpPoll = time.Hour // keep the worker out of it
	if err := ig.Refresh(ctx, "github:1"); err != nil {
		t.Fatal(err)
	}
	if err := ig.RefreshFollowUp(ctx, "github:1"); err != nil {
		t.Fatal(err)
	}
	d.set("Succeeded")
	if ok, err := ig.ScheduledRefresh(ctx, "github:1"); err != nil || !ok {
		t.Fatalf("ScheduledRefresh = %v, %v; want a run", ok, err)
	}
	if owed(ig, "github:1") || d.count() != 2 {
		t.Fatalf("the tick's run paid the follow-up; owed %v, %d runs", owed(ig, "github:1"), d.count())
	}
}

// A worker whose run never becomes possible gives up after followUpGiveUp and drops
// the debt, rather than polling forever.
func TestRefreshFollowUpGivesUp(t *testing.T) {
	ctx := context.Background()
	d := &gateDispatcher{phase: "Running"}
	ig := fastFollowUps(d, 30*time.Millisecond)
	if err := ig.Refresh(ctx, "github:1"); err != nil {
		t.Fatal(err)
	}
	if err := ig.RefreshFollowUp(ctx, "github:1"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); owed(ig, "github:1"); {
		if time.Now().After(deadline) {
			t.Fatal("the worker never gave up")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if d.count() != 1 {
		t.Fatalf("nothing could run; got %d runs", d.count())
	}
}
