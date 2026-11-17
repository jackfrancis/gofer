# Backup and restore

gofer's web tier keeps all of its durable state in one SQLite database,
`/var/lib/gofer/gofer.db`, on the PersistentVolumeClaim **`data-gofer-0`** in the
`gofer` namespace (see [ADR 0004](adr/0004-sqlite-persistent-storage.md)). It holds
sessions, the worklist with its conversation threads, and users' delegated provider
tokens, which are encrypted.

Recovering gofer needs three things, kept in different places:

| What | Where it lives | How it is protected |
| --- | --- | --- |
| The data volume | PVC `data-gofer-0` | reclaim policy `Retain` (set by `make deploy`), scheduled CSI snapshots (`make backup-apply`) |
| The token-encryption key | `VAULT_ENCRYPTION_KEY` in the `gofer-secrets` Secret | a copy in your secret manager, outside the cluster |
| The deployment's settings | `gofer-config` (`BASE_URL`, `ALLOWED_GITHUB_LOGINS`, …) and the other `gofer-secrets` keys | wherever you keep per-environment configuration; not in this repository |

Everything here is cluster-agnostic: which cloud, account, region or resource group
holds a disk or a snapshot is decided by the cluster's storage and snapshot classes,
which the cluster's administrator owns. The make targets take those names as
parameters.

kind has no snapshot support, so the snapshot and restore targets do not work on the
development cluster.

**Status.** Rehearsed on AKS with the Azure Disk CSI driver, against a live gofer:
`backup-apply`, an on-demand `snapshot` (ready in about 15 seconds), pruning
(including the stored snapshot itself), a full `restore` (about 40 seconds, gofer down
for about 25) and [going back](#going-back-to-the-previous-volume) to the previous
volume. Not yet rehearsed: [restoring into a new cluster or
namespace](#restoring-into-a-new-cluster-or-namespace). Rehearse a restore on your
own cluster before relying on it. The additional restore preflight, backup-pause,
and failure-recovery safeguards are covered by fake-kubectl tests; they have not yet
been re-rehearsed against a live cluster.

## Protection that needs no setup

`make deploy` sets the data volume's reclaim policy to **`Retain`**
(`DATA_RECLAIM_POLICY`). Storage classes usually default to `Delete`, which destroys
the disk when its claim goes; with `Retain`, deleting the claim or the whole `gofer`
namespace leaves the disk, as a `Released` PersistentVolume you can bind again (see
[Going back](#going-back-to-the-previous-volume)). `make dev-up` uses `Delete`, so
`make dev-down` leaves nothing behind on kind.

Scaling the StatefulSet to zero, deleting it, deleting the pod, and
`kubectl rollout restart` never touch the claim.

## The encryption key

`VAULT_ENCRYPTION_KEY` is not on the volume, so a volume snapshot does not contain
it. Store a copy wherever you keep other long-lived secrets (a cloud key vault, a
password manager), outside the cluster:

```sh
kubectl -n gofer get secret gofer-secrets -o jsonpath='{.data.VAULT_ENCRYPTION_KEY}' | base64 -d
```

Keeping the key apart from the volume backups is deliberate: a leaked snapshot then
exposes no provider tokens.

**If the key is lost** and the volume is restored, sessions and the worklist come
back intact, but every stored provider token is unreadable. gofer cannot fetch from
GitHub on a user's behalf until that user signs out and in again, which replaces
their stored token under the new key. Nothing else is lost. Set a new key with
`openssl rand -base64 32`.

`SESSION_SECRET` matters less: if it changes, existing cookies stop verifying and
everyone signs in again. Back up the whole `gofer-secrets` Secret if you want a
restore to be invisible to users.

## Scheduled snapshots

### 1. A VolumeSnapshotClass (cluster administrator, once)

The cluster needs CSI snapshot support (the `volumesnapshots` CRD and the snapshot
controller; managed Kubernetes services usually install both) and a
`VolumeSnapshotClass` for the driver behind the data volume. The class decides where
snapshots are stored, so it is per-cluster configuration and is not in this
repository. Two settings matter:

- **`deletionPolicy: Retain`**, so that deleting the gofer namespace, or a
  VolumeSnapshot object by mistake, keeps the stored snapshot. `make backup-apply`
  warns if the class says `Delete`. Pruning still frees storage: it switches each
  snapshot it removes to `Delete` first.
- **Where snapshots are stored.** Put them somewhere that outlives the cluster. On
  managed clusters, the default is often a resource group or project the cluster owns
  and that is deleted along with it.

For example, on AKS with the Azure Disk CSI driver (fill in the placeholder):

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotClass
metadata:
  name: gofer-snapshots
driver: disk.csi.azure.com
deletionPolicy: Retain
parameters:
  incremental: "true"
  # A resource group that is NOT the cluster's node resource group (MC_...), which is
  # deleted with the cluster.
  resourceGroup: <resource-group>
```

The managed Azure Disk CSI driver acts as the cluster's own managed identity
(`az aks show -g <cluster-group> -n <cluster> --query identity.principalId`). Give it
the built-in **Disk Snapshot Contributor** role on that resource group, which allows
creating, reading and deleting snapshots there and nothing else:

```sh
az role assignment create --assignee-object-id <principal-id> --assignee-principal-type ServicePrincipal \
  --role "Disk Snapshot Contributor" --scope "$(az group show -n <resource-group> --query id -o tsv)"
```

### 2. Schedule snapshots

```sh
make backup-apply KUBECONFIG=<file> SNAPSHOT_CLASS=<class> \
    [SNAPSHOT_SCHEDULE="0 */6 * * *"] [SNAPSHOT_KEEP=28]
```

This installs the `gofer-snapshot` CronJob (`deploy/k8s/backup`). It runs
`/snapshot`, a second binary in the web image, on the schedule (cron syntax, UTC).
Each run takes a VolumeSnapshot of `data-gofer-0` named `data-gofer-0-<UTC time>`,
waits until it is ready to use, and then deletes the oldest beyond the newest
`SNAPSHOT_KEEP` usable ones. The defaults keep a week of six-hourly snapshots.

- A run whose snapshot fails, or is not ready within ten minutes, fails the job and
  prunes nothing, so a broken backup never deletes good ones. Watch for failed jobs
  with `kubectl -n gofer get jobs`.
- Pruning only touches snapshots of `data-gofer-0` that the job labeled. Snapshots
  you make by hand are never pruned.
- The job uses its own ServiceAccount, `gofer-snapshotter`: it can create, read,
  list and delete VolumeSnapshots in the `gofer` namespace, and read and patch
  VolumeSnapshotContents (cluster-scoped), which pruning needs. The web tier gains no
  rights.
- `make deploy` keeps the job on the image it deploys. Re-run `backup-apply` to
  change the class, schedule or retention.

Take one now, for instance before an upgrade, and list them:

```sh
make snapshot  KUBECONFIG=<file>
make snapshots KUBECONFIG=<file>
```

`make backup-down` stops the schedule and keeps every snapshot.

## Why a snapshot of the live database is safe

A CSI snapshot of one volume is a point-in-time image of the whole disk, which is
what the disk would contain if the node lost power at that instant. SQLite is built
to recover from exactly that:

- The database, its write-ahead log (`gofer.db-wal`) and its index (`gofer.db-shm`)
  are on the same volume, so the snapshot captures them together.
- gofer runs SQLite in WAL mode with `synchronous=FULL`: every committed
  transaction has been fsynced to the WAL before the write returns to the caller.
- On the next open, SQLite replays the WAL. Committed transactions are present; a
  transaction in flight at the snapshot instant is absent, as a whole.

So a snapshot restores to the last transaction committed before it was taken. No
quiescing, freeze hook or `sqlite3 .backup` step is required, which is as well: the
image is distroless and has no shell to run one in.

This reasoning needs the snapshot to be atomic across the volume. That holds for a
single CSI disk snapshot. It does **not** hold for a file-by-file copy of a live
volume (see Velero's file-system backup, below).

If you want a snapshot with an empty WAL, scale gofer to zero first: shutdown
checkpoints the WAL into `gofer.db`. It is never required.

## Restore from a snapshot

```sh
make snapshots KUBECONFIG=<file>                                   # pick one
make restore   KUBECONFIG=<file> SNAPSHOT=<name>                   # prints the plan
make restore   KUBECONFIG=<file> SNAPSHOT=<name> CONFIRM=<name>    # does it
```

Without `CONFIRM` repeating the snapshot name, `restore` prints what it would do and
changes nothing. The confirmed restore uses Bash and `jq` (`hack/restore/restore.sh`).
Before stopping the app it fetches and validates the replacement claim, temporarily
suspends the backup CronJob if it was enabled, and checks for unfinished backup Jobs,
unready snapshots of the source claim, and the snapshot-source protection finalizer.
A failed API read or invalid claim aborts rather than continuing with an empty file.

Existing Jobs are not cancelled by suspending a CronJob. If one is still pending or
running, let it finish and retry. A failed/timed-out backup Job can leave an unready
VolumeSnapshot behind: resolve the snapshot error and wait for readiness, or
explicitly abort that snapshot before retrying. **Do not remove PVC protection
finalizers manually.** The snapshot controller must release the source claim.
These checks include manual backup Jobs and unlabelled source snapshots, and are
repeated after shutdown. Do not start another restore, deployment, or manual
snapshot concurrently with this procedure.

Once the preflight succeeds, restore:

1. sets the current volume's reclaim policy to `Retain`, so it survives the swap;
2. scales gofer to zero and waits for the pod to go (the volume is ReadWriteOnce);
3. deletes the claim `data-gofer-0` and creates it again, with the same name,
   labels, storage class and access mode, from the snapshot;
4. scales gofer back to one; the StatefulSet adopts the restored claim;
5. sets the restored volume to `DATA_RECLAIM_POLICY` and prints the old volume's
   name, with how to delete it or go back to it.

Pod and PVC deletion each have a bounded wait (`RESTORE_TIMEOUT=120s` by default);
a pod-deletion timeout is an error, not permission to delete its claim anyway.
The original backup-schedule state is restored after success or a failure before
claim deletion. An already-suspended schedule stays suspended; a missing CronJob is
allowed for manually created snapshots. If shutdown or the second preflight fails
before deletion is attempted, gofer's previous replica count is restored too.

Failure handling distinguishes replacement/startup from post-restore finalization:

- **Before a restored rollout is confirmed:** failed PVC deletion, replacement
  creation, startup, or rollout leaves backups suspended and prints the retained old
  PV and recovery guidance. The message distinguishes an unconfirmed replacement from
  a successfully created claim whose rollout is not yet confirmed. A failed DELETE
  response can still mean the claim is terminating, so cleanup never automatically
  starts the StatefulSet against a possibly missing claim. Inspect the PVC, pod and
  rollout; follow [Going back](#going-back-to-the-previous-volume) where necessary.
  Resume backups using the printed, cluster-qualified command only after recovery.
- **After the restored rollout succeeds:** a later PVC read or reclaim-policy error
  is reported as incomplete finalization, not failed application recovery. The command
  still exits nonzero, but does not stop or roll back the restored app and restores
  the prior backup-schedule state. Verify and repair the restored PV's requested
  reclaim policy; do not repeat the restore solely to repair volume protection. If
  resuming backups also fails, that is reported separately with a manual resume
  command. An already-paused or absent CronJob remains untouched.

The script never force-removes storage finalizers or deletes an unfinished snapshot
on your behalf. Automatic cleanup of abandoned snapshot attempts is a separate
snapshot-lifecycle follow-up; this restore preflight remains strict.

The restore operator needs read access to Jobs and VolumeSnapshots and get/patch
access to the backup CronJob, in addition to the existing PVC/PV/StatefulSet rights.
Failure-injection tests run with `go test ./hack/restore` (also part of `go test ./...`)
using a fake kubectl; they require `make`, Bash, and `jq`, and never access a cluster.
The child process uses a minimal environment and pinned Make configuration so exported
deployment settings, Make includes/overrides, and shell startup hooks cannot alter it.

**Before restoring, check the encryption key.** `VAULT_ENCRYPTION_KEY` in
`gofer-secrets` must be the key in use when the snapshot was taken. If the Secret was
recreated since, put the backed-up key back first:

```sh
kubectl -n gofer patch secret gofer-secrets --type=merge \
  -p '{"stringData":{"VAULT_ENCRYPTION_KEY":"<the backed-up key>"}}'
```

**Verify** after the restore:

```sh
kubectl -n gofer logs gofer-0 | grep -E 'persistent storage enabled|storage error'
```

Expect `persistent storage enabled`, with the same `vault_key_id` as before, and no
`storage error`. Then load the landing page: you are still signed in if your session
predates the snapshot, and the worklist and threads are as of the snapshot. Trigger a
refresh to confirm the stored GitHub token still opens; a failure there with
everything else intact means the encryption key does not match.

**What a restore rolls back:** everything written after the snapshot. Users who
signed in after it are signed out. A GitHub token refreshed after it may have been
rotated at the provider, in which case that user signs in again. Worklist items come
back on the next refresh; conversation turns after the snapshot are gone.

### Going back to the previous volume

The old volume is kept as a `Released` PersistentVolume (`restore` prints its name).
To return to it, bind a new `data-gofer-0` to it explicitly:

```sh
OLD=<the old volume's name>
kubectl -n gofer scale statefulset/gofer --replicas=0
kubectl -n gofer wait --for=delete pod/gofer-0 --timeout=120s
NEW=$(kubectl -n gofer get pvc data-gofer-0 -o jsonpath='{.spec.volumeName}')
kubectl patch pv "$NEW" -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}'   # or Delete to discard it
kubectl -n gofer delete pvc data-gofer-0
kubectl patch pv "$OLD" --type=json -p '[{"op":"remove","path":"/spec/claimRef"}]'
kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data-gofer-0
  namespace: gofer
  labels:
    app.kubernetes.io/name: gofer
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: $(kubectl get pv "$OLD" -o jsonpath='{.spec.storageClassName}')
  volumeName: $OLD
  resources:
    requests:
      storage: $(kubectl get pv "$OLD" -o jsonpath='{.spec.capacity.storage}')
EOF
kubectl -n gofer scale statefulset/gofer --replicas=1
```

Once you are happy with whichever volume is in use, delete the other one, and its
disk, by setting its reclaim policy to `Delete`:

```sh
kubectl patch pv <volume> -p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}'
```

### Restoring into a new cluster or namespace

`make restore` replaces an existing claim. For a new cluster, or after the `gofer`
namespace was deleted, the claim has to exist before gofer is deployed, or the
StatefulSet creates an empty one:

1. Create the namespace and `gofer-secrets`, with the backed-up
   `VAULT_ENCRYPTION_KEY`.
2. Make the snapshot visible there. In the same cluster, a retained
   VolumeSnapshotContent can be bound to a new VolumeSnapshot. Otherwise create a
   pre-provisioned one from the stored snapshot's handle (on AKS, the Azure snapshot's
   resource ID):

   ```yaml
   apiVersion: snapshot.storage.k8s.io/v1
   kind: VolumeSnapshotContent
   metadata:
     name: gofer-restore
   spec:
     driver: <csi-driver>
     deletionPolicy: Retain
     source:
       snapshotHandle: <stored-snapshot-handle>
     volumeSnapshotRef:
       name: gofer-restore
       namespace: gofer
   ---
   apiVersion: snapshot.storage.k8s.io/v1
   kind: VolumeSnapshot
   metadata:
     name: gofer-restore
     namespace: gofer
   spec:
     source:
       volumeSnapshotContentName: gofer-restore
   ```

3. Create `data-gofer-0` from it: a claim like the one `restore` creates, with
   `dataSource` naming the VolumeSnapshot.
4. Deploy gofer (`make deploy`). The StatefulSet adopts the claim.

## Velero: the whole-namespace alternative

[Velero](https://velero.io) backs up the namespace's objects (the StatefulSet,
ConfigMap, Service, the `gofer-secrets` Secret, the claim) together with the volume
data, on a schedule, to object storage. It suits disaster recovery and moving
between clusters.

```sh
velero backup create gofer-20261113 --include-namespaces gofer --snapshot-move-data
velero restore create --from-backup gofer-20261113
```

Three things to get right:

- **Use CSI snapshots for the volume**, which is what Velero does for CSI volumes
  when its CSI support is enabled; `--snapshot-move-data` additionally copies the
  snapshot's contents to object storage, so the backup survives the loss of the
  cluster's region or subscription. The safety argument is the one above.
- **Do not use file-system backup (Kopia/Restic pod volume backup) on the running
  pod.** It copies `gofer.db` and `gofer.db-wal` one after the other while they
  change, which can pair a database file with a WAL from a different moment. If
  file-system backup is all you have, scale gofer to zero for the backup. A
  pre-backup hook cannot fix it, because the image has no shell.
- **The backup contains `gofer-secrets`**, including `VAULT_ENCRYPTION_KEY` next to
  the data it protects. That is convenient for restore and removes the separation
  described under "The encryption key". Encrypt the backup storage location, or
  exclude Secrets (`--exclude-resources secrets`) and restore the key from your
  secret manager.

The AEI side (`aei-system`, the `AgentApp`'s provider class, the signing key) is not
in the `gofer` namespace and is not covered by a gofer backup.

## Things that delete the data

- Deleting the claim `data-gofer-0` or the `gofer` namespace while the volume's
  reclaim policy is `Delete`. `make deploy` sets `Retain`; `make dev-up` sets
  `Delete`, and `make dev-down` deletes the namespace.
- Setting a retained volume to `Delete`, which is how you delete one on purpose.
- Pruning, for snapshots beyond `SNAPSHOT_KEEP`.
