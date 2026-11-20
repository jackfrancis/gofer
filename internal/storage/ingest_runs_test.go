package storage

import (
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/ingest"
)

// Each owner's last ingest run is replaced in place and survives reopening the
// database, as a restarted web tier reads it.
func TestIngestRunsPersist(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	db := open(t, dir)
	s := db.Worklist()
	if got, err := s.IngestRuns(ctx); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v, %v", got, err)
	}
	at := time.Date(2026, 11, 20, 12, 0, 0, 123, time.UTC)
	if err := s.PutIngestRun(ctx, "github:1", ingest.IngestRecord{Gen: 1, RunGen: 1, Pending: true, RunAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIngestRun(ctx, "github:1", ingest.IngestRecord{Gen: 2, Done: 1, RunGen: 2, RunID: "run-2", RunAt: at.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIngestRun(ctx, "github:2", ingest.IngestRecord{Gen: 7, Done: 7}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIngestRun(ctx, "", ingest.IngestRecord{Gen: 1}); err == nil {
		t.Fatal("an empty owner must be refused")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := open(t, dir).Worklist().IngestRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]ingest.IngestRecord{
		"github:1": {Gen: 2, Done: 1, RunGen: 2, RunID: "run-2", RunAt: at.Add(time.Minute)},
		"github:2": {Gen: 7, Done: 7},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for o, w := range want {
		g := got[o]
		if g.Gen != w.Gen || g.Done != w.Done || g.RunGen != w.RunGen || g.RunID != w.RunID || g.Pending != w.Pending || !g.RunAt.Equal(w.RunAt) {
			t.Fatalf("%s: got %+v, want %+v", o, g, w)
		}
	}
}
