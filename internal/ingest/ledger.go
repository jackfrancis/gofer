package ingest

import (
	"context"
	"time"
)

// IngestRecord is an owner's latest accepted ingest run, as a RunLedger keeps it.
type IngestRecord struct {
	RunID      string
	Gen        uint64    // the run's generation: how many ingest runs the backend has accepted for the owner
	Done       uint64    // the highest generation known to have ended
	AcceptedAt time.Time // when the backend accepted the run
}

// RunLedger persists each owner's latest accepted ingest run. The in-flight gate and
// the refresh version are otherwise in memory, and runs outlive a web-tier restart:
// without it a restarted web tier could start a second ingest while the first is
// still writing (two runs landing out of order), and a page open across the restart
// would compare refresh versions from two processes. storage.WorklistStore
// implements it; without one (no DATA_DIR) a restart forgets both, as before.
type RunLedger interface {
	IngestRuns(ctx context.Context) (map[string]IngestRecord, error)
	PutIngestRun(ctx context.Context, ownerID string, rec IngestRecord) error
}

// ledgerTimeout bounds one ledger write. A write is best effort: a failure is
// logged, and only a restart before the next successful write forgets the run.
const ledgerTimeout = 5 * time.Second

// UseLedger loads every owner's last accepted ingest run from l and keeps l up to
// date from now on. A run that has not been seen to end and is within its deadline
// is restored into the gate, so neither the schedule, a Refresh, nor the
// empty-worklist poll starts another until the backend reports it ended (an id the
// backend no longer knows reads as a failed status read, which blocks only until the
// deadline). One past its deadline counts as ended. Call it before serving.
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
		i.gens[owner], i.dones[owner] = rec.Gen, rec.Done
		i.accepted[owner] = acceptedRun{id: rec.RunID, at: rec.AcceptedAt}
		if rec.Done >= rec.Gen || rec.RunID == "" {
			continue
		}
		if now.Sub(rec.AcceptedAt) >= refreshGate.ttl {
			i.dones[owner] = rec.Gen
			continue
		}
		i.runs[ingestKey(owner)] = &run{id: rec.RunID, at: rec.AcceptedAt, g: refreshGate, gen: rec.Gen}
	}
	return nil
}

// acceptedRun is the id and acceptance time of an owner's latest accepted ingest run.
type acceptedRun struct {
	id string
	at time.Time
}

// persist writes ownerID's current ingest record to the ledger, if there is one.
// Writes for one Ingestor are serialized and each takes the state as it is when its
// turn comes, so the last write is always the newest state.
func (i *Ingestor) persist(ownerID string) {
	i.persistMu.Lock()
	defer i.persistMu.Unlock()
	i.mu.Lock()
	l := i.ledger
	a := i.accepted[ownerID]
	rec := IngestRecord{RunID: a.id, Gen: i.gens[ownerID], Done: i.dones[ownerID], AcceptedAt: a.at}
	i.mu.Unlock()
	if l == nil || rec.Gen == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), ledgerTimeout)
	defer cancel()
	if err := l.PutIngestRun(ctx, ownerID, rec); err != nil {
		i.log.Warn("recording the ingest run failed", "owner", ownerID, "run_id", rec.RunID, "err", err)
	}
}
