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
resume_args=(-n "$namespace" patch cronjob gofer-snapshot --type=merge -p '{"spec":{"suspend":false}}')
resume_command() {
    printf '  ' >&2
    printf '%q ' "${kubectl[@]}" "${resume_args[@]}" >&2
    printf '\n' >&2
}

tmp=$(mktemp -d)
resume_backups=false
stop_attempted=false
restore_phase=preflight
pv=
replicas=1
expand_to=
size=
cleanup() {
    local status=$?
    trap - EXIT
    local may_resume_backups=false
    case "$restore_phase" in
        preflight|rollout_complete) may_resume_backups=true;;
    esac
    if [[ $status -ne 0 ]]; then
        case "$restore_phase" in
            preflight)
                if [[ $stop_attempted == true ]]; then
                    echo "The claim was not deleted; restoring gofer's previous replica count ($replicas)." >&2
                    kube -n "$namespace" scale statefulset/gofer --replicas="$replicas" >/dev/null || \
                        echo "ERROR: could not restore gofer's replica count; check the StatefulSet." >&2
                fi
                ;;
            deleting_claim|claim_created)
                # A DELETE or CREATE error can have an ambiguous outcome. Even
                # after a confirmed CREATE, startup may not have succeeded. Do
                # not restart or take backups until the restored rollout is known.
                if [[ $restore_phase == claim_created ]]; then
                    echo "ERROR: replacement claim $namespace/$claim was created, but gofer's rollout was not confirmed." >&2
                    echo "Inspect the PVC, pod and rollout; see 'Going back' in docs/backup-restore.md if recovery is needed." >&2
                else
                    echo "ERROR: restore stopped after attempting claim deletion; replacement creation was not confirmed." >&2
                    echo "Inspect PVC $namespace/$claim; see 'Going back' in docs/backup-restore.md before restarting gofer." >&2
                fi
                echo "Original volume $pv is retained." >&2
                ;;
            rollout_complete)
                # Once rollout succeeded, a later read/policy failure concerns
                # finalization, not claim replacement or application startup.
                echo "ERROR: gofer's restored rollout succeeded, but post-restore finalization is incomplete." >&2
                echo "Verify the bound PV for $namespace/$claim and its requested reclaim policy ($policy)." >&2
                if [[ -n $expand_to ]]; then
                    echo "Then verify that $namespace/$claim requests $expand_to (it was restored at $size)." >&2
                fi
                echo "No rollback or additional stop was attempted. Do not repeat restore solely to repair volume protection." >&2
                echo "Original volume $pv is retained." >&2
                ;;
        esac
    fi
    if [[ $resume_backups == true ]]; then
        if [[ $may_resume_backups == true ]]; then
            if ! kube "${resume_args[@]}" >/dev/null; then
                echo "ERROR: could not resume scheduled backups. Resume them with:" >&2
                resume_command
                status=1
            fi
        else
            echo "Scheduled backups remain suspended. After recovery, resume them with:" >&2
            resume_command
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
# Restore at the snapshot's restoreSize, which every CSI driver can provision (some,
# such as Dell Unity, reject any other size). If the claim was expanded after the
# snapshot was taken, expand the restored claim back to the current request once it
# is bound. Without a usable restoreSize (absent or 0), request the current size.
# Quantities follow the Kubernetes grammar; an unparseable one fails here, before
# anything changes.
jq -e --arg snap "$snapshot" --arg size "$size" '
    def bytes:
        (capture("^(?<sign>[+-]?)(?<n>[0-9]+(\\.[0-9]*)?|\\.[0-9]+)(?<s>Ki|Mi|Gi|Ti|Pi|Ei|[eE][+-]?[0-9]+|n|u|m|k|M|G|T|P|E)?$")
         // error("invalid storage quantity: \(.)")) as $q
        # Normalize "4." and ".5" for every jq version.
        | ($q.n | if endswith(".") then . + "0" else . end | if startswith(".") then "0" + . else . end | tonumber)
          * (if $q.sign == "-" then -1 else 1 end)
          * (if $q.s == null then 1
             elif ($q.s | test("^[eE][+-]?[0-9]+$")) then pow(10; $q.s[1:] | ltrimstr("+") | tonumber)
             else {Ki: 1024, Mi: 1048576, Gi: 1073741824, Ti: 1099511627776,
                   Pi: 1125899906842624, Ei: 1152921504606846976,
                   n: 1e-9, u: 1e-6, m: 1e-3, k: 1e3, M: 1e6, G: 1e9,
                   T: 1e12, P: 1e15, E: 1e18}[$q.s] end);
    select((.spec.accessModes | type == "array" and length > 0)
        and (.spec.resources.requests.storage | type == "string" and length > 0))
    | .spec.resources.requests.storage as $current
    | ($current | bytes) as $current_bytes
    | select($current_bytes > 0)
    | (if $size == "" then 0 else ($size | bytes) end) as $restore_bytes
    | (if $restore_bytes <= 0 or $restore_bytes == $current_bytes then $current else $size end) as $storage
    | {expand_to: (if $restore_bytes > 0 and $restore_bytes < $current_bytes then $current else "" end),
       storage_class: (.spec.storageClassName // ""),
       claim: {apiVersion: "v1", kind: "PersistentVolumeClaim",
         metadata: {name: .metadata.name, namespace: .metadata.namespace, labels: (.metadata.labels // {})},
         spec: {accessModes: .spec.accessModes, storageClassName: .spec.storageClassName,
                volumeMode: (.spec.volumeMode // "Filesystem"),
                resources: {requests: {storage: $storage}},
                dataSource: {apiGroup: "snapshot.storage.k8s.io", kind: "VolumeSnapshot", name: $snap}}}}
' "$tmp/original.json" > "$tmp/plan.json" || fail "could not build a valid replacement claim. Nothing changed."
jq -e '.claim' "$tmp/plan.json" > "$tmp/claim.json"
expand_to=$(jq -er '.expand_to' "$tmp/plan.json")
if [[ -n "$expand_to" ]]; then
    # Restoring at the smaller snapshot size is only safe if the claim can grow
    # back afterward; otherwise the restore would permanently shrink it.
    storage_class=$(jq -er '.storage_class' "$tmp/plan.json")
    [[ -n "$storage_class" ]] || fail "$claim must grow from $size to $expand_to after restore, but has no storage class. Nothing changed."
    kube get storageclass "$storage_class" -o json > "$tmp/storageclass.json"
    jq -e '.allowVolumeExpansion == true' "$tmp/storageclass.json" >/dev/null || \
        fail "snapshot $snapshot restores at $size, smaller than $claim ($expand_to), and storage class $storage_class does not allow volume expansion. Nothing changed."
fi
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
    # The replacement is sized from the first read. A resize during restore keeps
    # the claim's UID and volume, so compare the request itself.
    jq -e --slurpfile original "$tmp/original.json" '
        .spec.resources.requests.storage == $original[0].spec.resources.requests.storage
    ' "$tmp/current.json" >/dev/null || fail "$claim's storage request changed during restore. Retry restore so the replacement matches it."
    # Delete only the exact object verified here; see the preconditions below.
    jq -e '{apiVersion: "v1", kind: "DeleteOptions",
            preconditions: {uid: (.metadata.uid | select(type == "string" and length > 0)),
                            resourceVersion: (.metadata.resourceVersion | select(type == "string" and length > 0))}}
    ' "$tmp/current.json" > "$tmp/delete-options.json" || fail "$claim has no UID or resourceVersion."
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

steps=5
[[ -z "$expand_to" ]] || steps=6
echo "1/$steps keeping the current volume $pv"
kube patch pv "$pv" -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}' >/dev/null
echo "2/$steps stopping gofer"
stop_attempted=true
kube -n "$namespace" scale statefulset/gofer --replicas=0 >/dev/null
kube -n "$namespace" wait --for=delete pod/gofer-0 --timeout="$wait_timeout"
# Recheck after shutdown in case a previously dispatched/manual backup started.
check_quiet
echo "3/$steps replacing $claim"
restore_phase=deleting_claim
# kubectl delete has no precondition flags; send DeleteOptions to the claim's URL.
# A change after the final check (for example, a resize) then fails the delete.
if ! kube delete --raw "/api/v1/namespaces/$namespace/persistentvolumeclaims/$claim" -f "$tmp/delete-options.json" >/dev/null; then
    # Restart the original app only if the claim was demonstrably not deleted.
    if kube -n "$namespace" get pvc "$claim" -o json > "$tmp/after-delete.json" && \
        jq -e --slurpfile original "$tmp/original.json" '
            .metadata.uid == $original[0].metadata.uid and .metadata.deletionTimestamp == null
        ' "$tmp/after-delete.json" >/dev/null; then
        restore_phase=preflight
        fail "$claim changed after the final check and was not deleted. Retry restore."
    fi
    exit 1
fi
kube -n "$namespace" wait --for=delete "pvc/$claim" --timeout="$wait_timeout"
kube create -f "$tmp/claim.json" >/dev/null
restore_phase=claim_created
# A successful create is the only point at which starting the StatefulSet is safe.
echo "4/$steps starting gofer on the restored volume"
kube -n "$namespace" scale statefulset/gofer --replicas=1 >/dev/null
kube -n "$namespace" rollout status statefulset/gofer --timeout="$rollout_timeout"
restore_phase=rollout_complete
new=$(kube -n "$namespace" get pvc "$claim" -o jsonpath='{.spec.volumeName}')
[[ -n "$new" ]] || fail "the restored PVC has no bound volume"
echo "5/$steps setting the restored volume $new to reclaim policy $policy"
kube patch pv "$new" -p "{\"spec\":{\"persistentVolumeReclaimPolicy\":\"$policy\"}}" >/dev/null
if [[ -n "$expand_to" ]]; then
    echo "6/$steps expanding $claim from the snapshot's $size to $expand_to"
    kube -n "$namespace" patch pvc "$claim" --type=merge \
        -p "{\"spec\":{\"resources\":{\"requests\":{\"storage\":\"$expand_to\"}}}}" >/dev/null
    echo "The CSI resizer completes the expansion; check the claim's status.capacity."
fi
kube -n "$namespace" logs gofer-0 | grep -m1 'persistent storage' || true
echo
echo "Restored $claim from $snapshot. Original volume $pv is retained."
echo "To discard the old volume and its disk:"
printf '  '
printf '%q ' "${kubectl[@]}" patch pv "$pv" -p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}'
printf '\n'
echo "To undo the restore, see 'Going back' in docs/backup-restore.md."
