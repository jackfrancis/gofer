// Package modelgateway tests hack/model-gateway/model-gateway.sh, the cluster half of
// the optional model gateway, against a stateful fake kubectl: the order of its
// writes, and that a failed or ignored write never loses the model key or the
// provider endpoints.
package modelgateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackfrancis/gofer/internal/config"
)

// fakeKubectl keeps Secrets, gofer-config and the gateway Deployment as files in
// $MOCK_STATE and logs every call (and each ConfigMap patch's content, as PATCH). A
// call containing a MOCK_FAIL substring fails; one containing a MOCK_DROP substring
// reports success and changes nothing. Applying rendered manifests that hold
// gofer-config sets the AI_CONNECTIONS keys they carry, as a client-side apply does.
const fakeKubectl = `#!/usr/bin/env bash
set -u
S=$MOCK_STATE
printf '%s\n' "$*" >>"$S/calls"
call="$*"
# matches LIST: does this call contain one of LIST's |-separated substrings?
matches() {
	local p
	[ -n "$1" ] || return 1
	local IFS='|'
	for p in $1; do case "$call" in *"$p"*) return 0;; esac; done
	return 1
}
matches "${MOCK_FAIL:-}" && { echo "FAIL $call" >>"$S/calls"; echo "fake: refused" >&2; exit 1; }
matches "${MOCK_DROP:-}" && { echo "DROP $call" >>"$S/calls"; exit 0; }
[ "$1" = -n ] && shift 2
arg_after() { local want=$1; shift; while [ $# -gt 0 ]; do [ "$1" = "$want" ] && { echo "$2"; return; }; shift; done; }
case "$1 $2" in
"get secret")
	f="$S/secret.$3.json"
	if [ -f "$f" ]; then jq '{data: .}' "$f"; else case "$*" in *--ignore-not-found*) ;; *) exit 1;; esac; fi ;;
"get configmap") jq '{data: .}' "$S/configmap.json" ;;
"get deployment")
	if [ -f "$S/deployment" ]; then
		case "$*" in *"-o json"*) jq -n --arg u "$(cat "$S/deployment")" '{metadata: {annotations: {"gofer/model-gateway-upstream": $u}}}';; esac
	else case "$*" in *--ignore-not-found*) ;; *) exit 1;; esac; fi ;;
"get statefulset") echo statefulset.apps/gofer ;;
"apply -f")
	if jq -e '.kind == "Secret"' "$3" >/dev/null 2>&1; then
		jq '.data' "$3" >"$S/secret.$(jq -r .metadata.name "$3").json"
	elif grep -q '^  name: gofer-config$' "$3"; then
		cm=$(cat "$S/configmap.json")
		while IFS= read -r line; do
			k=${line%%: *}; k=${k#  }; v=${line#*: }
			case $v in
			\"*) v=$(jq -r . <<<"$v") ;;
			\'*) v=${v#\'}; v=${v%\'}; v=${v//\'\'/\'} ;;
			esac
			cm=$(jq --arg k "$k" --arg v "$v" '.[$k] = $v' <<<"$cm")
		done < <(grep -E '^  (AI_CONNECTIONS|AI_GATEWAY_URL|AI_CONNECTIONS_BEFORE_GATEWAY): ' "$3")
		printf '%s' "$cm" >"$S/configmap.json"
	else
		sed -n 's|.*gofer/model-gateway-upstream: "\(.*\)"|\1|p' "$3" >"$S/deployment"
	fi ;;
"patch secret")
	f="$S/secret.$3.json"; [ -f "$f" ] || exit 1
	p=$(arg_after --patch-file "$@")
	case "$*" in
	*--type=json*) jq 'del(.AI_TOKEN)' "$f" >"$f.new" ;;
	*) jq --slurpfile p "$p" '. * $p[0].data | with_entries(select(.value != null))' "$f" >"$f.new" ;;
	esac
	mv "$f.new" "$f" ;;
"patch configmap")
	p=$(arg_after --patch-file "$@")
	echo "PATCH $(jq -c . "$p")" >>"$S/calls"
	jq --slurpfile p "$p" '. * $p[0].data | with_entries(select(.value != null))' "$S/configmap.json" >"$S/cm.new"
	mv "$S/cm.new" "$S/configmap.json" ;;
"delete --ignore-not-found") rm -f "$S/deployment" ;;
"kustomize deploy/k8s/model-gateway")
	printf 'kind: Deployment\nmetadata:\n  annotations:\n    gofer/model-gateway-upstream: "@UPSTREAM@"\n' ;;
esac
exit 0
`

// jqShim logs every jq invocation's arguments, so a test can show the model key
// never reaches a process's command line.
const jqShim = `#!/usr/bin/env bash
printf '%s\n' "$*" >>"$MOCK_STATE/jq-args"
exec @JQ@ "$@"
`

const (
	gw          = "http://gofer-model-gateway.gofer.svc.cluster.local"
	customUp    = "https://models.example/v1"
	customConns = `[{"endpoint":"https://models.example/v1/chat/completions","models":["a","b"]},{"endpoint":"https://models.example/v1/responses","models":["c"]}]`
	webKey      = "Y3VzdG9tLWtleQ==" // "custom-key"
)

type cluster struct {
	t     *testing.T
	state string
	fake  string
	shims string
}

func newCluster(t *testing.T, conns, key string) *cluster {
	t.Helper()
	c := &cluster{t: t, state: t.TempDir()}
	c.fake = filepath.Join(t.TempDir(), "kubectl")
	if err := os.WriteFile(c.fake, []byte(fakeKubectl), 0o755); err != nil {
		t.Fatal(err)
	}
	realJQ, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq is not installed")
	}
	c.shims = t.TempDir()
	if err := os.WriteFile(filepath.Join(c.shims, "jq"), []byte(strings.Replace(jqShim, "@JQ@", realJQ, 1)), 0o755); err != nil {
		t.Fatal(err)
	}
	c.writeJSON("configmap.json", map[string]string{"AI_CONNECTIONS": conns, "BASE_URL": "http://x"})
	web := map[string]string{"SESSION_SECRET": "cw=="}
	if key != "" {
		web["AI_TOKEN"] = key
	}
	c.writeJSON("secret.gofer-secrets.json", web)
	return c
}

func (c *cluster) writeJSON(name string, v any) {
	b, _ := json.Marshal(v)
	if err := os.WriteFile(filepath.Join(c.state, name), b, 0o600); err != nil {
		c.t.Fatal(err)
	}
}

func (c *cluster) readJSON(name string) map[string]string {
	m := map[string]string{}
	b, err := os.ReadFile(filepath.Join(c.state, name))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		c.t.Fatal(err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		c.t.Fatal(err)
	}
	return m
}

func (c *cluster) config() map[string]string { return c.readJSON("configmap.json") }
func (c *cluster) web() map[string]string    { return c.readJSON("secret.gofer-secrets.json") }
func (c *cluster) gatewayKey() string {
	return c.readJSON("secret.gofer-model-gateway.json")["AI_TOKEN"]
}
func (c *cluster) installedUpstream() string {
	b, err := os.ReadFile(filepath.Join(c.state, "deployment"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// calls returns the kubectl calls logged so far and clears the log.
func (c *cluster) calls() []string {
	p := filepath.Join(c.state, "calls")
	b, _ := os.ReadFile(p)
	_ = os.Remove(p)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (c *cluster) env(extra ...string) []string {
	return append([]string{
		"PATH=" + c.shims + ":" + os.Getenv("PATH"), "HOME=" + c.t.TempDir(), "MOCK_STATE=" + c.state,
		"KUBE_NS=gofer", "MODEL_GATEWAY_URL=" + gw,
		"MODEL_GATEWAY_BURST=20", "MODEL_GATEWAY_REFILL=2", "MODEL_GATEWAY_INTERVAL=1s",
	}, extra...)
}

// run runs one script command with env (KEY=VALUE) and returns its error and output.
func (c *cluster) run(cmd string, env ...string) (error, string) {
	c.t.Helper()
	err, out, _ := c.runIn(cmd, nil, env...)
	return err, out
}

// runIn is run with stdin; it returns stdout separately (for render).
func (c *cluster) runIn(cmd string, stdin []byte, env ...string) (error, string, []byte) {
	c.t.Helper()
	x := exec.Command("bash", "hack/model-gateway/model-gateway.sh", cmd, c.fake)
	x.Dir = filepath.Join("..", "..")
	x.Env = c.env(env...)
	x.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	x.Stdout, x.Stderr = &stdout, &stderr
	err := x.Run()
	return err, stderr.String() + stdout.String(), stdout.Bytes()
}

// deploy runs make deploy's gateway steps around the base apply, in the Makefile's
// order: preflight, render the base, apply the rendered manifests, sync.
func (c *cluster) deploy(base []byte, env ...string) (error, string) {
	c.t.Helper()
	if err, out := c.run("preflight", env...); err != nil {
		return err, "preflight: " + out
	}
	err, out, rendered := c.runIn("render", base, env...)
	if err != nil {
		return err, "render: " + out
	}
	f := filepath.Join(c.t.TempDir(), "base.rendered.yaml")
	if err := os.WriteFile(f, rendered, 0o600); err != nil {
		c.t.Fatal(err)
	}
	x := exec.Command(c.fake, "apply", "-f", f)
	x.Env = c.env(env...)
	if b, err := x.CombinedOutput(); err != nil {
		return err, "apply: " + string(b)
	}
	if err, out := c.run("sync", env...); err != nil {
		return err, "sync: " + out
	}
	return nil, ""
}

// goferStarts loads the web tier's model configuration from the cluster state the way
// gofer does at startup, and fails the test if gofer would refuse it.
func (c *cluster) goferStarts() []config.AIConnection {
	c.t.Helper()
	env := c.config()
	if k := c.web()["AI_TOKEN"]; k != "" {
		raw, _ := base64.StdEncoding.DecodeString(k)
		env["AI_TOKEN"] = string(raw)
	}
	conns, err := config.LoadConnections(func(k string) string { return env[k] })
	if err != nil {
		c.t.Fatalf("gofer would refuse to start: %v\nconfig: %v", err, env)
	}
	if len(conns) == 0 {
		c.t.Fatalf("gofer would start with the model off: %v", env)
	}
	return conns
}

// shippedBases returns the base manifests make deploy applies: the shipped
// gofer-config, and the full kustomize render when kubectl is installed.
func shippedBases(t *testing.T) map[string][]byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "k8s", "base", "configmap.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	bases := map[string][]byte{"configmap.yaml": raw}
	if kubectl, err := exec.LookPath("kubectl"); err == nil {
		x := exec.Command(kubectl, "kustomize", filepath.Join("..", "..", "deploy", "k8s", "base"))
		if out, err := x.Output(); err == nil {
			bases["kustomize"] = out
		}
	}
	return bases
}

func index(calls []string, sub string) int {
	for i, c := range calls {
		if strings.Contains(c, sub) && !strings.HasPrefix(c, "FAIL ") {
			return i
		}
	}
	return -1
}

func has(calls []string, sub string) bool { return index(calls, sub) >= 0 }

// injected reports whether the fake actually refused or dropped a call matching the
// scenario's pattern, so a failure test cannot pass on some earlier, unrelated error.
func injected(calls []string, env string) bool {
	_, pattern, _ := strings.Cut(env, "=")
	for _, c := range calls {
		if (strings.HasPrefix(c, "FAIL ") || strings.HasPrefix(c, "DROP ")) && strings.Contains(c, pattern) {
			return true
		}
	}
	return false
}

// install runs a successful apply for a custom provider and returns the calls.
func install(t *testing.T) (*cluster, []string) {
	t.Helper()
	c := newCluster(t, customConns, webKey)
	if err, out := c.run("apply", "MODEL_GATEWAY_UPSTREAM="+customUp); err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	return c, c.calls()
}

func TestApplyMovesTheKeyOnlyOnceTheGatewayHoldsIt(t *testing.T) {
	c, calls := install(t)
	if c.gatewayKey() != webKey {
		t.Fatalf("gateway key = %q, want the key from gofer-secrets", c.gatewayKey())
	}
	if _, ok := c.web()["AI_TOKEN"]; ok {
		t.Fatal("gofer-secrets still holds AI_TOKEN")
	}
	cfg := c.config()
	if cfg["AI_GATEWAY_URL"] != gw || cfg["AI_CONNECTIONS_BEFORE_GATEWAY"] != customConns {
		t.Fatalf("config = %v, want AI_GATEWAY_URL and the original AI_CONNECTIONS saved verbatim", cfg)
	}
	var conns []struct{ Endpoint string }
	_ = json.Unmarshal([]byte(cfg["AI_CONNECTIONS"]), &conns)
	if len(conns) != 2 || conns[0].Endpoint != gw+"/v1/chat/completions" || conns[1].Endpoint != gw+"/v1/responses" {
		t.Fatalf("AI_CONNECTIONS = %s, want both endpoints on the gateway", cfg["AI_CONNECTIONS"])
	}
	if c.installedUpstream() != customUp {
		t.Fatalf("recorded upstream = %q, want %q", c.installedUpstream(), customUp)
	}
	if conns := c.goferStarts(); !conns[0].Gateway {
		t.Fatal("gofer would not route through the gateway")
	}
	// The order that never loses the key: gateway Secret, gateway ready, gofer's
	// config, and only then gofer's copy goes, then gofer restarts.
	steps := []string{"gateway-secret.json", "rollout status deployment/gofer-model-gateway", "patch configmap gofer-config", "patch secret gofer-secrets", "rollout restart statefulset/gofer"}
	for i := 1; i < len(steps); i++ {
		a, b := index(calls, steps[i-1]), index(calls, steps[i])
		if a < 0 || b < 0 || a > b {
			t.Fatalf("%q (at %d) must come before %q (at %d):\n%s", steps[i-1], a, steps[i], b, strings.Join(calls, "\n"))
		}
	}
}

// Every way installation can stop before the gateway holds the key and gofer's config
// points at it leaves gofer's copy of the key, and gofer itself, untouched.
func TestApplyKeepsTheWebKeyWhenAStepFails(t *testing.T) {
	for name, env := range map[string]string{
		"gateway Secret write refused": "MOCK_FAIL=gateway-secret.json",
		"gateway Secret write ignored": "MOCK_DROP=gateway-secret.json",
		"gateway never ready":          "MOCK_FAIL=rollout status deployment/gofer-model-gateway",
		"config patch refused":         "MOCK_FAIL=patch configmap",
		"config patch ignored":         "MOCK_DROP=patch configmap",
	} {
		t.Run(name, func(t *testing.T) {
			c := newCluster(t, customConns, webKey)
			err, out := c.run("apply", "MODEL_GATEWAY_UPSTREAM="+customUp, env)
			if err == nil {
				t.Fatalf("apply succeeded:\n%s", out)
			}
			if c.web()["AI_TOKEN"] != webKey {
				t.Fatalf("gofer-secrets lost its AI_TOKEN:\n%s", out)
			}
			calls := c.calls()
			if !injected(calls, env) {
				t.Fatalf("the injected failure (%s) never fired; apply stopped earlier:\n%s", env, out)
			}
			if has(calls, "patch secret gofer-secrets") || has(calls, "statefulset/gofer") {
				t.Fatalf("gofer's key or pod was touched after a failed step:\n%s", strings.Join(calls, "\n"))
			}
		})
	}
	t.Run("config patch ignored while AI_GATEWAY_URL is already set", func(t *testing.T) {
		// Checking AI_GATEWAY_URL alone would pass here; the endpoints are what count.
		c, _ := install(t)
		cfg := c.config()
		cfg["AI_CONNECTIONS"] = customConns
		c.writeJSON("configmap.json", cfg)
		err, out := c.run("apply", "MOCK_DROP=patch configmap")
		if err == nil {
			t.Fatalf("apply succeeded with every endpoint still on the provider:\n%s", out)
		}
		if calls := c.calls(); !injected(calls, "MOCK_DROP=patch configmap") || has(calls, "statefulset/gofer") {
			t.Fatalf("want the dropped patch caught before gofer restarts:\n%s", strings.Join(calls, "\n"))
		}
	})
	t.Run("removing the web key refused", func(t *testing.T) {
		c := newCluster(t, customConns, webKey)
		err, out := c.run("apply", "MODEL_GATEWAY_UPSTREAM="+customUp, "MOCK_FAIL=patch secret gofer-secrets")
		if err == nil {
			t.Fatalf("apply succeeded:\n%s", out)
		}
		calls := c.calls()
		if !injected(calls, "MOCK_FAIL=patch secret gofer-secrets") {
			t.Fatalf("the injected failure never fired:\n%s", out)
		}
		if has(calls, "rollout restart statefulset/gofer") {
			t.Fatal("gofer restarted with AI_TOKEN beside AI_GATEWAY_URL")
		}
	})
}

// Rollback restores the endpoints saved at installation, verbatim: it never rebuilds
// them from this invocation's upstream, which would send a custom provider's key and
// prompts to the default host.
func TestDownRestoresTheOriginalEndpointsVerbatim(t *testing.T) {
	c, _ := install(t)
	if err, out := c.run("down"); err != nil { // no MODEL_GATEWAY_UPSTREAM
		t.Fatalf("down: %v\n%s", err, out)
	}
	cfg := c.config()
	if cfg["AI_CONNECTIONS"] != customConns {
		t.Fatalf("AI_CONNECTIONS = %s, want the original %s", cfg["AI_CONNECTIONS"], customConns)
	}
	for _, k := range []string{"AI_GATEWAY_URL", "AI_CONNECTIONS_BEFORE_GATEWAY"} {
		if _, ok := cfg[k]; ok {
			t.Errorf("%s is still set", k)
		}
	}
	if c.web()["AI_TOKEN"] != webKey {
		t.Fatal("the key did not return to gofer-secrets")
	}
	c.goferStarts()
	calls := c.calls()
	// One patch restores the endpoints and drops the gateway URL; the saved copy is
	// forgotten only after the gateway is gone.
	restore, forget := -1, -1
	for i, call := range calls {
		if strings.HasPrefix(call, "PATCH ") && strings.Contains(call, `"AI_GATEWAY_URL":null`) && strings.Contains(call, `"AI_CONNECTIONS":`) {
			restore = i
		}
		if strings.HasPrefix(call, "PATCH ") && strings.Contains(call, `"AI_CONNECTIONS_BEFORE_GATEWAY":null`) {
			forget = i
		}
	}
	r, d := index(calls, "rollout restart statefulset/gofer"), index(calls, "delete")
	if restore < 0 || r < restore || d < r || forget < d {
		t.Fatalf("want endpoints and gateway URL restored in one patch, then gofer restarted, then the gateway deleted, then the saved copy forgotten:\n%s", strings.Join(calls, "\n"))
	}
}

// A down that stops part-way keeps what it needs to finish, and simply re-running it
// completes the rollback.
func TestDownCanBeResumed(t *testing.T) {
	for name, env := range map[string]string{
		"gofer rollout times out": "MOCK_FAIL=rollout status statefulset/gofer",
		"gateway delete refused":  "MOCK_FAIL=delete",
		"forgetting refused":      "MOCK_FAIL=forget.json",
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := install(t)
			if err, out := c.run("down", env); err == nil {
				t.Fatalf("down succeeded:\n%s", out)
			}
			if !injected(c.calls(), env) {
				t.Fatalf("the injected failure (%s) never fired", env)
			}
			if c.installedUpstream() != "" {
				// A deploy now says what to do rather than failing obscurely.
				if err, out := c.run("preflight"); err == nil || !strings.Contains(out, "did not finish") {
					t.Fatalf("preflight after a partial down = %v, %s; want a pointer to re-run down", err, out)
				}
			}
			if err, out := c.run("down"); err != nil {
				t.Fatalf("re-run down: %v\n%s", err, out)
			}
			cfg := c.config()
			if cfg["AI_CONNECTIONS"] != customConns || cfg["AI_GATEWAY_URL"] != "" || cfg["AI_CONNECTIONS_BEFORE_GATEWAY"] != "" || c.installedUpstream() != "" {
				t.Fatalf("after the re-run: config %v, gateway %q", cfg, c.installedUpstream())
			}
			c.goferStarts()
			if err, out := c.run("down"); err != nil || !strings.Contains(out, "nothing to undo") {
				t.Fatalf("down when already down = %v, %s", err, out)
			}
		})
	}
}

func TestDownStopsWhenAWriteFails(t *testing.T) {
	for name, env := range map[string]string{
		"key write refused":    "MOCK_FAIL=patch secret gofer-secrets",
		"key write ignored":    "MOCK_DROP=patch secret gofer-secrets",
		"config patch refused": "MOCK_FAIL=patch configmap",
		"config patch ignored": "MOCK_DROP=patch configmap",
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := install(t)
			err, out := c.run("down", env)
			if err == nil {
				t.Fatalf("down succeeded:\n%s", out)
			}
			calls := c.calls()
			if !injected(calls, env) {
				t.Fatalf("the injected failure (%s) never fired; down stopped earlier:\n%s", env, out)
			}
			if has(calls, "delete") || has(calls, "statefulset/gofer") {
				t.Fatalf("down went on after a failed write:\n%s", strings.Join(calls, "\n"))
			}
			if c.installedUpstream() == "" {
				t.Fatal("the gateway was removed")
			}
		})
	}
	t.Run("nothing saved", func(t *testing.T) {
		c, _ := install(t)
		cfg := c.config()
		delete(cfg, "AI_CONNECTIONS_BEFORE_GATEWAY")
		c.writeJSON("configmap.json", cfg)
		if err, _ := c.run("down"); err == nil {
			t.Fatal("down guessed the provider endpoints")
		}
		if has(c.calls(), "patch") {
			t.Fatal("down wrote something without the saved endpoints")
		}
	})
}

// A routine make deploy re-applies the shipped base, whose AI_CONNECTIONS names GitHub
// Copilot. Behind a custom-provider gateway the rendered base must carry the gateway
// endpoints built from the saved provider connections, so gofer starts; and a deploy
// never writes a key, even with AI_TOKEN in the shell.
func TestDeployWithTheShippedBase(t *testing.T) {
	for name, base := range shippedBases(t) {
		t.Run(name, func(t *testing.T) {
			c, _ := install(t)
			if err, out := c.deploy(base, "AI_TOKEN=a-dev-key"); err != nil {
				t.Fatalf("deploy: %v\n%s", err, out)
			}
			for _, call := range c.calls() {
				if strings.Contains(call, "secret") && !strings.Contains(call, "get secret") {
					t.Fatalf("a deploy wrote a Secret: %s", call)
				}
			}
			if c.gatewayKey() != webKey {
				t.Fatal("a deploy changed the gateway's key")
			}
			cfg := c.config()
			if cfg["AI_CONNECTIONS_BEFORE_GATEWAY"] != customConns || cfg["AI_GATEWAY_URL"] != gw {
				t.Fatalf("config after deploy = %v, want the saved provider connections kept", cfg)
			}
			conns := c.goferStarts()
			if !conns[0].Gateway || conns[0].Endpoint != gw+"/v1/chat/completions" || conns[1].Endpoint != gw+"/v1/responses" {
				t.Fatalf("connections after deploy = %+v, want the custom provider's routed through the gateway", conns)
			}
		})
	}
}

func TestDeployWithoutTheGateway(t *testing.T) {
	for name, base := range shippedBases(t) {
		t.Run(name, func(t *testing.T) {
			c := newCluster(t, customConns, webKey)
			if err, out := c.deploy(base); err != nil {
				t.Fatalf("deploy: %v\n%s", err, out)
			}
			cfg := c.config()
			if strings.Contains(cfg["AI_CONNECTIONS"], gw) || cfg["AI_GATEWAY_URL"] != "" {
				t.Fatalf("config = %v, want the shipped provider endpoints and no gateway", cfg)
			}
			c.goferStarts()
			// The AI_CONNECTIONS override is part of the rendered base, verbatim.
			if err, out := c.deploy(base, "AI_CONNECTIONS="+customConns); err != nil {
				t.Fatalf("deploy with override: %v\n%s", err, out)
			}
			if got := c.config()["AI_CONNECTIONS"]; got != customConns {
				t.Fatalf("AI_CONNECTIONS = %s, want the override", got)
			}
		})
	}
}

// Behind the gateway an AI_CONNECTIONS override names the provider connections: they
// are saved, and their gateway form is what gofer gets.
func TestDeployWithAnOverrideBehindTheGateway(t *testing.T) {
	base := shippedBases(t)["configmap.yaml"]
	c, _ := install(t)
	override := `[{"endpoint":"https://models.example/v1/responses","models":["d","e"]}]`
	if err, out := c.deploy(base, "AI_CONNECTIONS="+override); err != nil {
		t.Fatalf("deploy: %v\n%s", err, out)
	}
	if got := c.config()["AI_CONNECTIONS_BEFORE_GATEWAY"]; got != override {
		t.Fatalf("saved = %s, want the override", got)
	}
	if conns := c.goferStarts(); len(conns) != 1 || conns[0].Endpoint != gw+"/v1/responses" || conns[0].Default() != "d" {
		t.Fatalf("connections = %+v", conns)
	}
}

// A deploy that cannot build a valid routed config applies nothing.
func TestDeployRefusesBeforeApplying(t *testing.T) {
	base := shippedBases(t)["configmap.yaml"]
	t.Run("an override on another provider", func(t *testing.T) {
		c, _ := install(t)
		err, out := c.deploy(base, "AI_CONNECTIONS="+`[{"endpoint":"https://api.githubcopilot.com/responses","models":["x"]}]`)
		if err == nil || !strings.HasPrefix(out, "render:") {
			t.Fatalf("deploy = %v, %s; want render to refuse", err, out)
		}
		if has(c.calls(), "apply -f") {
			t.Fatal("applied after a refused render")
		}
	})
	t.Run("nothing saved", func(t *testing.T) {
		c, _ := install(t)
		cfg := c.config()
		delete(cfg, "AI_CONNECTIONS_BEFORE_GATEWAY")
		c.writeJSON("configmap.json", cfg)
		if err, out := c.deploy(base); err == nil || !strings.HasPrefix(out, "render:") {
			t.Fatalf("deploy = %v, %s; want render to refuse", err, out)
		}
	})
}

// sync compares the endpoints themselves, not just the gateway URL, before gofer
// restarts.
func TestSyncChecksTheEndpoints(t *testing.T) {
	base := shippedBases(t)["configmap.yaml"]
	c, _ := install(t)
	cfg := c.config()
	cfg["AI_CONNECTIONS"] = customConns // AI_GATEWAY_URL is still set
	c.writeJSON("configmap.json", cfg)
	err, out := c.deploy(base, "MOCK_DROP=base.rendered.yaml")
	if err == nil || !strings.HasPrefix(out, "sync:") {
		t.Fatalf("deploy = %v, %s; want sync to catch the endpoints still on the provider", err, out)
	}
}

// A read that fails is not an absent gateway or key.
func TestFailedReadsStopTheScript(t *testing.T) {
	c, _ := install(t)
	for _, cmd := range []string{"preflight", "render", "sync", "down"} {
		if err, out := c.run(cmd, "MOCK_FAIL=get deployment"); err == nil {
			t.Errorf("%s with an unreadable Deployment succeeded:\n%s", cmd, out)
		}
	}
	if err, out := c.run("preflight", "MOCK_FAIL=get secret gofer-secrets"); err == nil {
		t.Errorf("preflight with an unreadable gofer-secrets succeeded:\n%s", out)
	}
	plain := newCluster(t, customConns, webKey)
	if err, _ := plain.run("preflight", "MOCK_FAIL=get deployment"); err == nil {
		t.Error("preflight read an unreadable Deployment as absent")
	}
}

// The model key reaches jq on stdin and kubectl in files: it is never in a process's
// arguments.
func TestTheKeyIsNeverAnArgument(t *testing.T) {
	c := newCluster(t, customConns, webKey)
	steps := [][]string{
		{"apply", "MODEL_GATEWAY_UPSTREAM=" + customUp},
		{"apply", "AI_TOKEN=rotated-raw-key"},
		{"down"},
	}
	for _, s := range steps {
		if err, out := c.run(s[0], s[1:]...); err != nil {
			t.Fatalf("%v: %v\n%s", s, err, out)
		}
	}
	jqArgs, _ := os.ReadFile(filepath.Join(c.state, "jq-args"))
	kubeCalls := strings.Join(c.calls(), "\n")
	for _, secret := range []string{webKey, "custom-key", "rotated-raw-key", base64.StdEncoding.EncodeToString([]byte("rotated-raw-key"))} {
		if strings.Contains(string(jqArgs), secret) {
			t.Errorf("the key %q was a jq argument", secret)
		}
		if strings.Contains(kubeCalls, secret) {
			t.Errorf("the key %q was a kubectl argument", secret)
		}
	}
	if len(jqArgs) == 0 {
		t.Fatal("the jq shim saw no calls; the check proves nothing")
	}
}

func TestPreflightRefusesTheKeyBesideTheGateway(t *testing.T) {
	c, _ := install(t)
	web := c.web()
	web["AI_TOKEN"] = webKey
	c.writeJSON("secret.gofer-secrets.json", web)
	if err, out := c.run("preflight"); err == nil || !strings.Contains(out, "AI_TOKEN") {
		t.Fatalf("preflight = %v, %s; want a refusal naming AI_TOKEN", err, out)
	}
	// Without the gateway, preflight and sync do nothing.
	plain := newCluster(t, customConns, webKey)
	for _, cmd := range []string{"preflight", "sync"} {
		if err, out := plain.run(cmd); err != nil {
			t.Fatalf("%s without the gateway: %v\n%s", cmd, err, out)
		}
	}
	if has(plain.calls(), "patch") {
		t.Fatal("preflight or sync wrote something without the gateway")
	}
}

func TestApplyRefusesBeforeWritingAnything(t *testing.T) {
	t.Run("a different provider for an installed gateway", func(t *testing.T) {
		c, _ := install(t)
		if err, _ := c.run("apply", "MODEL_GATEWAY_UPSTREAM=https://api.githubcopilot.com"); err == nil {
			t.Fatal("apply switched the provider of an installed gateway")
		}
		if calls := c.calls(); has(calls, "apply -f") || has(calls, "patch") {
			t.Fatalf("wrote before refusing:\n%s", strings.Join(calls, "\n"))
		}
	})
	t.Run("mixed endpoints", func(t *testing.T) {
		c := newCluster(t, `[{"endpoint":"`+gw+`/v1/responses","models":["a"]},{"endpoint":"https://models.example/v1/responses","models":["b"]}]`, webKey)
		if err, _ := c.run("apply", "MODEL_GATEWAY_UPSTREAM="+customUp); err == nil {
			t.Fatal("apply accepted a mix of gateway and provider endpoints")
		}
		if calls := c.calls(); has(calls, "apply -f") || has(calls, "patch") {
			t.Fatalf("wrote before refusing:\n%s", strings.Join(calls, "\n"))
		}
	})
	t.Run("an endpoint off the provider", func(t *testing.T) {
		c := newCluster(t, customConns, webKey)
		if err, _ := c.run("apply"); err == nil { // default upstream is Copilot
			t.Fatal("apply routed another provider's endpoints")
		}
		if calls := c.calls(); has(calls, "apply -f") || has(calls, "patch") {
			t.Fatalf("wrote before refusing:\n%s", strings.Join(calls, "\n"))
		}
	})
}

// Re-running apply with nothing new changes no key and keeps the saved endpoints.
func TestApplyIsRepeatable(t *testing.T) {
	c, _ := install(t)
	if err, out := c.run("apply"); err != nil {
		t.Fatalf("second apply: %v\n%s", err, out)
	}
	if has(c.calls(), "gateway-secret.json") {
		t.Fatal("second apply rewrote an unchanged key")
	}
	if c.config()["AI_CONNECTIONS_BEFORE_GATEWAY"] != customConns || c.installedUpstream() != customUp {
		t.Fatalf("second apply lost the saved endpoints or the provider: %v / %q", c.config(), c.installedUpstream())
	}
}

// An explicit AI_TOKEN rotates the gateway's key.
func TestApplyRotatesTheKey(t *testing.T) {
	c, _ := install(t)
	if err, out := c.run("apply", "AI_TOKEN=new-key"); err != nil {
		t.Fatalf("rotate: %v\n%s", err, out)
	}
	if c.gatewayKey() != "bmV3LWtleQ==" {
		t.Fatalf("gateway key = %q, want the new key", c.gatewayKey())
	}
}
