package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackfrancis/gofer/internal/principal"
	"github.com/jackfrancis/gofer/internal/vault"
)

// The broker vends the single shared chat-model token as provider "ai"; every model
// on the endpoint uses it (the model id travels as a run parameter, not the token).
func TestCredentialVendsSharedAIToken(t *testing.T) {
	h := NewCredentialHandler(vault.NewMemoryVault(), "tok-ai")

	if got := vendToken(t, h, "ai"); got != "tok-ai" {
		t.Errorf("provider ai -> %q, want tok-ai", got)
	}

	// Named ai:<name> providers are no longer brokered (one token per endpoint) -> 404.
	rec := httptest.NewRecorder()
	h.Vend(rec, authedGet("/agent/credential?provider=ai:second"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("ai:second status = %d, want 404", rec.Code)
	}

	// No token configured -> provider ai 404s.
	off := NewCredentialHandler(vault.NewMemoryVault(), "")
	rec2 := httptest.NewRecorder()
	off.Vend(rec2, authedGet("/agent/credential?provider=ai"))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("disabled ai status = %d, want 404", rec2.Code)
	}
}

func vendToken(t *testing.T, h *CredentialHandler, provider string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Vend(rec, authedGet("/agent/credential?provider="+provider))
	if rec.Code != http.StatusOK {
		t.Fatalf("provider %q status = %d", provider, rec.Code)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&out)
	return out.AccessToken
}

func authedGet(target string) *http.Request {
	p := &principal.Principal{
		Kind:         principal.KindWorkload,
		Subject:      "u1",
		ActingUserID: "u1",
		JobID:        "run1",
		Scopes:       []principal.Scope{principal.ScopeSignalsRead},
	}
	return httptest.NewRequest(http.MethodGet, target, nil).WithContext(principal.NewContext(context.Background(), p))
}
