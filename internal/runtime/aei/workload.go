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

// RunWorkload is gofer's agent runtime entrypoint (cmd/runtime). It hands control to
// the AEI runtime SDK, which loads the run, executes work under the run's deadline,
// and reports the terminal outcome — one shot on a workload substrate, or a standing
// actor loop on a durable one, selected by the platform rather than by this code.
func RunWorkload() {
	aeiruntime.Main(os.Getenv, nil, measured(work))
}

// measured wraps fn so the run reports what it spent on the model. The SDK's own
// completion report carries only provisioning and runtime seconds, so the wrapper
// attaches an llm.Collector to the run's context, times the run itself, and reports
// the terminal outcome with the full Timing (model seconds, model calls, tool calls)
// before handing fn's result back. The control plane keeps the first terminal report
// it receives for a run, so the SDK's report after this one is a no-op; if this one
// fails to land, the SDK's still reports the outcome, without the model numbers.
func measured(fn func(context.Context, *aeiruntime.Runtime) error) func(context.Context, *aeiruntime.Runtime) error {
	return func(ctx context.Context, rt *aeiruntime.Runtime) error {
		start := time.Now()
		ctx, collector := llm.WithCollector(ctx)
		runErr := fn(ctx, rt)

		timing := aeiruntime.Timing{
			RuntimeSeconds: time.Since(start).Seconds(),
			ModelSeconds:   collector.ModelDuration().Seconds(),
			ModelCalls:     collector.ModelCalls(),
			ToolCalls:      collector.ToolCalls(),
		}
		// Provisioning is dispatch-start to work-start, measured the way the SDK does.
		if at := rt.Spec().DispatchedAt; !at.IsZero() {
			if d := start.Sub(at); d > 0 {
				timing.ProvisioningSeconds = d.Seconds()
			}
		}
		log := slog.Default() // work routes it to the run's JSON handler
		log.Info("gofer runtime finished", "run", rt.Spec().RunID, "ok", runErr == nil,
			"runtime_seconds", timing.RuntimeSeconds, "model_seconds", timing.ModelSeconds,
			"model_calls", timing.ModelCalls, "tool_calls", timing.ToolCalls)

		// A fresh context, so the report still lands after the run's deadline cancelled ctx.
		reportCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := rt.Complete(reportCtx, runErr, timing, ""); err != nil {
			log.Warn("completion report with model timing failed; the SDK reports the outcome without it", "run", rt.Spec().RunID, "err", err)
		}
		return runErr
	}
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
