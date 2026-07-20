package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/authn"
	"github.com/jackfrancis/gofer/internal/identity"
	"github.com/jackfrancis/gofer/internal/principal"
	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// A gofer run credential — the audience-bound Ed25519 token the control plane now
// mints via gofer's TokenAuthority — authenticates gofer's own agent plane: the
// runtime writes results to /agent/worklist with the same token it uses for the
// AEI ABI. This proves the token unification (ADR 0002).
func TestAgentIngestWithRunToken(t *testing.T) {
	pub, priv, err := identity.GenerateEd25519()
	if err != nil {
		t.Fatal(err)
	}
	authority := identity.NewEd25519Authority(priv, "gofer-agent", time.Minute)
	auth := authn.New(
		session.NewManager([]byte("test-session-secret-0123456789ab"), false),
		authn.NewIdentityValidator(identity.NewEd25519Verifier(pub, "gofer-agent")),
	)

	store := worklist.NewMemoryStore()
	h := NewIngestHandler(store)
	ingest := auth.RequireScope(principal.ScopeMetadataWrite, http.HandlerFunc(h.Ingest))

	body, _ := json.Marshal(map[string]any{"items": []worklist.WorkItem{
		{ID: "github:o/r#1", GitHub: worklist.GitHubRef{Number: 1, Repo: "o/r"}},
	}})

	// A full run token (has metadata:write) writes, scoped to the acting user.
	tok, _ := authority.Mint(identity.Claims{RunID: "run1", Subject: "u1", Scopes: []string{"signals:read", "metadata:write"}})
	req := httptest.NewRequest(http.MethodPost, "/agent/worklist", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	ingest.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	items, _ := store.List(context.Background(), "u1")
	if len(items) != 1 || items[0].GitHub.Number != 1 {
		t.Fatalf("expected the ingested item stored for u1, got %+v", items)
	}

	// A token without metadata:write is rejected.
	weak, _ := authority.Mint(identity.Claims{RunID: "run2", Subject: "u1", Scopes: []string{"signals:read"}})
	req2 := httptest.NewRequest(http.MethodPost, "/agent/worklist", bytes.NewReader(body))
	req2.Header.Set("Authorization", "Bearer "+weak)
	rec2 := httptest.NewRecorder()
	ingest.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("want 403 for a token without metadata:write, got %d", rec2.Code)
	}

	// A browser session (no bearer) never reaches the agent plane.
	req3 := httptest.NewRequest(http.MethodPost, "/agent/worklist", bytes.NewReader(body))
	rec3 := httptest.NewRecorder()
	ingest.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 for no credential, got %d", rec3.Code)
	}
}
