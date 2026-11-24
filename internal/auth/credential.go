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
	// exchangeTimeout bounds a refresh exchange and the write that persists its
	// result. Both run detached from the requesting run's context: once GitHub has
	// rotated the pair the old refresh token is spent, so a run that disconnects must
	// not abandon the new pair. It also bounds how long a refresh holds the lock.
	exchangeTimeout = 20 * time.Second
	// pendingLogoutWait bounds the background deletion that finishes a logout which
	// could not take the lock in time.
	pendingLogoutWait = 2 * time.Minute

	// GitHub's settings pages where a user revokes gofer by hand.
	githubAppsSettings = "https://github.com/settings/apps/authorizations"
	oauthAppsSettings  = "https://github.com/settings/applications"
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

// logoutLockWait is how long logout waits for the credential lock, detached from the
// browser request. Every holder's work is bounded (exchangeTimeout, the HTTP client's
// timeouts), so this is normally enough; if not, the deletion is handed to the next
// lock holder (the logout tombstone). A variable so tests can shorten it.
var logoutLockWait = 15 * time.Second

// credLocks is one mutex per (user, provider). The web tier is a single replica by
// construction, so an in-process lock serialises every writer of a credential. A
// lock waits on its context, so a stuck holder cannot wedge a request forever.
//
// It also keeps this process's side of pending logouts (see logoutPending): whether
// a logout of the (user, provider) is pending here, and whether an operation in this
// process is still running it. The durable side is the vault's LogoutLedger.
type credLocks struct {
	mu      sync.Mutex
	m       map[string]*credLock
	logouts map[string]*localLogout
	markers map[string]*sync.Mutex
}

// markerLock serialises the transitions of one (user, provider)'s pending logout —
// the durable marker together with this process's ownership of it: logout recording
// it, a sign-in clearing it, logout's cleanup clearing it. Each holds it only for a
// bounded ledger write, never while calling GitHub. It is taken after the credential
// lock where both are held, never before.
func (l *credLocks) markerLock(userID, provider string) func() {
	key := credKey(userID, provider)
	l.mu.Lock()
	if l.markers == nil {
		l.markers = make(map[string]*sync.Mutex)
	}
	m := l.markers[key]
	if m == nil {
		m = &sync.Mutex{}
		l.markers[key] = m
	}
	l.mu.Unlock()
	m.Lock()
	return m.Unlock
}

type credLock struct {
	ch   chan struct{}
	refs int // holders and waiters; the entry is dropped at zero
}

func (l *credLocks) lock(ctx context.Context, userID, provider string) (func(), error) {
	key := credKey(userID, provider)
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
	if isPersonalToken(cred) {
		// A personal access token is the write grant's alternative to an OAuth token,
		// usable only while the deployment allows tokens (ADR 0011).
		return p.grant && p.tokens
	}
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

// ScheduledRunAllowed reports whether gofer may start a run nobody requested (the
// scheduled refresh) on the user's behalf for the provider. Every condition must be
// confirmed: gofer signs in with the provider; no logout of the credential is
// pending; a credential is stored, readable, and from the configured client (a legacy
// row counts only where the broker would tag and vend it); and the account that
// granted it is still allowed to sign in (ALLOWED_GITHUB_LOGINS, checked against the
// login recorded at sign-in, so a row stored before gofer recorded one is refused
// while an allow-list is set). A read error answers false with the error. It reads
// only: it takes no credential lock and refreshes nothing; the run's own vend does.
func (h *Handler) ScheduledRunAllowed(ctx context.Context, userID, providerName string) (bool, error) {
	p, ok := h.providers[providerName]
	if !ok || p.grant || h.vault == nil {
		return false, nil
	}
	pending, err := h.logoutPending(ctx, userID, providerName)
	if err != nil {
		return false, fmt.Errorf("read pending logout: %w", err)
	}
	if pending {
		return false, nil
	}
	cred, err := h.vault.Get(ctx, userID, providerName)
	if errors.Is(err, vault.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !fromConfiguredClient(p, cred) {
		return false, nil
	}
	if h.cfg != nil && !h.cfg.SignInAllowed(providerName, cred.Login) {
		return false, nil
	}
	return true, nil
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
	if !ok || p.grant {
		// Not a provider gofer signs in with, or the write grant, which never leaves
		// the web tier: the broker must never vend it to a run (ADR 0009).
		return vault.Credential{}, vault.ErrNotFound
	}
	return h.vend(ctx, userID, p, req)
}

// vend is Credential's work for a provider the caller has already chosen: under the
// credential lock, and checked against a pending logout before and after.
func (h *Handler) vend(ctx context.Context, userID string, p *provider, req VendRequest) (vault.Credential, error) {
	providerName := p.name
	if h.vault == nil {
		return vault.Credential{}, vault.ErrSignInRequired
	}
	unlock, err := h.locks.lock(ctx, userID, providerName)
	if err != nil {
		return vault.Credential{}, err
	}
	defer unlock()

	cred, err := h.credentialLocked(ctx, userID, p, req)
	// Checked again after the work above, which can block (a refresh, check-token):
	// a logout that began meanwhile must not see a token vended after it. Logout's
	// grant revocation kills anything vended before it.
	if err == nil {
		pending, perr := h.logoutPending(ctx, userID, providerName)
		if perr != nil {
			// Withheld, but not "sign-in required": the ledger could not be read, so
			// this is a temporary failure, not a confirmed reason to reconnect.
			return vault.Credential{}, fmt.Errorf("read pending logout: %w", perr)
		}
		if pending {
			return vault.Credential{}, vault.ErrSignInRequired
		}
	}
	return cred, err
}

// credentialLocked is Credential's work, holding the credential lock.
func (h *Handler) credentialLocked(ctx context.Context, userID string, p *provider, req VendRequest) (vault.Credential, error) {
	providerName := p.name
	pending, mine, err := h.claimLogout(ctx, userID, providerName)
	if err != nil {
		// Fail closed: a logout may be pending.
		return vault.Credential{}, fmt.Errorf("read pending logout: %w", err)
	}
	if pending {
		// Refuse. A logout running here owns the credential, and needs its token to
		// revoke the grant. One nobody ran (begun before a restart, or whose
		// background wait gave up) was just claimed: finish it, holding the lock.
		if mine != nil {
			res := h.runLogout(ctx, userID, providerName, mine)
			slog.InfoContext(ctx, "finished a pending logout", "user", userID, "provider", providerName, "confirmed", res.confirmed)
		}
		return vault.Credential{}, vault.ErrSignInRequired
	}
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
// token in it, oauth2 has nothing to reuse. The exchange runs detached from ctx's
// cancellation, under exchangeTimeout: cancelling a run after GitHub has rotated the
// pair would otherwise leave the spent pair stored and the user signed out.
func (h *Handler) refresher(ctx context.Context, p *provider, refreshToken string) oauth2.TokenSource {
	return detachedSource{h: h, p: p, ctx: ctx, refreshToken: refreshToken}
}

type detachedSource struct {
	h            *Handler
	p            *provider
	ctx          context.Context
	refreshToken string
}

func (d detachedSource) Token() (*oauth2.Token, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(d.ctx), exchangeTimeout)
	defer cancel()
	if d.h.client != nil {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, d.h.client)
	}
	return d.p.oauth.TokenSource(ctx, &oauth2.Token{RefreshToken: d.refreshToken}).Token()
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
		Login:         old.Login,
	}
	if fresh.RefreshToken == "" {
		fresh.RefreshToken = old.RefreshToken
		fresh.RefreshExpiry = old.RefreshExpiry
	}
	// The old refresh token is spent: finish this write even if the requesting run
	// goes away.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), exchangeTimeout)
	defer cancel()
	// Stored even if a logout began meanwhile: logout revokes with the newest token,
	// and Credential refuses to vend it.
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

// storeRotated writes the pair a refresh returned over cred, unconditionally (the
// caller holds the lock), and returns it.
func (h *Handler) storeRotated(ctx context.Context, userID string, p *provider, old vault.Credential, tok *oauth2.Token) (vault.Credential, error) {
	fresh := vault.Credential{
		Provider:      old.Provider,
		AccessToken:   tok.AccessToken,
		RefreshToken:  tok.RefreshToken,
		TokenType:     tok.TokenType,
		Expiry:        tok.Expiry,
		RefreshExpiry: refreshExpiry(tok, h.now()),
		ClientID:      p.oauth.ClientID,
		Version:       newVersion(),
		Login:         old.Login,
	}
	if fresh.RefreshToken == "" {
		fresh.RefreshToken, fresh.RefreshExpiry = old.RefreshToken, old.RefreshExpiry
	}
	return fresh, h.vault.Put(ctx, userID, fresh)
}

// refreshExpiry is when tok's refresh token expires, from GitHub's
// refresh_token_expires_in, or zero.
func refreshExpiry(tok *oauth2.Token, now time.Time) time.Time {
	// A JSON response gives a float64; a form-encoded one gives the pinned oauth2
	// package's int64 (or a string, for a value it cannot parse as one).
	var secs float64
	switch v := tok.Extra("refresh_token_expires_in").(type) {
	case float64:
		secs = v
	case int64:
		secs = float64(v)
	case int:
		secs = float64(v)
	case json.Number:
		secs, _ = v.Float64()
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
	if pending, err := h.logoutPending(ctx, userID, cred.Provider); pending || err != nil {
		return // a pending logout deletes it, after trying to revoke with it
	}
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
// is not. Anything else is inconclusive and an error: 422 can mean a validation
// failure or abuse protection, and treating it as "revoked" would delete or rotate a
// good credential.
func (h *Handler) checkToken(ctx context.Context, p *provider, accessToken string) (bool, error) {
	status, err := h.applicationCall(ctx, p, http.MethodPost, "/token", accessToken)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
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
func (h *Handler) storeSignIn(ctx context.Context, p *provider, userID, login string, tok *oauth2.Token) (restart bool, err error) {
	unlock, err := h.locks.lock(ctx, userID, p.name)
	if err != nil {
		return false, err
	}
	defer unlock()
	if p.github() {
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
		Login:         login,
	}
	// Best-effort, as before: a vault failure must not block interactive login. The
	// broker then answers "sign-in required", and the page says so.
	//
	// The new credential and the clearing of any pending logout commit together, so
	// a restart sees either the pending logout or the sign-in that replaced it, never
	// the new credential paired with the old logout (whose recovery would revoke the
	// grant the user just gave). The marker lock keeps a logout that commits right
	// after this write from losing its ownership to this sign-in's clear.
	unlockMarker := h.locks.markerLock(userID, p.name)
	defer unlockMarker()
	if l, ok := h.vault.(vault.LogoutLedger); ok {
		err = l.PutClearingLogout(ctx, userID, cred)
	} else if err = h.vault.Put(ctx, userID, cred); err == nil {
		err = h.ledger.ClearLogout(ctx, userID, p.name)
	}
	if err != nil {
		// A pending logout stays pending: the old row may still be there, and it
		// must be neither vended nor spared.
		slog.WarnContext(ctx, "storing the sign-in credential failed", "user", userID, "provider", p.name, "err", err)
		return false, nil
	}
	h.supersedeLogoutLocked(userID, p.name)
	return false, nil
}
