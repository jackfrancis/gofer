// Command runtime is gofer's agent runtime. Built on the AEI runtime SDK, it
// learns its run through the runtime ABI, vends the acting user's GitHub
// credential on demand, runs the agent pipeline (fetch → build work items →
// score), writes the results back to gofer's agent sink, and reports completion
// idempotently. The same binary runs on every AEI substrate.
//
// The run credential is gofer's audience-bound Ed25519 token (minted by the AEI
// control plane, which is configured with gofer's Ed25519 authority — ADR 0002),
// so one credential authenticates both the AEI ABI (/complete to the control
// plane) and gofer's domain plane (GET /agent/credential to vend the GitHub token,
// /agent/worklist to read and write results), which gofer's own authn verifies
// with only the public key.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackfrancis/agent-execution-interface/aeiruntime"
	"github.com/jackfrancis/gofer/internal/agent"
	"github.com/jackfrancis/gofer/internal/llm"
	"github.com/jackfrancis/gofer/internal/worklist"
)

func main() {
	aeiruntime.Main(os.Getenv, nil, run)
}

// run dispatches on the run's TaskRef (github-ingest, llm-rank, github-converse)
// through agent.Run, vending the GitHub credential via the AEI broker and writing
// results back to gofer's agent sink.
func run(ctx context.Context, rt *aeiruntime.Runtime) error {
	spec := rt.Spec()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	// The agent pipeline logs its phases through slog.Default(); route those to the
	// same JSON handler so fetch/enrich/rank/complete are visible in the run logs.
	slog.SetDefault(log)
	log.Info("gofer runtime starting", "run", spec.RunID, "task", spec.TaskRef, "owner", spec.Parameters["owner"])

	var (
		vendor agent.Vendor
		sink   agent.Sink
	)
	if base := spec.Parameters["gofer_url"]; base != "" {
		gc := &goferClient{base: strings.TrimRight(base, "/"), token: rt.Credential(), client: &http.Client{Timeout: 30 * time.Second}}
		vendor, sink = gc, gc
	} else {
		// No gofer URL in the run parameters: a smoke run that can neither vend nor
		// persist. Vend fails clearly and results are only logged.
		vendor, sink = errVendor{}, &logSink{log: log}
	}

	// The rank phase (chained by the backfill) and the llm-rank/github-converse/
	// github-research jobs need a chat model. Its coordinates travel as non-secret
	// run parameters (ai_endpoint, ai_model); the token is vended from gofer's
	// broker per run (provider "ai"), so the sandbox never holds a standing model
	// secret. With no coordinates the ranker falls back to the deterministic stub
	// and the converser/researcher are disabled.
	var ranker worklist.AxisRanker
	var converser worklist.Conversationalist
	var researcher worklist.ResearchRanker
	aiEndpoint := strings.TrimSpace(spec.Parameters["ai_endpoint"])
	aiModel := strings.TrimSpace(spec.Parameters["ai_model"])
	if aiEndpoint != "" && aiModel != "" {
		token, err := vendor.Vend(ctx, "ai")
		if err != nil {
			return fmt.Errorf("vend ai credential: %w", err)
		}
		aicfg := llm.Config{Endpoint: aiEndpoint, Model: aiModel, Token: token, Logger: log}
		ranker = llm.NewRanker(aicfg)
		converser = llm.NewConverser(aicfg)
		researcher = llm.NewResearchRanker(aicfg)
		log.Info("chat model configured", "endpoint", aiEndpoint, "model", aiModel)
	} else {
		log.Info("chat model not configured; ranking with the deterministic stub")
	}

	return agent.Run(ctx, agent.Params{
		JobType:     spec.TaskRef,
		Provider:    "github",
		ItemID:      spec.Parameters["item"],
		Model:       aiModel,
		Ranker:      ranker,
		Converser:   converser,
		Independent: strings.TrimSpace(spec.Parameters["independent"]) == "true",
		Researcher:  researcher,
		Logger:      log,
	}, vendor, sink)
}

// goferClient talks to gofer's agent plane, authenticating every call with the run
// credential (gofer's audience-bound Ed25519 token, ADR 0002). It is BOTH the
// runtime's credential broker (GET /agent/credential — replacing AEI /vend for
// gofer's delegated tokens, since the control plane on the aei-controller cannot
// reach gofer's vault) and its worklist sink (GET/POST /agent/worklist).
type goferClient struct {
	base   string
	token  string
	client *http.Client
}

// Vend fetches the acting user's delegated provider token from gofer. The runtime
// holds it for the run only — never a standing secret.
func (c *goferClient) Vend(ctx context.Context, provider string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/agent/credential?provider="+url.QueryEscape(provider), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("vend credential: status %d", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", errors.New("vend credential: empty token")
	}
	return out.AccessToken, nil
}

func (c *goferClient) List(ctx context.Context) ([]worklist.WorkItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/agent/worklist", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list worklist: status %d", resp.StatusCode)
	}
	var out struct {
		Items []worklist.WorkItem `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

func (c *goferClient) Ingest(ctx context.Context, items []worklist.WorkItem) error {
	body, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/agent/worklist", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ingest: status %d", resp.StatusCode)
	}
	return nil
}

// errVendor is the no-URL fallback vendor: it cannot reach gofer, so it fails
// clearly rather than silently returning an empty credential.
type errVendor struct{}

func (errVendor) Vend(context.Context, string) (string, error) {
	return "", errors.New("no gofer_url in run parameters: cannot vend a credential")
}

// logSink is the no-URL fallback: it logs the fetched item count without
// persisting.
type logSink struct{ log *slog.Logger }

func (s *logSink) List(context.Context) ([]worklist.WorkItem, error) { return nil, nil }

func (s *logSink) Ingest(_ context.Context, items []worklist.WorkItem) error {
	s.log.Info("runtime fetched work items (no sink URL; not persisted)", "count", len(items))
	return nil
}
