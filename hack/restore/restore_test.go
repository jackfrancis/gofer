package restore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type result struct {
	output  string
	err     error
	calls   [][]string
	created map[string]any
	// deleteOptions is the DeleteOptions body sent with the claim deletion.
	deleteOptions map[string]any
}

const claimPath = "/api/v1/namespaces/gofer/persistentvolumeclaims/data-gofer-0"

// Exercise the real make target and shell script. Every kubectl invocation goes
// to this test binary in helper mode; no kubeconfig or live cluster is accessed.
func runRestore(t *testing.T, scenario string, confirm bool) result {
	t.Helper()
	for _, tool := range []string{"make", "bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("restore integration tests need %s: %v", tool, err)
		}
	}
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec \"$MOCK_TEST_BINARY\" -test.run='^TestKubectlHelper$' -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	packageDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "make", "--no-print-directory", "restore", "SHELL=/bin/sh",
		"KUBE_NS=gofer", "DATA_CLAIM=data-gofer-0", "KUBE_CONTEXT=review-fake",
		"KUBECTL="+wrapper+" --context review-fake --kubeconfig 'review kubeconfig'", "SNAPSHOT=backup",
		"DATA_RECLAIM_POLICY=Retain", "RESTORE_TIMEOUT=1s", "ROLLOUT_TIMEOUT=1s")
	if confirm {
		cmd.Args = append(cmd.Args, "CONFIRM=backup")
	} else {
		cmd.Args = append(cmd.Args, "CONFIRM=")
	}
	cmd.Dir = filepath.Clean(filepath.Join(packageDir, "../.."))
	// Keep only the host tool search path. In particular, do not inherit Make
	// overrides/includes, shell startup hooks, or deployment configuration.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + dir, "LC_ALL=C",
		"MOCK_TEST_BINARY=" + exe, "MOCK_DIR=" + dir, "MOCK_SCENARIO=" + scenario,
		"GORACE=atexit_sleep_ms=0",
	}
	out, err := cmd.CombinedOutput()
	r := result{output: string(out), err: err, calls: readCalls(dir)}
	if data, readErr := os.ReadFile(filepath.Join(dir, "created.json")); readErr == nil {
		if err := json.Unmarshal(data, &r.created); err != nil {
			t.Fatal(err)
		}
	}
	if data, readErr := os.ReadFile(filepath.Join(dir, "delete-options.json")); readErr == nil {
		if err := json.Unmarshal(data, &r.deleteOptions); err != nil {
			t.Fatal(err)
		}
	}
	if ctx.Err() != nil {
		t.Fatalf("restore test timed out: %s", out)
	}
	return r
}

func (r result) has(parts ...string) bool {
	return r.index(parts...) >= 0
}

// index returns the position of the first call starting with parts, or -1.
func (r result) index(parts ...string) int {
	return slices.IndexFunc(r.calls, func(call []string) bool {
		return slices.Equal(call[:min(len(parts), len(call))], parts)
	})
}

// expansion is the patch that grows the restored claim back to its old request.
func expansion(size string) []string {
	return []string{"patch", "pvc", "data-gofer-0", "--type=merge", "-p", `{"spec":{"resources":{"requests":{"storage":"` + size + `"}}}}`}
}

// verifiedDelete reports whether the claim deletion was conditioned on the UID and
// resourceVersion of the last read.
func (r result) verifiedDelete(resourceVersion string) bool {
	pre, _ := r.deleteOptions["preconditions"].(map[string]any)
	return r.has("delete", "--raw", claimPath, "-f") && r.deleteOptions["kind"] == "DeleteOptions" &&
		pre["uid"] == "pvc-uid" && pre["resourceVersion"] == resourceVersion
}

func TestRestoreRejectsInvalidInputBeforeMutating(t *testing.T) {
	for _, scenario := range []string{"claim_read_error", "claim_empty", "claim_invalid", "claim_no_access", "snapshot_read_error", "snapshot_unready", "invalid_restore_size", "cronjob_read_error", "no_expansion", "storageclass_read_error",
		"sizes=4Gi,1.2.3", "sizes=4Gi,4GB", "sizes=4Gi,4Gi ", "sizes=4Gi,e3", "sizes=4Gi,4Ei1", "sizes=bogus,2Gi", "sizes=0,2Gi"} {
		t.Run(scenario, func(t *testing.T) {
			r := runRestore(t, scenario, true)
			if r.err == nil {
				t.Fatalf("restore succeeded: %s", r.output)
			}
			for _, verb := range []string{"patch", "scale", "delete", "create"} {
				if r.has(verb) {
					t.Errorf("unexpected %s after failed preflight: %v\n%s", verb, r.calls, r.output)
				}
			}
			if scenario == "no_expansion" && !strings.Contains(r.output, "does not allow volume expansion") {
				t.Errorf("must explain why a smaller snapshot cannot be restored: %s", r.output)
			}
		})
	}
}

func TestRestoreNeedsConfirmation(t *testing.T) {
	r := runRestore(t, "normal", false)
	if r.err == nil || !strings.Contains(r.output, "Nothing changed") {
		t.Fatalf("expected the read-only plan: %s", r.output)
	}
	for _, verb := range []string{"patch", "scale", "delete", "create"} {
		if r.has(verb) {
			t.Fatalf("unconfirmed restore mutated the cluster: %v", r.calls)
		}
	}
}

func TestRestoreRejectsUnfinishedBackupsBeforeStopping(t *testing.T) {
	for _, scenario := range []string{"active_job", "pending_job", "failed_snapshot", "pending_snapshot", "source_finalizer", "jobs_read_error", "snapshots_read_error", "pvc_changed", "early_resize", "snapshot_changed", "snapshot_deleting", "suspend_error"} {
		t.Run(scenario, func(t *testing.T) {
			r := runRestore(t, scenario, true)
			if r.err == nil || r.has("scale") || r.has("delete") || r.has("create") || r.has("patch", "pv") {
				t.Fatalf("preflight must fail without touching app/storage: %v\n%s", r.calls, r.output)
			}
			if !r.has("patch", "cronjob", "gofer-snapshot", "--type=merge", "-p", `{"spec":{"suspend":false}}`) {
				t.Fatalf("must restore the original schedule on preflight failure: %v", r.calls)
			}
		})
	}
}

func TestRestoreAbortsBeforeDeletionAndRestartsOriginalApp(t *testing.T) {
	for _, scenario := range []string{"pod_timeout", "scale_error", "late_job", "late_snapshot", "late_resize"} {
		t.Run(scenario, func(t *testing.T) {
			r := runRestore(t, scenario, true)
			if r.err == nil || r.has("delete") || r.has("create") {
				t.Fatalf("must not delete PVC after a shutdown/recheck failure: %v\n%s", r.calls, r.output)
			}
			if !r.has("scale", "statefulset/gofer", "--replicas=1") || !strings.Contains(r.output, "claim was not deleted") {
				t.Fatalf("must restart the original app: %s", r.output)
			}
			if !r.has("patch", "cronjob", "gofer-snapshot", "--type=merge", "-p", `{"spec":{"suspend":false}}`) {
				t.Fatal("backup schedule was not restored")
			}
			if scenario == "late_resize" && !strings.Contains(r.output, "storage request changed during restore") {
				t.Fatalf("must report the resize: %s", r.output)
			}
		})
	}
}

// A resize between the final read and the DELETE changes the claim's
// resourceVersion; the precondition rejects the deletion and the original app
// restarts on its unchanged claim.
func TestRestoreDeletesOnlyTheVerifiedClaim(t *testing.T) {
	r := runRestore(t, "delete_conflict", true)
	if r.err == nil || r.has("create") || !r.verifiedDelete("100") {
		t.Fatalf("must condition deletion on the last verified claim and stop when it fails: %v %v\n%s", r.deleteOptions, r.calls, r.output)
	}
	if !r.has("scale", "statefulset/gofer", "--replicas=1") || !strings.Contains(r.output, "was not deleted") {
		t.Fatalf("must restart the original app on its undeleted claim: %s", r.output)
	}
	if !r.has("patch", "cronjob", "gofer-snapshot", "--type=merge", "-p", `{"spec":{"suspend":false}}`) {
		t.Fatal("backup schedule was not restored")
	}
}

func TestRestoreLeavesBackupsPausedAfterDestructiveFailure(t *testing.T) {
	for _, scenario := range []string{"delete_error", "delete_timeout", "create_error", "start_error", "rollout_error"} {
		t.Run(scenario, func(t *testing.T) {
			r := runRestore(t, scenario, true)
			if r.err == nil || !r.verifiedDelete("100") || (scenario != "delete_error" && !r.has("wait", "--for=delete", "pvc/data-gofer-0", "--timeout=1s")) {
				t.Fatalf("expected a bounded failed restore: %v\n%s", r.calls, r.output)
			}
			if r.has("patch", "cronjob", "gofer-snapshot", "--type=merge", "-p", `{"spec":{"suspend":false}}`) {
				t.Fatal("backups must stay paused while the claim needs recovery")
			}
			if (scenario == "delete_error" || scenario == "delete_timeout" || scenario == "create_error") && r.has("scale", "statefulset/gofer", "--replicas=1") {
				t.Fatal("must not start gofer without a successfully created replacement claim")
			}
			if !strings.Contains(r.output, "Original volume original-pv is retained") || !strings.Contains(r.output, "Scheduled backups remain suspended") {
				t.Fatalf("missing recovery instructions: %s", r.output)
			}
			phaseMessage := "replacement creation was not confirmed"
			if scenario == "start_error" || scenario == "rollout_error" {
				phaseMessage = "replacement claim gofer/data-gofer-0 was created, but gofer's rollout was not confirmed"
			}
			if !strings.Contains(r.output, phaseMessage) || strings.Contains(r.output, "post-restore finalization is incomplete") {
				t.Fatalf("incorrect restore phase reported: %s", r.output)
			}
		})
	}
}

func TestRestoreReportsPostRolloutFinalizationFailures(t *testing.T) {
	for _, scenario := range []string{"restored_pvc_read_error", "restored_pvc_unbound", "reclaim_policy_error", "reclaim_policy_resume_error", "paused_reclaim_policy_error", "no_cronjob_reclaim_policy_error", "expand_error"} {
		t.Run(scenario, func(t *testing.T) {
			r := runRestore(t, scenario, true)
			if r.err == nil || r.created == nil || !r.has("rollout", "status", "statefulset/gofer", "--timeout=1s") {
				t.Fatalf("expected failure after a confirmed restored rollout: %v\n%s", r.calls, r.output)
			}
			if !strings.Contains(r.output, "restored rollout succeeded, but post-restore finalization is incomplete") || !strings.Contains(r.output, "requested reclaim policy (Retain)") {
				t.Fatalf("missing finalization guidance: %s", r.output)
			}
			for _, misleading := range []string{"before restarting gofer", "Going back", "Scheduled backups remain suspended"} {
				if strings.Contains(r.output, misleading) {
					t.Errorf("unexpected failed-replacement guidance %q: %s", misleading, r.output)
				}
			}
			var scales, deletes int
			for _, call := range r.calls {
				if call[0] == "scale" {
					scales++
				}
				if call[0] == "delete" {
					deletes++
				}
			}
			if scales != 2 || deletes != 1 {
				t.Fatalf("cleanup must not restart or roll back the restored app: %v", r.calls)
			}
			shouldResume := !strings.HasPrefix(scenario, "paused_") && !strings.HasPrefix(scenario, "no_cronjob_")
			resumeAttempted := r.has("patch", "cronjob", "gofer-snapshot", "--type=merge", "-p", `{"spec":{"suspend":false}}`)
			if resumeAttempted != shouldResume {
				t.Fatalf("did not preserve the original schedule state: %v", r.calls)
			}
			if !shouldResume && r.has("patch", "cronjob") {
				t.Fatalf("must not change an absent or already-paused CronJob: %v", r.calls)
			}
			if !strings.Contains(r.output, "verify that gofer/data-gofer-0 requests 4Gi (it was restored at 2Gi)") {
				t.Fatalf("missing expansion guidance: %s", r.output)
			}
			if scenario == "reclaim_policy_resume_error" && !strings.Contains(r.output, "could not resume scheduled backups") {
				t.Fatalf("must report both finalization and schedule-resume failures: %s", r.output)
			}
		})
	}
}

func TestRestoreIgnoresCallerConfiguration(t *testing.T) {
	dir := t.TempDir()
	makefile := filepath.Join(dir, "caller.mk")
	startup := filepath.Join(dir, "caller.sh")
	if err := os.WriteFile(makefile, []byte("$(error caller Makefile must not be loaded)\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(startup, []byte("echo 'caller shell startup must not be loaded' >&2\nexit 99\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, env := range map[string]map[string]string{
		"deployment": {
			"KUBE_NS": "caller-namespace", "DATA_CLAIM": "caller-claim", "KUBE_CONTEXT": "caller-context",
			"KUBECONFIG": "caller-missing-kubeconfig", "KUBECTL": "caller-kubectl",
			"SNAPSHOT": "caller-snapshot", "CONFIRM": "caller-confirmation",
			"DATA_RECLAIM_POLICY": "invalid", "RESTORE_TIMEOUT": "invalid", "ROLLOUT_TIMEOUT": "invalid",
		},
		"make-and-shell": {
			"MAKEFILES": makefile, "MAKEFLAGS": "-e", "MFLAGS": "-n", "MAKEOVERRIDES": "KUBE_NS=caller-namespace",
			"SHELL": "/caller-missing-shell", "BASH_ENV": startup, "ENV": startup,
		},
	} {
		t.Run(name, func(t *testing.T) {
			for key, value := range env {
				t.Setenv(key, value)
			}
			r := runRestore(t, "normal", true)
			if r.err != nil {
				t.Fatalf("caller configuration affected restore: %v\n%s", r.err, r.output)
			}
			if !r.has("patch", "pv", "restored-pv", "-p", `{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}`) || !r.has("rollout", "status", "statefulset/gofer", "--timeout=1s") {
				t.Fatalf("test configuration was not pinned: %v", r.calls)
			}
		})
	}
}

func TestRestoreSuccess(t *testing.T) {
	// The fake claim requests 4Gi; the fake snapshot's restoreSize is 2Gi unless a
	// scenario changes it ("sizes=<claim>,<restoreSize>" sets both). The replacement
	// requests restoreSize, which the CSI driver can always provision, and is then
	// expanded to the claim's request if that is larger. Without a usable
	// restoreSize, it requests the claim's size.
	type sizes struct{ create, expand string }
	want := map[string]sizes{
		"normal":              {"2Gi", "4Gi"}, // the claim was expanded after the snapshot
		"already_suspended":   {"2Gi", "4Gi"},
		"no_cronjob":          {"2Gi", "4Gi"},
		"no_restore_size":     {"4Gi", ""},
		"zero_restore_size":   {"4Gi", ""},        // some drivers report 0
		"larger_restore_size": {"8Gi", ""},        // the snapshot needs more than the claim asks
		"byte_restore_size":   {"5000000000", ""}, // plain bytes, larger than 4Gi (4294967296)
		"equal_restore_size":  {"4Gi", ""},        // 4294967296 bytes == 4Gi: keep the claim's form
		// Kubernetes quantity spellings, preserved as written.
		"sizes=+4Gi,2Gi":           {"2Gi", "+4Gi"},
		"sizes=4.Gi,2Gi":           {"2Gi", "4.Gi"},
		"sizes=4Gi,+2Gi":           {"+2Gi", "4Gi"},
		"sizes=4Gi,2.Gi":           {"2.Gi", "4Gi"},
		"sizes=4Gi,.5Gi":           {".5Gi", "4Gi"},
		"sizes=4Gi,4.Gi":           {"4Gi", ""}, // equal
		"sizes=4Gi,4096Mi":         {"4Gi", ""},
		"sizes=4Gi,4e9":            {"4e9", "4Gi"},
		"sizes=4Gi,4E+9":           {"4E+9", "4Gi"},
		"sizes=5e9,4294967296e0":   {"4294967296e0", "5e9"},
		"sizes=2E,1Ei":             {"1Ei", "2E"}, // 1Ei (1.15e18) < 2E (2e18): E and Ei are suffixes
		"sizes=1Ei,1E":             {"1E", "1Ei"},
		"sizes=1Ei,1.5E":           {"1.5E", ""}, // the snapshot is larger
		"sizes=4000M,3.5G":         {"3.5G", "4000M"},
		"sizes=4Gi,4194304k":       {"4194304k", "4Gi"},
		"sizes=4Gi,4294967296000m": {"4Gi", ""},
	}
	for scenario, w := range want {
		t.Run(scenario, func(t *testing.T) {
			r := runRestore(t, scenario, true)
			if r.err != nil {
				t.Fatalf("restore failed: %v\n%s", r.err, r.output)
			}
			if !r.has("wait", "--for=delete", "pod/gofer-0", "--timeout=1s") || !r.has("wait", "--for=delete", "pvc/data-gofer-0", "--timeout=1s") {
				t.Fatal("pod and PVC deletion must both have bounded waits")
			}
			if !r.verifiedDelete("100") {
				t.Fatalf("deletion must be conditioned on the last verified claim: %v %v", r.deleteOptions, r.calls)
			}
			reclaim := r.index("patch", "pv", "restored-pv")
			if reclaim < 0 {
				t.Fatal("restored volume must get its reclaim policy")
			}
			resumed := r.has("patch", "cronjob", "gofer-snapshot", "--type=merge", "-p", `{"spec":{"suspend":false}}`)
			if resumed != (scenario != "already_suspended" && scenario != "no_cronjob") {
				t.Fatalf("changed the original backup schedule state: %v", r.calls)
			}
			spec := r.created["spec"].(map[string]any)
			if spec["storageClassName"] != "test-storage-class" || spec["volumeMode"] != "Filesystem" || spec["accessModes"].([]any)[0] != "ReadWriteOnce" || spec["volumeName"] != nil || spec["dataSource"].(map[string]any)["name"] != "backup" || spec["resources"].(map[string]any)["requests"].(map[string]any)["storage"] != w.create {
				t.Fatalf("incorrect replacement claim, want %s: %v", w.create, spec)
			}
			meta := r.created["metadata"].(map[string]any)
			if meta["uid"] != nil || meta["name"] != "data-gofer-0" || meta["namespace"] != "gofer" || meta["labels"].(map[string]any)["app"] != "gofer" {
				t.Fatalf("incorrect replacement metadata: %v", meta)
			}
			expanded := r.index(expansion(w.expand)...)
			if w.expand == "" {
				if r.has("patch", "pvc") || r.has("get", "storageclass") {
					t.Fatalf("must not expand a claim restored at its full size: %v", r.calls)
				}
				return
			}
			if !r.has("get", "storageclass", "test-storage-class", "-o", "json") {
				t.Fatalf("must check that the claim can be expanded before restoring: %v", r.calls)
			}
			if expanded < reclaim || expanded < r.index("rollout", "status") {
				t.Fatalf("must expand the bound, protected claim to %s after rollout: %v", w.expand, r.calls)
			}
		})
	}
}

func TestRestorePreservesPausedScheduleOnFailure(t *testing.T) {
	r := runRestore(t, "paused_failed_snapshot", true)
	if r.err == nil || r.has("patch", "cronjob") || r.has("scale") || r.has("delete") {
		t.Fatalf("must leave the paused schedule and app untouched: %v\n%s", r.calls, r.output)
	}
}

func TestRestoreReportsFailureToResumeBackups(t *testing.T) {
	r := runRestore(t, "resume_error", true)
	if r.err == nil || r.created == nil || !strings.Contains(r.output, "could not resume scheduled backups") {
		t.Fatalf("must report the restored volume separately from the failed schedule resume: %s", r.output)
	}
}

func readCalls(dir string) [][]string {
	data, _ := os.ReadFile(filepath.Join(dir, "calls.jsonl"))
	var calls [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var call []string
		if json.Unmarshal([]byte(line), &call) == nil {
			calls = append(calls, call)
		}
	}
	return calls
}

// TestKubectlHelper is a strict fake: unknown commands fail rather than falling
// through to the real kubectl. Normal test runs never enter helper mode.
func TestKubectlHelper(t *testing.T) {
	dir := os.Getenv("MOCK_DIR")
	separator := slices.Index(os.Args, "--")
	if dir == "" || separator < 0 {
		return
	}
	args := os.Args[separator+1:]
	if len(args) < 4 || !slices.Equal(args[:4], []string{"--context", "review-fake", "--kubeconfig", "review kubeconfig"}) {
		fmt.Fprintln(os.Stderr, "lost cluster-selection arguments", args)
		os.Exit(1)
	}
	args = args[4:]
	if len(args) >= 2 && args[0] == "-n" {
		if args[1] != "gofer" {
			os.Exit(1)
		}
		args = args[2:]
	}
	log, err := os.OpenFile(filepath.Join(dir, "calls.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(1)
	}
	_ = json.NewEncoder(log).Encode(args)
	_ = log.Close()
	calls := readCalls(dir)
	scenario := os.Getenv("MOCK_SCENARIO")
	paused := strings.HasPrefix(scenario, "paused_") || scenario == "already_suspended"
	noCronjob := strings.HasPrefix(scenario, "no_cronjob_") || scenario == "no_cronjob"
	scenario = strings.TrimPrefix(strings.TrimPrefix(scenario, "paused_"), "no_cronjob_")
	claimSize, restoreSize := "4Gi", "2Gi"
	if spec, ok := strings.CutPrefix(scenario, "sizes="); ok {
		claimSize, restoreSize, _ = strings.Cut(spec, ",")
	}
	is := func(parts ...string) bool { return slices.Equal(args[:min(len(parts), len(args))], parts) }
	count := func(parts ...string) int {
		n := 0
		for _, call := range calls {
			if slices.Equal(call[:min(len(parts), len(call))], parts) {
				n++
			}
		}
		return n
	}
	emit := func(v any) { _ = json.NewEncoder(os.Stdout).Encode(v) }
	fail := func() { fmt.Fprintln(os.Stderr, "injected failure:", scenario); os.Exit(1) }
	snap := func(name, claim string, ready bool) map[string]any {
		return map[string]any{"metadata": map[string]any{"name": name, "uid": name + "-uid"}, "spec": map[string]any{"source": map[string]any{"persistentVolumeClaimName": claim}}, "status": map[string]any{"readyToUse": ready, "restoreSize": restoreSize}}
	}
	switch {
	case is("config", "current-context"):
		fmt.Println("review-fake")
	case is("config", "view"):
		fmt.Println("https://review-fake.invalid")
	case is("get", "pvc", "data-gofer-0", "-o", "json"):
		n := count("get", "pvc", "data-gofer-0", "-o", "json")
		if scenario == "claim_read_error" {
			fail()
		}
		if scenario == "claim_empty" {
			os.Exit(0)
		}
		if scenario == "claim_invalid" {
			fmt.Println("null")
			os.Exit(0)
		}
		var pvc map[string]any
		_ = json.Unmarshal([]byte(`{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"data-gofer-0","namespace":"gofer","uid":"pvc-uid","resourceVersion":"100","labels":{"app":"gofer"},"finalizers":["kubernetes.io/pvc-protection"]},"spec":{"volumeName":"original-pv","storageClassName":"test-storage-class","accessModes":["ReadWriteOnce"],"volumeMode":"Filesystem","resources":{"requests":{"storage":"4Gi"}}},"status":{"phase":"Bound"}}`), &pvc)
		pvc["spec"].(map[string]any)["resources"].(map[string]any)["requests"].(map[string]any)["storage"] = claimSize
		if scenario == "early_resize" && n >= 2 || scenario == "late_resize" && n >= 3 {
			// A resize keeps the claim's UID and volume.
			pvc["metadata"].(map[string]any)["resourceVersion"] = "101"
			pvc["spec"].(map[string]any)["resources"].(map[string]any)["requests"].(map[string]any)["storage"] = "8Gi"
		}
		if scenario == "delete_error" && count("delete") > 0 {
			fmt.Fprintln(os.Stderr, "not found")
			os.Exit(1)
		}
		if scenario == "claim_no_access" {
			delete(pvc["spec"].(map[string]any), "accessModes")
		}
		if scenario == "source_finalizer" {
			pvc["metadata"].(map[string]any)["finalizers"] = []string{"snapshot.storage.kubernetes.io/pvc-as-source-protection"}
		}
		if scenario == "pvc_changed" && n >= 2 {
			pvc["metadata"].(map[string]any)["uid"] = "replacement-uid"
		}
		emit(pvc)
	case is("get", "pvc", "data-gofer-0", "-o"):
		if _, err := os.Stat(filepath.Join(dir, "created.json")); err == nil {
			if scenario == "restored_pvc_read_error" {
				fail()
			}
			if scenario == "restored_pvc_unbound" {
				os.Exit(0)
			}
			fmt.Println("restored-pv")
		} else {
			fmt.Println("original-pv")
		}
	case is("get", "pvc", "data-gofer-0"):
		fmt.Println("data-gofer-0 Bound")
	case is("get", "volumesnapshot", "backup", "-o", "json"):
		if scenario == "snapshot_read_error" {
			fail()
		}
		v := snap("backup", "data-gofer-0", scenario != "snapshot_unready")
		if scenario == "no_restore_size" {
			delete(v["status"].(map[string]any), "restoreSize")
		}
		if size, ok := map[string]string{
			"larger_restore_size": "8Gi", "byte_restore_size": "5000000000",
			"equal_restore_size": "4294967296", "zero_restore_size": "0", "invalid_restore_size": "bogus",
		}[scenario]; ok {
			v["status"].(map[string]any)["restoreSize"] = size
		}
		if count("get", "volumesnapshot", "backup", "-o", "json") >= 2 {
			if scenario == "snapshot_changed" {
				v["metadata"].(map[string]any)["uid"] = "other-uid"
			}
			if scenario == "snapshot_deleting" {
				v["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-17T00:00:00Z"
			}
		}
		emit(v)
	case is("get", "volumesnapshot", "backup", "-o"):
		if strings.Contains(args[len(args)-1], "readyToUse") {
			fmt.Println("true")
		} else {
			fmt.Println("2026-11-16T00:00:00Z")
		}
	case is("get", "storageclass", "test-storage-class", "-o", "json"):
		if scenario == "storageclass_read_error" {
			fail()
		}
		emit(map[string]any{"metadata": map[string]any{"name": "test-storage-class"}, "allowVolumeExpansion": scenario != "no_expansion"})
	case is("get", "statefulset", "gofer"):
		fmt.Println("1")
	case is("get", "cronjob", "gofer-snapshot"):
		if scenario == "cronjob_read_error" {
			fail()
		}
		if !noCronjob {
			emit(map[string]any{"spec": map[string]any{"suspend": paused}})
		}
	case is("get", "jobs"):
		if scenario == "jobs_read_error" {
			fail()
		}
		job := func(name, sa, condition string) map[string]any {
			status := map[string]any{}
			if condition != "" {
				status["conditions"] = []any{map[string]any{"type": condition, "status": "True"}}
			}
			return map[string]any{"metadata": map[string]any{"name": name}, "spec": map[string]any{"template": map[string]any{"spec": map[string]any{"serviceAccountName": sa}}}, "status": status}
		}
		items := []any{job("old-success", "gofer-snapshotter", "Complete"), job("old-failure", "gofer-snapshotter", "Failed"), job("unrelated", "other", "")}
		if scenario == "pending_job" || scenario == "active_job" || (scenario == "late_job" && count("get", "jobs") >= 2) {
			v := job("unfinished-backup", "gofer-snapshotter", "")
			if scenario == "active_job" {
				v["status"].(map[string]any)["active"] = 1
			}
			items = append(items, v)
		}
		emit(map[string]any{"items": items})
	case is("get", "volumesnapshots"):
		if scenario == "snapshots_read_error" {
			fail()
		}
		items := []any{snap("backup", "data-gofer-0", true), snap("other", "other-pvc", false)}
		if scenario == "failed_snapshot" || scenario == "pending_snapshot" || (scenario == "late_snapshot" && count("get", "volumesnapshots") >= 2) {
			v := snap("unfinished-snapshot", "data-gofer-0", false)
			if scenario == "failed_snapshot" {
				v["status"].(map[string]any)["error"] = map[string]any{"message": "quota exceeded"}
			}
			if scenario == "pending_snapshot" {
				delete(v, "status")
			}
			items = append(items, v)
		}
		emit(map[string]any{"items": items})
	case is("patch", "cronjob"):
		if (scenario == "resume_error" || scenario == "reclaim_policy_resume_error") && strings.Contains(args[len(args)-1], "false") {
			fail()
		}
		if scenario == "suspend_error" && strings.Contains(args[len(args)-1], "true") {
			fail()
		}
	case is("patch", "pv"):
		if (scenario == "reclaim_policy_error" || scenario == "reclaim_policy_resume_error") && args[2] == "restored-pv" {
			fail()
		}
	case is("scale", "statefulset/gofer"):
		if scenario == "start_error" && slices.Contains(args, "--replicas=1") {
			fail()
		}
		if scenario == "scale_error" && slices.Contains(args, "--replicas=0") {
			fail()
		}
	case is("wait", "--for=delete", "pod/gofer-0"):
		if scenario == "pod_timeout" {
			fail()
		}
	case is("delete", "--raw", "/api/v1/namespaces/gofer/persistentvolumeclaims/data-gofer-0", "-f") && len(args) == 5:
		data, err := os.ReadFile(args[4])
		if err != nil || !json.Valid(data) {
			fail()
		}
		if err := os.WriteFile(filepath.Join(dir, "delete-options.json"), data, 0600); err != nil {
			fail()
		}
		if scenario == "delete_conflict" || scenario == "delete_error" {
			fail()
		}
	case is("wait", "--for=delete", "pvc/data-gofer-0"):
		if scenario == "delete_timeout" {
			fail()
		}
	case is("patch", "pvc", "data-gofer-0"):
		if scenario == "expand_error" {
			fail()
		}
	case is("create", "-f"):
		if scenario == "create_error" {
			fail()
		}
		data, err := os.ReadFile(args[2])
		if err != nil || !json.Valid(data) {
			fail()
		}
		if err := os.WriteFile(filepath.Join(dir, "created.json"), data, 0600); err != nil {
			fail()
		}
	case is("rollout", "status"):
		if scenario == "rollout_error" {
			fail()
		}
	case is("logs", "gofer-0"):
		fmt.Println("persistent storage enabled")
	default:
		fmt.Fprintln(os.Stderr, "unexpected kubectl call", args)
		os.Exit(1)
	}
	os.Exit(0)
}
