package ingest

import (
	"context"
	"time"
)

// Task refs gofer dispatches.
const (
	taskIngest   = "github-ingest"
	taskConverse = "github-converse"
)

// runKey names what a run does: the owner, the task, and for a converse run the item
// and the turn it answers. The in-flight gate holds one run per key, so two identical
// requests share a run while requests for different items or turns run concurrently.
// An ingest run is keyed by owner alone (item and turn empty).
//
// The gate only saves the cost of a duplicate run. What guarantees a turn one answer
// is the store (worklist.AppendReply): the gate is in memory, so a restart, or a run
// that outlives its deadline, can still start a second run for a turn.
type runKey struct {
	owner, task, item, turn string
}

func ingestKey(ownerID string) runKey { return runKey{owner: ownerID, task: taskIngest} }

// run is the most recent run tracked for one key. For ingest it lets a run that fails
// be surfaced (BackfillFailure) instead of leaving the worklist spinning, a run that
// succeeds without producing work be reported as genuinely empty (BackfillSucceeded),
// and a healthy run not be re-dispatched by every poll of an empty worklist. A terminal
// outcome is cached here, so the backend is probed once per run rather than once per
// page render. Every field is read and written under Ingestor.mu.
type run struct {
	id        string
	at        time.Time // when it was reserved, then when Submit succeeded
	g         gate      // the policy the run was reserved under
	pending   bool      // reserved; Submit has not returned yet
	noRun     bool      // Submit failed: there is no run behind this reservation
	failed    bool
	succeeded bool
	message   string
	logged    bool
	// prev is the run this reservation replaced, restored if Submit fails. It is kept
	// only while it may still be needed: until this reservation's Submit succeeds, or
	// until it is rolled back.
	prev *run
}

// live reports whether r still counts as in flight at now, as far as gofer knows
// without asking the backend. A failure always frees the key; a success frees it only
// under a probing gate.
func (r *run) live(now time.Time) bool {
	if r.failed || (r.succeeded && r.g.probe) {
		return false
	}
	return now.Sub(r.at) < r.g.ttl
}

// gate is the in-flight policy for one kind of run.
type gate struct {
	// ttl is how long a run counts as in flight: its deadline, after which a lost or
	// stuck run no longer blocks a new one.
	ttl time.Duration
	// probe asks the backend (Dispatcher.Status) about a run that still looks live,
	// and lets a new run start once it is terminal, whether it failed or succeeded:
	// a converse turn whose run ended with only a notice can be retried at once.
	// Without it only a failure gofer has observed, or the ttl, frees the key: the
	// empty-worklist poll must not start a new ingest run after one that succeeded
	// and found nothing.
	probe bool
}

var (
	ingestGate   = gate{ttl: retryAfter}
	converseGate = gate{ttl: converseDeadline, probe: true}
)

// reserve claims key for a new run under g and reports whether the caller should
// submit it. It refuses while a run for key is in flight. A reservation is settled
// with settle once Submit returns. With force set, a live run does not block (an
// explicit Refresh); the reservation still replaces it, so a concurrent poll waits on
// the forced run.
func (i *Ingestor) reserve(ctx context.Context, key runKey, g gate, force bool) (mine *run, ok bool) {
	for probed := false; ; probed = true {
		now := time.Now()
		i.mu.Lock()
		cur := i.runs[key]
		if force || cur == nil || !cur.live(now) {
			mine = &run{at: now, g: g, pending: true, prev: cur}
			i.runs[key] = mine
			i.pruneLocked(now)
			i.mu.Unlock()
			return mine, true
		}
		canProbe := g.probe && !probed && !cur.pending && cur.id != ""
		i.mu.Unlock()
		if !canProbe {
			return nil, false // in flight, and nothing more to ask
		}
		// Ask the backend once, without the lock; probeRun caches a terminal outcome
		// on cur, and the next pass reserves only if no one has replaced cur meanwhile.
		if failed, succeeded, _, err := i.probeRun(ctx, key); err != nil || (!failed && !succeeded) {
			return nil, false
		}
	}
}

// settle records the outcome of Submit on the reservation mine, whether or not it is
// still the key's current run, so a run restored by a later rollback is accurate. On
// error, if mine is current, the key goes back to the run mine replaced, skipping any
// reservation whose own Submit failed.
func (i *Ingestor) settle(key runKey, mine *run, id string, err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	mine.pending = false
	if err == nil {
		mine.id = id
		// The run's deadline runs from when the backend accepted it, so the retry
		// window does too; until then it ran from the reservation.
		mine.at = time.Now()
		mine.prev = nil // never rolled back now
		return
	}
	mine.noRun = true
	if i.runs[key] != mine {
		// Superseded: keep prev, so a rollback of the reservation that replaced mine
		// can reach past it.
		return
	}
	p := mine.prev
	for p != nil && p.noRun {
		p = p.prev
	}
	mine.prev = nil
	if p == nil {
		delete(i.runs, key)
	} else {
		i.runs[key] = p
	}
}

// pruneLocked drops converse runs that no longer gate anything, so the tracker does
// not grow with every turn ever answered. Ingest runs are kept: there is one per
// owner, and the read model asks about the latest one. The caller holds i.mu.
func (i *Ingestor) pruneLocked(now time.Time) {
	for k, r := range i.runs {
		if k.task != taskIngest && !r.pending && !r.live(now) {
			delete(i.runs, k)
		}
	}
}
