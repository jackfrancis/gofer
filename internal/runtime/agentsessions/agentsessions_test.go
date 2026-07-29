package agentsessions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/ingest"
	"github.com/jackfrancis/gofer/internal/vault"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// githubStub serves the minimum GitHub API surface a github-ingest run touches: the
// viewer lookup and the issue search.
func githubStub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_, _ = w.Write([]byte(`{"login":"me","id":1}`))
			return
		}
		if strings.Contains(r.URL.Query().Get("q"), "review-requested:@me") {
			_, _ = w.Write([]byte(`{"items":[{"number":7,"title":"T","html_url":"u","state":"open","repository_url":"https://api.github.com/repos/o/r","pull_request":{"url":"x"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// backend builds a dispatcher wired to an in-memory vault + store, with the acting
// user's GitHub credential already vaulted (as the OAuth login would leave it).
func backend(t *testing.T, ownerID, githubBaseURL string) (*Dispatcher, worklist.Store) {
	t.Helper()
	vlt := vault.NewMemoryVault()
	if err := vlt.Put(context.Background(), ownerID, vault.Credential{
		Provider: "github", AccessToken: "tok", TokenType: "bearer",
	}); err != nil {
		t.Fatalf("seed vault: %v", err)
	}
	store := worklist.NewMemoryStore()
	d, err := New(Config{Vault: vlt, Store: store, GitHubBaseURL: githubBaseURL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d, store
}

// waitFor polls until cond holds or the deadline passes — runs execute asynchronously,
// because gofer's dispatch is fire-and-forget.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A submitted run executes gofer's workload on agentsessions and lands its results in
// gofer's store — the end-to-end proof that the backend satisfies the dispatch seam.
func TestSubmitRunsWorkloadAndPopulatesWorklist(t *testing.T) {
	const owner = "github:1"
	srv := githubStub(t)
	d, store := backend(t, owner, srv.URL)

	// A realistic deadline: the workload's chained enrich and rank stages reserve part
	// of the run's remaining budget, and skip themselves under a short one.
	runID, err := d.Submit(context.Background(), ingest.RunSpec{
		TaskRef:    "github-ingest",
		Parameters: map[string]string{"owner": owner},
		Subject:    owner,
		Scopes:     []string{"signals:read", "metadata:write"},
		Deadline:   5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if runID == "" {
		t.Fatal("Submit returned an empty run id")
	}

	waitFor(t, "the worklist to be populated", func() bool {
		items, err := store.List(context.Background(), owner)
		return err == nil && len(items) > 0
	})

	items, err := store.List(context.Background(), owner)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || items[0].GitHub.Number != 7 {
		t.Fatalf("worklist = %+v, want the one fetched item (#7)", items)
	}

	// The run's lifecycle is read back from the session journal, not tracked in memory.
	waitFor(t, "the run to report Succeeded", func() bool {
		st, err := d.Status(context.Background(), runID)
		return err == nil && st.Phase == "Succeeded"
	})
}

// A run whose workload fails is journaled as a failure and reported through Status.
func TestFailedRunReportsFailedStatus(t *testing.T) {
	const owner = "github:1"
	// No GitHub credential in the vault: the vend fails, so the workload errors.
	store := worklist.NewMemoryStore()
	d, err := New(Config{Vault: vault.NewMemoryVault(), Store: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	runID, err := d.Submit(context.Background(), ingest.RunSpec{
		TaskRef:    "github-ingest",
		Parameters: map[string]string{"owner": owner},
		Deadline:   30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitFor(t, "the run to report Failed", func() bool {
		st, err := d.Status(context.Background(), runID)
		return err == nil && st.Phase == "Failed"
	})
}

// A run with no task ref is rejected before a session is started.
func TestSubmitRejectsSpecWithoutTaskRef(t *testing.T) {
	d, _ := backend(t, "github:1", "")
	if _, err := d.Submit(context.Background(), ingest.RunSpec{Parameters: map[string]string{"owner": "github:1"}}); err == nil {
		t.Fatal("expected an error for a spec with no task ref")
	}
}

// The backend satisfies gofer's dispatch seam.
func TestImplementsDispatcher(t *testing.T) {
	var _ ingest.Dispatcher = (*Dispatcher)(nil)
}
