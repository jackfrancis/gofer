package aeicheck_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeKubectl records every call and answers the aei-controller Service lookup the
// way the scenario says. Every other call succeeds with no output, except the few the
// deploy path reads.
const fakeKubectl = `#!/bin/sh
printf '%s\n' "$*" >> "$MOCK_DIR/calls"
case "$*" in
*"-n aei-system get service aei-controller"*)
	[ -z "$MOCK_SLOW" ] || sleep 1
	case "$MOCK_SCENARIO" in
	ok) printf 'http 8080 TCP\ndispatch 8081 TCP\n' ;;
	old) printf 'http 8080 TCP\n' ;;
	wrongport) printf 'http 8080 TCP\ndispatch 8443 TCP\n' ;;
	udp) printf 'http 8080 TCP\ndispatch 8081 UDP\n' ;;
	forbidden)
		echo 'Error from server (Forbidden): services "aei-controller" is forbidden: User "deployer" cannot get resource "services" in API group "" in the namespace "aei-system"' >&2
		exit 1 ;;
	notfound)
		echo 'Error from server (NotFound): services "aei-controller" not found' >&2
		exit 1 ;;
	esac ;;
*"get secret gofer-secrets"*) printf 'eA==' ;;
*"get secret aei-signing"*) printf 'eA==' ;;
*"config current-context"*) echo review-fake ;;
*"get cronjob gofer-snapshot"*) exit 1 ;;
*"get deployment gofer-model-gateway"*) ;; # not installed (--ignore-not-found: no output)
esac
exit 0
`

type result struct {
	output string
	err    error
	calls  []string
}

// index returns the position of the first call containing part, or -1.
func (r result) index(part string) int {
	for i, c := range r.calls {
		if strings.Contains(c, part) {
			return i
		}
	}
	return -1
}

// run drives the real make target with kubectl replaced by fakeKubectl; no kubeconfig
// or live cluster is touched. The fake is found through PATH, so the recipe shell
// never word-splits its path (a TMPDIR with a space in it).
func run(t *testing.T, scenario string, env []string, args ...string) result {
	t.Helper()
	for _, tool := range []string{"make", "bash", "awk", "sed"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("aei-check tests need %s: %v", tool, err)
		}
	}
	dir := t.TempDir()
	kubectl := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(kubectl, []byte(fakeKubectl), 0o700); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "make", append([]string{"--no-print-directory", "SHELL=/bin/sh",
		"KUBE_NS=gofer", "KUBE_CONTEXT=review-fake", "KUBECTL=kubectl --context review-fake",
		"ROLLOUT_TIMEOUT=1s"}, args...)...)
	cmd.Dir = filepath.Clean(filepath.Join(wd, "../.."))
	// Keep only the tool search path: no Make overrides, shell hooks or deployment
	// configuration from the caller.
	cmd.Env = append([]string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + dir, "LC_ALL=C",
		"MOCK_DIR=" + dir, "MOCK_SCENARIO=" + scenario,
	}, env...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("make timed out: %s", out)
	}
	r := result{output: string(out), err: err}
	if b, readErr := os.ReadFile(filepath.Join(dir, "calls")); readErr == nil {
		r.calls = strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	return r
}

// The check passes only when the Service serves a port named dispatch on the TCP port
// gofer-config dials (8081), and says why otherwise.
func TestAEICheckRequiresTheDispatchListener(t *testing.T) {
	for _, tc := range []struct {
		scenario string
		ok       bool
		want     []string
	}{
		{scenario: "ok", ok: true},
		{scenario: "old", want: []string{"TCP 8081", "http 8080 TCP", "Upgrade AEI"}},
		{scenario: "wrongport", want: []string{"TCP 8081", "dispatch 8443 TCP", "Upgrade AEI"}},
		{scenario: "udp", want: []string{"TCP 8081", "dispatch 8081 UDP", "Upgrade AEI"}},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			r := run(t, tc.scenario, nil, "aei-check")
			if tc.ok != (r.err == nil) {
				t.Fatalf("aei-check ok = %v, want %v:\n%s", r.err == nil, tc.ok, r.output)
			}
			for _, w := range tc.want {
				if !strings.Contains(r.output, w) {
					t.Errorf("output does not mention %q:\n%s", w, r.output)
				}
			}
		})
	}
}

// A failed lookup is reported as itself, not as an old AEI: a deploy identity without
// the new cross-namespace grant sees the API server's Forbidden and the permission it
// needs.
func TestAEICheckReportsTheLookupError(t *testing.T) {
	for scenario, server := range map[string]string{
		"forbidden": `services "aei-controller" is forbidden`,
		"notfound":  `services "aei-controller" not found`,
	} {
		t.Run(scenario, func(t *testing.T) {
			r := run(t, scenario, nil, "aei-check")
			if r.err == nil {
				t.Fatalf("aei-check passed:\n%s", r.output)
			}
			for _, w := range []string{server, "'get' on services/aei-controller in aei-system"} {
				if !strings.Contains(r.output, w) {
					t.Errorf("output does not mention %q:\n%s", w, r.output)
				}
			}
			if strings.Contains(r.output, "Upgrade AEI") {
				t.Errorf("a failed lookup is reported as an old AEI:\n%s", r.output)
			}
		})
	}
}

// Under make -j the checks run side by side, but nothing is applied and the pod is not
// restarted until they have all passed: a deploy onto an AEI without the dispatch
// listener must leave the running pod alone. The lookup is slowed so an apply that
// did not wait for it would be seen.
func TestDeployAppliesNothingUntilTheChecksPass(t *testing.T) {
	slow := []string{"MOCK_SLOW=1"}
	r := run(t, "old", slow, "-j4", "deploy")
	if r.err == nil {
		t.Fatalf("deploy passed against an AEI without the dispatch listener:\n%s", r.output)
	}
	for _, c := range r.calls {
		if strings.Contains(c, " apply ") || strings.Contains(c, "rollout") {
			t.Errorf("deploy ran %q although aei-check failed", c)
		}
	}

	r = run(t, "ok", slow, "-j4", "deploy")
	if r.err != nil {
		t.Fatalf("deploy: %v\n%s", r.err, r.output)
	}
	lookup, apply := r.index("get service aei-controller"), r.index(" apply ")
	if lookup < 0 || apply < 0 || apply < lookup {
		t.Errorf("apply (call %d) did not follow the dispatch check (call %d):\n%s", apply, lookup, strings.Join(r.calls, "\n"))
	}
}

// writes reports the calls that change the cluster.
func (r result) writes() []string {
	var w []string
	for _, c := range r.calls {
		for _, verb := range []string{" apply ", " create ", " patch ", " delete ", "rollout"} {
			if strings.Contains(" "+c+" ", verb) {
				w = append(w, c)
				break
			}
		}
	}
	return w
}

// dev-up, too, writes nothing to the cluster (namespace, gofer-secrets with the verify
// key and any supplied GitHub credential, the manifests) until aei-check has passed,
// also under make -j. The kind and image steps are skipped with -o; they never touch
// gofer's objects.
func TestDevUpWritesNothingUntilTheChecksPass(t *testing.T) {
	env := []string{"MOCK_SLOW=1", "MINT_PUBLIC_KEY=test-key", "GITHUB_CLIENT_ID=test-client"}
	skip := []string{"-o", "cluster-up", "-o", "kind-load", "-o", "kind-load-runtime", "-j4", "dev-up"}

	r := run(t, "old", env, skip...)
	if r.err == nil {
		t.Fatalf("dev-up passed against an AEI without the dispatch listener:\n%s", r.output)
	}
	if w := r.writes(); len(w) > 0 {
		t.Errorf("dev-up wrote to the cluster although aei-check failed:\n%s", strings.Join(w, "\n"))
	}

	r = run(t, "ok", env, skip...)
	if r.err != nil {
		t.Fatalf("dev-up: %v\n%s", r.err, r.output)
	}
	lookup := r.index("get service aei-controller")
	w := r.writes()
	if lookup < 0 || len(w) == 0 || r.index(w[0]) < lookup {
		t.Errorf("dev-up wrote before the dispatch check (call %d):\n%s", lookup, strings.Join(r.calls, "\n"))
	}
	if r.index("patch secret gofer-secrets") < 0 {
		t.Errorf("dev-up did not write gofer-secrets:\n%s", strings.Join(r.calls, "\n"))
	}
}
