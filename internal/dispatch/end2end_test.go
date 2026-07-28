package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/api"
	"github.com/jackfrancis/gofer/internal/authn"
	"github.com/jackfrancis/gofer/internal/identity"
	"github.com/jackfrancis/gofer/internal/principal"
	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/vault"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// One credential, both planes: the same gofer Ed25519 run token authenticates
// gofer's credential broker (GET /agent/credential — vending the user's GitHub
// token) AND its worklist sink (POST /agent/worklist). In production the AEI
// control plane mints this token with gofer's Ed25519 authority (ADR 0002); here
// an identity.Authority stands in for the control plane's minter. gofer's web tier
// verifies with only the public key and can never mint one — the payoff of moving
// the vend onto gofer's own domain plane while the control plane holds the key.
func TestOneCredentialBothPlanes(t *testing.T) {
	pub, priv, err := identity.GenerateEd25519()
	if err != nil {
		t.Fatal(err)
	}
	// The control plane's minter (gofer's Ed25519 authority), verify-only web tier.
	authority := identity.NewEd25519Authority(priv, "gofer-agent", time.Minute)

	vlt := vault.NewMemoryVault()
	_ = vlt.Put(context.Background(), "u1", vault.Credential{Provider: "github", AccessToken: "gho_secret"})
	store := worklist.NewMemoryStore()

	auth := authn.New(
		session.NewManager([]byte("test-session-secret-0123456789ab"), false),
		authn.NewIdentityValidator(identity.NewEd25519Verifier(pub, "gofer-agent")),
	)
	ingestHandler := api.NewIngestHandler(store, nil)
	credentialHandler := api.NewCredentialHandler(vlt, "")

	mux := http.NewServeMux()
	mux.Handle("GET /agent/credential", auth.RequireScope(principal.ScopeSignalsRead, http.HandlerFunc(credentialHandler.Vend)))
	mux.Handle("POST /agent/worklist", auth.RequireScope(principal.ScopeMetadataWrite, http.HandlerFunc(ingestHandler.Ingest)))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// The run credential, minted exactly as the control plane mints it on dispatch.
	tok, err := authority.Mint(identity.Claims{RunID: "run1", Subject: "u1", Scopes: []string{"signals:read", "metadata:write"}})
	if err != nil {
		t.Fatal(err)
	}

	// Plane 1 — gofer's credential broker: vend the acting user's provider token.
	credReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/agent/credential?provider=github", nil)
	credReq.Header.Set("Authorization", "Bearer "+tok)
	credResp, err := http.DefaultClient.Do(credReq)
	if err != nil {
		t.Fatal(err)
	}
	defer credResp.Body.Close()
	if credResp.StatusCode != http.StatusOK {
		t.Fatalf("credential vend: want 200, got %d", credResp.StatusCode)
	}
	var vended struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(credResp.Body).Decode(&vended)
	if vended.AccessToken != "gho_secret" {
		t.Fatalf("credential vend returned %q, want the user's token", vended.AccessToken)
	}

	// Plane 2 — gofer's domain sink: write results with the SAME token.
	ingBody, _ := json.Marshal(map[string]any{"items": []worklist.WorkItem{
		{ID: "github:o/r#1", GitHub: worklist.GitHubRef{Number: 1, Repo: "o/r"}},
	}})
	ingReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/agent/worklist", bytes.NewReader(ingBody))
	ingReq.Header.Set("Authorization", "Bearer "+tok)
	ingResp, err := http.DefaultClient.Do(ingReq)
	if err != nil {
		t.Fatal(err)
	}
	defer ingResp.Body.Close()
	if ingResp.StatusCode != http.StatusOK {
		t.Fatalf("domain ingest: want 200, got %d", ingResp.StatusCode)
	}

	items, _ := store.List(context.Background(), "u1")
	if len(items) != 1 {
		t.Fatalf("expected 1 stored item, got %d", len(items))
	}
}
