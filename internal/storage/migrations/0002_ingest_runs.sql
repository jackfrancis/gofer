-- Schema version 2: each owner's ingest gate state (ingest.RunLedger), so a
-- restarted web tier still holds a run the runtime may still be executing, or one
-- whose acceptance it never learned, and the radar's refresh version keeps counting
-- across restarts.
CREATE TABLE ingest_runs (
    owner_id TEXT    PRIMARY KEY,
    gen      INTEGER NOT NULL,          -- the last generation handed to a reservation
    done     INTEGER NOT NULL,          -- the highest generation known to have ended
    run_gen  INTEGER NOT NULL,          -- the current run's generation; 0 if none
    run_id   TEXT    NOT NULL,          -- its runtime id; '' while acceptance is unknown
    run_at   INTEGER NOT NULL,          -- reserved, or accepted once it was (Unix nanoseconds)
    pending  INTEGER NOT NULL,          -- 1: whether the runtime accepted it is not known
    run_until INTEGER NOT NULL          -- when it stops counting as in flight however it went (Unix nanoseconds)
);
