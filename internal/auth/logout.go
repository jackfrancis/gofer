package auth

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/vault"
)

// This file is logout's side of the credential (ADR 0007): recording a logout,
// deciding who performs its revoke-then-delete, and reporting the outcome.
//
// A logout of a (user, provider) is pending from the commit that records it until
// the credential is deleted, or a new sign-in replaces it. While it is pending,
// logout alone revokes and deletes the credential; every other holder refuses to vend
// it and deletes nothing, so the token logout needs for revocation stays in place.
//
// Two records say a logout is pending:
//
//   - durably, the vault's LogoutLedger marker, which survives a restart;
//   - in this process, a *localLogout, the logout operation itself: whether an
//     operation here is running its revoke-then-delete, and, once known, its
//     outcome.
//
// Both change together, under the (user, provider)'s marker lock. A localLogout is
// published only after the commit that records the marker succeeds. A second logout
// of the same credential while one is running joins it: one revoke-then-delete, one
// outcome, which every request waiting on it reports. A marker no operation here is
// running (begun before a restart, or whose background wait gave up) is an orphan:
// the next credential lock holder claims it, under the marker lock, and finishes it.

// stepTimeout bounds one ledger read or write, detached from the caller.
const stepTimeout = 5 * time.Second

func credKey(userID, provider string) string { return userID + "\x00" + provider }

// localLogout is one logout operation as this process knows it.
type localLogout struct {
	running bool          // an operation here is running its revoke-then-delete
	done    chan struct{} // closed once the logout has an outcome
	result  logoutResult  // the outcome; read only after done is closed
}

func newLocalLogout() *localLogout {
	return &localLogout{running: true, done: make(chan struct{})}
}

func (lo *localLogout) finished() bool {
	select {
	case <-lo.done:
		return true
	default:
		return false
	}
}

// finish records lo's outcome, once.
func (lo *localLogout) finish(res logoutResult) {
	if !lo.finished() {
		lo.result = res
		close(lo.done)
	}
	lo.running = false
}

// The accessors below guard the map with l.mu. Callers that change a logout's state
// also hold the key's marker lock, so the change and the durable marker agree.

func (l *credLocks) currentLogout(userID, provider string) *localLogout {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.logouts[credKey(userID, provider)]
}

func (l *credLocks) setLogout(userID, provider string, lo *localLogout) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.logouts == nil {
		l.logouts = make(map[string]*localLogout)
	}
	l.logouts[credKey(userID, provider)] = lo
}

// removeLogout forgets lo, if it is still the current logout.
func (l *credLocks) removeLogout(userID, provider string, lo *localLogout) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.logouts[credKey(userID, provider)] == lo {
		delete(l.logouts, credKey(userID, provider))
	}
}

// isCurrent reports whether lo is the current logout of the credential.
func (l *credLocks) isCurrent(userID, provider string, lo *localLogout) bool {
	return l.currentLogout(userID, provider) == lo
}

// logoutCommitter is a vault that consumes the browser session and records a pending
// logout in one commit (storage.VaultStore, over the database that also holds the
// sessions). It answers session.ErrNotFound, recording nothing, when the session is
// already gone.
type logoutCommitter interface {
	MarkLogoutEndingSession(ctx context.Context, sessionKey, userID, provider string) error
}

// beginLogout records a logout of the user's credential and consumes the browser
// session (sessionKey, the hashed id a session store sees; "" for none) in one
// commit where the vault allows it, then publishes the logout operation and returns
// it. The operation exists only once the commit succeeded: on any error nothing
// changed, here or durably. session.ErrNotFound means another logout already
// consumed this session; that one owns the revocation. If a logout of the credential
// is already running (another session's, or an orphan a credential request
// claimed), the new one joins it.
func (h *Handler) beginLogout(ctx context.Context, sessionKey, userID, providerName string) (*localLogout, error) {
	defer h.locks.markerLock(userID, providerName)()
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stepTimeout)
	defer cancel()
	var err error
	if c, ok := h.vault.(logoutCommitter); ok {
		err = c.MarkLogoutEndingSession(lctx, sessionKey, userID, providerName)
	} else {
		// An in-memory vault: there is no restart to survive. Consume the session
		// from the manager's own store first, so each session still starts at most
		// one logout.
		if sessionKey != "" {
			consumed, cerr := h.sessions.ConsumeStored(lctx, sessionKey)
			switch {
			case cerr != nil:
				err = cerr
			case !consumed:
				err = session.ErrNotFound
			}
		}
		if err == nil {
			err = h.ledger.MarkLogout(lctx, userID, providerName)
		}
	}
	if err != nil {
		return nil, err
	}
	// An unfinished logout of this credential is reused, never replaced: joined if
	// it is running, resumed if its background wait gave up. Replacing it would leave
	// its waiters holding a record nobody supersedes when the user signs in again.
	if lo := h.locks.currentLogout(userID, providerName); lo != nil && !lo.finished() {
		lo.running = true
		return lo, nil
	}
	lo := newLocalLogout()
	h.locks.setLogout(userID, providerName, lo)
	return lo, nil
}

// markLogout records a logout with no browser session to end.
func (h *Handler) markLogout(ctx context.Context, userID, providerName string) (*localLogout, error) {
	return h.beginLogout(ctx, "", userID, providerName)
}

// logoutPending reports whether a logout of the credential is pending, reading this
// process's record and the durable marker together under the marker lock. A ledger
// that cannot be read counts as pending, and its error is returned: the caller
// withholds the credential as a temporary failure.
func (h *Handler) logoutPending(ctx context.Context, userID, providerName string) (bool, error) {
	defer h.locks.markerLock(userID, providerName)()
	local := h.locks.currentLogout(userID, providerName) != nil
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stepTimeout)
	defer cancel()
	marked, err := h.ledger.LogoutPending(lctx, userID, providerName)
	if err != nil {
		return true, err
	}
	return local || marked, nil
}

// claimLogout is logoutPending for a credential lock holder about to vend: it
// classifies the pending logout and, if it is an orphan (no operation here running
// it), claims it in the same marker-lock section, so a logout that starts meanwhile
// cannot be mistaken for an abandoned one. A non-nil mine is the caller's to finish
// (logoutLocked, then completeLogout) while it holds the credential lock.
func (h *Handler) claimLogout(ctx context.Context, userID, providerName string) (pending bool, mine *localLogout, err error) {
	defer h.locks.markerLock(userID, providerName)()
	lo := h.locks.currentLogout(userID, providerName)
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stepTimeout)
	defer cancel()
	marked, err := h.ledger.LogoutPending(lctx, userID, providerName)
	if err != nil {
		return true, nil, err
	}
	switch {
	case lo == nil && !marked:
		return false, nil, nil
	case lo != nil && lo.running && !lo.finished():
		return true, nil, nil // an operation here owns it
	case lo != nil && !lo.finished():
		lo.running = true // its background wait gave up: carry on with it
		return true, lo, nil
	}
	mine = newLocalLogout()
	h.locks.setLogout(userID, providerName, mine)
	return true, mine, nil
}

// completeLogout records lo's outcome for everyone waiting on it. If the credential
// was deleted, the durable marker is cleared and the logout forgotten, in the same
// marker-lock section; otherwise it stays pending, for the next lock holder to
// finish.
func (h *Handler) completeLogout(ctx context.Context, userID, providerName string, lo *localLogout, res logoutResult, deleted bool) {
	defer h.locks.markerLock(userID, providerName)()
	lo.finish(res)
	if !deleted || !h.locks.isCurrent(userID, providerName, lo) {
		return
	}
	for attempt := 0; attempt < 2; attempt++ {
		lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stepTimeout)
		err := h.ledger.ClearLogout(lctx, userID, providerName)
		cancel()
		if err == nil {
			h.locks.removeLogout(userID, providerName, lo)
			return
		}
		// The marker stays, which fails closed: the credential is gone anyway, and
		// the next lock holder or a new sign-in clears it.
		slog.WarnContext(ctx, "clearing a pending logout failed", "user", userID, "provider", providerName, "attempt", attempt+1, "err", err)
	}
}

// supersedeLogoutLocked forgets the pending logout because a new sign-in replaced
// the credential: its revoke-then-delete must not run, and anyone waiting on it is
// told the old grant was not revoked. The caller holds the marker lock and has just
// cleared the durable marker in the same section.
func (h *Handler) supersedeLogoutLocked(userID, providerName string) {
	lo := h.locks.currentLogout(userID, providerName)
	if lo == nil {
		return
	}
	h.locks.removeLogout(userID, providerName, lo)
	lo.finish(h.unconfirmed(providerName, h.providers[providerName], nil))
}

// releaseLogout says no operation here runs lo any more (its background wait gave
// up): the next credential lock holder claims and finishes it.
func (h *Handler) releaseLogout(userID, providerName string, lo *localLogout) {
	defer h.locks.markerLock(userID, providerName)()
	lo.running = false
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
		unlock := h.locks.markerLock(k.UserID, k.Provider)
		var lo *localLogout
		if h.locks.currentLogout(k.UserID, k.Provider) == nil {
			lo = newLocalLogout()
			h.locks.setLogout(k.UserID, k.Provider, lo)
		}
		unlock()
		if lo != nil {
			go h.pendingLogout(context.WithoutCancel(ctx), k.UserID, k.Provider, lo)
		}
	}
}

// logoutResult is what logout learned about revocation at GitHub.
type logoutResult struct {
	confirmed bool
	// settings lists the GitHub settings pages where the user can revoke gofer by
	// hand when revocation is not confirmed: the issuing client's kind, or both
	// pages when gofer cannot tell which client issued the credential.
	settings []string
}

// revokeAndDelete runs logout operation lo, begun by beginLogout: under the
// credential lock, read the credential, refresh it if it has expired or is about to
// (revoking the grant needs a valid access token, and a session outlives one), store
// the rotated pair, revoke the grant, and delete the row (logoutLocked). If another
// request performed lo meanwhile (it joined this one), its outcome is reported; if a
// sign-in replaced the credential, lo is finished as unconfirmed and nothing is
// revoked.
//
// The lock is waited for detached from the browser request, for logoutLockWait. If
// it is still busy, logout reports revocation unconfirmed and a background wait
// (pendingLogout) carries on with lo once the lock is free. If that wait gives up,
// or the process restarts, the durable marker keeps the credential unvendable, and
// the next lock holder finishes the logout.
//
// Revocation is confirmed only when GitHub confirmed it, or for a provider gofer
// revokes nothing at. A missing GitHub credential is unconfirmed: the broker deletes
// a row whose refresh token is dead, which after a restore from backup does not mean
// the grant is gone.
func (h *Handler) revokeAndDelete(ctx context.Context, userID, providerName string, lo *localLogout) logoutResult {
	// Wait for the lock, or for lo's outcome if another request performs it first
	// (this one joined it): then there is nothing left to do but report it.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), logoutLockWait)
	defer cancel()
	go func() {
		select {
		case <-lo.done:
			cancel()
		case <-wctx.Done():
		}
	}()
	unlock, err := h.locks.lock(wctx, userID, providerName)
	if err != nil {
		if lo.finished() {
			return lo.result
		}
		slog.WarnContext(ctx, "logout: credential lock busy; revocation deferred", "user", userID, "provider", providerName)
		go h.pendingLogout(context.WithoutCancel(ctx), userID, providerName, lo)
		return h.unconfirmed(providerName, h.providers[providerName], nil)
	}
	defer unlock()
	return h.runLogout(ctx, userID, providerName, lo)
}

// pendingLogout carries on with lo when logout could not take the lock in time, or
// one resumed after a restart.
func (h *Handler) pendingLogout(ctx context.Context, userID, providerName string, lo *localLogout) {
	wctx, cancel := context.WithTimeout(ctx, pendingLogoutWait)
	defer cancel()
	go func() {
		select {
		case <-lo.done:
			cancel()
		case <-wctx.Done():
		}
	}()
	unlock, err := h.locks.lock(wctx, userID, providerName)
	if err != nil {
		if lo.finished() {
			return // another request performed it
		}
		h.releaseLogout(userID, providerName, lo)
		slog.WarnContext(ctx, "logout: deferred revocation timed out; left to the next credential access", "user", userID, "provider", providerName)
		return
	}
	defer unlock()
	res := h.runLogout(ctx, userID, providerName, lo)
	slog.InfoContext(ctx, "logout: deferred revocation finished", "user", userID, "provider", providerName, "confirmed", res.confirmed)
}

// runLogout performs lo, holding the credential lock, unless it already has an
// outcome (another request performed it, or a sign-in superseded it) or is no longer
// the credential's current logout. The ownership check is made under the marker
// lock, right before the revocation: a logout that is not current must never revoke,
// since the credential may be one the user has just signed in to get.
func (h *Handler) runLogout(ctx context.Context, userID, providerName string, lo *localLogout) logoutResult {
	if res, owned := h.ownLogout(userID, providerName, lo); !owned {
		return res
	}
	res, deleted := h.logoutLocked(ctx, userID, providerName)
	h.completeLogout(ctx, userID, providerName, lo, res, deleted)
	return res
}

// ownLogout reports whether lo may run: unfinished and still the current logout.
// If it is neither, it returns the outcome to report, finishing a stale lo as
// unconfirmed (nothing was revoked for it). The caller holds the credential lock,
// under which only a sign-in (which supersedes, finishing lo) changes the current
// logout to something else.
func (h *Handler) ownLogout(userID, providerName string, lo *localLogout) (logoutResult, bool) {
	defer h.locks.markerLock(userID, providerName)()
	if lo.finished() {
		return lo.result, false
	}
	if !h.locks.isCurrent(userID, providerName, lo) {
		lo.finish(h.unconfirmed(providerName, h.providers[providerName], nil))
		return lo.result, false
	}
	lo.running = true
	return logoutResult{}, true
}

// logoutLocked revokes and deletes the credential, holding the credential lock, and
// reports whether the row was deleted. Each step gets its own deadline, detached from
// the caller's: waiting for the lock must not leave too little time to read the row
// or to revoke with a token a refresh has just rotated.
func (h *Handler) logoutLocked(ctx context.Context, userID, providerName string) (logoutResult, bool) {
	res := h.revokeLocked(ctx, userID, providerName)
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stepTimeout)
	defer cancel()
	if err := h.vault.Delete(dctx, userID, providerName); err != nil {
		// The logout stays pending, so the credential is still never vended, and the
		// next lock holder tries again.
		slog.WarnContext(ctx, "deleting a credential on logout failed", "user", userID, "provider", providerName, "err", err)
		return res, false
	}
	return res, true
}

// revokeLocked is logoutLocked's revocation.
func (h *Handler) revokeLocked(ctx context.Context, userID, providerName string) logoutResult {
	step := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), exchangeTimeout)
	}
	if !isGitHub(providerName) {
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
			// The old refresh token is spent. Store the rotated pair (still under the
			// pending logout, so never vended) before asking GitHub to revoke: if the
			// process stops in between, recovery revokes with this token instead of
			// finding only the spent one.
			sctx, scancel := step()
			if _, err := h.storeRotated(sctx, userID, p, cred, tok); err != nil {
				slog.WarnContext(ctx, "logout: storing the rotated pair failed; revoking anyway", "user", userID, "err", err)
			}
			scancel()
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

// unconfirmed is an unconfirmed logoutResult, with the settings pages for the
// credential's issuer. Non-GitHub providers get none: gofer revokes nothing there.
func (h *Handler) unconfirmed(providerName string, p *provider, cred *vault.Credential) logoutResult {
	if !isGitHub(providerName) {
		return logoutResult{confirmed: true}
	}
	if providerName == WriteProvider {
		// The write grant is always a classic OAuth app (ADR 0009).
		return logoutResult{settings: []string{oauthAppsSettings}}
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
