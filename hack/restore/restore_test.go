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
}

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
	if ctx.Err() != nil {
		t.Fatalf("restore test timed out: %s", out)
	}
	return r
}

func (r result) has(parts ...string) bool {
	return slices.ContainsFunc(r.calls, func(call []string) bool {
		return slices.Equal(call[:min(len(parts), len(call))], parts)
	})
}

func TestRestoreRejectsInvalidInputBeforeMutating(t *testing.T) {
	for _, scenario := range []string{"claim_read_error", "claim_empty", "claim_invalid", "claim_no_access", "snapshot_read_error", "snapshot_unready", "cronjob_read_error"} {
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
	for _, scenario := range []string{"active_job", "pending_job", "failed_snapshot", "pending_snapshot", "source_finalizer", "jobs_read_error", "snapshots_read_error", "pvc_changed", "snapshot_changed", "snapshot_deleting", "suspend_error"} {
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
	for _, scenario := range []string{"pod_timeout", "scale_error", "late_job", "late_snapshot"} {
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
		})
	}
}

func TestRestoreLeavesBackupsPausedAfterDestructiveFailure(t *testing.T) {
	for _, scenario := range []string{"delete_timeout", "create_error", "start_error", "rollout_error"} {
		t.Run(scenario, func(t *testing.T) {
			r := runRestore(t, scenario, true)
			if r.err == nil || !r.has("delete", "pvc", "data-gofer-0", "--wait=true", "--timeout=1s") {
				t.Fatalf("expected a bounded failed restore: %v\n%s", r.calls, r.output)
			}
			if r.has("patch", "cronjob", "gofer-snapshot", "--type=merge", "-p", `{"spec":{"suspend":false}}`) {
				t.Fatal("backups must stay paused while the claim needs recovery")
			}
			if (scenario == "delete_timeout" || scenario == "create_error") && r.has("scale", "statefulset/gofer", "--replicas=1") {
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
	for _, scenario := range []string{"restored_pvc_read_error", "restored_pvc_unbound", "reclaim_policy_error", "reclaim_policy_resume_error", "paused_reclaim_policy_error", "no_cronjob_reclaim_policy_error"} {
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
	for _, scenario := range []string{"normal", "already_suspended", "no_cronjob", "no_restore_size"} {
		t.Run(scenario, func(t *testing.T) {
			r := runRestore(t, scenario, true)
			if r.err != nil {
				t.Fatalf("restore failed: %v\n%s", r.err, r.output)
			}
			if !r.has("wait", "--for=delete", "pod/gofer-0", "--timeout=1s") || !r.has("delete", "pvc", "data-gofer-0", "--wait=true", "--timeout=1s") {
				t.Fatal("pod and PVC deletion must both have bounded waits")
			}
			if !r.has("patch", "pv", "restored-pv") {
				t.Fatal("restored volume must get its reclaim policy")
			}
			resumed := r.has("patch", "cronjob", "gofer-snapshot", "--type=merge", "-p", `{"spec":{"suspend":false}}`)
			if resumed != (scenario != "already_suspended" && scenario != "no_cronjob") {
				t.Fatalf("changed the original backup schedule state: %v", r.calls)
			}
			spec := r.created["spec"].(map[string]any)
			wantSize := "2Gi"
			if scenario == "no_restore_size" {
				wantSize = "4Gi"
			}
			if spec["storageClassName"] != "test-storage-class" || spec["volumeMode"] != "Filesystem" || spec["accessModes"].([]any)[0] != "ReadWriteOnce" || spec["volumeName"] != nil || spec["dataSource"].(map[string]any)["name"] != "backup" || spec["resources"].(map[string]any)["requests"].(map[string]any)["storage"] != wantSize {
				t.Fatalf("incorrect replacement claim: %v", spec)
			}
			meta := r.created["metadata"].(map[string]any)
			if meta["uid"] != nil || meta["name"] != "data-gofer-0" || meta["namespace"] != "gofer" || meta["labels"].(map[string]any)["app"] != "gofer" {
				t.Fatalf("incorrect replacement metadata: %v", meta)
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
		return map[string]any{"metadata": map[string]any{"name": name, "uid": name + "-uid"}, "spec": map[string]any{"source": map[string]any{"persistentVolumeClaimName": claim}}, "status": map[string]any{"readyToUse": ready, "restoreSize": "2Gi"}}
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
		_ = json.Unmarshal([]byte(`{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"data-gofer-0","namespace":"gofer","uid":"pvc-uid","labels":{"app":"gofer"},"finalizers":["kubernetes.io/pvc-protection"]},"spec":{"volumeName":"original-pv","storageClassName":"test-storage-class","accessModes":["ReadWriteOnce"],"volumeMode":"Filesystem","resources":{"requests":{"storage":"4Gi"}}},"status":{"phase":"Bound"}}`), &pvc)
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
	case is("delete", "pvc", "data-gofer-0"):
		if scenario == "delete_timeout" {
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
