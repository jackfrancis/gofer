package aei

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackfrancis/agent-execution-interface/aeiruntime"

	"github.com/jackfrancis/gofer/internal/agent"
	"github.com/jackfrancis/gofer/internal/llm"
)

// This file is the WORKLOAD half of the backend: what AEI launches per run. The
// control plane runs gofer's runtime image (cmd/runtime) on whatever substrate the
// app's provider class names, so the workload executes OUT-OF-PROCESS and reaches
// gofer over its agent plane. The same binary runs unchanged on every AEI substrate:
// the runtime SDK learns the run through the runtime ABI, holds the run credential in
// memory only, and reports completion idempotently.

// RunWorkload is gofer's agent runtime entrypoint (cmd/runtime). On a workload
// substrate it runs one shot itself (runOnce), so the run sends a single completion
// report that carries its model timing. On a durable substrate, where the platform
// sets AEI_SERVE_ADDR, it hands control to the AEI runtime SDK's standing actor loop,
// with each run wrapped by measured.
func RunWorkload() {
	if os.Getenv(aeiruntime.EnvServeAddr) != "" {
		aeiruntime.Main(os.Getenv, nil, measured(work))
		return
	}
	os.Exit(runOnce(os.Getenv, work))
}

// runOnce is the one-shot run gofer drives itself instead of aeiruntime.Main, and
// returns the process exit code. Main reports the outcome after fn returns with only
// provisioning and runtime seconds, and it reports even when fn already has, so an app
// that reported its own model timing would pay for two reports against a stalled
// controller and could push a finished run past its deadline. runOnce instead loads
// the run, executes fn under the run's deadline, and sends exactly one report with the
// full Timing. It keeps Main's exit codes: 2 when the run cannot load, 1 when fn
// fails, 0 on success.
//
// The report gets no timeout of gofer's own: on a one-shot run rt.Complete applies the
// SDK's budget, which retries a failed or stalled attempt and stops in time for the
// process to exit before the run deadline.
func runOnce(getenv func(string) string, fn func(context.Context, *aeiruntime.Runtime) error) int {
	rt, err := aeiruntime.Load(context.Background(), getenv, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aeiruntime:", err)
		return 2
	}
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if deadline := rt.Spec().Deadline; !deadline.IsZero() {
		ctx, cancel = context.WithDeadline(context.Background(), deadline)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	timing, runErr := measure(ctx, rt, fn)
	cancel()
	// A fresh context, so the report still lands after the deadline cancelled the run.
	if err := rt.Complete(context.Background(), runErr, timing, ""); err != nil {
		slog.Default().Warn("completion report failed", "run", rt.Spec().RunID, "err", err)
	}
	if runErr != nil {
		fmt.Fprintln(os.Stderr, "aeiruntime: run failed:", runErr)
		return 1
	}
	return 0
}

// measured wraps fn for the SDK's durable serve loop, which applies the run's deadline
// and sends the run's one terminal report itself after fn returns, holding the actor's
// concurrency slot until that report is done. The SDK takes no timing from fn, so a
// report of gofer's own would be a second one, and the loop's duplicate could hold the
// slot for its whole retry budget after the controller had already accepted gofer's.
// So the wrapper measures and logs the run's model time and calls but sends nothing:
// on a durable substrate the completion carries provisioning and runtime seconds only,
// until the SDK can take an app's timing into its own report.
func measured(fn func(context.Context, *aeiruntime.Runtime) error) func(context.Context, *aeiruntime.Runtime) error {
	return func(ctx context.Context, rt *aeiruntime.Runtime) error {
		_, runErr := measure(ctx, rt, fn)
		return runErr
	}
}

// measure runs fn with an llm.Collector on its context and returns what the run spent:
// provisioning (dispatch-start to work-start, the SDK's definition), runtime, and the
// model time and model and tool calls the collector observed.
func measure(ctx context.Context, rt *aeiruntime.Runtime, fn func(context.Context, *aeiruntime.Runtime) error) (aeiruntime.Timing, error) {
	start := time.Now()
	ctx, collector := llm.WithCollector(ctx)
	runErr := fn(ctx, rt)

	// Runtime and model time are always measured, so they are sent even when zero (a
	// run that made no model calls spent zero seconds in the model, and the controller
	// counts that sample); an unmarked zero would be omitted as "not measured".
	timing := aeiruntime.Timing{
		RuntimeSeconds: time.Since(start).Seconds(),
		ModelSeconds:   collector.ModelDuration().Seconds(),
		ModelCalls:     collector.ModelCalls(),
		ToolCalls:      collector.ToolCalls(),
		Measured:       aeiruntime.StageRuntime | aeiruntime.StageModel,
	}
	if at := rt.Spec().DispatchedAt; !at.IsZero() {
		timing.Measured |= aeiruntime.StageProvisioning
		if d := start.Sub(at); d > 0 {
			timing.ProvisioningSeconds = d.Seconds()
		}
	}
	slog.Default().Info("gofer runtime finished", "run", rt.Spec().RunID, "ok", runErr == nil,
		"runtime_seconds", timing.RuntimeSeconds, "model_seconds", timing.ModelSeconds,
		"model_calls", timing.ModelCalls, "tool_calls", timing.ToolCalls)
	return timing, runErr
}

// work executes one gofer job for the loaded run. It is gofer's workload (agent.Run)
// wearing AEI's clothes: the run's TaskRef names the job, its parameters carry the
// owner and the non-secret model coordinates, and the run credential authenticates
// every call back to gofer.
func work(ctx context.Context, rt *aeiruntime.Runtime) error {
	spec := rt.Spec()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	// The workload logs its phases through slog.Default(); route those to the same
	// handler so fetch, enrich, rank and converse are visible in the run's logs.
	slog.SetDefault(log)
	log.Info("gofer runtime starting", "run", spec.RunID, "task", spec.TaskRef, "owner", spec.Parameters["owner"])

	// gofer's agent plane is the workload's whole view of gofer: it vends the acting
	// user's delegated credential (GET /agent/credential) and reads and writes the
	// worklist (GET/POST /agent/worklist), authenticated by the run credential the
	// control plane minted for this run. gofer supplies the client, so the backend
	// writes no plumbing of its own.
	base := strings.TrimSpace(spec.Parameters["gofer_url"])
	if base == "" {
		return fmt.Errorf("aei: run %s carries no gofer_url: the workload cannot reach gofer's agent plane", spec.RunID)
	}
	plane := agent.NewPlaneClient(base, rt.Credential(), nil)

	params := agent.Params{
		JobType:     spec.TaskRef,
		Provider:    "github",
		ItemID:      spec.Parameters["item"],
		Turn:        strings.TrimSpace(spec.Parameters["turn"]),
		Independent: strings.TrimSpace(spec.Parameters["independent"]) == "true",
		Logger:      log,
	}

	// The chat model's coordinates travel as non-secret run parameters; its token is
	// VENDED for this run (provider "ai"), so the sandbox never holds a standing model
	// secret. Without both, ranking falls back to the deterministic stub and the
	// converse and research jobs are disabled.
	endpoint, model := strings.TrimSpace(spec.Parameters["ai_endpoint"]), strings.TrimSpace(spec.Parameters["ai_model"])
	if endpoint != "" && model != "" {
		token, err := plane.Vend(ctx, "ai")
		if err != nil {
			return fmt.Errorf("vend model credential: %w", err)
		}
		cfg := llm.Config{Endpoint: endpoint, Model: model, Token: token, Logger: log}
		params.Model = model
		params.Ranker = llm.NewRanker(cfg)
		params.Converser = llm.NewConverser(cfg)
		params.Researcher = llm.NewResearchRanker(cfg)
		log.Info("chat model configured", "endpoint", endpoint, "model", model)
	} else {
		log.Info("chat model not configured; ranking with the deterministic stub")
	}

	return agent.Run(ctx, params, plane, plane)
}
