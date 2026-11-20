package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/jackfrancis/gofer/internal/ingest"
)

var _ ingest.RunLedger = (*WorklistStore)(nil)

// IngestRuns returns every owner's latest accepted ingest run (ingest.RunLedger).
func (s *WorklistStore) IngestRuns(ctx context.Context) (map[string]ingest.IngestRecord, error) {
	rows, err := s.db.sql.QueryContext(ctx,
		`SELECT owner_id, run_id, gen, done, accepted_at FROM ingest_runs`)
	if err != nil {
		return nil, fmt.Errorf("storage: list ingest runs: %w", err)
	}
	defer rows.Close()
	out := map[string]ingest.IngestRecord{}
	for rows.Next() {
		var (
			owner, runID  string
			gen, done, at int64
		)
		if err := rows.Scan(&owner, &runID, &gen, &done, &at); err != nil {
			return nil, fmt.Errorf("storage: list ingest runs: %w", err)
		}
		out[owner] = ingest.IngestRecord{RunID: runID, Gen: uint64(gen), Done: uint64(done), AcceptedAt: time.Unix(0, at)}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list ingest runs: %w", err)
	}
	return out, nil
}

// PutIngestRun records ownerID's latest accepted ingest run, replacing the previous
// record (ingest.RunLedger).
func (s *WorklistStore) PutIngestRun(ctx context.Context, ownerID string, rec ingest.IngestRecord) error {
	if ownerID == "" {
		return fmt.Errorf("storage: ingest run: owner is required")
	}
	_, err := s.db.sql.ExecContext(ctx, `
		INSERT INTO ingest_runs (owner_id, run_id, gen, done, accepted_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (owner_id) DO UPDATE SET
			run_id = excluded.run_id, gen = excluded.gen, done = excluded.done, accepted_at = excluded.accepted_at`,
		ownerID, rec.RunID, int64(rec.Gen), int64(rec.Done), rec.AcceptedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("storage: record ingest run: %w", err)
	}
	return nil
}
