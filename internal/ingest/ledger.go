package ingest

import (
	"context"
	"time"
)

// IngestRecord is what a RunLedger keeps for one owner: the generation counter, the
// refresh version, and the owner's current ingest run as the gate holds it.
type IngestRecord struct {
	Gen  uint64 // the last generation handed to a reservation (never reused)
	Done uint64 // the highest generation known to have ended
	// The current run; RunGen is 0 when there is none.
	RunGen  uint64    // its generation
	RunID   string    // the backend's id for it; empty while acceptance is unknown
	RunAt   time.Time // when it was reserved, or accepted once it was
	Pending bool      // reserved, and whether the backend accepted it is not known
	// RunUntil is when the run stops counting as in flight however it went: for one
	// of unknown acceptance, the latest moment the backend could have accepted it
	// plus the run's deadline (holdPending, holdUncertain); for an accepted run,
	// RunAt plus the deadline.
	RunUntil time.Time
}

// RunLedger persists each owner's IngestRecord. The in-flight gate and the refresh
// version are otherwise in memory, and runs outlive a web-tier restart: without it a
// restarted web tier could start a second ingest while the first is still writing
// (two runs landing out of order), and a page open across the restart would compare
// refresh versions from two processes. storage.WorklistStore implements it; without
// one (no DATA_DIR) a restart forgets both, as before.
type RunLedger interface {
	IngestRuns(ctx context.Context) (map[string]IngestRecord, error)
	PutIngestRun(ctx context.Context, ownerID string, rec IngestRecord) error
}

// ledgerTimeout bounds one ledger write.
const ledgerTimeout = 5 * time.Second

// UseLedger loads every owner's record from l and keeps l up to date from now on; it
// fails if the records cannot be read, and then attaches nothing (the caller should
// not serve, since the gate would be empty). A current run that has not been seen
// to end and is within its deadline is restored into the gate, so neither the
// schedule, a Refresh, nor the empty-worklist poll starts another until it ends: an
// accepted run until the backend reports it ended (an id the backend no longer knows
// reads as a failed status read, which blocks only until the deadline), one of
// unknown acceptance until the latest moment it could still be running (RunUntil). A
// run past that counts as ended.
// Call it before serving.
func (i *Ingestor) UseLedger(ctx context.Context, l RunLedger) error {
	recs, err := l.IngestRuns(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	i.mu.Lock()
	defer i.mu.Unlock()
	i.ledger = l
	for owner, rec := range recs {
		i.gens[owner], i.dones[owner] = max(rec.Gen, rec.RunGen, rec.Done), rec.Done
		if rec.RunGen <= rec.Done {
			continue
		}
		r := &run{id: rec.RunID, at: rec.RunAt, g: refreshGate, gen: rec.RunGen,
			uncertain: rec.Pending || rec.RunID == "", until: rec.RunUntil}
		if r.uncertain && r.until.IsZero() {
			r.until = rec.RunAt.Add(holdPending) // a record without its expiry: assume the longest
		}
		if r.expired(now) {
			i.dones[owner] = rec.RunGen
			continue
		}
		i.runs[ingestKey(owner)] = r
	}
	return nil
}

// persist writes ownerID's record, as the gate now holds it, to the ledger, if there
// is one, and returns the write's error (also logged). Writes for one Ingestor are
// serialized and each takes the state as it is when its turn comes, so the last write
// is always the newest state.
func (i *Ingestor) persist(ownerID string) error {
	i.persistMu.Lock()
	defer i.persistMu.Unlock()
	i.mu.Lock()
	l := i.ledger
	rec := IngestRecord{Gen: i.gens[ownerID], Done: i.dones[ownerID]}
	if cur := i.runs[ingestKey(ownerID)]; cur != nil && cur.gen > 0 {
		rec.RunGen, rec.RunID, rec.RunAt = cur.gen, cur.id, cur.at
		rec.Pending = cur.pending || cur.uncertain
		rec.RunUntil = cur.deadline()
		if cur.pending {
			// Submit has not returned (and starts only after this write): if gofer
			// stops now, the backend may still accept the run up to the end of the
			// dispatch window, counted from this write, not from the reservation,
			// since the wait for earlier writes is not bounded by ledgerTimeout.
			rec.RunUntil = time.Now().Add(holdPending)
		}
	}
	i.mu.Unlock()
	if l == nil || rec.Gen == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), ledgerTimeout)
	defer cancel()
	if err := l.PutIngestRun(ctx, ownerID, rec); err != nil {
		i.log.Warn("recording the ingest run failed", "owner", ownerID, "run_id", rec.RunID, "err", err)
		return err
	}
	return nil
}
