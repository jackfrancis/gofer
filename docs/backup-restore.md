# Backup and restore

gofer's web tier keeps all of its durable state in one SQLite database,
`/var/lib/gofer/gofer.db`, on the PersistentVolumeClaim **`data-gofer-0`** in the
`gofer` namespace (see [ADR 0004](adr/0004-sqlite-persistent-storage.md)). It holds
sessions, the worklist with its conversation threads, and users' delegated provider
tokens, which are encrypted.

A complete backup is two things, kept in two places:

| What | Where it lives | How to back it up |
| --- | --- | --- |
| The data volume | PVC `data-gofer-0` | a CSI VolumeSnapshot (below), or Velero |
| The token-encryption key | `VAULT_ENCRYPTION_KEY` in the `gofer-secrets` Secret | your secret manager, separately from the volume |

**Status of this document.** The procedures follow from how StatefulSets, CSI
snapshots and SQLite behave, and the manifests are valid, but snapshot and restore
have not been run end to end: the kind development cluster has no snapshot support.
Rehearse a restore on the target cluster before relying on it.

## The encryption key

`VAULT_ENCRYPTION_KEY` is not on the volume, so a volume snapshot does not contain
it. Store a copy wherever you keep other long-lived secrets (for example Azure Key
Vault):

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

## Prerequisites for volume snapshots

1. A CSI driver that supports snapshots, backing the storage class the claim uses.
   On AKS the default class (`managed-csi`, Azure Disk CSI) does.
2. The snapshot CRDs and snapshot controller. AKS installs them. Check with
   `kubectl get crd volumesnapshots.snapshot.storage.k8s.io`.
3. A `VolumeSnapshotClass` for that driver. AKS does not always create one for
   Azure Disk; if `kubectl get volumesnapshotclass` shows none for
   `disk.csi.azure.com`, create it:

   ```yaml
   apiVersion: snapshot.storage.k8s.io/v1
   kind: VolumeSnapshotClass
   metadata:
     name: gofer-disk-snapshots
   driver: disk.csi.azure.com
   # Retain: deleting the VolumeSnapshot object (or the gofer namespace) keeps the
   # underlying Azure snapshot. Use Delete if you want them garbage-collected.
   deletionPolicy: Retain
   parameters:
     incremental: "true"
   ```

kind's default provisioner (local-path) has no snapshot support, so none of this
works on the development cluster.

## Take a snapshot

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: gofer-data-20261113   # name it by date
  namespace: gofer
spec:
  volumeSnapshotClassName: gofer-disk-snapshots
  source:
    persistentVolumeClaimName: data-gofer-0
```

```sh
kubectl apply -f snapshot.yaml
kubectl -n gofer get volumesnapshot gofer-data-20261113 -w   # wait for READYTOUSE=true
```

gofer keeps running; no downtime is needed.

### Why a snapshot of the live database is safe

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

If you want a snapshot with an empty WAL, for instance before an upgrade, scale to
zero first. Shutdown checkpoints the WAL into `gofer.db`:

```sh
kubectl -n gofer scale statefulset/gofer --replicas=0
kubectl -n gofer wait --for=delete pod/gofer-0 --timeout=60s
kubectl apply -f snapshot.yaml    # then wait for READYTOUSE=true
kubectl -n gofer scale statefulset/gofer --replicas=1
```

## Restore from a snapshot

The StatefulSet's pod always mounts the claim named `data-gofer-0`. Restoring
means replacing that claim with one created from the snapshot, while no pod is
using it. Do the steps in this order.

1. **Confirm the snapshot is usable.**

   ```sh
   kubectl -n gofer get volumesnapshot gofer-data-20261113
   ```

   `READYTOUSE` must be `true`. Note its `RESTORESIZE`.

2. **Confirm the encryption key in `gofer-secrets` is the one in use when the
   snapshot was taken.** If the Secret was recreated, put the backed-up key back
   first (see above for what happens otherwise):

   ```sh
   kubectl -n gofer patch secret gofer-secrets --type=merge \
     -p '{"stringData":{"VAULT_ENCRYPTION_KEY":"<the backed-up key>"}}'
   ```

3. **Stop gofer and wait for the pod to be gone.** The volume is ReadWriteOnce and
   the claim cannot be deleted while a pod mounts it.

   ```sh
   kubectl -n gofer scale statefulset/gofer --replicas=0
   kubectl -n gofer wait --for=delete pod/gofer-0 --timeout=60s
   ```

4. **Optional but recommended: keep the current disk.** With the usual `Delete`
   reclaim policy, deleting the claim in the next step deletes the disk. To be able
   to go back, switch its PersistentVolume to `Retain` first:

   ```sh
   PV=$(kubectl -n gofer get pvc data-gofer-0 -o jsonpath='{.spec.volumeName}')
   kubectl patch pv "$PV" -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}'
   ```

   The old disk then survives as a `Released` PV, to delete by hand once the
   restore is confirmed.

5. **Delete the current claim.**

   ```sh
   kubectl -n gofer delete pvc data-gofer-0
   ```

6. **Create the claim again, from the snapshot, with the same name.**

   ```yaml
   apiVersion: v1
   kind: PersistentVolumeClaim
   metadata:
     name: data-gofer-0          # must be exactly this
     namespace: gofer
   spec:
     accessModes: ["ReadWriteOnce"]
     # storageClassName: managed-csi   # set it if the snapshot's class is not the cluster default
     resources:
       requests:
         storage: 1Gi             # at least the snapshot's RESTORESIZE
     dataSource:
       apiGroup: snapshot.storage.k8s.io
       kind: VolumeSnapshot
       name: gofer-data-20261113
   ```

   ```sh
   kubectl apply -f restore-pvc.yaml
   ```

   With a `WaitForFirstConsumer` storage class (the AKS default) the claim stays
   `Pending` until the pod is scheduled. That is expected; continue.

7. **Start gofer.** The StatefulSet adopts the existing claim instead of creating a
   new one.

   ```sh
   kubectl -n gofer scale statefulset/gofer --replicas=1
   kubectl -n gofer rollout status statefulset/gofer --timeout=180s
   ```

8. **Verify.**

   ```sh
   kubectl -n gofer logs gofer-0 | grep -E 'persistent storage enabled|storage error'
   ```

   Expect `persistent storage enabled` and no `storage error`. Then load the
   landing page: you should still be signed in if your session predates the
   snapshot, and the worklist and threads should be as of the snapshot. Trigger a
   refresh to confirm the stored GitHub token still opens; a failure there with
   everything else intact means the encryption key does not match.

What a restore rolls back: everything written after the snapshot. Users who signed
in after it are signed out. A GitHub token refreshed after it may have been rotated
at the provider, in which case that user must sign in again. Worklist items are
re-ingested on the next refresh; conversation turns after the snapshot are gone.

**Restoring into a new cluster or namespace** is the same, with the order changed:
create the namespace and `gofer-secrets` (with the backed-up key), make the snapshot
available there (a pre-provisioned `VolumeSnapshotContent` pointing at the cloud
snapshot), create `data-gofer-0` from it (step 6), and only then apply
`deploy/k8s/base`. If the StatefulSet is applied first it creates an empty
`data-gofer-0`, and you are back at step 3.

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

- `make dev-down` deletes the `gofer` namespace, and the claim with it.
- Deleting the claim `data-gofer-0`, with the default `Delete` reclaim policy.

Scaling the StatefulSet to zero, deleting the StatefulSet, deleting the pod, and
`kubectl rollout restart` do not: the claim is retained.
