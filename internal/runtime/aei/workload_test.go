package aei

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackfrancis/agent-execution-interface/aeiruntime"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// These tests drive the WORKLOAD half — what an AEI substrate launches per run. The
// workload runs out-of-process, so its whole view of gofer is the agent plane: it
// vends with the run credential and reads and writes the worklist over HTTP.

// goferPlane is a fake of gofer's /agent/* plane: the credential broker and the
// worklist sink. It records the bearer it was called with and the items written back.
type goferPlane struct {
	srv      *httptest.Server
	bearers  map[string]bool
	items    []worklist.WorkItem
	written  []worklist.WorkItem
	vendedAI int
}

func newGoferPlane(t *testing.T, items []worklist.WorkItem) *goferPlane {
	t.Helper()
	p := &goferPlane{bearers: map[string]bool{}, items: items}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent/credential", func(w http.ResponseWriter, r *http.Request) {
		p.bearers[r.Header.Get("Authorization")] = true
		if r.URL.Query().Get("provider") == "ai" {
			p.vendedAI++
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "vended-" + r.URL.Query().Get("provider")})
	})
	mux.HandleFunc("GET /agent/worklist", func(w http.ResponseWriter, r *http.Request) {
		p.bearers[r.Header.Get("Authorization")] = true
		_ = json.NewEncoder(w).Encode(map[string]any{"items": p.items})
	})
	mux.HandleFunc("POST /agent/worklist", func(w http.ResponseWriter, r *http.Request) {
		p.bearers[r.Header.Get("Authorization")] = true
		var body struct {
			Items []worklist.WorkItem `json:"items"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p.written = append(p.written, body.Items...)
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

// modelStub answers the chat-completions call the ranker makes with a strict axes
// document, so a run can be driven end to end without a real model.
func modelStub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer vended-ai" {
			http.Error(w, "the model must be called with the VENDED token, got "+got, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{
				"content": `{"relevance":0.9,"impact":0.8,"engagement":0.7,"urgency":0.6,"confidence":0.5,"rationale":"blocked on you"}`,
			}}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// loadRun builds the runtime's view of a run exactly as a launcher injects it (the
// AEI runtime ABI): the run id, the workload, its parameters, and the run credential.
func loadRun(t *testing.T, taskRef string, params map[string]string) *aeiruntime.Runtime {
	t.Helper()
	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("encode params: %v", err)
	}
	env := map[string]string{
		aeiruntime.EnvRunID:      "run-1",
		aeiruntime.EnvTaskRef:    taskRef,
		aeiruntime.EnvParams:     string(encoded),
		aeiruntime.EnvControlURL: "http://aei-controller.invalid",
		aeiruntime.EnvCredential: "run-credential",
	}
	rt, err := aeiruntime.Load(context.Background(), func(k string) string { return env[k] }, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return rt
}

// A dispatched run executes gofer's workload against the agent plane and writes its
// results back — the end-to-end proof that the out-of-process shape works: the run
// credential authenticates every call, the model token is vended per run, and the
// decorated item lands in gofer's store.
func TestWorkRunsTheWorkloadAgainstTheAgentPlane(t *testing.T) {
	plane := newGoferPlane(t, []worklist.WorkItem{{
		ID:      "github:o/r#7",
		OwnerID: "github:1",
		Source:  "github",
		Type:    worklist.TypePullRequest,
		GitHub:  worklist.GitHubRef{Number: 7, Repo: "o/r", Title: "T", State: "open"},
	}})
	model := modelStub(t)

	rt := loadRun(t, "llm-rank", map[string]string{
		"owner":       "github:1",
		"gofer_url":   plane.srv.URL,
		"ai_endpoint": model.URL + "/chat/completions",
		"ai_model":    "test-model",
	})

	if err := work(context.Background(), rt); err != nil {
		t.Fatalf("work: %v", err)
	}
	if plane.vendedAI != 1 {
		t.Errorf("vended the model token %d times, want 1 (per run, never a standing secret)", plane.vendedAI)
	}
	if len(plane.written) != 1 {
		t.Fatalf("wrote back %d items, want 1", len(plane.written))
	}
	if plane.written[0].Signals.Proposed == nil {
		t.Fatal("item written back without the model's proposal")
	}
	if got := plane.written[0].Signals.Proposed.Relevance; got != 0.9 {
		t.Errorf("proposed relevance = %v, want 0.9", got)
	}
	// Every agent-plane call carries the run credential the control plane minted, and
	// nothing else.
	for bearer := range plane.bearers {
		if bearer != "Bearer run-credential" {
			t.Errorf("agent-plane call authenticated with %q", bearer)
		}
	}
}

// Without a model the workload still runs: ranking falls back to the deterministic
// stub, so a run degrades rather than failing when no chat model is configured.
func TestWorkWithoutModelCoordinatesUsesTheStub(t *testing.T) {
	plane := newGoferPlane(t, []worklist.WorkItem{{ID: "github:o/r#7", OwnerID: "github:1", Source: "github"}})
	rt := loadRun(t, "llm-rank", map[string]string{"owner": "github:1", "gofer_url": plane.srv.URL})

	if err := work(context.Background(), rt); err != nil {
		t.Fatalf("work: %v", err)
	}
	if plane.vendedAI != 0 {
		t.Errorf("vended a model token with no model configured (%d times)", plane.vendedAI)
	}
}

// A run that cannot reach gofer can neither vend nor persist, so it fails loudly
// rather than reporting a success that wrote nothing.
func TestWorkWithoutSinkURLFails(t *testing.T) {
	rt := loadRun(t, "github-ingest", map[string]string{"owner": "github:1"})

	err := work(context.Background(), rt)
	if err == nil {
		t.Fatal("want an error when the run carries no gofer_url")
	}
	if !strings.Contains(err.Error(), "gofer_url") {
		t.Errorf("error = %v, want it to name the missing gofer_url", err)
	}
}
