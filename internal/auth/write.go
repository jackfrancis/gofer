package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/oauth2"

	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/vault"
)

// This file is the optional write grant (ADR 0009): a separate classic OAuth app a
// signed-in GitHub user connects so gofer can post their own comments. Its token is
// stored as provider WriteProvider, under the same lock, versioning and logout ledger
// as sign-in, but it is never vended to a run (Credential refuses it) and never signs
// anyone in: only the web tier reads it, through WriteCredential.

// WriteProvider is the vault provider name of the write grant.
const WriteProvider = "github-write"

// writePath is the grant flow's route prefix, and the path its cookie is scoped to.
const writePath = "/auth/github-write"

// CommentingState says whether the user can post comments through gofer: Enabled when
// the deployment configures the write client, Connected when the user has a usable
// write grant stored (from the configured client, no logout pending).
type CommentingState struct {
	Enabled   bool
	Connected bool
}

// CommentingState reports the user's write-grant state. It reads only; a dead grant
// is found when posting (WriteRejected). An unreadable vault reads as not connected.
func (h *Handler) CommentingState(ctx context.Context, userID string) CommentingState {
	p, ok := h.providers[WriteProvider]
	if !ok || h.vault == nil {
		return CommentingState{}
	}
	st := CommentingState{Enabled: true}
	if pending, err := h.logoutPending(ctx, userID, WriteProvider); err != nil || pending {
		return st
	}
	cred, err := h.vault.Get(ctx, userID, WriteProvider)
	st.Connected = err == nil && fromConfiguredClient(p, cred)
	return st
}

// WriteCredential returns the user's write grant for the web tier's own use (posting
// the user's comment). vault.ErrSignInRequired means none is usable: never connected,
// revoked, a logout pending, or the account no longer in ALLOWED_GITHUB_LOGINS.
func (h *Handler) WriteCredential(ctx context.Context, userID string) (vault.Credential, error) {
	p, ok := h.providers[WriteProvider]
	if !ok {
		return vault.Credential{}, vault.ErrNotFound
	}
	cred, err := h.vend(ctx, userID, p, VendRequest{})
	if err != nil {
		return vault.Credential{}, err
	}
	if h.cfg != nil && !h.cfg.SignInAllowed("github", cred.Login) {
		return vault.Credential{}, vault.ErrSignInRequired
	}
	return cred, nil
}

// WriteRejected handles a 401 GitHub gave the write grant at version: if GitHub's
// check-token confirms the token is dead (the user revoked gofer in their settings),
// the stored grant is deleted so the page asks them to connect again. A 401 that
// check-token does not confirm changes nothing.
func (h *Handler) WriteRejected(ctx context.Context, userID, version string) {
	p, ok := h.providers[WriteProvider]
	if !ok || h.vault == nil || version == "" {
		return
	}
	unlock, err := h.locks.lock(ctx, userID, WriteProvider)
	if err != nil {
		return
	}
	defer unlock()
	cred, err := h.vault.Get(ctx, userID, WriteProvider)
	if err != nil || cred.Version != version {
		return
	}
	valid, err := h.checkToken(ctx, p, cred.AccessToken)
	if err != nil || valid {
		return
	}
	slog.InfoContext(ctx, "write grant is revoked; connect required", "user", userID)
	h.deleteIfVersion(ctx, userID, cred)
}

// ConnectWrite handles GET /auth/github-write/login: a signed-in GitHub user starts
// connecting the write grant. The flow runs under its own cookie, so the session is
// untouched, and is bound to the user who began it.
func (h *Handler) ConnectWrite(w http.ResponseWriter, r *http.Request) {
	p, ok := h.providers[WriteProvider]
	if !ok {
		http.Error(w, "commenting is not configured", http.StatusNotFound)
		return
	}
	user := h.sessions.CurrentUser(r)
	if user == nil || user.Provider != "github" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	state := session.NewToken()
	verifier := oauth2.GenerateVerifier()
	h.sessions.StartGrant(w, writePath, &session.OAuthFlow{
		Provider: p.name,
		State:    state,
		Verifier: verifier,
		UserID:   user.ID,
	})
	http.Redirect(w, r, p.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

// ConnectWriteCallback handles GET /auth/github-write/callback. It completes the
// grant only for the signed-in user who began it, and only if GitHub says the grant
// came from that same GitHub account; a grant from any other account, or from one
// ALLOWED_GITHUB_LOGINS no longer admits, is revoked and nothing is stored.
func (h *Handler) ConnectWriteCallback(w http.ResponseWriter, r *http.Request) {
	p, ok := h.providers[WriteProvider]
	if !ok {
		http.Error(w, "commenting is not configured", http.StatusNotFound)
		return
	}
	flow := h.sessions.GrantFlow(r)
	h.sessions.EndGrant(w, r, writePath) // a grant flow completes at most once
	user := h.sessions.CurrentUser(r)
	if flow == nil || flow.Provider != p.name || user == nil || flow.UserID != user.ID {
		http.Error(w, "no connection in progress for this session", http.StatusBadRequest)
		return
	}
	if state := r.URL.Query().Get("state"); state == "" || state != flow.State {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		// The user declined at GitHub: nothing to store.
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, h.client)
	token, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(flow.Verifier))
	if err != nil {
		slog.WarnContext(ctx, "write grant: token exchange failed", "user", user.ID, "err", err)
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}
	raw, err := h.fetchUser(ctx, p, token)
	if err != nil {
		slog.WarnContext(ctx, "write grant: user fetch failed", "user", user.ID, "err", err)
		http.Error(w, "failed to fetch user profile", http.StatusBadGateway)
		return
	}
	granted, err := p.mapUser(raw)
	switch {
	case err != nil:
		h.refuseGrant(ctx, w, p, token, user.ID, "write grant: invalid user profile", "GitHub returned an unexpected profile.")
		return
	case granted.ID != user.ID:
		h.refuseGrant(ctx, w, p, token, user.ID, "write grant: granted by a different GitHub account",
			"Connect with the same GitHub account you signed in to gofer with ("+user.Login+").")
		return
	case h.cfg != nil && !h.cfg.SignInAllowed("github", granted.Login):
		h.refuseGrant(ctx, w, p, token, user.ID, "write grant: account not in ALLOWED_GITHUB_LOGINS",
			"This account is not allowed to use this gofer.")
		return
	}
	if h.vault == nil {
		http.Error(w, "gofer has nowhere to keep the grant", http.StatusInternalServerError)
		return
	}
	restart, err := h.storeSignIn(ctx, p, user.ID, granted.Login, token)
	if err != nil {
		slog.WarnContext(ctx, "write grant: could not verify the new token", "user", user.ID, "err", err)
		http.Error(w, "could not verify the grant with GitHub", http.StatusBadGateway)
		return
	}
	if restart {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>gofer</title>`+
			`<p>The grant was revoked while you were connecting it, so it was not saved.</p>`+
			`<p><a href="`+writePath+`/login">Connect again</a> &middot; <a href="/">Back to radar</a></p>`)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// refuseGrant revokes a grant gofer will not keep and tells the user why.
func (h *Handler) refuseGrant(ctx context.Context, w http.ResponseWriter, p *provider, token *oauth2.Token, userID, logMsg, userMsg string) {
	slog.WarnContext(ctx, logMsg, "user", userID)
	if token.AccessToken != "" {
		if err := h.revokeGrant(ctx, p, token.AccessToken); err != nil {
			slog.WarnContext(ctx, "write grant: revoking a refused grant failed", "user", userID, "err", err)
		}
	}
	http.Error(w, userMsg, http.StatusForbidden)
}

// DisconnectWrite handles POST /auth/github-write/disconnect: the user withdraws the
// write grant. It is revoked at GitHub and deleted through logout's machinery, so it
// is never used again even if revocation has to finish in the background.
func (h *Handler) DisconnectWrite(w http.ResponseWriter, r *http.Request) {
	user := h.sessions.CurrentUser(r)
	if _, ok := h.providers[WriteProvider]; !ok || user == nil || h.vault == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	res, ok := h.logoutWrite(r.Context(), user.ID)
	switch {
	case !ok:
		http.Redirect(w, r, "/?commenting=error", http.StatusSeeOther)
	case !res.confirmed:
		http.Redirect(w, r, "/?commenting=unconfirmed", http.StatusSeeOther)
	default:
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// logoutWrite revokes and deletes the user's write grant, if they hold one. ok is
// false when there was nothing to do (no grant stored, none pending, commenting not
// configured) or the logout could not be recorded; a stored grant that cannot be
// read is revoked anyway (fail closed).
func (h *Handler) logoutWrite(ctx context.Context, userID string) (logoutResult, bool) {
	if _, ok := h.providers[WriteProvider]; !ok || h.vault == nil {
		return logoutResult{}, false
	}
	gctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stepTimeout)
	_, err := h.vault.Get(gctx, userID, WriteProvider)
	cancel()
	if errors.Is(err, vault.ErrNotFound) {
		pending, perr := h.logoutPending(ctx, userID, WriteProvider)
		if perr == nil && !pending {
			return logoutResult{}, false // never connected: nothing to revoke
		}
	}
	lo, err := h.markLogout(ctx, userID, WriteProvider)
	if err != nil {
		slog.WarnContext(ctx, "write grant: recording its logout failed", "user", userID, "err", err)
		return logoutResult{}, false
	}
	return h.revokeAndDelete(ctx, userID, WriteProvider, lo), true
}

// mergeLogout combines two logouts' outcomes: confirmed only if both were, with
// every settings page either names.
func mergeLogout(a, b logoutResult) logoutResult {
	out := logoutResult{confirmed: a.confirmed && b.confirmed}
	if out.confirmed {
		return out
	}
	seen := map[string]bool{}
	for _, s := range append(append([]string{}, a.settings...), b.settings...) {
		if !seen[s] {
			seen[s] = true
			out.settings = append(out.settings, s)
		}
	}
	if len(out.settings) == 0 {
		out.settings = []string{oauthAppsSettings}
	}
	return out
}
