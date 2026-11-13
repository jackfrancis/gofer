# ADR 0004 — Web-tier state is durable in SQLite on a PersistentVolume

Status: accepted
Date: 2026-11-13

(0003 is taken by the repository-checkout ADR on a parallel branch.)

## Context

All web-tier state lived in memory in the one web pod: the worklist (each
`WorkItem` with its whole conversation thread), the vault of delegated provider
tokens, and sessions. A pod restart lost all three: every user was signed out, the
radar was re-ingested from scratch, and every thread was gone. That is acceptable
on a kind cluster and not on a production cluster such as AKS, where the state must
also be backed up and restored with standard Kubernetes tooling.

The backend is decided: **SQLite on a PersistentVolume, gofer as a single
replica.** PostgreSQL and a dual-backend design were considered and rejected for
now. This ADR records how that choice is implemented, not whether to make it.

The seams were already small. `worklist.Store` is `List`/`Upsert` plus
`worklist.Lister.All`; `vault.Vault` is `Put`/`Get`/`Delete`; `session.Manager` was
a concrete type with no storage seam.

## Decision

### One database file, one package

`internal/storage` opens one SQLite file, `$DATA_DIR/gofer.db`, once in
`cmd/server`, and hands out three stores over it: `Worklist()` (`worklist.Store` +
`worklist.Lister`), `Vault(keyring)` (`vault.Vault`), and `Sessions()`
(`session.Store`). `internal/storage` is the only package that imports the driver.

### Driver: `modernc.org/sqlite`

The web binary is cross-compiled on the host with `CGO_ENABLED=0` and shipped in
distroless/static, so the driver must be pure Go. `modernc.org/sqlite` is a
mechanical translation of the upstream SQLite C source to Go, exposed through
`database/sql`. It is the most widely used pure-Go SQLite, tracks upstream releases,
and needs no C toolchain, libc or shell in the image.

The alternative, `github.com/ncruces/go-sqlite3`, runs SQLite as WebAssembly under
wazero. It is also cgo-free, but adds a WASM runtime to the process for no benefit
here. `mattn/go-sqlite3` requires cgo and is excluded by the build.

Cost: this is gofer's largest dependency (the translated SQLite plus `modernc.org/libc`
and a few small helpers) and it adds about 4 MiB to the stripped web binary (roughly
13 to 17 MiB on linux/arm64). Only the web
tier pays it: `cmd/runtime` does not import `internal/storage`.

### Connection settings

Applied to every pooled connection through the DSN:

| Setting | Value | Why |
| --- | --- | --- |
| `journal_mode` | `WAL` | readers do not block the writer; one sequential log to fsync |
| `synchronous` | `FULL` | every commit is fsynced to the WAL before `Upsert`/`Put` returns. `NORMAL` would be faster and still never corrupt, but can lose the most recent commits on power loss — and one of those may be a rotated refresh token that the provider has already invalidated. Write volume is low, so durability wins |
| `busy_timeout` | 5000 ms | a writer waits for the lock instead of failing with `SQLITE_BUSY` |
| `_txlock` | `immediate` | write transactions take the write lock at `BEGIN`, so the busy timeout applies; a deferred transaction that upgrades mid-way fails immediately instead |
| `foreign_keys` | `ON` | off by default in SQLite; on for any future schema that uses them |
| `temp_store` | `MEMORY` | the root filesystem is read-only; SQLite must not write temp files to `/tmp` |

The file is created `0600` in a `0700` directory; the `-wal` and `-shm` files
inherit the mode. The pool is capped at 8 connections. On shutdown the server
drains HTTP, then checkpoints the WAL (`wal_checkpoint(TRUNCATE)`) and closes.

### Schema and migrations

Migrations are numbered SQL files embedded in the binary
(`internal/storage/migrations/NNNN_name.sql`). The schema version is SQLite's
`PRAGMA user_version`. On open, each migration above the current version runs in
its own transaction together with the version bump, so a crash mid-migration leaves
the previous version intact. A file whose version is newer than the binary knows is
refused, so an older image cannot silently run against a newer schema.

```
work_items   (seq INTEGER PRIMARY KEY, owner_id, id, doc TEXT, updated_at, UNIQUE(owner_id, id))
credentials  (user_id, provider, key_id, sealed BLOB, updated_at, PRIMARY KEY(user_id, provider))
sessions     (id_hash TEXT PRIMARY KEY, user TEXT, expires_at)
```

### Worklist

Each `WorkItem` is stored as its JSON document, keyed by `(owner_id, id)`. This
matches `MemoryStore`:

- every item is re-scoped to the owner on write (and again on read, from the
  column, so a document can never claim another owner);
- `Upsert` replaces by id and keeps the item's original position; `List` returns
  items in first-insertion order (`seq`);
- `List` for an unknown owner returns an empty, non-nil slice.

Each `Upsert` call is one transaction: all of its items land or none do.

One difference is inherent to storing documents: an item read back has been through
JSON. Timestamps keep their instant but lose their monotonic reading and named
location, and an empty slice in an `omitempty` field comes back nil. Nothing in
gofer depends on either, and items arriving on the agent plane have already been
through JSON.

**Read-modify-write in the callers.** `internal/webui` and `internal/api` read an
owner's items with `List`, change some, and write them back with `Upsert`. Two
overlapping requests that change the *same item* can therefore lose one update (for
example a user hiding a message while a run appends its reply to that thread). This
was already true of `MemoryStore`, whose lock also covers only the single call.
SQLite does not change the kind of race, and because `Upsert` replaces only the
items it is given, the exposure is still per item, not per owner. It does make the
window somewhat wider: the gap between read and write now includes disk I/O
(milliseconds, not microseconds). The callers are not redesigned here. The fix,
when it is wanted, is optimistic concurrency: a `version` column that `Upsert`
checks and bumps, with callers retrying on conflict. That is one migration plus a
change to the `Store` contract.

### Vault: tokens are encrypted at rest

`vault.Vault` stays the seam. The SQLite implementation seals each credential
(access token, refresh token, token type, expiry — the whole record, as JSON) with
**AES-256-GCM** from the standard library, using a fresh random 96-bit nonce per
write.

- **Key.** `VAULT_ENCRYPTION_KEY` in the `gofer-secrets` Secret: base64 of 32
  random bytes (`openssl rand -base64 32`). It is deliberately separate from
  `SESSION_SECRET`, which authenticates cookies. If `DATA_DIR` is set and the key
  is missing or malformed, the server refuses to start.
- **Associated data.** A domain tag plus the length-prefixed user id and provider.
  A sealed blob copied onto another user's row, or another provider's, fails
  authentication.
- **Key id.** Each row stores the id of the key that sealed it: the first 8 bytes
  of `SHA-256("gofer-vault-key-id" ‖ key)`. It is derived, so it cannot be
  misconfigured. `storage.Keyring` seals with one primary key and can open with
  additional retired keys, so rotation is: make the new key primary, keep the old
  one as retired, and rows re-seal as users sign in or tokens refresh. The keyring
  supports this today; the configuration for retired keys is not wired, because
  nothing needs it yet.
- **Failure.** A row sealed with an unknown key, or one that fails authentication,
  is an error from `Get` (not `ErrNotFound`), and is never logged with its
  contents. Signing in again overwrites the row.

A KMS-backed vault (envelope encryption, or tokens held outside the database
entirely) replaces `storage`'s vault behind the same interface; nothing here
precludes it and none is built.

Not encrypted: the worklist documents and the session rows (user id, provider,
email, name, login). They are personal data but not credentials. Volume-level
encryption at rest is the cluster's job (Azure managed disks are encrypted by
default).

### Sessions

`internal/session` gains the smallest seam that makes logins durable:

```go
type Store interface {
	Save(ctx context.Context, key string, u User, expiresAt time.Time) error
	Load(ctx context.Context, key string) (User, time.Time, error) // ErrNotFound
	Delete(ctx context.Context, key string) error
	DeleteExpired(ctx context.Context, now time.Time) error
}
```

`NewManager(secret, secure)` keeps its signature and uses an in-memory `Store`;
`WithStore` swaps in the SQLite one. The key is always the hex **SHA-256 of the
session id**, never the id itself, so a copy of the database (a snapshot, a backup)
contains nothing that can be replayed as a cookie. The id is 256 random bits, so a
plain hash needs no salt. The manager enforces expiry on every load, and its
existing ten-minute sweep now also deletes expired rows.

The **pre-auth OAuth flow state stays in memory.** It lives for the seconds between
the redirect to the provider and the callback. If the pod restarts in that window
the callback answers "no login in progress" and the user clicks sign-in again.
Persisting it would write PKCE verifiers and state tokens to disk to save one click
in a rare case. Its lifetime is also cut from 24 hours to 10 minutes.

`Authenticate` now returns an error, so a failed write surfaces as a failed login
instead of a cookie that resolves to nothing.

### Configuration

One setting: `DATA_DIR`. The database is `$DATA_DIR/gofer.db`.

- **Unset:** in-memory stores, exactly as before, with a startup log line saying
  state will not survive a restart. `make run` and the existing tests are unchanged.
- **Set:** SQLite at that path; `VAULT_ENCRYPTION_KEY` is then required.

The ingest backfill tracker stays in memory. It only de-duplicates in-flight
backfills; after a restart the next page load re-dispatches one if needed.

### Kubernetes

The web tier becomes a single-replica **StatefulSet** with a
`volumeClaimTemplate` (`data`, `ReadWriteOnce`, 1Gi, no `storageClassName`, so the
cluster default applies: local-path on kind, the managed CSI disk on AKS), mounted
at `/var/lib/gofer`.

- A StatefulSet, not a Deployment with a PVC, because its update is
  delete-then-create: there are never two pods on the database, and never a new pod
  stuck waiting for an RWO volume the old pod still holds.
- `fsGroup: 65532` with `fsGroupChangePolicy: OnRootMismatch` lets the non-root
  user write the volume. `readOnlyRootFilesystem` stays on.
- The PVC is `data-gofer-0` and is retained when the StatefulSet is scaled down or
  deleted. That stable name is what makes restore a claim swap (see
  `docs/backup-restore.md`).
- `serviceName` points at the existing ClusterIP Service. A headless governing
  Service only adds per-pod DNS, which a single replica does not need.
- The server already drains on SIGTERM within 10 seconds, well inside the default
  30-second grace period; it now closes the database after draining.

## Backup and restore

`docs/backup-restore.md` is the procedure. The design points:

- The unit of backup is the volume: a CSI `VolumeSnapshot` of `data-gofer-0`. A
  snapshot of the live database is crash-consistent, and SQLite in WAL mode with
  `synchronous=FULL` recovers from a crash at any instant to the last committed
  transaction, so no quiescing is required.
- The encryption key is not on the volume and must be backed up separately. Without
  it the worklist and sessions restore, but every stored provider token is
  unreadable and every user must sign in again.

**Proposed, not built: an application-level export.** A `server backup <file>`
subcommand (or an admin endpoint) running `VACUUM INTO` would produce a consistent,
compacted single-file copy from the live database, without snapshot support in the
cluster. It is about 30 lines. It would let kind exercise a real backup and restore
(kind's provisioner has no snapshots), and it gives a file that can be shipped to
object storage. The open question is where the file goes: the pod has a read-only
root and no shell, so it needs either space on the data volume plus `kubectl cp`
(which needs `tar` in the image, which distroless does not have) or a streaming
HTTP endpoint with its own authorization. That is a design decision to take
separately.

## Consequences

- A pod restart keeps logins, delegated tokens, the worklist and its threads.
- gofer is a single replica by construction: the volume is `ReadWriteOnce` and
  SQLite is a single-node database. High availability means a different backend
  behind the same three seams.
- A new secret, `VAULT_ENCRYPTION_KEY`, must exist before the pod starts and must
  be backed up. Losing it loses the stored tokens, not the rest.
- A rolling update now has a brief outage (old pod stops before the new one
  starts). It was already a state-losing event; now it is only a short one.
- The web binary grows by the SQLite driver. The runtime image is unaffected.
- A committed write costs an fsync. At gofer's write rate this is not measurable
  to a user; it is the price of `synchronous=FULL`.
