-- Schema version 2: each owner's latest accepted ingest run (ingest.RunLedger), so
-- a restarted web tier still gates on a run the runtime may still be executing, and
-- the radar's refresh version keeps counting across restarts.
CREATE TABLE ingest_runs (
    owner_id    TEXT    PRIMARY KEY,
    run_id      TEXT    NOT NULL,          -- the runtime's id for the run
    gen         INTEGER NOT NULL,          -- its generation: ingest runs accepted for the owner
    done        INTEGER NOT NULL,          -- the highest generation known to have ended
    accepted_at INTEGER NOT NULL           -- when the runtime accepted it (Unix nanoseconds)
);
