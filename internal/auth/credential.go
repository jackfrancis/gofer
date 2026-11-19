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
}

type credLock struct {
	ch   chan struct{}
	refs int // holders and waiters; the entry is dropped at zero
}

// localLogout is a logout pending in this process.
type localLogout struct {
	running bool // an operation here owns its revoke-then-delete
}

func credKey(userID, provider string) string { return userID + "\x00" + provider }

// beginLogout records a logout pending here, owned by the caller.
func (l *credLocks) beginLogout(userID, provider string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.logouts == nil {
		l.logouts = make(map[string]*localLogout)
	}
	l.logouts[credKey(userID, provider)] = &localLogout{running: true}
}

// localLogoutState reports whether a logout is pending here, and whether an
// operation here is running it.
func (l *credLocks) localLogoutState(userID, provider string) (pending, running bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	lo := l.logouts[credKey(userID, provider)]
	return lo != nil, lo != nil && lo.running
}

// releaseLogout says no operation here runs the pending logout any more (it gave
// up, or its delete failed): the next lock holder finishes it.
func (l *credLocks) releaseLogout(userID, provider string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if lo := l.logouts[credKey(userID, provider)]; lo != nil {
		lo.running = false
	}
}

// dropLogout forgets the pending logout: it finished, or a new sign-in replaced
// the credential.
func (l *credLocks) dropLogout(userID, provider string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.logouts, credKey(userID, provider))
}

// stepTimeout bounds one ledger read or write, detached from the caller.
const stepTimeout = 5 * time.Second

// markLogout records a pending logout, here and durably in the ledger, so the
// refusal to vend survives a restart. Logout calls it before it discards the
// session. A ledger failure is logged: the logout still proceeds, protected in this
// process only.
func (h *Handler) markLogout(ctx context.Context, userID, providerName string) {
	h.locks.beginLogout(userID, providerName)
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stepTimeout)
	defer cancel()
	if err := h.ledger.MarkLogout(lctx, userID, providerName); err != nil {
		slog.ErrorContext(ctx, "logout: recording the pending logout failed; it will not survive a restart", "user", userID, "provider", providerName, "err", err)
	}
}

// clearLogout forgets a pending logout, here and in the ledger: logout deleted the
// credential, or a new sign-in stored its replacement. A ledger failure is retried
// once; if it persists the marker stays, which fails closed (the user signs in
// again).
func (h *Handler) clearLogout(ctx context.Context, userID, providerName string) {
	h.locks.dropLogout(userID, providerName)
	for attempt := 0; attempt < 2; attempt++ {
		lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stepTimeout)
		err := h.ledger.ClearLogout(lctx, userID, providerName)
		cancel()
		if err == nil {
			return
		}
		slog.WarnContext(ctx, "clearing a pending logout failed", "user", userID, "provider", providerName, "attempt", attempt+1, "err", err)
	}
}

// logoutPending reports whether a logout of the (user, provider) has begun and not
// finished — pending in this process, or recorded in the ledger by one before a
// restart — and whether it is orphaned: pending, with no operation in this process
// running it. While a logout is pending, logout alone revokes and deletes the
// credential; every other holder refuses to vend it and deletes nothing, so the token
// logout needs for revocation stays in place. An orphan is finished by the next lock
// holder. A ledger that cannot be read counts as pending, not orphaned: it fails
// closed.
func (h *Handler) logoutPending(ctx context.Context, userID, providerName string) (pending, orphan bool, err error) {
	local, running := h.locks.localLogoutState(userID, providerName)
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stepTimeout)
	defer cancel()
	marked, err := h.ledger.LogoutPending(lctx, userID, providerName)
	if err != nil {
		return true, false, err
	}
	pending = local || marked
	return pending, pending && !running, nil
}

// ResumePendingLogouts finishes, in the background, every logout the ledger says a
// previous process began and did not finish. The web tier calls it at startup.
func (h *Handler) ResumePendingLogouts(ctx context.Context) {
	lctx, cancel := context.WithTimeout(ctx, stepTimeout)
	keys, err := h.ledger.PendingLogouts(lctx)
	cancel()
	if err != nil {
		slog.WarnContext(ctx, "listing pending logouts failed; they finish at the next credential access", "err", err)
		return
	}
	for _, k := range keys {
		if pending, _ := h.locks.localLogoutState(k.UserID, k.Provider); pending {
			continue
		}
		h.locks.beginLogout(k.UserID, k.Provider)
		go h.pendingLogout(context.WithoutCancel(ctx), k.UserID, k.Provider)
	}
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

	cred, err := h.credentialLocked(ctx, userID, p, req)
	// Checked again after the work above, which can block (a refresh, check-token):
	// a logout that began meanwhile must not see a token vended after it. Logout's
	// grant revocation kills anything vended before it.
	if err == nil {
		if pending, _, perr := h.logoutPending(ctx, userID, providerName); pending || perr != nil {
			return vault.Credential{}, vault.ErrSignInRequired
		}
	}
	return cred, err
}

// credentialLocked is Credential's work, holding the credential lock.
func (h *Handler) credentialLocked(ctx context.Context, userID string, p *provider, req VendRequest) (vault.Credential, error) {
	providerName := p.name
	pending, orphan, err := h.logoutPending(ctx, userID, providerName)
	if err != nil {
		// Fail closed: a logout may be pending.
		return vault.Credential{}, fmt.Errorf("read pending logout: %w", err)
	}
	if pending {
		// Refuse. A logout running here owns the credential, and needs its token to
		// revoke the grant. One nobody runs (begun before a restart, or whose
		// background wait gave up) is finished now, holding the lock.
		if orphan {
			res := h.logoutLocked(ctx, userID, providerName)
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
	if pending, _, err := h.logoutPending(ctx, userID, cred.Provider); pending || err != nil {
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
		// A pending logout stays pending: the old row may still be there, and it
		// must be neither vended nor spared.
		slog.WarnContext(ctx, "storing the sign-in credential failed", "user", userID, "provider", p.name, "err", err)
		return false, nil
	}
	// The new credential replaced whatever a pending logout was going to remove.
	h.clearLogout(ctx, userID, p.name)
	return false, nil
}

// logoutResult is what logout learned about revocation at GitHub.
type logoutResult struct {
	confirmed bool
	// settings lists the GitHub settings pages where the user can revoke gofer by
	// hand when revocation is not confirmed: the issuing client's kind, or both
	// pages when gofer cannot tell which client issued the credential.
	settings []string
}

// revokeAndDelete is logout's credential step. The logout is marked pending first
// (markLogout; Logout does it before discarding the session), which makes logout the
// credential's only remaining writer: from then on every other holder refuses to
// vend it and deletes nothing. Then, under the credential lock (logoutLocked): read
// the credential, refresh it if it has expired or is about to (revoking the grant
// needs a valid access token, and a session outlives one), revoke the grant, and
// delete the row.
//
// The lock is waited for detached from the browser request, for logoutLockWait. If
// it is still busy, logout reports revocation unconfirmed and a background wait
// (pendingLogout) does the same revoke-then-delete once the lock is free. If that
// wait gives up, or the process restarts, the pending mark (durable in the ledger)
// keeps the credential unvendable, and the next lock holder finishes the logout.
//
// Revocation is confirmed only when GitHub confirmed it, or for a provider gofer
// revokes nothing at. A missing GitHub credential is unconfirmed: the broker deletes
// a row whose refresh token is dead, which after a restore from backup does not mean
// the grant is gone.
func (h *Handler) revokeAndDelete(ctx context.Context, userID, providerName string) logoutResult {
	if pending, _ := h.locks.localLogoutState(userID, providerName); !pending {
		h.markLogout(ctx, userID, providerName)
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), logoutLockWait)
	defer cancel()
	unlock, err := h.locks.lock(wctx, userID, providerName)
	if err != nil {
		slog.WarnContext(ctx, "logout: credential lock busy; revocation deferred", "user", userID, "provider", providerName)
		go h.pendingLogout(context.WithoutCancel(ctx), userID, providerName)
		return h.unconfirmed(providerName, h.providers[providerName], nil)
	}
	defer unlock()
	defer h.locks.releaseLogout(userID, providerName) // still pending only if the delete failed
	if pending, _ := h.locks.localLogoutState(userID, providerName); !pending {
		// A new sign-in stored a replacement while logout waited: leave it, and say
		// gofer's access was not revoked.
		return h.unconfirmed(providerName, h.providers[providerName], nil)
	}
	return h.logoutLocked(ctx, userID, providerName)
}

// pendingLogout finishes a logout that could not take the lock in time, or one
// resumed after a restart: it waits for the lock and runs the same
// revoke-then-delete, unless a new sign-in stored a replacement first.
func (h *Handler) pendingLogout(ctx context.Context, userID, providerName string) {
	defer h.locks.releaseLogout(userID, providerName)
	wctx, cancel := context.WithTimeout(ctx, pendingLogoutWait)
	defer cancel()
	unlock, err := h.locks.lock(wctx, userID, providerName)
	if err != nil {
		// Still pending, and durable: nothing is vended, and the next lock holder
		// finishes the logout.
		slog.WarnContext(ctx, "logout: deferred revocation timed out; left to the next credential access", "user", userID, "provider", providerName)
		return
	}
	defer unlock()
	if pending, _ := h.locks.localLogoutState(userID, providerName); !pending {
		return
	}
	res := h.logoutLocked(ctx, userID, providerName)
	slog.InfoContext(ctx, "logout: deferred revocation finished", "user", userID, "provider", providerName, "confirmed", res.confirmed)
}

// logoutLocked revokes and deletes the credential, holding the credential lock.
// Each step gets its own deadline, detached from the caller's: waiting for the lock
// must not leave too little time to read the row or to revoke with a token a
// refresh has just rotated.
func (h *Handler) logoutLocked(ctx context.Context, userID, providerName string) logoutResult {
	defer h.finishLogout(ctx, userID, providerName)
	step := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), exchangeTimeout)
	}
	if providerName != "github" {
		return logoutResult{confirmed: true} // gofer revokes nothing at other providers
	}
	p, ok := h.providers[providerName]
	rctx, cancel := step()
	cred, err := h.vault.Get(rctx, userID, providerName)
	cancel()
	switch {
	case errors.Is(err, vault.ErrNotFound):
		slog.InfoContext(ctx, "logout: no stored github credential; revocation unconfirmed", "user", userID)
		return h.unconfirmed(providerName, p, nil)
	case err != nil:
		slog.WarnContext(ctx, "reading a credential on logout failed", "user", userID, "provider", providerName, "err", err)
		return h.unconfirmed(providerName, p, nil)
	case !ok:
		slog.InfoContext(ctx, "logout: github sign-in is not configured; revocation unconfirmed", "user", userID)
		return h.unconfirmed(providerName, nil, &cred)
	case !fromConfiguredClient(p, cred):
		// Issued by another client (before a cutover or rollback): this client's
		// credentials cannot revoke it.
		slog.InfoContext(ctx, "logout: credential is from another client; revocation unconfirmed", "user", userID)
		return h.unconfirmed(providerName, p, &cred)
	}
	token := cred.AccessToken
	if cred.RefreshToken != "" && !cred.Expiry.IsZero() && h.now().Add(earlyRefresh).After(cred.Expiry) {
		tok, err := h.refresher(ctx, p, cred.RefreshToken).Token() // its own deadline
		switch {
		case err == nil:
			token = tok.AccessToken
		case refreshErrorCode(err) == errBadRefreshToken:
			// This does not prove the grant is gone: after a restore from backup the
			// vault can hold a spent refresh token while a newer pair still works.
			slog.InfoContext(ctx, "logout: refresh token is dead; revocation unconfirmed", "user", userID)
			return h.unconfirmed(providerName, p, &cred)
		case !h.now().Before(cred.Expiry):
			slog.WarnContext(ctx, "logout: refresh failed and the token has expired; revocation unconfirmed", "user", userID, "code", refreshErrorCode(err))
			return h.unconfirmed(providerName, p, &cred)
		}
	}
	rctx, cancel = step()
	defer cancel()
	if err := h.revokeGrant(rctx, p, token); err != nil {
		slog.WarnContext(ctx, "logout: grant revocation failed; unconfirmed", "user", userID, "err", err)
		return h.unconfirmed(providerName, p, &cred)
	}
	return logoutResult{confirmed: true}
}

// finishLogout deletes the row and clears the pending logout, holding the
// credential lock. If the delete fails the logout stays pending, so the credential
// is still never vended, and the next lock holder tries again.
func (h *Handler) finishLogout(ctx context.Context, userID, providerName string) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := h.vault.Delete(dctx, userID, providerName); err != nil {
		slog.WarnContext(ctx, "deleting a credential on logout failed", "user", userID, "provider", providerName, "err", err)
		return
	}
	h.clearLogout(ctx, userID, providerName)
}

// unconfirmed is an unconfirmed logoutResult, with the settings pages for the
// credential's issuer. Non-GitHub providers get none: gofer revokes nothing there.
func (h *Handler) unconfirmed(providerName string, p *provider, cred *vault.Credential) logoutResult {
	if providerName != "github" {
		return logoutResult{confirmed: true}
	}
	return logoutResult{settings: h.revocationSettings(p, cred)}
}

// revocationSettings picks where a user revokes gofer by hand, from the client that
// issued cred: a legacy row (no client recorded) came from the OAuth app; a row from
// the configured client follows GITHUB_CLIENT_KIND; anything else — another client
// after a cutover or rollback, or no credential at all — could be either, so both
// pages are offered.
func (h *Handler) revocationSettings(p *provider, cred *vault.Credential) []string {
	both := []string{githubAppsSettings, oauthAppsSettings}
	switch {
	case cred == nil:
		return both
	case cred.ClientID == "":
		return []string{oauthAppsSettings}
	case p != nil && cred.ClientID == p.oauth.ClientID:
		if h.cfg != nil && h.cfg.Providers.IsGitHubApp() {
			return []string{githubAppsSettings}
		}
		return []string{oauthAppsSettings}
	}
	return both
}
