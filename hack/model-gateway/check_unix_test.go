//go:build unix

package modelgateway

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// curlStub answers the check's probes as a gateway refusing callers without a run
// credential (401). On the last probe it records that the probes are done, then waits
// until the port-forward has exited or become a zombie, for MOCK_PF=exit-after-probes.
const curlStub = `#!/usr/bin/env bash
case "$*" in *http_code*)
	n=$(cat "$MOCK_STATE/probes" 2>/dev/null || echo 0); n=$((n + 1)); echo $n >"$MOCK_STATE/probes"
	if [ $n -ge 2 ] && [ "${MOCK_PF:-}" = exit-after-probes ]; then
		touch "$MOCK_STATE/probes-done"
		pid=$(cat "$MOCK_STATE/port-forward.pid")
		for _ in $(seq 1 200); do st=$(ps -o stat= -p "$pid" 2>/dev/null); case $st in ""|Z*) break;; esac; sleep 0.02; done
	fi
	printf 401 ;;
esac
exit 0
`

// runCheck runs the deployed-profile check against an installed fake cluster with
// curlStub on PATH.
func runCheck(t *testing.T, env ...string) (*cluster, error, string) {
	t.Helper()
	c, _ := install(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(curlStub), 0o755); err != nil {
		t.Fatal(err)
	}
	err, out := c.run("check", append([]string{"PATH=" + bin + ":" + c.shims + ":" + os.Getenv("PATH")}, env...)...)
	return c, err, out
}

// portForwardPID reads the pid the fake port-forward recorded, refusing anything that
// is not a single process: kill(2) with 0 or a negative pid signals a process group.
func portForwardPID(t *testing.T, c *cluster) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(c.state, "port-forward.pid"))
	if err != nil {
		t.Fatalf("the check started no port-forward: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid < 2 {
		t.Fatalf("the fake port-forward recorded no usable pid: %q", b)
	}
	return pid
}

// A check that passes exits 0, quietly, and stops the port-forward it started (not just
// the shell around it).
func TestCheckExitsZeroWhenItPasses(t *testing.T) {
	c, err, out := runCheck(t)
	if err != nil || !strings.Contains(out, "OK:") {
		t.Fatalf("check = %v, want exit 0 after OK:\n%s", err, out)
	}
	if strings.Contains(out, "unbound variable") || strings.Contains(out, "Terminated") {
		t.Fatalf("the EXIT trap failed or was noisy:\n%s", out)
	}
	pid := portForwardPID(t, c)
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the port-forward (pid %d) was still running after the check exited", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A port-forward that is already gone when the check cleans up (it exited after the
// last probe) does not turn a pass into a failure.
func TestCheckPassesWhenThePortForwardExitsFirst(t *testing.T) {
	_, err, out := runCheck(t, "MOCK_PF=exit-after-probes")
	if err != nil || !strings.Contains(out, "OK:") {
		t.Fatalf("check = %v, want exit 0 after OK:\n%s", err, out)
	}
}

// If the check's own port-forward cannot bind, the probes would reach whatever holds
// the port (a stale forward to another cluster's gateway would also answer 401), so
// the check fails without probing.
func TestCheckFailsWhenItsPortForwardCannotBind(t *testing.T) {
	c, err, out := runCheck(t, "MOCK_PF=fail")
	if err == nil || strings.Contains(out, "OK:") {
		t.Fatalf("check = %v, want a failure when its port-forward cannot bind:\n%s", err, out)
	}
	if !strings.Contains(out, "unable to listen") {
		t.Errorf("the failure does not say why:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(c.state, "probes")); err == nil {
		t.Fatal("the check probed through a port-forward that never bound")
	}
}
