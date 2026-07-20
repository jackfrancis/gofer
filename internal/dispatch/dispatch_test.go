package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackfrancis/agent-execution-interface/aei"
)

// Submit POSTs the run to the AEI dispatch API (/aei/v1alpha1/dispatch) as gofer's
// app, carrying the task, parameters, and the requested identity (subject +
// scopes; the AgentApp fixes the audience server-side), and returns the run id the
// control plane assigns. gofer embeds no control plane and no launcher — dispatch
// is an app-to-controller HTTP call.
func TestSubmitDispatchesToControlPlane(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/aei/v1alpha1/dispatch" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"runId":"run-123"}`))
	}))
	defer srv.Close()

	engine := New(Config{Endpoint: srv.URL, App: "gofer", Token: "sa-token"})
	runID, err := engine.Submit(context.Background(), aei.RunSpec{
		TaskRef:    "github-ingest",
		Parameters: map[string]string{"owner": "u1"},
		Identity: aei.IdentityRequest{
			Subject:  "u1",
			Scopes:   []string{"signals:read", "metadata:write"},
			Audience: "gofer-agent",
		},
		Deadline: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if runID != "run-123" {
		t.Fatalf("runID = %q, want run-123", runID)
	}
	if gotAuth != "Bearer sa-token" {
		t.Fatalf("dispatch used auth %q, want the projected SA token", gotAuth)
	}
	if gotBody["app"] != "gofer" || gotBody["taskRef"] != "github-ingest" {
		t.Fatalf("dispatch body app/taskRef wrong: %+v", gotBody)
	}
	id, _ := gotBody["identity"].(map[string]any)
	if id == nil || id["subject"] != "u1" {
		t.Fatalf("dispatch body identity wrong: %+v", gotBody["identity"])
	}
	// The client requests scopes but never the audience: the AgentApp policy fixes
	// it, so a run can narrow the grant but never widen or re-target it.
	if _, ok := id["audience"]; ok {
		t.Fatalf("dispatch body must not carry an audience, got %+v", id)
	}
}
