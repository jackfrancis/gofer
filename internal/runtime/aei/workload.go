package aei

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/jackfrancis/agent-execution-interface/aeiruntime"

	"github.com/jackfrancis/gofer/internal/agent"
	"github.com/jackfrancis/gofer/internal/ingest"
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
// measured adds the run's model time and model and tool calls to that report.
func RunWorkload() {
	aeiruntime.Main(os.Getenv, nil, measured(work))
}

// measured wraps fn so the SDK's completion report carries what the run spent on the
// model. It attaches an llm.Collector to the run's context and, when fn returns, hands
// the totals to rt.SetModelTiming, which the SDK merges into the one terminal report
// it sends (with provisioning and runtime seconds, which it measures itself). gofer
// never calls rt.Complete: the SDK owns delivery, its retry budget and, on a durable
// actor, releasing the concurrency slot once that report is acknowledged.
func measured(fn func(context.Context, *aeiruntime.Runtime) error) func(context.Context, *aeiruntime.Runtime) error {
	return func(ctx context.Context, rt *aeiruntime.Runtime) error {
		ctx, collector := llm.WithCollector(ctx)
		runErr := fn(ctx, rt)
		seconds := collector.ModelDuration().Seconds()
		rt.SetModelTiming(seconds, collector.ModelCalls(), collector.ToolCalls())
		slog.Default().Info("gofer runtime finished", "run", rt.Spec().RunID, "ok", runErr == nil,
			"model_seconds", seconds, "model_calls", collector.ModelCalls(), "tool_calls", collector.ToolCalls())
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
		Attempt:     strings.TrimSpace(spec.Parameters["attempt"]),
		Bots:        splitBots(spec.Parameters["bots"]),
		Logger:      log,
	}

	// The chat model's coordinates travel as non-secret run parameters; its token is
	// VENDED for this run (provider "ai"), so the sandbox never holds a standing model
	// secret — or, behind gofer's model gateway, the run never holds a model token at
	// all (modelToken). Without both coordinates, ranking falls back to the
	// deterministic stub and the converse and research jobs are disabled.
	endpoint, model := strings.TrimSpace(spec.Parameters["ai_endpoint"]), strings.TrimSpace(spec.Parameters["ai_model"])
	if endpoint != "" && model != "" {
		token, err := modelToken(ctx, spec.Parameters["ai_auth"], rt.Credential(), plane)
		if err != nil {
			return err
		}
		cfg := llm.Config{Endpoint: endpoint, Model: model, Token: token, Logger: log}
		params.Model = model
		params.Ranker = llm.NewRanker(cfg)
		params.Converser = llm.NewConverser(cfg)
		params.Researcher = llm.NewResearchRanker(cfg)
		params.TurnReader = llm.NewTurnReader(cfg)
		log.Info("chat model configured", "endpoint", endpoint, "model", model)
	} else {
		log.Info("chat model not configured; ranking with the deterministic stub")
	}

	return agent.Run(ctx, params, plane, plane)
}

// modelToken returns the bearer the run's model client sends. By default it vends the
// chat-model token from gofer's broker (provider "ai"). With ai_auth=gateway the
// model is behind gofer's model gateway, which holds the upstream key: the run
// presents its own run credential, which the gateway verifies with gofer before it
// attaches the key upstream, so the run never receives a model key. An unknown
// ai_auth fails the run rather than guessing where a credential may be sent.
func modelToken(ctx context.Context, aiAuth, runCredential string, v agent.Vendor) (string, error) {
	switch strings.TrimSpace(aiAuth) {
	case "":
		token, err := v.Vend(ctx, "ai")
		if err != nil {
			return "", fmt.Errorf("vend model credential: %w", err)
		}
		return token, nil
	case ingest.AIAuthGateway:
		if runCredential == "" {
			return "", errors.New("aei: the model gateway needs the run credential, and this run has none")
		}
		return runCredential, nil
	default:
		return "", fmt.Errorf("aei: unknown ai_auth %q", aiAuth)
	}
}

// splitBots parses run parameter bots: the web tier's BOT_REVIEWERS, comma-separated.
func splitBots(v string) []string {
	var out []string
	for _, b := range strings.Split(v, ",") {
		if b = strings.TrimSpace(b); b != "" {
			out = append(out, b)
		}
	}
	return out
}
