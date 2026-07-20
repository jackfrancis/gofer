package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackfrancis/gofer/internal/principal"
	"github.com/jackfrancis/gofer/internal/vault"
)

// CredentialHandler is the agent-plane credential broker: a runtime fetches the
// acting user's delegated provider token (GET /agent/credential?provider=github)
// to call the provider directly. It replaces AEI's POST /vend for gofer's own
// delegated tokens — the control plane runs on the aei-controller, which has no
// access to gofer's vault, so gofer vends from its own domain plane instead.
//
// The token is scoped to the caller's ActingUserID (from the verified run
// credential), held by the runtime for the run only, and never a standing secret.
// The route is behind RequireScope, so only a workload token (never a browser
// session) reaches it.
type CredentialHandler struct {
	vault   vault.Vault
	aiToken string
}

// NewCredentialHandler constructs a CredentialHandler over the vault. aiToken is
// gofer's app-level chat-model token, brokered to runtimes as provider "ai"
// (empty disables it); every other provider is a per-user vault credential.
func NewCredentialHandler(v vault.Vault, aiToken string) *CredentialHandler {
	return &CredentialHandler{vault: v, aiToken: aiToken}
}

// Vend handles GET /agent/credential. It returns the acting user's stored token
// for the requested provider (default "github"), short-lived and vended on demand.
func (h *CredentialHandler) Vend(w http.ResponseWriter, r *http.Request) {
	p, ok := principal.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	provider := r.URL.Query().Get("provider")
	if provider == "" {
		provider = "github"
	}
	// "ai" is gofer's app-level model token, not a per-user delegated credential:
	// the web tier holds it and brokers it per run so the model secret is never a
	// standing secret in the sandbox. Every other provider is a vault credential.
	if provider == "ai" {
		if h.aiToken == "" {
			writeError(w, http.StatusNotFound, "no credential for provider")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"access_token": h.aiToken, "token_type": "bearer"})
		return
	}
	cred, err := h.vault.Get(r.Context(), p.ActingUserID, provider)
	if err != nil {
		if errors.Is(err, vault.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no credential for provider")
			return
		}
		writeError(w, http.StatusBadGateway, "could not load credential")
		return
	}
	resp := map[string]any{
		"access_token": cred.AccessToken,
		"token_type":   cred.TokenType,
	}
	if !cred.Expiry.IsZero() {
		if secs := int(time.Until(cred.Expiry).Seconds()); secs > 0 {
			resp["expires_in"] = secs
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
