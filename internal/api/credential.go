package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackfrancis/gofer/internal/auth"
	"github.com/jackfrancis/gofer/internal/principal"
	"github.com/jackfrancis/gofer/internal/vault"
)

// CodeSignInRequired is the error code of the broker's "sign-in required" answer,
// served with StatusSignInRequired.
const CodeSignInRequired = "sign_in_required"

// StatusSignInRequired is the broker's status for "sign-in required": gofer holds
// no usable credential for the user (none, one from another client, or one GitHub
// says is dead). It is distinct from every other answer this route gives — 401/403
// belong to the run credential, 404 to a provider gofer does not broker, 502 to a failure that
// may pass — so a runtime can tell that only the user signing in again helps.
const StatusSignInRequired = http.StatusGone

// CredentialSource is where the broker gets the credential it vends: the web
// tier's auth.Handler, which refreshes, persists and confirms rejections under the
// credential lock (ADR 0007).
type CredentialSource interface {
	Credential(ctx context.Context, userID, provider string, req auth.VendRequest) (vault.Credential, error)
}

// CredentialHandler is the agent-plane credential broker: a runtime fetches the
// acting user's delegated provider token (GET /agent/credential?provider=github)
// to call the provider directly. gofer vends from its own domain plane so a runtime
// never needs access to gofer's vault.
//
// The token is scoped to the caller's ActingUserID (from the verified run
// credential), held by the runtime for the run only, and never a standing secret.
// The route is behind RequireScope, so only a workload token (never a browser
// session) reaches it. The refresh token never leaves the web tier: the answer
// carries only the access token, its type and lifetime, and the credential's
// version (not a secret), which a run names in rejected=<version> when GitHub
// answers that token with 401.
type CredentialHandler struct {
	source  CredentialSource
	aiToken string // shared chat-model bearer token, vended as provider "ai"
}

// NewCredentialHandler constructs a CredentialHandler over the credential source.
// aiToken is the shared chat-model token gofer brokers to runtimes as provider "ai"
// (every model on the endpoint uses it; the model id travels as a run parameter).
// Every other provider is a per-user credential. An empty aiToken disables the chat
// model.
func NewCredentialHandler(src CredentialSource, aiToken string) *CredentialHandler {
	return &CredentialHandler{source: src, aiToken: aiToken}
}

// Vend handles GET /agent/credential[?provider=<p>][&rejected=<version>]. It
// returns the acting user's token for the requested provider (default "github"),
// refreshed first if it has less than 30 minutes left.
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
	// "ai" is gofer's app-level chat-model token, not a per-user delegated
	// credential: the web tier holds it and brokers it per run so the model secret is
	// never a standing secret in the sandbox. Every model on the endpoint shares this
	// one token; the model id travels as a run parameter. Every other provider is a
	// vault credential.
	if provider == "ai" {
		if h.aiToken == "" {
			writeError(w, http.StatusNotFound, "no credential for provider")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"access_token": h.aiToken, "token_type": "bearer"})
		return
	}
	// The write grant (ADR 0009) never leaves the web tier. Credential refuses it
	// too; this second, independent check keeps a run from ever reaching it if
	// either is changed.
	if provider == auth.WriteProvider {
		writeError(w, http.StatusNotFound, "no credential for provider")
		return
	}
	cred, err := h.source.Credential(r.Context(), p.ActingUserID, provider, auth.VendRequest{
		Rejected: r.URL.Query().Get("rejected"),
		RunID:    p.JobID,
	})
	if err != nil {
		if errors.Is(err, vault.ErrSignInRequired) {
			writeJSON(w, StatusSignInRequired, map[string]string{
				"error": "sign-in required: the user must sign in to gofer again",
				"code":  CodeSignInRequired,
			})
			return
		}
		if errors.Is(err, vault.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no credential for provider")
			return
		}
		slog.WarnContext(r.Context(), "credential vend failed", "user", p.ActingUserID, "provider", provider, "err", err)
		writeError(w, http.StatusBadGateway, "could not load credential")
		return
	}
	resp := map[string]any{
		"access_token":       cred.AccessToken,
		"token_type":         cred.TokenType,
		"credential_version": cred.Version,
	}
	if !cred.Expiry.IsZero() {
		if secs := int(time.Until(cred.Expiry).Seconds()); secs > 0 {
			resp["expires_in"] = secs
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
