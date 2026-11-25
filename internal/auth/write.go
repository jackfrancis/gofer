package auth

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"slices"
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
// write grant stored (from the configured client or, where allowed, an unexpired
// personal access token; no logout pending). TokensAllowed says the deployment lets
// users connect with a token (ADR 0011); Token and Expires describe a connected one.
// TokenExpired says a token is stored but has expired (Expires is when): not
// connected, but gofer still holds a copy the user can replace or disconnect.
// TokenDisabled says an unexpired token is stored while the deployment no longer
// allows tokens: unused, and in the way of the OAuth grant until the user disconnects
// it. OtherClient says an OAuth grant from another commenting app (before
// GITHUB_WRITE_CLIENT_ID changed) is stored: unused, and in the way of a token, so
// the user disconnects it first. CleanupPending says a disconnect or logout of the
// write grant has not finished deleting gofer's copy (unused meanwhile): Disconnect
// retries it.
type CommentingState struct {
	Enabled        bool
	Connected      bool
	TokensAllowed  bool
	Token          bool
	TokenExpired   bool
	TokenDisabled  bool
	OtherClient    bool
	CleanupPending bool
	Expires        time.Time
}

// CommentingState reports the user's write-grant state. It reads only; a dead grant
// is found when posting (WriteRejected). An unreadable vault reads as not connected.
func (h *Handler) CommentingState(ctx context.Context, userID string) CommentingState {
	p, ok := h.providers[WriteProvider]
	if !ok || h.vault == nil {
		return CommentingState{}
	}
	st := CommentingState{Enabled: true, TokensAllowed: p.tokens}
	switch pending, err := h.logoutPending(ctx, userID, WriteProvider); {
	case err != nil:
		return st
	case pending:
		st.CleanupPending = true
		return st
	}
	cred, err := h.vault.Get(ctx, userID, WriteProvider)
	st.Connected = err == nil && fromConfiguredClient(p, cred) && !tokenExpired(cred, h.now())
	switch {
	case st.Connected && isPersonalToken(cred):
		st.Token, st.Expires = true, cred.Expiry
	case err == nil && tokenExpired(cred, h.now()):
		st.TokenExpired, st.Expires = true, cred.Expiry
	case err == nil && isPersonalToken(cred):
		// Stored while tokens were allowed, and they no longer are.
		st.TokenDisabled, st.Expires = true, cred.Expiry
	case err == nil && !st.Connected:
		// Any other stored grant gofer cannot use: one from another commenting app.
		st.OtherClient = true
	}
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
	if tokenExpired(cred, h.now()) {
		// An expired personal access token: connect again with a new one. The row
		// stays until then, so a disconnect still tells the user to delete it.
		return vault.Credential{}, vault.ErrSignInRequired
	}
	return cred, nil
}

// WriteRejected handles a 401 GitHub gave the write grant at version: if GitHub
// confirms the token is dead (check-token for an OAuth grant, the user revoked gofer
// in their settings; a 401 on GET /user for a personal access token, deleted or
// expired), the stored grant is deleted so the page asks them to connect again. A 401
// GitHub does not confirm changes nothing.
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
	var valid bool
	if isPersonalToken(cred) {
		valid, err = h.tokenAlive(ctx, cred.AccessToken)
	} else {
		valid, err = h.checkToken(ctx, p, cred.AccessToken)
	}
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
		Provider:   p.name,
		State:      state,
		Verifier:   verifier,
		UserID:     user.ID,
		SessionKey: h.sessions.SessionKey(r),
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
	if flow == nil || flow.Provider != p.name || user == nil || flow.UserID != user.ID ||
		flow.SessionKey == "" || flow.SessionKey != h.sessions.SessionKey(r) {
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
	// From here GitHub has issued a token: every way out but a stored grant discards
	// it (failConnect), so no authorization is left behind that logout could not find.
	raw, err := h.fetchUser(ctx, p, token)
	if err != nil {
		slog.WarnContext(ctx, "write grant: user fetch failed", "user", user.ID, "err", err)
		h.failConnect(ctx, w, p, token, user.ID, http.StatusBadGateway, "gofer could not read your GitHub profile, so the grant was not saved")
		return
	}
	granted, err := p.mapUser(raw)
	switch {
	case err != nil:
		slog.WarnContext(ctx, "write grant: invalid user profile", "user", user.ID, "err", err)
		h.failConnect(ctx, w, p, token, user.ID, http.StatusBadGateway, "GitHub returned an unexpected profile, so the grant was not saved")
		return
	case granted.ID != user.ID:
		slog.WarnContext(ctx, "write grant: granted by a different GitHub account", "user", user.ID)
		h.failConnect(ctx, w, p, token, user.ID, http.StatusForbidden,
			"the grant came from a different GitHub account; connect with the one you signed in to gofer with ("+html.EscapeString(user.Login)+")")
		return
	case h.cfg != nil && !h.cfg.SignInAllowed("github", granted.Login):
		slog.WarnContext(ctx, "write grant: account not in ALLOWED_GITHUB_LOGINS", "user", user.ID)
		h.failConnect(ctx, w, p, token, user.ID, http.StatusForbidden, "this account is not allowed to use this gofer")
		return
	case h.vault == nil:
		h.failConnect(ctx, w, p, token, user.ID, http.StatusInternalServerError, "gofer has nowhere to keep the grant")
		return
	}
	// The session is checked again when the grant is stored, under the lock logout
	// holds while it records what to revoke: a logout that ends this session while the
	// code is being exchanged must not be followed by a grant stored for it. It must be
	// the very session that began the flow, not one signed in again since.
	sessionValid := func(ctx context.Context) (bool, error) {
		u, err := h.sessions.Lookup(r.WithContext(ctx))
		return u != nil && u.ID == user.ID && h.sessions.SessionKey(r) == flow.SessionKey, err
	}
	outcome, err := h.storeWriteGrant(ctx, p, user.ID, granted.Login, token, sessionValid)
	switch {
	case err != nil:
		slog.WarnContext(ctx, "write grant: not stored", "user", user.ID, "err", err)
		h.failConnect(ctx, w, p, token, user.ID, http.StatusBadGateway, "gofer could not save the grant")
		return
	case outcome == grantSessionEnded:
		h.failConnect(ctx, w, p, token, user.ID, http.StatusConflict, "you signed out of gofer while connecting, so the grant was not saved")
		return
	case outcome == grantTokenConnected:
		h.failConnect(ctx, w, p, token, user.ID, http.StatusConflict, "gofer holds your personal access token for commenting, so the grant was not saved; disconnect commenting on the radar first to switch")
		return
	case outcome == grantRevokedMidFlow:
		writeGrantPage(w, http.StatusConflict, `<p>The grant was revoked while you were connecting it, so it was not saved.</p>`+
			`<p><a href="`+writePath+`/login">Connect again</a> &middot; <a href="/">Back to radar</a></p>`)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func writeGrantPage(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>gofer</title>`+body)
}

// grantOutcome is what storeWriteGrant did.
type grantOutcome int

const (
	grantStored         grantOutcome = iota
	grantRevokedMidFlow              // check-token says the new token is already dead
	grantSessionEnded                // the session that began the flow has ended
)

// storeWriteGrant stores a new write grant, strictly: unlike sign-in's storeSignIn,
// a vault failure is an error, since a grant gofer has not saved is one it could not
// revoke at logout. Under the credential lock it re-checks the token with GitHub;
// then, under the write grant's marker lock (which logout holds while it decides what
// to revoke), it re-checks the initiating session and only then writes the grant and
// clears any pending logout of it, together. On grantSessionEnded or an error the
// caller revokes the token.
func (h *Handler) storeWriteGrant(ctx context.Context, p *provider, userID, login string, tok *oauth2.Token, sessionValid func(context.Context) (bool, error)) (grantOutcome, error) {
	unlock, err := h.locks.lock(ctx, userID, p.name)
	if err != nil {
		return 0, err
	}
	defer unlock()
	valid, err := h.checkToken(ctx, p, tok.AccessToken)
	if err != nil {
		return 0, fmt.Errorf("verify the grant: %w", err)
	}
	if !valid {
		return grantRevokedMidFlow, nil
	}
	defer h.locks.markerLock(userID, p.name)()
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stepTimeout)
	defer cancel()
	ok, err := sessionValid(lctx)
	if err != nil {
		return 0, fmt.Errorf("re-check the session: %w", err)
	}
	if !ok {
		return grantSessionEnded, nil
	}
	// A live personal access token connected meanwhile (another tab, ADR 0011) is not
	// replaced: the user would be left holding a token they no longer know gofer
	// stopped using. The new OAuth token is discarded instead.
	switch existing, err := h.vault.Get(lctx, userID, p.name); {
	case err == nil && isPersonalToken(existing) && !tokenExpired(existing, h.now()):
		return grantTokenConnected, nil
	case err != nil && !errors.Is(err, vault.ErrNotFound):
		return 0, fmt.Errorf("read the write grant: %w", err)
	}
	cred := vault.Credential{
		Provider:    p.name,
		AccessToken: tok.AccessToken,
		TokenType:   tok.TokenType,
		Expiry:      tok.Expiry,
		ClientID:    p.oauth.ClientID,
		Version:     newVersion(),
		Login:       login,
	}
	if l, ok := h.vault.(vault.LogoutLedger); ok {
		err = l.PutClearingLogout(lctx, userID, cred)
	} else if err = h.vault.Put(lctx, userID, cred); err == nil {
		err = h.ledger.ClearLogout(lctx, userID, p.name)
	}
	if err != nil {
		return 0, fmt.Errorf("store the grant: %w", err)
	}
	h.supersedeLogoutLocked(userID, p.name)
	return grantStored, nil
}

// failConnect discards the token a failed connection was issued (discardToken) and
// tells the user why nothing was saved, and whether GitHub confirmed the token is
// gone.
func (h *Handler) failConnect(ctx context.Context, w http.ResponseWriter, p *provider, token *oauth2.Token, userID string, status int, why string) {
	msg := `<p>` + why + `. GitHub confirmed the token it issued is revoked.</p>`
	if !h.discardToken(ctx, p, token, userID) {
		msg = `<p>` + why + `. GitHub did not confirm the token it issued is revoked: check ` +
			`<a href="` + oauthAppsSettings + `">GitHub's settings</a> (Authorized OAuth Apps) and revoke gofer's commenting app if it is there.</p>`
	}
	writeGrantPage(w, status, msg+`<p><a href="/">Back to radar</a></p>`)
}

// discardToken revokes one token a failed connection was issued, and reports whether
// GitHub confirmed it. It revokes that token only (DELETE /applications/{id}/token),
// never the whole authorization: a connection that succeeded meanwhile, in another
// tab, holds a different token of the same authorization, which must survive. It
// runs detached from the browser request, on its own deadline.
func (h *Handler) discardToken(ctx context.Context, p *provider, token *oauth2.Token, userID string) bool {
	if token == nil || token.AccessToken == "" {
		return true
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), exchangeTimeout)
	defer cancel()
	if err := h.revokeToken(rctx, p, token.AccessToken); err != nil {
		slog.WarnContext(ctx, "write grant: revoking an unsaved token failed", "user", userID, "err", err)
		return false
	}
	return true
}

// revokeToken revokes one access token of p's client
// (DELETE /applications/{client_id}/token). Anything but a 2xx is unconfirmed.
func (h *Handler) revokeToken(ctx context.Context, p *provider, accessToken string) error {
	status, err := h.applicationCall(ctx, p, http.MethodDelete, "/token", accessToken)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("github token revocation: status %d", status)
	}
	return nil
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
	lo, err := h.beginWriteDisconnect(r.Context(), user.ID)
	if err != nil {
		slog.WarnContext(r.Context(), "write grant: recording its disconnect failed", "user", user.ID, "err", err)
		http.Redirect(w, r, "/?commenting=error", http.StatusSeeOther)
		return
	}
	if lo == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther) // nothing was connected
		return
	}
	switch res := h.revokeAndDelete(r.Context(), user.ID, WriteProvider, lo); {
	case res.confirmed && res.cleanupPending:
		// Revoked at GitHub, but gofer's copy is not deleted yet: say both.
		http.Redirect(w, r, "/?commenting=revoked-pending", http.StatusSeeOther)
	case res.confirmed:
		http.Redirect(w, r, "/", http.StatusSeeOther)
	case res.cleanupPending:
		// Not deleted yet: the pending logout keeps it unused and finishes later.
		http.Redirect(w, r, "/?commenting=disconnect-pending", http.StatusSeeOther)
	case res.tokenDeleted:
		// A personal access token: deleted here, and only the user can revoke it.
		http.Redirect(w, r, "/?commenting=token-deleted", http.StatusSeeOther)
	case slices.Contains(res.settings, tokensSettings):
		// gofer could not tell which kind it held.
		http.Redirect(w, r, "/?commenting=unconfirmed-any", http.StatusSeeOther)
	default:
		http.Redirect(w, r, "/?commenting=unconfirmed", http.StatusSeeOther)
	}
}

// beginWriteDisconnect records the write grant's pending logout, if a grant is held
// (or cannot be read: fail closed), and returns its operation; nil with no error
// means there was nothing to revoke. An error means nothing was recorded.
func (h *Handler) beginWriteDisconnect(ctx context.Context, userID string) (*localLogout, error) {
	defer h.locks.markerLock(userID, WriteProvider)()
	held, err := h.writeGrantHeldLocked(ctx, userID)
	if err == nil && !held {
		return nil, nil
	}
	if err := h.commitLogouts(ctx, "", userID, []string{WriteProvider}); err != nil {
		return nil, err
	}
	return h.publishLogoutLocked(userID, WriteProvider), nil
}

// mergeLogout combines two logouts' outcomes: confirmed only if both were, with
// every settings page either names.
func mergeLogout(a, b logoutResult) logoutResult {
	out := logoutResult{
		confirmed:      a.confirmed && b.confirmed,
		tokenDeleted:   a.tokenDeleted || b.tokenDeleted,
		cleanupPending: a.cleanupPending || b.cleanupPending,
		cleanupUnknown: a.cleanupUnknown || b.cleanupUnknown,
	}
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
