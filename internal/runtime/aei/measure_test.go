package aei

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// completion is the POST /complete body the runtime sends.
type completion struct {
	RunID  string            `json:"run_id"`
	Error  string            `json:"error"`
	Timing aeiruntime.Timing `json:"timing"`
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
		var body completion
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
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

// run executes fn as the one-shot runtime does, against env, and returns its exit code.
func run(env map[string]string, fn func(context.Context, *aeiruntime.Runtime) error) int {
	return runOnce(func(k string) string { return env[k] }, fn)
}

// reviewWith is a run function that asks the model to review an item with box.
func reviewWith(model *httptest.Server, box worklist.ToolBox) func(context.Context, *aeiruntime.Runtime) error {
	return func(ctx context.Context, _ *aeiruntime.Runtime) error {
		c := llm.NewConverser(llm.Config{Endpoint: model.URL, Model: "m", Token: "t"})
		_, err := c.Reply(ctx, worklist.WorkItem{Type: worklist.TypePullRequest}, "alice", "", nil, "review this", box)
		return err
	}
}

// A tool-using review reports the calls the workload observed — two model calls, one
// tool call — and its model time, on the one completion the control plane receives.
// The model time is part of the runtime, and provisioning (dispatch to start) is
// reported beside it, so an operator can tell waiting for a provider from time in the
// model.
func TestRunOnceReportsAToolUsingReview(t *testing.T) {
	const delay = 25 * time.Millisecond
	control := newFakeControl(t)
	model := reviewModel(t, delay)
	dispatched := time.Now().Add(-3 * time.Second)
	env := runEnv(t, "github-converse", nil, map[string]string{
		aeiruntime.EnvControlURL:   control.srv.URL,
		aeiruntime.EnvDispatchedAt: strconv.FormatInt(dispatched.UnixMilli(), 10),
	})

	box := &oneTool{}
	if code := run(env, reviewWith(model, box)); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	got := control.only(t)
	if got.RunID != "run-1" || got.Error != "" {
		t.Errorf("report = %+v, want a success for run-1", got)
	}
	if got.Timing.ModelCalls != 2 {
		t.Errorf("model_calls = %d, want 2", got.Timing.ModelCalls)
	}
	if got.Timing.ToolCalls != box.invoked || box.invoked != 1 {
		t.Errorf("tool_calls = %d, want %d (the calls the tool ran)", got.Timing.ToolCalls, box.invoked)
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

// The real workload, run one shot as RunWorkload runs it, reports its model calls: an
// llm-rank run over one item makes one model call and no tool calls.
func TestRunOnceReportsARankRun(t *testing.T) {
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

	if code := run(env, work); code != 0 {
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

// A failed run reports its error with the timing it gathered and exits 1, as
// aeiruntime.Main does.
func TestRunOnceReportsAFailedRun(t *testing.T) {
	control := newFakeControl(t)
	model := reviewModel(t, 0)
	env := runEnv(t, "github-converse", nil, map[string]string{aeiruntime.EnvControlURL: control.srv.URL})

	boom := errors.New("review failed")
	review := reviewWith(model, &oneTool{})
	fail := func(ctx context.Context, rt *aeiruntime.Runtime) error {
		_ = review(ctx, rt)
		return boom
	}
	if code := run(env, fail); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	got := control.only(t)
	if got.Error != boom.Error() {
		t.Errorf("reported error = %q, want %q", got.Error, boom.Error())
	}
	if got.Timing.ModelCalls != 2 || got.Timing.ToolCalls != 1 {
		t.Errorf("timing = %+v, want 2 model calls and 1 tool call", got.Timing)
	}
}

// Against a stalled controller a run makes one completion attempt and gives up after
// one report budget, so a finished run exits in time; aeiruntime.Main would wait out a
// second budget for its own report after gofer's.
func TestRunOnceSendsOneReportWithinOneBudget(t *testing.T) {
	old := reportTimeout
	reportTimeout = 200 * time.Millisecond
	t.Cleanup(func() { reportTimeout = old })

	var (
		mu       sync.Mutex
		attempts int
	)
	release := make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(stalled.Close)
	t.Cleanup(func() { close(release) }) // runs first: unblock the handler before Close
	env := runEnv(t, "github-converse", nil, map[string]string{aeiruntime.EnvControlURL: stalled.URL})

	began := time.Now()
	if code := run(env, func(context.Context, *aeiruntime.Runtime) error { return nil }); code != 0 {
		t.Fatalf("exit code = %d, want 0 (the run succeeded; only its report stalled)", code)
	}
	if took := time.Since(began); took >= 2*reportTimeout {
		t.Errorf("run took %v against a stalled controller, want under %v (one report budget)", took, 2*reportTimeout)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 1 {
		t.Errorf("completion attempts = %d, want 1", attempts)
	}
}

// The run executes under its deadline (AEI_DEADLINE), as it does under the SDK.
func TestRunOnceAppliesTheRunDeadline(t *testing.T) {
	control := newFakeControl(t)
	deadline := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	env := runEnv(t, "github-converse", nil, map[string]string{
		aeiruntime.EnvControlURL: control.srv.URL,
		aeiruntime.EnvDeadline:   strconv.FormatInt(deadline.UnixMilli(), 10),
	})

	var got time.Time
	var ok bool
	run(env, func(ctx context.Context, _ *aeiruntime.Runtime) error {
		got, ok = ctx.Deadline()
		return nil
	})
	if !ok || !got.Equal(deadline) {
		t.Errorf("run context deadline = %v (set %v), want %v", got, ok, deadline)
	}
}

// A run that cannot load exits 2 and reports nothing, as aeiruntime.Main does.
func TestRunOnceCannotLoad(t *testing.T) {
	ran := false
	code := run(map[string]string{}, func(context.Context, *aeiruntime.Runtime) error { ran = true; return nil })
	if code != 2 || ran {
		t.Errorf("exit code = %d, ran = %v; want 2 and not run", code, ran)
	}
}

// On a durable substrate the SDK's serve loop runs each run; measured reports first,
// with the full timing, even once the run's context is cancelled (after its deadline),
// and hands back the run's error for the loop's own report.
func TestMeasuredReportsFirstForTheServeLoop(t *testing.T) {
	control := newFakeControl(t)
	model := reviewModel(t, 0)
	rt := loadRunWith(t, "github-converse", nil, map[string]string{aeiruntime.EnvControlURL: control.srv.URL})

	boom := errors.New("deadline exceeded mid-review")
	review := reviewWith(model, &oneTool{})
	fail := func(ctx context.Context, rt *aeiruntime.Runtime) error {
		_ = review(context.WithoutCancel(ctx), rt)
		return boom
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := measured(fail)(ctx, rt); !errors.Is(err, boom) {
		t.Fatalf("measured returned %v, want the run's error", err)
	}
	got := control.only(t)
	if got.Error != boom.Error() || got.Timing.ModelCalls != 2 || got.Timing.ToolCalls != 1 {
		t.Errorf("report = %+v, want the run's error with 2 model calls and 1 tool call", got)
	}
}
