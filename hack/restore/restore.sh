#!/usr/bin/env bash
# The confirmed half of `make restore`. Keep kubectl and its cluster-selection
# flags as separate arguments; never eval a command string.
set -euo pipefail

namespace=${KUBE_NS:-gofer}
claim=${DATA_CLAIM:-data-gofer-0}
snapshot=${SNAPSHOT:?SNAPSHOT must be set}
[[ ${CONFIRM:-} == "$snapshot" ]] || { echo "ERROR: CONFIRM must repeat SNAPSHOT." >&2; exit 1; }
policy=${DATA_RECLAIM_POLICY:-Retain}
wait_timeout=${RESTORE_TIMEOUT:-120s}
rollout_timeout=${ROLLOUT_TIMEOUT:-180s}
[[ $# -gt 0 ]] || { echo "ERROR: pass kubectl and its cluster-selection flags." >&2; exit 1; }
kubectl=("$@")
case "$policy" in Retain|Delete) ;; *) echo "ERROR: invalid DATA_RECLAIM_POLICY: $policy" >&2; exit 1;; esac

fail() { echo "ERROR: $*" >&2; exit 1; }
kube() { "${kubectl[@]}" "$@"; }
resume_command() {
    printf '  ' >&2
    printf '%q ' "${kubectl[@]}" -n "$namespace" patch cronjob gofer-snapshot --type=merge -p '{"spec":{"suspend":false}}' >&2
    printf '\n' >&2
}

tmp=$(mktemp -d)
resume_backups=false
stop_attempted=false
delete_attempted=false
pv=
replicas=1
cleanup() {
    local status=$?
    trap - EXIT
    # Once DELETE has been sent, even an error may mean the claim is terminating.
    # Never restart the StatefulSet in that state: it could create an empty claim.
    if [[ $status -ne 0 && $delete_attempted == true ]]; then
        echo "ERROR: restore stopped after attempting claim deletion. Original volume $pv is retained." >&2
        echo "Inspect PVC $namespace/$claim; see 'Going back' in docs/backup-restore.md before restarting gofer." >&2
        if [[ $resume_backups == true ]]; then
            echo "Scheduled backups remain suspended. After recovery, resume them with:" >&2
            resume_command
        fi
    else
        if [[ $status -ne 0 && $stop_attempted == true ]]; then
            echo "The claim was not deleted; restoring gofer's previous replica count ($replicas)." >&2
            kube -n "$namespace" scale statefulset/gofer --replicas="$replicas" >/dev/null || \
                echo "ERROR: could not restore gofer's replica count; check the StatefulSet." >&2
        fi
        if [[ $resume_backups == true ]]; then
            if ! kube -n "$namespace" patch cronjob gofer-snapshot --type=merge -p '{"spec":{"suspend":false}}' >/dev/null; then
                echo "ERROR: could not resume scheduled backups. Resume them with:" >&2
                resume_command
                status=1
            fi
        fi
    fi
    rm -rf "$tmp"
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Fetch separately from jq. In particular, a failed GET must not become a
# successful empty jq input followed by deletion of the only working claim.
kube -n "$namespace" get pvc "$claim" -o json > "$tmp/original.json"
pv=$(jq -er --arg name "$claim" --arg ns "$namespace" '
    select(.kind == "PersistentVolumeClaim" and .metadata.name == $name and .metadata.namespace == $ns)
    | select(.metadata.uid | type == "string" and length > 0)
    | select(.metadata.deletionTimestamp == null and .status.phase == "Bound")
    | .spec.volumeName | select(type == "string" and length > 0)
' "$tmp/original.json") || fail "the source claim is not a valid, bound PVC. Nothing changed."
kube -n "$namespace" get volumesnapshot "$snapshot" -o json > "$tmp/snapshot.json"
jq -e '.status.readyToUse == true and .metadata.deletionTimestamp == null and (.metadata.uid | type == "string" and length > 0)' "$tmp/snapshot.json" >/dev/null || \
    fail "snapshot $snapshot is no longer ready to use. Nothing changed."
size=$(jq -er '.status.restoreSize // "" | select(type == "string")' "$tmp/snapshot.json")
jq -e --arg snap "$snapshot" --arg size "$size" '
    select((.spec.accessModes | type == "array" and length > 0)
        and (.spec.resources.requests.storage | type == "string" and length > 0))
    | {apiVersion: "v1", kind: "PersistentVolumeClaim",
       metadata: {name: .metadata.name, namespace: .metadata.namespace, labels: (.metadata.labels // {})},
       spec: {accessModes: .spec.accessModes, storageClassName: .spec.storageClassName,
              volumeMode: (.spec.volumeMode // "Filesystem"),
              resources: {requests: {storage: (if $size != "" then $size else .spec.resources.requests.storage end)}},
              dataSource: {apiGroup: "snapshot.storage.k8s.io", kind: "VolumeSnapshot", name: $snap}}}
' "$tmp/original.json" > "$tmp/claim.json" || fail "could not build a valid replacement claim. Nothing changed."
replicas=$(kube -n "$namespace" get statefulset gofer -o jsonpath='{.spec.replicas}')
case "$replicas" in 0|1) ;; *) fail "expected gofer to have zero or one replicas, got: $replicas";; esac

# --ignore-not-found permits manual snapshots without an installed CronJob, but
# authorization/transport failures still abort. Preserve an already-paused schedule.
kube -n "$namespace" get cronjob gofer-snapshot --ignore-not-found -o json > "$tmp/cronjob.json"
if [[ -s "$tmp/cronjob.json" ]]; then
    suspended=$(jq -er 'if .spec.suspend == true then "true" else "false" end' "$tmp/cronjob.json")
    if [[ $suspended == false ]]; then
        resume_backups=true # also restore the old state if PATCH has an ambiguous failure
        kube -n "$namespace" patch cronjob gofer-snapshot --type=merge -p '{"spec":{"suspend":true}}' >/dev/null
    fi
fi

# Suspending a CronJob neither stops its existing Jobs nor aborts CSI snapshots.
# Reject unfinished work (including pending/manual Jobs and unlabelled snapshots)
# before downtime. Never strip storage-protection finalizers to force a restore.
check_quiet() {
    local jobs snapshots
    kube -n "$namespace" get jobs -o json > "$tmp/jobs.json"
    jobs=$(jq -er '
        if (.items | type) != "array" then error("invalid Job list") else
        [.items[] | select(.spec.template.spec.serviceAccountName == "gofer-snapshotter")
         | select((.status.active // 0) > 0 or
             ([.status.conditions[]? | select(.status == "True" and (.type == "Complete" or .type == "Failed"))] | length == 0))
         | .metadata.name] | join(", ") end
    ' "$tmp/jobs.json")
    [[ -z "$jobs" ]] || fail "unfinished backup Jobs: $jobs. Wait for them to finish (or resolve them), then retry restore."
    kube -n "$namespace" get volumesnapshots -o json > "$tmp/snapshots.json"
    snapshots=$(jq -er --arg claim "$claim" '
        if (.items | type) != "array" then error("invalid VolumeSnapshot list") else
        [.items[] | select(.spec.source.persistentVolumeClaimName == $claim and .status.readyToUse != true)
         | .metadata.name] | join(", ") end
    ' "$tmp/snapshots.json")
    [[ -z "$snapshots" ]] || fail "unfinished source snapshots: $snapshots. Wait for readiness or explicitly abort failed snapshots, then retry restore."
    kube -n "$namespace" get pvc "$claim" -o json > "$tmp/current.json"
    jq -e --arg pv "$pv" --slurpfile original "$tmp/original.json" '
        .metadata.uid == $original[0].metadata.uid and .spec.volumeName == $pv
        and .metadata.deletionTimestamp == null
        and ((.metadata.finalizers // [] | index("snapshot.storage.kubernetes.io/pvc-as-source-protection")) == null)
    ' "$tmp/current.json" >/dev/null || fail "$claim changed or is still protected as a snapshot source. Wait for the snapshot controller; do not remove finalizers manually."
    # A backup may have pruned the selected snapshot between the initial read and
    # suspension. Refuse a changed/deleting source before replacing the PVC.
    kube -n "$namespace" get volumesnapshot "$snapshot" -o json > "$tmp/current-snapshot.json"
    jq -e --slurpfile original "$tmp/snapshot.json" '
        .metadata.uid == $original[0].metadata.uid and .metadata.deletionTimestamp == null and .status.readyToUse == true
    ' "$tmp/current-snapshot.json" >/dev/null || fail "snapshot $snapshot changed or is no longer ready."
}
check_quiet

echo "1/5 keeping the current volume $pv"
kube patch pv "$pv" -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}' >/dev/null
echo "2/5 stopping gofer"
stop_attempted=true
kube -n "$namespace" scale statefulset/gofer --replicas=0 >/dev/null
kube -n "$namespace" wait --for=delete pod/gofer-0 --timeout="$wait_timeout"
# Recheck after shutdown in case a previously dispatched/manual backup started.
check_quiet
echo "3/5 replacing $claim"
delete_attempted=true
kube -n "$namespace" delete pvc "$claim" --wait=true --timeout="$wait_timeout"
kube create -f "$tmp/claim.json" >/dev/null
# A successful create is the only point at which starting the StatefulSet is safe.
echo "4/5 starting gofer on the restored volume"
kube -n "$namespace" scale statefulset/gofer --replicas=1 >/dev/null
kube -n "$namespace" rollout status statefulset/gofer --timeout="$rollout_timeout"
new=$(kube -n "$namespace" get pvc "$claim" -o jsonpath='{.spec.volumeName}')
[[ -n "$new" ]] || fail "the restored PVC has no bound volume"
echo "5/5 setting the restored volume $new to reclaim policy $policy"
kube patch pv "$new" -p "{\"spec\":{\"persistentVolumeReclaimPolicy\":\"$policy\"}}" >/dev/null
kube -n "$namespace" logs gofer-0 | grep -m1 'persistent storage' || true
echo
echo "Restored $claim from $snapshot. Original volume $pv is retained."
echo "To discard the old volume and its disk:"
printf '  '
printf '%q ' "${kubectl[@]}" patch pv "$pv" -p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}'
printf '\n'
echo "To undo the restore, see 'Going back' in docs/backup-restore.md."
