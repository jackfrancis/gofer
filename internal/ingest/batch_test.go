package ingest

import (
	"context"
	"testing"
	"time"
)

type batchObs struct {
	d         time.Duration
	size      int
	completed bool
}

type captureBatch struct{ done chan batchObs }

func (c *captureBatch) ObserveReviewAll(d time.Duration, size int, completed bool) {
	c.done <- batchObs{d, size, completed}
}

// ReviewAll times the batch off the runtimes' write-backs: it finalizes exactly when
// the last dispatched run's review lands (NoteWriteback), is idempotent per run, and
// does not finalize early.
func TestReviewAllTimesToLastWriteback(t *testing.T) {
	d := &recordDispatcher{} // Submit returns "run-"+item
	obs := &captureBatch{done: make(chan batchObs, 1)}
	ing := New(d, "gofer-agent", "", nil, "", "")
	ing.SetBatchObserver(obs)

	items := []string{"o/r#1", "o/r#2", "o/r#3"}
	if err := ing.ReviewAll(context.Background(), "u1", items, "", ""); err != nil {
		t.Fatal(err)
	}

	// Two of three landed (one twice) -> not finalized.
	ing.NoteWriteback("run-o/r#1")
	ing.NoteWriteback("run-o/r#1") // duplicate (e.g. a chained research write) -> ignored
	ing.NoteWriteback("run-o/r#2")
	select {
	case got := <-obs.done:
		t.Fatalf("batch finalized before the last review landed: %+v", got)
	case <-time.After(20 * time.Millisecond):
	}

	ing.NoteWriteback("run-o/r#3") // the last review -> finalize
	select {
	case got := <-obs.done:
		if !got.completed || got.size != 3 {
			t.Fatalf("observed %+v, want completed size 3", got)
		}
	case <-time.After(time.Second):
		t.Fatal("batch did not finalize when the last review landed")
	}

	// A write-back after finalization is a harmless no-op.
	ing.NoteWriteback("run-o/r#3")
}

// A run that never writes back is bounded by batchTimeout: recorded as not completed
// but still carrying the batch size.
func TestReviewAllTimesOutWhenARunNeverLands(t *testing.T) {
	d := &recordDispatcher{}
	obs := &captureBatch{done: make(chan batchObs, 1)}
	ing := New(d, "gofer-agent", "", nil, "", "")
	ing.SetBatchObserver(obs)
	ing.batchTimeout = 20 * time.Millisecond

	if err := ing.ReviewAll(context.Background(), "u1", []string{"o/r#1", "o/r#2"}, "", ""); err != nil {
		t.Fatal(err)
	}
	ing.NoteWriteback("run-o/r#1") // only one of two lands

	select {
	case got := <-obs.done:
		if got.completed || got.size != 2 {
			t.Fatalf("observed %+v, want timeout size 2", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("batch did not time out")
	}
}

// With no observer, ReviewAll registers no batch and NoteWriteback is a no-op.
func TestReviewAllNoObserverNoTracker(t *testing.T) {
	d := &recordDispatcher{}
	ing := New(d, "gofer-agent", "", nil, "", "")
	if err := ing.ReviewAll(context.Background(), "u1", []string{"o/r#1"}, "", ""); err != nil {
		t.Fatal(err)
	}
	ing.NoteWriteback("run-o/r#1") // must not panic or block
}
