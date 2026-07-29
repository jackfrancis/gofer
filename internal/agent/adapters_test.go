package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackfrancis/gofer/internal/vault"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// The in-process vendor reads the acting user's credential from the vault and brokers
// the shared chat-model token as the pseudo-provider "ai" — the same policy the agent
// plane serves over HTTP.
func TestVaultVendor(t *testing.T) {
	ctx := context.Background()
	v := vault.NewMemoryVault()
	if err := v.Put(ctx, "u1", vault.Credential{Provider: "github", AccessToken: "gh-tok"}); err != nil {
		t.Fatal(err)
	}
	vendor := NewVaultVendor(v, "u1", "ai-tok")

	if got, err := vendor.Vend(ctx, "github"); err != nil || got != "gh-tok" {
		t.Fatalf("Vend(github) = (%q, %v), want gh-tok", got, err)
	}
	if got, err := vendor.Vend(ctx, "ai"); err != nil || got != "ai-tok" {
		t.Fatalf("Vend(ai) = (%q, %v), want ai-tok", got, err)
	}
	// A provider the user has no credential for fails rather than returning empty.
	if _, err := vendor.Vend(ctx, "gitlab"); err == nil {
		t.Fatal("expected an error vending an absent credential")
	}
	// With no chat-model token configured, "ai" is unavailable.
	if _, err := NewVaultVendor(v, "u1", "").Vend(ctx, "ai"); err == nil {
		t.Fatal("expected an error vending ai with no token configured")
	}
}

// The in-process sink scopes every read and write to the run's owner.
func TestStoreSinkScopesToOwner(t *testing.T) {
	ctx := context.Background()
	store := worklist.NewMemoryStore()
	sink := NewStoreSink(store, "u1")

	if err := sink.Ingest(ctx, []worklist.WorkItem{{ID: "i1", OwnerID: "u1"}}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	items, err := sink.List(ctx)
	if err != nil || len(items) != 1 || items[0].ID != "i1" {
		t.Fatalf("List = (%+v, %v), want the one written item", items, err)
	}
	// Another owner's sink sees nothing.
	other, err := NewStoreSink(store, "u2").List(ctx)
	if err != nil || len(other) != 0 {
		t.Fatalf("other owner List = (%+v, %v), want empty", other, err)
	}
	// An empty write is a no-op.
	if err := sink.Ingest(ctx, nil); err != nil {
		t.Fatalf("empty Ingest: %v", err)
	}
}

// The out-of-process client is both Vendor and Sink over gofer's agent plane, and
// authenticates every call with the run credential.
func TestPlaneClient(t *testing.T) {
	var seenAuth, seenProvider string
	var posted struct {
		Items []worklist.WorkItem `json:"items"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		switch {
		case r.URL.Path == "/agent/credential":
			seenProvider = r.URL.Query().Get("provider")
			_, _ = w.Write([]byte(`{"access_token":"vended","token_type":"bearer"}`))
		case r.URL.Path == "/agent/worklist" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"items":[{"id":"i1"}]}`))
		case r.URL.Path == "/agent/worklist" && r.Method == http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(&posted)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	c := NewPlaneClient(srv.URL, "run-cred", srv.Client())

	tok, err := c.Vend(ctx, "github")
	if err != nil || tok != "vended" {
		t.Fatalf("Vend = (%q, %v), want vended", tok, err)
	}
	if seenAuth != "Bearer run-cred" {
		t.Fatalf("Authorization = %q, want the run credential", seenAuth)
	}
	if seenProvider != "github" {
		t.Fatalf("provider = %q, want github", seenProvider)
	}

	items, err := c.List(ctx)
	if err != nil || len(items) != 1 || items[0].ID != "i1" {
		t.Fatalf("List = (%+v, %v), want one item", items, err)
	}

	if err := c.Ingest(ctx, []worklist.WorkItem{{ID: "i2"}}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(posted.Items) != 1 || posted.Items[0].ID != "i2" {
		t.Fatalf("posted %+v, want the one written item", posted.Items)
	}
	// An empty write never leaves the process.
	if err := c.Ingest(ctx, nil); err != nil {
		t.Fatalf("empty Ingest: %v", err)
	}
}

// A non-2xx from the agent plane is an error, not a silent empty result.
func TestPlaneClientSurfacesHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewPlaneClient(srv.URL, "bad", srv.Client())
	if _, err := c.Vend(context.Background(), "github"); err == nil {
		t.Fatal("expected an error for a 401 vend")
	}
	if _, err := c.List(context.Background()); err == nil {
		t.Fatal("expected an error for a 401 list")
	}
	if err := c.Ingest(context.Background(), []worklist.WorkItem{{ID: "i1"}}); err == nil {
		t.Fatal("expected an error for a 401 ingest")
	}
}

// Both shapes satisfy the workload's seams, so agent.Run is indifferent to which a
// backend supplies.
func TestAdaptersSatisfySeams(t *testing.T) {
	var (
		_ Vendor = (*VaultVendor)(nil)
		_ Sink   = (*StoreSink)(nil)
		_ Vendor = (*PlaneClient)(nil)
		_ Sink   = (*PlaneClient)(nil)
	)
}
