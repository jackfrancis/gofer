package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/jackfrancis/gofer/internal/vault"
)

// This file is the credential source behind the agent plane's broker (ADR 0007,
// "Expiry and refresh"): every write of a stored provider credential — sign-in,
// refresh, legacy tagging, logout's delete — happens here, under one lock per
// (user, provider).

const (
	// earlyRefresh is how much life a vended token must have left. It is more than
	// the longest run deadline (converse, 15 minutes) plus a margin, so a token
	// outlives the run that holds it. oauth2's own default is 10 seconds.
	earlyRefresh = 30 * time.Minute
	// forcedRefreshTTL is how long a run's forced-refresh allowance is remembered:
	// longer than any run credential lives.
	forcedRefreshTTL = time.Hour
	// errBadRefreshToken is GitHub's token-endpoint error for a refresh token that
	// is spent, expired or revoked.
	errBadRefreshToken = "bad_refresh_token"
)

// VendRequest qualifies a broker vend.
type VendRequest struct {
	// Rejected is the credential version a run says GitHub rejected with a 401.
	// The broker confirms it with GitHub before acting on it.
	Rejected string
	// RunID is the verified run credential's run ID (Principal.JobID). It keys the
	// one forced refresh a run is allowed.
	RunID string
}

// credLocks is one mutex per (user, provider). The web tier is a single replica by
// construction, so an in-process lock serialises every writer of a credential. A
// lock waits on its context, so a stuck holder cannot wedge a request forever.
type credLocks struct {
	mu sync.Mutex
	m  map[string]*credLock
}

type credLock struct {
	ch   chan struct{}
	refs int // holders and waiters; the entry is dropped at zero
}

func (l *credLocks) lock(ctx context.Context, userID, provider string) (func(), error) {
	key := userID + "\x00" + provider
	l.mu.Lock()
	if l.m == nil {
		l.m = make(map[string]*credLock)
	}
	e := l.m[key]
	if e == nil {
		e = &credLock{ch: make(chan struct{}, 1)}
		l.m[key] = e
	}
	e.refs++
	l.mu.Unlock()
	release := func() {
		l.mu.Lock()
		if e.refs--; e.refs == 0 {
			delete(l.m, key)
		}
		l.mu.Unlock()
	}
	select {
	case e.ch <- struct{}{}:
		return func() { <-e.ch; release() }, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

// newVersion mints a credential version: a random ID, not a secret.
func newVersion() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic("auth: failed to read random bytes: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// fromConfiguredClient reports whether cred was issued by p's configured client. A
// legacy row (no client recorded) counts only where p accepts legacy rows: never
// for GitHub under GITHUB_CLIENT_KIND=github-app, since the app never wrote one.
func fromConfiguredClient(p *provider, cred vault.Credential) bool {
	if cred.ClientID == "" {
		return p.legacyRows
	}
	return cred.ClientID == p.oauth.ClientID
}

// SignInRequired reports whether the user has no credential gofer could vend for
// the provider: none stored, or one from another client. It reads only; a dead
// credential is found (and deleted) by the broker. The landing page uses it to ask
// the user to sign in again.
func (h *Handler) SignInRequired(ctx context.Context, userID, providerName string) bool {
	p, ok := h.providers[providerName]
	if !ok || h.vault == nil {
		return false
	}
	cred, err := h.vault.Get(ctx, userID, providerName)
	if errors.Is(err, vault.ErrNotFound) {
		return true
	}
	if err != nil {
		return false // unreadable is an operator problem, not the user's
	}
	return !fromConfiguredClient(p, cred)
}

// Credential returns a credential the broker may vend for the user and provider,
// under the credential lock. A provider gofer does not sign in with is
// vault.ErrNotFound. Otherwise:
//
//   - none stored, one from another client, or a legacy row under a GitHub App
//     answer vault.ErrSignInRequired;
//   - a legacy row under an OAuth app is tagged with the client and a version and
//     written back before it is vended;
//   - with less than earlyRefresh left and a refresh token, it is refreshed, and the
//     rotated pair is persisted before the new token is returned;
//   - with req.Rejected naming the stored version, the rejection is confirmed with
//     GitHub's check-token endpoint, and only a confirmed one refreshes (once per
//     run) or deletes the credential.
//
// The refresh token never leaves this package's callers in the web tier: the broker
// vends only the access token.
func (h *Handler) Credential(ctx context.Context, userID, providerName string, req VendRequest) (vault.Credential, error) {
	p, ok := h.providers[providerName]
	if !ok {
		return vault.Credential{}, vault.ErrNotFound // not a provider gofer signs in with
	}
	if h.vault == nil {
		return vault.Credential{}, vault.ErrSignInRequired
	}
	unlock, err := h.locks.lock(ctx, userID, providerName)
	if err != nil {
		return vault.Credential{}, err
	}
	defer unlock()

	cred, err := h.vault.Get(ctx, userID, providerName)
	if errors.Is(err, vault.ErrNotFound) {
		return vault.Credential{}, vault.ErrSignInRequired
	}
	if err != nil {
		return vault.Credential{}, err
	}
	if !fromConfiguredClient(p, cred) {
		return vault.Credential{}, vault.ErrSignInRequired
	}
	if cred.ClientID == "" || cred.Version == "" {
		// A legacy row: its token never expires, so no refresh would ever version
		// it, and a run could never name it in a rejection. Tag it now.
		cred.ClientID, cred.Version = p.oauth.ClientID, newVersion()
		if err := h.vault.Put(ctx, userID, cred); err != nil {
			slog.WarnContext(ctx, "tagging a legacy credential failed", "user", userID, "provider", providerName, "err", err)
			return vault.Credential{}, fmt.Errorf("store %s credential: %w", providerName, err)
		}
	}
	if req.Rejected != "" && req.Rejected == cred.Version && providerName == "github" {
		return h.confirmRejection(ctx, userID, p, cred, req.RunID)
	}
	return h.refreshIfDue(ctx, userID, p, cred)
}

// refreshIfDue refreshes cred when it has less than earlyRefresh left and a
// refresh token, else returns it unchanged.
func (h *Handler) refreshIfDue(ctx context.Context, userID string, p *provider, cred vault.Credential) (vault.Credential, error) {
	if cred.RefreshToken == "" {
		return cred, nil
	}
	src := oauth2.ReuseTokenSourceWithExpiry(&oauth2.Token{
		AccessToken:  cred.AccessToken,
		RefreshToken: cred.RefreshToken,
		TokenType:    cred.TokenType,
		Expiry:       cred.Expiry,
	}, h.refresher(ctx, p, cred.RefreshToken), earlyRefresh)
	tok, err := src.Token()
	if err != nil {
		if h.refreshDead(ctx, userID, cred, err) {
			return vault.Credential{}, vault.ErrSignInRequired
		}
		// A transient failure: a token that has not expired still works.
		if !cred.Expiry.IsZero() && h.now().Before(cred.Expiry) {
			return cred, nil
		}
		return vault.Credential{}, fmt.Errorf("refresh %s credential failed", p.name)
	}
	if tok.AccessToken == cred.AccessToken {
		return cred, nil // still fresh enough: nothing rotated
	}
	return h.storeRefreshed(ctx, userID, p, cred, tok)
}

// confirmRejection handles a run's rejected=<version> when that is the stored
// version. A run's word alone never rotates or deletes a credential: GitHub's
// check-token endpoint decides.
func (h *Handler) confirmRejection(ctx context.Context, userID string, p *provider, cred vault.Credential, runID string) (vault.Credential, error) {
	// One forced refresh per run: each refresh mints a new version, so a version
	// check alone would let one run rotate again and again, killing the tokens
	// concurrent runs hold. A run without an ID gets no forced refresh at all.
	if runID == "" || h.forcedUsed(runID) {
		return cred, nil
	}
	valid, err := h.checkToken(ctx, p, cred.AccessToken)
	if err != nil {
		return vault.Credential{}, fmt.Errorf("confirm rejected %s credential: %w", p.name, err)
	}
	if valid {
		return cred, nil // the 401 was about something else
	}
	if cred.RefreshToken == "" {
		// A dead token with nothing to renew it (e.g. a legacy OAuth-app token
		// revoked at rollout): only a new sign-in helps.
		slog.InfoContext(ctx, "stored credential is revoked; sign-in required", "user", userID, "provider", p.name)
		h.deleteIfVersion(ctx, userID, cred)
		return vault.Credential{}, vault.ErrSignInRequired
	}
	h.useForced(runID)
	tok, err := h.refresher(ctx, p, cred.RefreshToken).Token()
	if err != nil {
		if h.refreshDead(ctx, userID, cred, err) {
			return vault.Credential{}, vault.ErrSignInRequired
		}
		return vault.Credential{}, fmt.Errorf("refresh %s credential failed", p.name)
	}
	return h.storeRefreshed(ctx, userID, p, cred, tok)
}

// refresher is a token source that always spends refreshToken: with no access
// token in it, oauth2 has nothing to reuse.
func (h *Handler) refresher(ctx context.Context, p *provider, refreshToken string) oauth2.TokenSource {
	if h.client != nil {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, h.client)
	}
	return p.oauth.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken})
}

// refreshDead reports whether err says the refresh token is dead, and if so
// deletes the credential (version-checked). Only the error code is logged: the
// token endpoint's error carries no token, but there is no reason to risk it.
func (h *Handler) refreshDead(ctx context.Context, userID string, cred vault.Credential, err error) bool {
	code := refreshErrorCode(err)
	if code != errBadRefreshToken {
		slog.WarnContext(ctx, "credential refresh failed", "user", userID, "provider", cred.Provider, "code", code)
		return false
	}
	slog.InfoContext(ctx, "refresh token is dead; sign-in required", "user", userID, "provider", cred.Provider)
	h.deleteIfVersion(ctx, userID, cred)
	return true
}

// refreshErrorCode is the OAuth error code in err, or a coarse label.
func refreshErrorCode(err error) string {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		if re.ErrorCode != "" {
			return re.ErrorCode
		}
		if re.Response != nil {
			return "status " + strconv.Itoa(re.Response.StatusCode)
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout"
	}
	return "transport"
}

// storeRefreshed persists a rotated pair, then returns it. The old refresh token is
// already spent, so a write that fails is retried once; if it still fails, the user
// must sign in again (the next vend's refresh will find the spent token dead).
func (h *Handler) storeRefreshed(ctx context.Context, userID string, p *provider, old vault.Credential, tok *oauth2.Token) (vault.Credential, error) {
	fresh := vault.Credential{
		Provider:      old.Provider,
		AccessToken:   tok.AccessToken,
		RefreshToken:  tok.RefreshToken,
		TokenType:     tok.TokenType,
		Expiry:        tok.Expiry,
		RefreshExpiry: refreshExpiry(tok, h.now()),
		ClientID:      p.oauth.ClientID,
		Version:       newVersion(),
	}
	if fresh.RefreshToken == "" {
		fresh.RefreshToken = old.RefreshToken
		fresh.RefreshExpiry = old.RefreshExpiry
	}
	for attempt := 0; attempt < 2; attempt++ {
		stored, err := h.putIfVersion(ctx, userID, old.Version, fresh)
		if err == nil && !stored {
			// Someone else wrote the row since it was read: theirs is newer.
			return h.vault.Get(ctx, userID, old.Provider)
		}
		if err == nil {
			return fresh, nil
		}
		slog.WarnContext(ctx, "storing a refreshed credential failed", "user", userID, "provider", old.Provider, "attempt", attempt+1, "err", err)
	}
	return vault.Credential{}, vault.ErrSignInRequired
}

// refreshExpiry is when tok's refresh token expires, from GitHub's
// refresh_token_expires_in, or zero.
func refreshExpiry(tok *oauth2.Token, now time.Time) time.Time {
	var secs float64
	switch v := tok.Extra("refresh_token_expires_in").(type) {
	case float64:
		secs = v
	case string:
		secs, _ = strconv.ParseFloat(v, 64)
	}
	if secs <= 0 {
		return time.Time{}
	}
	return now.Add(time.Duration(secs) * time.Second)
}

// putIfVersion writes cred only if the stored row still holds version expect. Every
// writer holds the credential lock, so this is the second line of defence the ADR
// asks for, not the first.
func (h *Handler) putIfVersion(ctx context.Context, userID, expect string, cred vault.Credential) (bool, error) {
	cur, err := h.vault.Get(ctx, userID, cred.Provider)
	if err != nil && !errors.Is(err, vault.ErrNotFound) {
		return false, err
	}
	if err != nil || cur.Version != expect {
		return false, nil
	}
	return true, h.vault.Put(ctx, userID, cred)
}

// deleteIfVersion deletes the credential only if the row still holds cred's
// version, so a failed refresh never deletes what a newer sign-in wrote.
func (h *Handler) deleteIfVersion(ctx context.Context, userID string, cred vault.Credential) {
	cur, err := h.vault.Get(ctx, userID, cred.Provider)
	if err != nil || cur.Version != cred.Version {
		return
	}
	if err := h.vault.Delete(ctx, userID, cred.Provider); err != nil {
		slog.WarnContext(ctx, "deleting a dead credential failed", "user", userID, "provider", cred.Provider, "err", err)
	}
}

func (h *Handler) forcedUsed(runID string) bool {
	h.forcedMu.Lock()
	defer h.forcedMu.Unlock()
	_, used := h.forced[runID]
	return used
}

func (h *Handler) useForced(runID string) {
	h.forcedMu.Lock()
	defer h.forcedMu.Unlock()
	now := h.now()
	for id, at := range h.forced {
		if now.Sub(at) > forcedRefreshTTL {
			delete(h.forced, id)
		}
	}
	if h.forced == nil {
		h.forced = make(map[string]time.Time)
	}
	h.forced[runID] = now
}

// checkToken asks GitHub whether accessToken is valid for p's client
// (POST /applications/{client_id}/token, authenticated with the client's own
// credentials; it does not count against failed-login limits). 200 is valid; 404
// or 422 is not; anything else is an error.
func (h *Handler) checkToken(ctx context.Context, p *provider, accessToken string) (bool, error) {
	status, err := h.applicationCall(ctx, p, http.MethodPost, "/token", accessToken)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound, http.StatusUnprocessableEntity:
		return false, nil
	}
	return false, fmt.Errorf("github check token: status %d", status)
}

// revokeGrant revokes the user's whole grant to p's client
// (DELETE /applications/{client_id}/grant), so the refresh token dies with the
// access token. It needs a valid access token: anything but a 2xx — including 404,
// which a token rotated away (say, by a restore from backup) also gets — leaves
// revocation unconfirmed.
func (h *Handler) revokeGrant(ctx context.Context, p *provider, accessToken string) error {
	status, err := h.applicationCall(ctx, p, http.MethodDelete, "/grant", accessToken)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("github grant revocation: status %d", status)
	}
	return nil
}

// applicationCall sends one /applications/{client_id}{suffix} request carrying
// accessToken in its body and returns the status.
func (h *Handler) applicationCall(ctx context.Context, p *provider, method, suffix, accessToken string) (int, error) {
	body, err := json.Marshal(map[string]string{"access_token": accessToken})
	if err != nil {
		return 0, err
	}
	url := h.githubAPIBase + "/applications/" + p.oauth.ClientID + suffix
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.SetBasicAuth(p.oauth.ClientID, p.oauth.ClientSecret)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "gofer")
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// The check-token answer echoes the token: read it only to drain, never keep it.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, nil
}

// storeSignIn is the callback's write, under the credential lock. For GitHub it
// first re-checks the new token with check-token: a logout that revoked the grant
// between the token exchange and this lock would otherwise leave a revoked token
// stored behind a fresh session. It reports restart when the token is no longer
// valid, so the user consents again.
func (h *Handler) storeSignIn(ctx context.Context, p *provider, userID string, tok *oauth2.Token) (restart bool, err error) {
	unlock, err := h.locks.lock(ctx, userID, p.name)
	if err != nil {
		return false, err
	}
	defer unlock()
	if p.name == "github" {
		valid, err := h.checkToken(ctx, p, tok.AccessToken)
		if err != nil {
			return false, err
		}
		if !valid {
			return true, nil
		}
	}
	cred := vault.Credential{
		Provider:      p.name,
		AccessToken:   tok.AccessToken,
		RefreshToken:  tok.RefreshToken,
		TokenType:     tok.TokenType,
		Expiry:        tok.Expiry,
		RefreshExpiry: refreshExpiry(tok, h.now()),
		ClientID:      p.oauth.ClientID,
		Version:       newVersion(),
	}
	// Best-effort, as before: a vault failure must not block interactive login. The
	// broker then answers "sign-in required", and the page says so.
	if err := h.vault.Put(ctx, userID, cred); err != nil {
		slog.WarnContext(ctx, "storing the sign-in credential failed", "user", userID, "provider", p.name, "err", err)
	}
	return false, nil
}

// revokeAndDelete is logout's credential step, under the credential lock: read the
// credential, refresh it if it has expired or is about to (revoking the grant needs
// a valid access token, and a session outlives one), revoke the grant, then delete
// the row. The row is deleted whatever happens. It reports whether revocation at
// GitHub is confirmed; nothing stored, or a provider gofer revokes nothing at, is
// confirmed.
func (h *Handler) revokeAndDelete(ctx context.Context, userID, providerName string) (confirmed bool) {
	del := func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := h.vault.Delete(dctx, userID, providerName); err != nil {
			slog.WarnContext(ctx, "deleting a credential on logout failed", "user", userID, "provider", providerName, "err", err)
		}
	}
	unlock, err := h.locks.lock(ctx, userID, providerName)
	if err != nil {
		// Logout is never blocked: delete anyway; a refresh in flight may write back.
		del()
		return false
	}
	defer unlock()
	cred, err := h.vault.Get(ctx, userID, providerName)
	if errors.Is(err, vault.ErrNotFound) {
		return true
	}
	defer del()
	if err != nil {
		slog.WarnContext(ctx, "reading a credential on logout failed", "user", userID, "provider", providerName, "err", err)
		return providerName != "github"
	}
	p, ok := h.providers[providerName]
	if providerName != "github" || !ok {
		return true // gofer revokes nothing at other providers
	}
	if !fromConfiguredClient(p, cred) {
		// Issued by another client (before a cutover or rollback): this client's
		// credentials cannot revoke it.
		slog.InfoContext(ctx, "logout: credential is from another client; revocation unconfirmed", "user", userID)
		return false
	}
	token := cred.AccessToken
	if cred.RefreshToken != "" && !cred.Expiry.IsZero() && h.now().Add(earlyRefresh).After(cred.Expiry) {
		tok, err := h.refresher(ctx, p, cred.RefreshToken).Token()
		switch {
		case err == nil:
			token = tok.AccessToken
		case refreshErrorCode(err) == errBadRefreshToken:
			// This does not prove the grant is gone: after a restore from backup the
			// vault can hold a spent refresh token while a newer pair still works.
			slog.InfoContext(ctx, "logout: refresh token is dead; revocation unconfirmed", "user", userID)
			return false
		case !h.now().Before(cred.Expiry):
			slog.WarnContext(ctx, "logout: refresh failed and the token has expired; revocation unconfirmed", "user", userID, "code", refreshErrorCode(err))
			return false
		}
	}
	if err := h.revokeGrant(ctx, p, token); err != nil {
		slog.WarnContext(ctx, "logout: grant revocation failed; unconfirmed", "user", userID, "err", err)
		return false
	}
	return true
}

// authorizedAppsURL is where a user revokes gofer at GitHub by hand.
func (h *Handler) authorizedAppsURL() string {
	if h.cfg != nil && h.cfg.Providers.IsGitHubApp() {
		return "https://github.com/settings/apps/authorizations"
	}
	return "https://github.com/settings/applications"
}
