package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	items, next, err := sink.Select(ctx, worklist.Selection{})
	if err != nil || len(items) != 1 || items[0].ID != "i1" || next != "" {
		t.Fatalf("Select = (%+v, %q, %v), want the one written item", items, next, err)
	}
	if it, ok, err := sink.Item(ctx, "i1", ""); err != nil || !ok || it.ID != "i1" {
		t.Fatalf("Item(i1) = (%+v, %v, %v), want it", it, ok, err)
	}
	// Another owner's sink sees nothing, and cannot read the item by ID.
	other := NewStoreSink(store, "u2")
	if got, _, err := other.Select(ctx, worklist.Selection{}); err != nil || len(got) != 0 {
		t.Fatalf("other owner Select = (%+v, %v), want empty", got, err)
	}
	if it, ok, err := other.Item(ctx, "i1", ""); err != nil || ok {
		t.Fatalf("other owner Item(i1) = (%+v, %v, %v), want not found", it, ok, err)
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
	var seenQuery url.Values
	var seenTurn string
	var posted struct {
		Items []worklist.WorkItem `json:"items"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		switch {
		case r.URL.Path == "/agent/credential":
			seenProvider = r.URL.Query().Get("provider")
			_, _ = w.Write([]byte(`{"access_token":"vended","token_type":"bearer"}`))
		case r.URL.Path == "/agent/worklist/select":
			seenQuery = r.URL.Query()
			_, _ = w.Write([]byte(`{"items":[{"id":"i1"}],"next":"i1"}`))
		case r.URL.Path == "/agent/worklist/item":
			seenTurn = r.URL.Query().Get("turn")
			if r.URL.Query().Get("id") == "i1" {
				_, _ = w.Write([]byte(`{"found":true,"item":{"id":"i1","thread":[{"role":"user","content":"hi"}]}}`))
				return
			}
			_, _ = w.Write([]byte(`{"found":false}`))
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

	items, next, err := c.Select(ctx, worklist.Selection{Open: true, After: "a", Limit: 5})
	if err != nil || len(items) != 1 || items[0].ID != "i1" || next != "i1" {
		t.Fatalf("Select = (%+v, %q, %v), want one item and its cursor", items, next, err)
	}
	if got := seenQuery.Encode(); got != "after=a&limit=5&open=1" {
		t.Fatalf("Select query = %q", got)
	}
	it, ok, err := c.Item(ctx, "i1", "t1")
	if err != nil || !ok || it.ID != "i1" || len(it.Thread) != 1 || seenTurn != "t1" {
		t.Fatalf("Item(i1) = (%+v, %v, %v), turn %q; want it with its thread", it, ok, err, seenTurn)
	}
	if _, ok, err := c.Item(ctx, "gone", ""); err != nil || ok {
		t.Fatalf("Item(gone) = (%v, %v), want not found", ok, err)
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
	if _, _, err := c.Select(context.Background(), worklist.Selection{}); err == nil {
		t.Fatal("expected an error for a 401 list")
	}
	// A 401 (or a 404 from a web tier without the route) is an error, never "gone".
	if _, _, err := c.Item(context.Background(), "i1", ""); err == nil {
		t.Fatal("expected an error for a 401 item read")
	}
	if err := c.Ingest(context.Background(), []worklist.WorkItem{{ID: "i1"}}); err == nil {
		t.Fatal("expected an error for a 401 ingest")
	}
	if err := c.Research(context.Background(), "i1", worklist.ResearchAdjustment{}); err == nil {
		t.Fatal("expected an error for a 401 research write")
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
