package ingest

import (
	"context"
	"testing"
	"time"
)

// waitCount waits for d to have recorded want submissions.
func waitCount(t *testing.T, d *gateDispatcher, want int) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); d.count() < want; {
		if time.Now().After(deadline) {
			t.Fatalf("submitted %d runs, want %d", d.count(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// With no ingest run in flight, RefreshFollowUp dispatches at once, like Refresh, and
// owes nothing afterwards.
func TestRefreshFollowUpDispatchesWhenIdle(t *testing.T) {
	ctx := context.Background()
	d := &gateDispatcher{phase: "Running"}
	ig := New(d, "", "", nil, "", "")
	if err := ig.RefreshFollowUp(ctx, "github:1"); err != nil {
		t.Fatal(err)
	}
	if d.count() != 1 {
		t.Fatalf("submitted %d runs, want 1", d.count())
	}
	d.set("Succeeded")
	if _, err := ig.Active(ctx, "github:1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if d.count() != 1 {
		t.Fatalf("nothing was owed, yet %d runs were submitted", d.count())
	}
}

// A follow-up requested while an ingest run is in flight (which may have read the
// store before the change) is dispatched once that run is seen to end, exactly once.
func TestRefreshFollowUpRunsAfterTheRunInFlight(t *testing.T) {
	ctx := context.Background()
	d := &gateDispatcher{phase: "Running"}
	ig := New(d, "", "", nil, "", "")
	if err := ig.Refresh(ctx, "github:1"); err != nil {
		t.Fatal(err)
	}
	if err := ig.RefreshFollowUp(ctx, "github:1"); err != nil {
		t.Fatal(err)
	}
	if err := ig.RefreshFollowUp(ctx, "github:1"); err != nil { // a second unhide owes no second run
		t.Fatal(err)
	}
	if d.count() != 1 {
		t.Fatalf("a follow-up must not overlap the run in flight: %d runs submitted", d.count())
	}
	if active, err := ig.Active(ctx, "github:1"); err != nil || !active {
		t.Fatalf("Active = %v, %v; want the first run still in flight", active, err)
	}
	time.Sleep(50 * time.Millisecond)
	if d.count() != 1 {
		t.Fatalf("the follow-up started before the run ended: %d runs", d.count())
	}

	d.set("Succeeded")
	if _, err := ig.Active(ctx, "github:1"); err != nil { // the radar's status poll sees it end
		t.Fatal(err)
	}
	waitCount(t, d, 2)
	if _, err := ig.Active(ctx, "github:1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if d.count() != 2 {
		t.Fatalf("the follow-up must run once, got %d runs", d.count())
	}
}

// A run reserved after the follow-up was owed reads the store after the change, so
// it pays the debt: no extra run follows it.
func TestRefreshFollowUpPaidByALaterRun(t *testing.T) {
	ctx := context.Background()
	d := &gateDispatcher{phase: "Running"}
	ig := New(d, "", "", nil, "", "")
	if err := ig.Refresh(ctx, "github:1"); err != nil {
		t.Fatal(err)
	}
	if err := ig.RefreshFollowUp(ctx, "github:1"); err != nil {
		t.Fatal(err)
	}
	// The run ends past its deadline, unobserved, and a tick reserves the next one.
	ig.mu.Lock()
	ig.runs[ingestKey("github:1")].at = time.Now().Add(-2 * retryAfter)
	ig.mu.Unlock()
	if ok, err := ig.ScheduledRefresh(ctx, "github:1"); err != nil || !ok {
		t.Fatalf("ScheduledRefresh = %v, %v; want a run", ok, err)
	}
	d.set("Succeeded")
	if _, err := ig.Active(ctx, "github:1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if d.count() != 2 {
		t.Fatalf("the tick's run paid the follow-up; want 2 runs, got %d", d.count())
	}
}
