package agentsessions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/ingest"
	"github.com/jackfrancis/gofer/internal/vault"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// These tests drive the backend through gofer's real Ingestor rather than calling the
// Dispatcher directly, so they cover the whole seam: gofer builds the run intent,
// the backend executes it as an agentsessions session, and the run's journaled
// lifecycle comes back through Dispatcher.Status to the read model.

// A backfill dispatched by the Ingestor runs the workload and lands its results, and
// the run is not reported as failed.
func TestIngestorBackfillPopulatesWorklist(t *testing.T) {
	const owner = "github:1"
	srv := githubStub(t)
	d, store := backend(t, owner, srv.URL)
	ig := ingest.New(d, "gofer-agent", "", nil, "", "")

	if err := ig.EnsureBackfill(context.Background(), owner); err != nil {
		t.Fatalf("EnsureBackfill: %v", err)
	}
	waitFor(t, "the backfill to land", func() bool {
		items, err := store.List(context.Background(), owner)
		return err == nil && len(items) > 0
	})

	failed, msg, err := ig.BackfillFailure(context.Background(), owner)
	if err != nil {
		t.Fatalf("BackfillFailure: %v", err)
	}
	if failed {
		t.Fatalf("healthy backfill reported failed (%q)", msg)
	}
}

// A backfill whose run fails surfaces through BackfillFailure, so the read model can
// show the failure instead of spinning on "Discovering…" forever. This is the path
// that only works because the backend reports run lifecycle through Status.
func TestIngestorSurfacesFailedBackfill(t *testing.T) {
	const owner = "github:1"
	// No GitHub credential in the vault, so the run's vend fails.
	store := worklist.NewMemoryStore()
	d, err := New(Config{Vault: vault.NewMemoryVault(), Store: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	ig := ingest.New(d, "gofer-agent", "", nil, "", "")

	if err := ig.EnsureBackfill(context.Background(), owner); err != nil {
		t.Fatalf("EnsureBackfill: %v", err)
	}
	waitFor(t, "the failed run to surface", func() bool {
		failed, _, err := ig.BackfillFailure(context.Background(), owner)
		return err == nil && failed
	})

	_, msg, err := ig.BackfillFailure(context.Background(), owner)
	if err != nil || msg == "" {
		t.Fatalf("BackfillFailure = (%q, %v), want a failure message", msg, err)
	}
}

// The empty-worklist view polls EnsureBackfill on every render, so a healthy in-flight
// run must not be re-dispatched — against a backend that really executes runs, that
// would be a run storm.
func TestIngestorDoesNotStormTheBackend(t *testing.T) {
	const owner = "github:1"
	srv := githubStub(t)
	d, _ := backend(t, owner, srv.URL)
	ig := ingest.New(d, "gofer-agent", "", nil, "", "")

	for range 5 {
		if err := ig.EnsureBackfill(context.Background(), owner); err != nil {
			t.Fatalf("EnsureBackfill: %v", err)
		}
	}
	if got := d.sessionCount(); got != 1 {
		t.Fatalf("started %d sessions, want 1 (the in-flight run gates the rest)", got)
	}
}

// sessionCount reports how many sessions this dispatcher has started (in-memory
// journals), so a test can assert that dispatch is not repeated.
func (d *Dispatcher) sessionCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.logs)
}

// emptyGitHubStub serves a viewer with no open work at all.
func emptyGitHubStub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_, _ = w.Write([]byte(`{"login":"me","id":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A run that SUCCEEDS having found nothing must resolve as ready-and-empty, not as a
// perpetual "Discovering…". This is the end-to-end proof that the backend's journaled
// run lifecycle reaches the read model: the worklist is empty either way, so only the
// run's reported outcome can tell the two apart.
func TestIngestorEmptyBackfillResolvesReady(t *testing.T) {
	const owner = "github:1"
	srv := emptyGitHubStub(t)
	d, store := backend(t, owner, srv.URL)
	ig := ingest.New(d, "gofer-agent", "", nil, "", "")
	ctx := context.Background()

	if err := ig.EnsureBackfill(ctx, owner); err != nil {
		t.Fatalf("EnsureBackfill: %v", err)
	}
	waitFor(t, "the empty run to complete", func() bool {
		done, err := ig.BackfillSucceeded(ctx, owner)
		return err == nil && done
	})

	res, err := worklist.Resolve(ctx, store, ig, time.Now(), owner, worklist.DefaultSort, true)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Status != worklist.StatusReady {
		t.Fatalf("status = %q, want %q (the run finished and found nothing)", res.Status, worklist.StatusReady)
	}
	if len(res.Items) != 0 {
		t.Fatalf("want no items, got %d", len(res.Items))
	}
}
