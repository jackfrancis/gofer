package agentsessions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aramase/agentsessions/api"

	"github.com/jackfrancis/gofer/internal/agent"
	"github.com/jackfrancis/gofer/internal/ingest"
	"github.com/jackfrancis/gofer/internal/llm"
)

// harnessID identifies gofer's workload harness to the runtime.
const harnessID = "gofer-workload"

// harness is gofer's agent workload expressed as an agentsessions api.Harness: one
// Run is one execution of one gofer job (github-ingest, llm-rank, github-converse, …).
// It reads its parameters from the run spec rather than from Start.Inputs; the journal
// carries the same intent as the turn's INPUT event so the log stays self-describing.
type harness struct {
	spec ingest.RunSpec
	cfg  Config
	log  *slog.Logger
}

var _ api.Harness = (*harness)(nil)

// Describe reports the static contract used to place the harness on a runtime. It
// holds no in-memory state between turns — every run re-derives everything from
// gofer's store — so it declares STATELESS_REPLAY and runs on any backend, including a
// plain pod. It is NOT ForkSafe: the workload performs external side effects (GitHub
// reads and worklist writes) directly rather than through host-mediated events, so a
// fork would repeat them rather than replay them.
func (h *harness) Describe(context.Context) (api.Descriptor, error) {
	d := api.Descriptor{
		ID: harnessID,
		Capabilities: api.Capabilities{
			Resumability: api.ResumabilityStatelessReplay,
			ForkSafe:     false,
		},
	}
	if m := strings.TrimSpace(h.spec.Parameters["ai_model"]); m != "" {
		d.Models = []string{m}
	}
	return d, nil
}

// Run executes one gofer job for the run's owner and records a summary as the turn's
// output. The workload runs IN-PROCESS here, so it reaches gofer through the adapters
// gofer supplies (agent.NewVaultVendor / agent.NewStoreSink); a substrate that runs it
// out-of-process would swap in agent.NewPlaneClient and change nothing else.
func (h *harness) Run(ctx context.Context, _ *api.Start, sink api.EventSink) error {
	p := h.spec.Parameters
	owner := p["owner"]
	if owner == "" {
		return errors.New("agentsessions: run spec carries no owner")
	}
	vendor := agent.NewVaultVendor(h.cfg.Vault, owner, h.cfg.AIToken)
	workSink := agent.NewStoreSink(h.cfg.Store, owner)

	params := agent.Params{
		JobType:       h.spec.TaskRef,
		Provider:      "github",
		GitHubBaseURL: h.cfg.GitHubBaseURL,
		ItemID:        p["item"],
		Independent:   strings.TrimSpace(p["independent"]) == "true",
		Logger:        h.log,
	}

	// The chat model's coordinates travel as non-secret run parameters; its token is
	// VENDED for this run (provider "ai"), so the workload never holds a standing model
	// secret. Without both, ranking falls back to the deterministic stub and the
	// converse/research jobs are disabled.
	endpoint, model := strings.TrimSpace(p["ai_endpoint"]), strings.TrimSpace(p["ai_model"])
	if endpoint != "" && model != "" {
		token, err := vendor.Vend(ctx, "ai")
		if err != nil {
			return fmt.Errorf("vend model credential: %w", err)
		}
		cfg := llm.Config{Endpoint: endpoint, Model: model, Token: token, Logger: h.log}
		params.Model = model
		params.Ranker = llm.NewRanker(cfg)
		params.Converser = llm.NewConverser(cfg)
		params.Researcher = llm.NewResearchRanker(cfg)
	}

	if err := agent.Run(ctx, params, vendor, workSink); err != nil {
		return err
	}
	return sink.Output(summary(h.spec.TaskRef, owner, p["item"]))
}

// summary is the human-readable result recorded as the turn's output.
func summary(taskRef, owner, item string) string {
	if item != "" {
		return fmt.Sprintf("%s completed for %s (item %s)", taskRef, owner, item)
	}
	return fmt.Sprintf("%s completed for %s", taskRef, owner)
}
