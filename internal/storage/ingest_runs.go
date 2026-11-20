package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/jackfrancis/gofer/internal/ingest"
)

var _ ingest.RunLedger = (*WorklistStore)(nil)

// IngestRuns returns every owner's ingest record (ingest.RunLedger).
func (s *WorklistStore) IngestRuns(ctx context.Context) (map[string]ingest.IngestRecord, error) {
	rows, err := s.db.sql.QueryContext(ctx,
		`SELECT owner_id, gen, done, run_gen, run_id, run_at, pending FROM ingest_runs`)
	if err != nil {
		return nil, fmt.Errorf("storage: list ingest runs: %w", err)
	}
	defer rows.Close()
	out := map[string]ingest.IngestRecord{}
	for rows.Next() {
		var (
			owner, runID                   string
			gen, done, runGen, runAt, pend int64
		)
		if err := rows.Scan(&owner, &gen, &done, &runGen, &runID, &runAt, &pend); err != nil {
			return nil, fmt.Errorf("storage: list ingest runs: %w", err)
		}
		rec := ingest.IngestRecord{Gen: uint64(gen), Done: uint64(done), RunGen: uint64(runGen), RunID: runID, Pending: pend != 0}
		if runAt != 0 {
			rec.RunAt = time.Unix(0, runAt)
		}
		out[owner] = rec
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list ingest runs: %w", err)
	}
	return out, nil
}

// PutIngestRun replaces ownerID's ingest record (ingest.RunLedger).
func (s *WorklistStore) PutIngestRun(ctx context.Context, ownerID string, rec ingest.IngestRecord) error {
	if ownerID == "" {
		return fmt.Errorf("storage: ingest run: owner is required")
	}
	var runAt, pend int64
	if !rec.RunAt.IsZero() {
		runAt = rec.RunAt.UnixNano()
	}
	if rec.Pending {
		pend = 1
	}
	_, err := s.db.sql.ExecContext(ctx, `
		INSERT INTO ingest_runs (owner_id, gen, done, run_gen, run_id, run_at, pending) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (owner_id) DO UPDATE SET
			gen = excluded.gen, done = excluded.done, run_gen = excluded.run_gen,
			run_id = excluded.run_id, run_at = excluded.run_at, pending = excluded.pending`,
		ownerID, int64(rec.Gen), int64(rec.Done), int64(rec.RunGen), rec.RunID, runAt, pend)
	if err != nil {
		return fmt.Errorf("storage: record ingest run: %w", err)
	}
	return nil
}
