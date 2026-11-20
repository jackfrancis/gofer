package aei

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackfrancis/gofer/internal/ingest"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// In the model-gateway profile a run reaches the model through the gateway with its
// run credential and never vends a model token: the gateway, not the run, holds the
// upstream key.
func TestWorkBehindTheModelGatewayNeverVendsAModelToken(t *testing.T) {
	plane := newGoferPlane(t, []worklist.WorkItem{{
		ID:      "github:o/r#7",
		OwnerID: "github:1",
		Source:  "github",
		Type:    worklist.TypePullRequest,
		GitHub:  worklist.GitHubRef{Number: 7, Repo: "o/r", Title: "T", State: "open"},
	}})
	var calls int
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if got := r.Header.Get("Authorization"); got != "Bearer run-credential" {
			http.Error(w, "the gateway must be called with the run credential, got "+got, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{
				"content": `{"relevance":0.9,"impact":0.8,"engagement":0.7,"urgency":0.6,"confidence":0.5,"rationale":"r"}`,
			}}},
		})
	}))
	t.Cleanup(gateway.Close)

	rt := loadRun(t, "llm-rank", map[string]string{
		"owner":       "github:1",
		"gofer_url":   plane.srv.URL,
		"ai_endpoint": gateway.URL + "/v1/chat/completions",
		"ai_model":    "test-model",
		"ai_auth":     ingest.AIAuthGateway,
	})
	if err := work(context.Background(), rt); err != nil {
		t.Fatalf("work: %v", err)
	}
	if plane.vendedAI != 0 {
		t.Errorf("vended the model token %d times behind the gateway, want 0", plane.vendedAI)
	}
	if calls == 0 {
		t.Fatal("the run never called the model through the gateway")
	}
	if len(plane.written) != 1 || plane.written[0].Signals.Proposed == nil {
		t.Fatalf("written back = %+v, want the item with the model's proposal", plane.written)
	}
}

type countingVendor struct{ n int }

func (v *countingVendor) Vend(context.Context, string) (string, error) {
	v.n++
	return "vended", nil
}

func TestModelToken(t *testing.T) {
	ctx := context.Background()
	t.Run("default vends", func(t *testing.T) {
		v := &countingVendor{}
		tok, err := modelToken(ctx, "", "cred", v)
		if err != nil || tok != "vended" || v.n != 1 {
			t.Fatalf("= (%q, %v) after %d vends, want the vended token", tok, err, v.n)
		}
	})
	t.Run("gateway presents the run credential", func(t *testing.T) {
		v := &countingVendor{}
		tok, err := modelToken(ctx, ingest.AIAuthGateway, "cred", v)
		if err != nil || tok != "cred" || v.n != 0 {
			t.Fatalf("= (%q, %v) after %d vends, want the run credential and no vend", tok, err, v.n)
		}
	})
	t.Run("gateway without a run credential fails", func(t *testing.T) {
		if _, err := modelToken(ctx, ingest.AIAuthGateway, "", &countingVendor{}); err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("unknown ai_auth fails without vending", func(t *testing.T) {
		v := &countingVendor{}
		_, err := modelToken(ctx, "bearer", "cred", v)
		if err == nil || !strings.Contains(err.Error(), "ai_auth") || v.n != 0 {
			t.Fatalf("err = %v after %d vends, want a refusal naming ai_auth", err, v.n)
		}
	})
}
