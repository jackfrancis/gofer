package aei

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackfrancis/gofer/internal/ingest"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// These tests drive the backend through gofer's real Ingestor rather than calling the
// Dispatcher directly, so they cover the whole seam: gofer builds the run intent, the
// backend dispatches it to the control plane, and the run's lifecycle comes back
// through Dispatcher.Status to the read model.

// ingestor wires gofer's Ingestor to the backend under test.
func ingestor(t *testing.T, c *controlPlane, sinkURL string) *ingest.Ingestor {
	t.Helper()
	return ingest.New(backend(t, c), "gofer-agent", sinkURL, nil, "", "")
}

// A backfill dispatched by the Ingestor arrives at the control plane as a scoped,
// deadline-bounded github-ingest run that tells the workload where to call gofer back.
func TestIngestorBackfillDispatchesTheRunIntent(t *testing.T) {
	c := newControlPlane(t)
	ig := ingestor(t, c, "http://gofer.gofer.svc:8080")

	if err := ig.EnsureBackfill(context.Background(), "github:1"); err != nil {
		t.Fatalf("EnsureBackfill: %v", err)
	}
	if c.got.TaskRef != "github-ingest" {
		t.Errorf("task ref = %q, want github-ingest", c.got.TaskRef)
	}
	if c.got.Parameters["gofer_url"] != "http://gofer.gofer.svc:8080" {
		t.Errorf("gofer_url = %q", c.got.Parameters["gofer_url"])
	}
	// Least privilege: source signals are read, only gofer's own metadata is written.
	if len(c.got.Identity.Scopes) != 2 {
		t.Errorf("scopes = %v, want signals:read + metadata:write", c.got.Identity.Scopes)
	}
	if c.got.TimeoutSeconds == 0 {
		t.Error("run dispatched with no deadline")
	}
}

// A run that fails on the substrate surfaces through BackfillFailure, so the read
// model can show the failure instead of spinning on "Discovering…" forever. This is
// the path that only works because the backend reports run lifecycle through Status.
func TestIngestorSurfacesFailedRun(t *testing.T) {
	c := newControlPlane(t)
	c.phase, c.msg = "Failed", "runtime: vend github credential: status 401"
	ig := ingestor(t, c, "")

	if err := ig.EnsureBackfill(context.Background(), "github:1"); err != nil {
		t.Fatalf("EnsureBackfill: %v", err)
	}
	failed, msg, err := ig.BackfillFailure(context.Background(), "github:1")
	if err != nil {
		t.Fatalf("BackfillFailure: %v", err)
	}
	if !failed || msg != c.msg {
		t.Fatalf("failure = %v (%q), want the run's failure reported", failed, msg)
	}
}

// The empty worklist is polled on every render of the "Discovering…" view, so the
// Ingestor must not dispatch a fresh run each time: a real substrate would run a storm
// of pods for one user.
func TestEnsureBackfillDoesNotStormTheControlPlane(t *testing.T) {
	c := newControlPlane(t)
	ig := ingestor(t, c, "")

	for range 5 {
		if err := ig.EnsureBackfill(context.Background(), "github:1"); err != nil {
			t.Fatalf("EnsureBackfill: %v", err)
		}
	}
	if c.runs != 1 {
		t.Fatalf("dispatched %d runs for one backfill, want 1", c.runs)
	}
}

// The backend's two halves compose: what gofer dispatches is what the launched
// workload receives. A stand-in launcher injects the dispatched spec through the
// runtime ABI and runs the workload, which reaches gofer on the URL the run carried
// and writes its results back. (It runs synchronously here; a real substrate launches
// it out-of-process, which is why the run intent has to be self-contained.)
func TestDispatchedRunReachesTheWorkload(t *testing.T) {
	plane := newGoferPlane(t, []worklist.WorkItem{{ID: "github:o/r#7", OwnerID: "github:1", Source: "github"}})

	var launched error
	mux := http.NewServeMux()
	mux.HandleFunc("POST /aei/v1alpha1/dispatch", func(w http.ResponseWriter, r *http.Request) {
		var spec dispatchBody
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		launched = work(r.Context(), loadRun(t, spec.TaskRef, spec.Parameters))
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"runId": "run-1"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	d, err := New(Config{Endpoint: srv.URL, App: "gofer", Token: "sa-token"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := d.Submit(context.Background(), ingest.RunSpec{
		TaskRef:    "llm-rank",
		Parameters: map[string]string{"owner": "github:1", "gofer_url": plane.srv.URL},
		Subject:    "github:1",
		Scopes:     []string{"signals:read", "metadata:write"},
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if launched != nil {
		t.Fatalf("the launched workload failed: %v", launched)
	}
	if len(plane.written) != 1 {
		t.Fatalf("the workload wrote back %d items, want 1", len(plane.written))
	}
}
