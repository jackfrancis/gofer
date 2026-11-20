package ingest

import (
	"context"
	"errors"
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
	id      string
	at      time.Time // when it was reserved, then when Submit succeeded
	g       gate      // the policy the run was reserved under
	pending bool      // reserved; Submit has not returned yet
	gen     uint64    // ingest only: the owner's refresh generation, taken when reserved
	// uncertain (ingest only): Submit failed without saying the backend refused
	// (ErrNotAccepted), so a run may exist that gofer cannot probe. It holds the key,
	// and counts as in flight, until its deadline.
	uncertain bool
	// until, when set, is the run's deadline instead of at + the gate's ttl: for a
	// run whose acceptance is unknown, the latest moment it could still be running
	// (see holdUncertain, holdPending).
	until     time.Time
	failed    bool
	succeeded bool
	message   string
	logged    bool
	// prev is the run this reservation replaced, restored if Submit fails. It is kept
	// only until this reservation's Submit returns.
	prev *run
}

// live reports whether r still counts as in flight at now for a caller reserving
// under a gate with the given probe setting, as far as gofer knows without asking the
// backend. A failure always frees the key; a success frees it only for a probing
// caller. So the empty-worklist poll (ingestGate) still waits out a successful ingest
// run, whoever started it, while a scheduled refresh (refreshGate) may follow one as
// soon as it has ended. The ttl is the run's own.
func (r *run) live(now time.Time, probe bool) bool {
	if r.failed || (r.succeeded && probe) {
		return false
	}
	return !r.expired(now)
}

// expired reports whether r is past its deadline: until when set, else at + ttl.
func (r *run) expired(now time.Time) bool {
	return !now.Before(r.deadline())
}

// deadline is when r stops counting as in flight however it went.
func (r *run) deadline() time.Time {
	if !r.until.IsZero() {
		return r.until
	}
	return r.at.Add(r.g.ttl)
}

// The backend starts a run's deadline when it accepts the run, which can be later
// than gofer reserved it. An accepted run's deadline here runs from when Submit
// returned, after any acceptance, so it already covers that. A run whose acceptance
// is unknown must be held until the latest moment the backend could have accepted
// it, plus the run's deadline:
const (
	// submitTimeout bounds one ingest Submit (dispatchIngest), so a dispatch window
	// is bounded however the caller's request goes.
	submitTimeout = 30 * time.Second
	// acceptGrace covers a backend still finishing a dispatch request after gofer
	// stopped waiting for its answer.
	acceptGrace = time.Minute
	// holdUncertain: a reservation whose Submit returned without saying whether
	// the backend accepted it is held this long from when Submit returned.
	holdUncertain = acceptGrace + ingestDeadline
	// holdPending: a reservation whose Submit had not returned when gofer last
	// recorded it (a restart mid-dispatch) is held this long from the reservation:
	// recording it, then the whole Submit, then holdUncertain.
	holdPending = ledgerTimeout + submitTimeout + holdUncertain
)

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
	// refreshGate is the policy of a Refresh click and a scheduled refresh tick for
	// the owner's ingest key: it starts nothing while any ingest run for the owner (a
	// backfill, a Refresh, an earlier tick) is still running, so two ingest runs never
	// write one owner's worklist at once, and it asks the backend rather than waiting
	// out the ttl, so a refresh after a finished run dispatches the next one.
	refreshGate = gate{ttl: retryAfter, probe: true}
)

// reserve claims key for a new run under g and reports whether the caller should
// submit it. It refuses while a run for key is in flight, which includes a
// reservation whose Submit has not returned, so a reservation is never replaced
// before it is settled. A reservation is settled with settle once Submit returns.
func (i *Ingestor) reserve(ctx context.Context, key runKey, g gate) (mine *run, ok bool) {
	for probed := false; ; probed = true {
		now := time.Now()
		i.mu.Lock()
		cur := i.runs[key]
		if cur == nil || !cur.live(now, g.probe) {
			if cur != nil {
				i.markDoneLocked(key, cur) // failed, succeeded, or past its deadline
			}
			mine = &run{at: now, g: g, pending: true, prev: cur}
			if key.task == taskIngest {
				i.gens[key.owner]++
				mine.gen = i.gens[key.owner]
			}
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

// settle records the outcome of Submit on the reservation mine, which is still the
// key's current run (see reserve). When the backend did not accept the run
// (ErrNotAccepted) the key goes back to the run mine replaced, so the read model
// still knows how the previous run ended. Any other error leaves acceptance unknown:
// for ingest the reservation stays, uncertain, until its deadline, because a second
// ingest started now could land out of order with one the backend did accept. A
// converse turn is rolled back either way; the store keeps one answer per turn.
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
	if key.task == taskIngest && !errors.Is(err, ErrNotAccepted) {
		mine.uncertain = true
		mine.until = time.Now().Add(holdUncertain)
		mine.prev = nil
		return
	}
	p := mine.prev
	mine.prev = nil
	if i.runs[key] != mine {
		return // not reachable while reserve never replaces a pending reservation
	}
	if p == nil {
		delete(i.runs, key)
	} else {
		i.runs[key] = p
	}
}

// markDoneLocked records that r, an ingest run for key's owner, has ended (failed,
// succeeded, or outlived its deadline), raising the owner's refresh version
// (RefreshState). Reservations the backend never accepted carry no generation. The
// caller holds i.mu.
func (i *Ingestor) markDoneLocked(key runKey, r *run) (changed bool) {
	if key.task == taskIngest && r.gen > i.dones[key.owner] {
		i.dones[key.owner] = r.gen
		return true
	}
	return false
}

// pruneLocked drops converse runs that no longer gate anything, so the tracker does
// not grow with every turn ever answered. Ingest runs are kept: there is one per
// owner, and the read model asks about the latest one. The caller holds i.mu.
func (i *Ingestor) pruneLocked(now time.Time) {
	for k, r := range i.runs {
		if k.task != taskIngest && !r.pending && !r.live(now, r.g.probe) {
			delete(i.runs, k)
		}
	}
}
