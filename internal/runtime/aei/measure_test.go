package aei

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackfrancis/agent-execution-interface/aeiruntime"

	"github.com/jackfrancis/gofer/internal/llm"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// These tests cover the workload's half of measuring a review (archive plan item 17):
// the run's completion report carries the model time and the model and tool calls the
// workload observed, which the control plane exports to Prometheus.

// completion is the POST /complete body the runtime sends, with its raw timing object.
type completion struct {
	RunID     string            `json:"run_id"`
	Error     string            `json:"error"`
	Timing    aeiruntime.Timing `json:"timing"`
	RawTiming map[string]any    `json:"-"`
}

// fakeControl is the aei-controller's /complete endpoint: it records each report and
// the bearer that authenticated it.
type fakeControl struct {
	srv     *httptest.Server
	mu      sync.Mutex
	reports []completion
	bearers []string
}

func newFakeControl(t *testing.T) *fakeControl {
	t.Helper()
	c := &fakeControl{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/complete" {
			http.NotFound(w, r)
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var body completion
		var wire struct {
			Timing map[string]any `json:"timing"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := json.Unmarshal(raw, &wire); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		body.RawTiming = wire.Timing
		c.mu.Lock()
		c.reports = append(c.reports, body)
		c.bearers = append(c.bearers, r.Header.Get("Authorization"))
		c.mu.Unlock()
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// only returns the single report the run sent, failing the test otherwise.
func (c *fakeControl) only(t *testing.T) completion {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reports) != 1 {
		t.Fatalf("got %d completion reports, want 1", len(c.reports))
	}
	if c.bearers[0] != "Bearer run-credential" {
		t.Errorf("completion authenticated with %q, want the run credential", c.bearers[0])
	}
	return c.reports[0]
}

// reviewModel is a chat-completions model that asks for one tool and then answers,
// taking delay per call.
func reviewModel(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	var (
		mu    sync.Mutex
		turns int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		mu.Lock()
		turns++
		n := turns
		mu.Unlock()
		if n == 1 {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"github_get_pull_request","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"LGTM"}}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// oneTool is a worklist.ToolBox with a single tool that counts its invocations.
type oneTool struct{ invoked int }

func (o *oneTool) Definitions() []worklist.ToolDef {
	return []worklist.ToolDef{{Name: "github_get_pull_request", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}}
}

func (o *oneTool) Invoke(context.Context, string, json.RawMessage) (string, error) {
	o.invoked++
	return "a pull request", nil
}

// The tests below run the real aeiruntime.Main, which exits the process, so each one
// re-runs this test binary as the runtime (TestRuntimeProcess) with a launcher's ABI
// environment, against a fake controller, and reads the one report it sends.

const (
	envChild = "GOFER_TEST_RUNTIME" // set in the child: run as the runtime
	envFn    = "GOFER_TEST_FN"      // which run function the child executes
	envModel = "GOFER_TEST_MODEL"   // the child's model endpoint
)

// TestRuntimeProcess is the child: it runs aeiruntime.Main exactly as RunWorkload
// does, with the run function GOFER_TEST_FN names. It does nothing in a normal run.
func TestRuntimeProcess(t *testing.T) {
	if os.Getenv(envChild) == "" {
		t.Skip("runs only as the runtime child of the tests below")
	}
	fns := map[string]func(context.Context, *aeiruntime.Runtime) error{
		"work":   work,
		"none":   func(context.Context, *aeiruntime.Runtime) error { return nil },
		"review": reviewAt(os.Getenv(envModel)),
		"fail": func(ctx context.Context, rt *aeiruntime.Runtime) error {
			_ = reviewAt(os.Getenv(envModel))(ctx, rt)
			return errors.New("review failed")
		},
	}
	aeiruntime.Main(os.Getenv, nil, measured(fns[os.Getenv(envFn)]))
	os.Exit(0) // Main returns on success
}

// reviewAt asks the model at endpoint to review an item, with one tool on offer.
func reviewAt(endpoint string) func(context.Context, *aeiruntime.Runtime) error {
	return func(ctx context.Context, _ *aeiruntime.Runtime) error {
		c := llm.NewConverser(llm.Config{Endpoint: endpoint, Model: "m", Token: "t"})
		_, err := c.Reply(ctx, worklist.WorkItem{Type: worklist.TypePullRequest}, "alice", "", nil, "review this", &oneTool{})
		return err
	}
}

// runRuntime runs the runtime child with run function fn and the ABI environment env,
// and returns its exit code.
func runRuntime(t *testing.T, fn string, env map[string]string) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRuntimeProcess$")
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "AEI_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, envChild+"=1", envFn+"="+fn)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		t.Logf("runtime output:\n%s", out)
		return exit.ExitCode()
	default:
		t.Fatalf("run the runtime child: %v\n%s", err, out)
		return -1
	}
}

// A tool-using review reports the calls the workload observed — two model calls, one
// tool call — and its model time, on the one completion the SDK sends. The model time
// is part of the runtime, and provisioning (dispatch to start) is reported beside it,
// so an operator can tell waiting for a provider from time in the model.
func TestMainReportsAToolUsingReview(t *testing.T) {
	const delay = 25 * time.Millisecond
	control := newFakeControl(t)
	model := reviewModel(t, delay)
	dispatched := time.Now().Add(-3 * time.Second)
	env := runEnv(t, "github-converse", nil, map[string]string{
		aeiruntime.EnvControlURL:   control.srv.URL,
		aeiruntime.EnvDispatchedAt: strconv.FormatInt(dispatched.UnixMilli(), 10),
		envModel:                   model.URL,
	})

	if code := runRuntime(t, "review", env); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	got := control.only(t)
	if got.RunID != "run-1" || got.Error != "" {
		t.Errorf("report = %+v, want a success for run-1", got)
	}
	if got.Timing.ModelCalls != 2 || got.Timing.ToolCalls != 1 {
		t.Errorf("timing = %+v, want 2 model calls and 1 tool call", got.Timing)
	}
	if floor := (2 * delay).Seconds(); got.Timing.ModelSeconds < floor {
		t.Errorf("model_seconds = %v, want at least %v", got.Timing.ModelSeconds, floor)
	}
	if got.Timing.RuntimeSeconds < got.Timing.ModelSeconds {
		t.Errorf("runtime_seconds %v < model_seconds %v: model time is part of the run", got.Timing.RuntimeSeconds, got.Timing.ModelSeconds)
	}
	if got.Timing.ProvisioningSeconds < 2.5 {
		t.Errorf("provisioning_seconds = %v, want about 3 (dispatch to start)", got.Timing.ProvisioningSeconds)
	}
}

// The real workload, run as RunWorkload runs it, reports its model calls: an llm-rank
// run over one item makes one model call and no tool calls.
func TestMainReportsARankRun(t *testing.T) {
	control := newFakeControl(t)
	plane := newGoferPlane(t, []worklist.WorkItem{{
		ID: "github:o/r#7", OwnerID: "github:1", Source: "github", Type: worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Number: 7, Repo: "o/r", Title: "T", State: "open"},
	}})
	model := modelStub(t)
	env := runEnv(t, "llm-rank", map[string]string{
		"owner":       "github:1",
		"gofer_url":   plane.srv.URL,
		"ai_endpoint": model.URL + "/chat/completions",
		"ai_model":    "test-model",
	}, map[string]string{aeiruntime.EnvControlURL: control.srv.URL})

	if code := runRuntime(t, "work", env); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := control.only(t)
	if got.Timing.ModelCalls != 1 || got.Timing.ToolCalls != 0 {
		t.Errorf("timing = %+v, want 1 model call and no tool calls", got.Timing)
	}
	if got.Timing.ModelSeconds <= 0 {
		t.Errorf("model_seconds = %v, want > 0", got.Timing.ModelSeconds)
	}
}

// A run that makes no model calls measured zero model time and sends it as an explicit
// zero, so the controller counts the sample instead of reading "not measured".
func TestMainSendsAnExplicitZeroModelTime(t *testing.T) {
	control := newFakeControl(t)
	env := runEnv(t, "github-ingest", nil, map[string]string{aeiruntime.EnvControlURL: control.srv.URL})

	if code := runRuntime(t, "none", env); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := control.only(t).RawTiming
	if v, ok := got["model_seconds"]; !ok || v != 0.0 {
		t.Errorf("model_seconds on the wire = %v (present %v), want an explicit 0; timing %v", v, ok, got)
	}
	if _, ok := got["runtime_seconds"]; !ok {
		t.Errorf("runtime_seconds missing from %v", got)
	}
}

// A failed run reports its error with the model timing it gathered, and the runtime
// exits non-zero.
func TestMainReportsAFailedRun(t *testing.T) {
	control := newFakeControl(t)
	model := reviewModel(t, 0)
	env := runEnv(t, "github-converse", nil, map[string]string{
		aeiruntime.EnvControlURL: control.srv.URL,
		envModel:                 model.URL,
	})

	if code := runRuntime(t, "fail", env); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	got := control.only(t)
	if got.Error != "review failed" {
		t.Errorf("reported error = %q, want %q", got.Error, "review failed")
	}
	if got.Timing.ModelCalls != 2 || got.Timing.ToolCalls != 1 {
		t.Errorf("timing = %+v, want 2 model calls and 1 tool call", got.Timing)
	}
}

// measured never reports itself: the SDK sends the run's one report (and, on a
// durable actor, frees the slot once it is acknowledged). It hands back the run's
// error.
func TestMeasuredLeavesTheReportToTheSDK(t *testing.T) {
	control := newFakeControl(t)
	model := reviewModel(t, 0)
	rt := loadRunWith(t, "github-converse", nil, map[string]string{aeiruntime.EnvControlURL: control.srv.URL})

	boom := errors.New("review failed")
	fail := func(ctx context.Context, rt *aeiruntime.Runtime) error {
		_ = reviewAt(model.URL)(ctx, rt)
		return boom
	}
	if err := measured(fail)(context.Background(), rt); !errors.Is(err, boom) {
		t.Fatalf("measured returned %v, want the run's error", err)
	}
	control.mu.Lock()
	defer control.mu.Unlock()
	if len(control.reports) != 0 {
		t.Errorf("measured sent %d completion reports, want none (the SDK reports)", len(control.reports))
	}
}
