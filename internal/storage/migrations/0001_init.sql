-- Schema version 1: the three web-tier stores (ADR 0004).

-- One WorkItem per row, stored as its JSON document. seq is the first-insertion
-- order: an upsert replaces doc in place and keeps seq, so List order matches the
-- in-memory store.
CREATE TABLE work_items (
    seq        INTEGER PRIMARY KEY,
    owner_id   TEXT    NOT NULL,
    id         TEXT    NOT NULL,
    doc        TEXT    NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (owner_id, id)
);

-- Delegated provider credentials, sealed with AES-256-GCM. key_id names the key
-- that sealed the row; sealed is nonce || ciphertext || tag over the credential
-- JSON, with (user_id, provider) bound as associated data.
CREATE TABLE credentials (
    user_id    TEXT    NOT NULL,
    provider   TEXT    NOT NULL,
    key_id     TEXT    NOT NULL,
    sealed     BLOB    NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, provider)
) WITHOUT ROWID;

-- Authenticated sessions. id_hash is the SHA-256 of the session id (hex); the raw
-- id exists only in the user's cookie.
CREATE TABLE sessions (
    id_hash    TEXT    NOT NULL PRIMARY KEY,
    user       TEXT    NOT NULL,
    expires_at INTEGER NOT NULL
) WITHOUT ROWID;

CREATE INDEX sessions_expires_at ON sessions (expires_at);
