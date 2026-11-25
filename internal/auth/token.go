package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackfrancis/gofer/internal/vault"
)

// This file is commenting with a personal access token (ADR 0011): an alternative to
// the write client's OAuth grant (ADR 0009) for organizations that block unapproved
// OAuth apps (the kubernetes org does). A signed-in GitHub user pastes a CLASSIC token
// whose only scope is public_repo, the same reach as the OAuth grant; gofer checks it
// and stores it as the write grant (provider WriteProvider, marked by
// PersonalTokenClient), under the same lock, versioning and logout ledger. Like the
// OAuth grant it never leaves the web tier and is never vended to a run.
//
// What differs: gofer cannot revoke a personal access token, so disconnect and logout
// delete gofer's copy and send the user to GitHub's token settings; and the token must
// expire, within maxTokenLifetime, so a copy that leaks stops working on its own.

// PersonalTokenClient is the ClientID recorded on a write grant that is a personal
// access token rather than an OAuth token. Not a secret.
const PersonalTokenClient = "github-personal-access-token"

// tokensSettings is GitHub's page where a user deletes their personal access tokens.
const tokensSettings = "https://github.com/settings/tokens"

// maxTokenLifetime is the longest expiry gofer accepts on a token: 90 days, the
// longest of GitHub's preset choices short of a custom date, plus a day of slack for
// a token created a little earlier or in another time zone.
const maxTokenLifetime = 91 * 24 * time.Hour

// classicToken is the shape of a classic personal access token.
var classicToken = regexp.MustCompile(`^ghp_[A-Za-z0-9]{36,251}$`)

// isPersonalToken reports whether cred is a personal access token.
func isPersonalToken(cred vault.Credential) bool { return cred.ClientID == PersonalTokenClient }

// tokenExpired reports whether a personal access token has expired at now. An OAuth
// grant never does here.
func tokenExpired(cred vault.Credential, now time.Time) bool {
	return isPersonalToken(cred) && !cred.Expiry.IsZero() && !now.Before(cred.Expiry)
}

// tokenInfo is what GitHub says about a token: the account, the token's scopes
// (classic tokens only), and when it expires (zero for none).
type tokenInfo struct {
	user    map[string]any
	scopes  []string
	classic bool // GitHub sent X-OAuth-Scopes, which it does for classic tokens only
	expiry  time.Time
}

// errTokenRejected is GitHub answering 401 for a token: wrong, expired or deleted.
var errTokenRejected = errors.New("github rejected the token")

// inspectToken asks GitHub who token belongs to (GET /user, authenticated with it)
// and reads its scopes and expiry from the answer's headers. The token never appears
// in an error.
func (h *Handler) inspectToken(ctx context.Context, token string) (tokenInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.githubAPIBase+"/user", nil)
	if err != nil {
		return tokenInfo{}, errors.New("github: could not build the request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "gofer")
	resp, err := h.client.Do(req)
	if err != nil {
		return tokenInfo{}, errors.New("github: request failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return tokenInfo{}, errTokenRejected
	case resp.StatusCode != http.StatusOK:
		return tokenInfo{}, fmt.Errorf("github: status %d", resp.StatusCode)
	case err != nil:
		return tokenInfo{}, errors.New("github: response lost")
	}
	info := tokenInfo{}
	if err := json.Unmarshal(body, &info.user); err != nil {
		return tokenInfo{}, errors.New("github: unreadable profile")
	}
	if v, ok := resp.Header[http.CanonicalHeaderKey("X-OAuth-Scopes")]; ok {
		info.classic = true
		for _, s := range strings.Split(strings.Join(v, ","), ",") {
			if s = strings.TrimSpace(s); s != "" {
				info.scopes = append(info.scopes, s)
			}
		}
	}
	if v := strings.TrimSpace(resp.Header.Get("GitHub-Authentication-Token-Expiration")); v != "" {
		for _, layout := range []string{"2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 -0700"} {
			if t, err := time.Parse(layout, v); err == nil {
				info.expiry = t.UTC()
				break
			}
		}
		if info.expiry.IsZero() {
			return tokenInfo{}, errors.New("github: unreadable token expiration")
		}
	}
	return info, nil
}

// tokenAlive reports whether GitHub still accepts token: false only on a 401. Any
// other failure is inconclusive and an error, so a good token is never dropped for a
// passing outage.
func (h *Handler) tokenAlive(ctx context.Context, token string) (bool, error) {
	_, err := h.inspectToken(ctx, token)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, errTokenRejected):
		return false, nil
	}
	return false, err
}

// ConnectWriteToken handles POST /auth/github-write/token (same-origin only): a
// signed-in GitHub user connects commenting with a classic personal access token
// (form "token"). It is refused unless the deployment allows tokens
// (GITHUB_WRITE_TOKENS); the token is classic, its only scope is public_repo, it
// expires within maxTokenLifetime, it belongs to the signed-in GitHub account, and
// that account is allowed to use gofer; and commenting is not already connected
// (disconnect first, so an OAuth grant is never orphaned unrevoked). The token is
// never logged, echoed or put in an error.
func (h *Handler) ConnectWriteToken(w http.ResponseWriter, r *http.Request) {
	p, ok := h.providers[WriteProvider]
	if !ok || !p.tokens {
		http.Error(w, "connecting commenting with a token is not enabled", http.StatusNotFound)
		return
	}
	user := h.sessions.CurrentUser(r)
	if user == nil || user.Provider != "github" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !sameOriginRequest(r) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/x-www-form-urlencoded" || r.ParseForm() != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	token := strings.TrimSpace(r.PostForm.Get("token"))
	fail := func(status int, why string) {
		writeGrantPage(w, status, `<p>`+why+`</p><p>gofer did not keep the token. If you will not use it, delete it in `+
			`<a href="`+tokensSettings+`">GitHub's token settings</a>.</p><p><a href="/">Back to radar</a></p>`)
	}
	switch {
	case strings.HasPrefix(token, "github_pat_"):
		fail(http.StatusBadRequest, "That is a fine-grained token. Organizations that block gofer's commenting app usually refuse those too: create a <strong>classic</strong> token (Settings, Developer settings, Personal access tokens, Tokens (classic)) with only the <code>public_repo</code> scope.")
		return
	case !classicToken.MatchString(token):
		fail(http.StatusBadRequest, "That does not look like a classic GitHub personal access token (they start with <code>ghp_</code>).")
		return
	case h.vault == nil:
		fail(http.StatusInternalServerError, "gofer has nowhere to keep the token.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	info, err := h.inspectToken(ctx, token)
	switch {
	case errors.Is(err, errTokenRejected):
		fail(http.StatusBadRequest, "GitHub rejected that token: it is wrong, expired or deleted.")
		return
	case err != nil:
		slog.WarnContext(ctx, "commenting token: could not check it with GitHub", "user", user.ID, "err", err)
		fail(http.StatusBadGateway, "gofer could not check the token with GitHub right now. Try again in a moment.")
		return
	case !info.classic:
		fail(http.StatusBadRequest, "That is not a classic token. Create a classic token with only the <code>public_repo</code> scope.")
		return
	case len(info.scopes) != 1 || info.scopes[0] != "public_repo":
		fail(http.StatusBadRequest, "The token must have exactly one scope, <code>public_repo</code>; it has "+html.EscapeString(scopeList(info.scopes))+". gofer refuses anything broader, so a leak can do no more than the OAuth grant could.")
		return
	case info.expiry.IsZero():
		fail(http.StatusBadRequest, "The token never expires. Create one with an expiry of 90 days or less, so a copy that leaks stops working on its own.")
		return
	case info.expiry.After(h.now().Add(maxTokenLifetime)):
		fail(http.StatusBadRequest, "The token expires more than 90 days from now. Create one with an expiry of 90 days or less.")
		return
	}
	granted, err := p.mapUser(info.user)
	switch {
	case err != nil:
		fail(http.StatusBadGateway, "GitHub returned an unexpected profile for the token.")
		return
	case granted.ID != user.ID:
		fail(http.StatusForbidden, "The token belongs to a different GitHub account. Use one from the account you signed in to gofer with ("+html.EscapeString(user.Login)+").")
		return
	case h.cfg != nil && !h.cfg.SignInAllowed("github", granted.Login):
		fail(http.StatusForbidden, "This account is not allowed to use this gofer.")
		return
	}
	sessionKey := h.sessions.SessionKey(r)
	sessionValid := func(ctx context.Context) (bool, error) {
		u, err := h.sessions.Lookup(r.WithContext(ctx))
		return u != nil && u.ID == user.ID && h.sessions.SessionKey(r) == sessionKey, err
	}
	outcome, prior, err := h.storeWriteToken(ctx, p, user.ID, granted.Login, token, info.expiry, sessionValid)
	if outcome != grantStored || err != nil {
		// The earlier disconnect it finished may still have left an OAuth grant live.
		if n := tokenConnectedNotice(prior); n == "token-unrevoked" {
			unrevoked := fail
			fail = func(status int, why string) {
				unrevoked(status, why+` gofer did finish deleting your earlier commenting grant, but GitHub did not confirm it was revoked: revoke gofer's commenting app in <a href="`+oauthAppsSettings+`">GitHub's settings</a> (Authorized OAuth Apps).`)
			}
		}
	}
	switch {
	case err != nil:
		slog.WarnContext(ctx, "commenting token: not stored", "user", user.ID, "err", err)
		fail(http.StatusBadGateway, "gofer could not save the token.")
		return
	case outcome == grantSessionEnded:
		fail(http.StatusConflict, "You signed out of gofer while connecting, so the token was not saved.")
		return
	case outcome == grantAlreadyConnected:
		fail(http.StatusConflict, "Commenting is already connected. Disconnect it first, then connect with the token.")
		return
	case outcome == grantBusy:
		fail(http.StatusConflict, "gofer is still disconnecting commenting. Try again in a moment.")
		return
	case outcome == grantCleanupFailed:
		fail(http.StatusServiceUnavailable, "gofer could not finish deleting your previous commenting credential, so the token was not saved. Try again later.")
		return
	}
	slog.InfoContext(ctx, "commenting connected with a personal access token", "user", user.ID, "expires", info.expiry)
	http.Redirect(w, r, "/?commenting="+tokenConnectedNotice(prior), http.StatusSeeOther)
}

// tokenConnectedNotice is the radar notice for a token just connected, after the
// pending disconnect it finished first (prior; nil if none). A disconnect GitHub did
// not confirm may have left an OAuth grant live, which gofer no longer holds to
// revoke: the user is sent to revoke it. A previous token gofer deleted is still
// live on GitHub until the user deletes it.
func tokenConnectedNotice(prior *logoutResult) string {
	switch {
	case prior == nil || prior.confirmed:
		return "token"
	case slices.Contains(prior.settings, oauthAppsSettings):
		return "token-unrevoked"
	case prior.tokenDeleted:
		return "token-replaced"
	}
	return "token"
}

// scopeList names scopes for a message.
func scopeList(scopes []string) string {
	if len(scopes) == 0 {
		return "none"
	}
	return strings.Join(scopes, ", ")
}

// Outcomes storeWriteToken adds to grantOutcome.
const (
	grantAlreadyConnected grantOutcome = iota + 100 // a usable write grant is already stored
	grantBusy                                       // a disconnect or logout of the write grant is pending
	grantTokenConnected                             // a live personal access token is stored (the OAuth callback must not replace it)
	grantCleanupFailed                              // a pending disconnect was retried and still could not delete gofer's copy
)

// storeWriteToken stores a checked personal access token as the user's write grant,
// strictly (an error stores nothing). Under the credential lock and then the write
// grant's marker lock, which logout holds while it decides what to revoke, it
// re-checks the session, and refuses while a logout of the write grant is pending
// (storing would supersede it, leaving an OAuth grant unrevoked) or while a usable
// grant is stored (disconnect first). A pending logout nobody here is running is
// finished first (claimLogout, runLogout), and the token is stored once it deleted
// the old copy; prior is that logout's outcome, nil if none ran. An expired token is
// replaced.
func (h *Handler) storeWriteToken(ctx context.Context, p *provider, userID, login, token string, expiry time.Time, sessionValid func(context.Context) (bool, error)) (outcome grantOutcome, prior *logoutResult, err error) {
	unlock, err := h.locks.lock(ctx, userID, p.name)
	if err != nil {
		return 0, nil, err
	}
	defer unlock()
	// A disconnect or logout of the write grant that no operation here is running (its
	// delete failed, its background wait gave up, or it began before a restart) is
	// finished first, holding the lock, as the broker finishes one: the user retrying
	// is what gets it done, so a vault that has recovered never keeps refusing. One an
	// operation here is running owns the credential; the user tries again shortly.
	pending, mine, err := h.claimLogout(ctx, userID, p.name)
	switch {
	case err != nil:
		return 0, prior, fmt.Errorf("read pending logout: %w", err)
	case pending && mine == nil:
		return grantBusy, prior, nil
	case pending:
		res := h.runLogout(ctx, userID, p.name, mine)
		if res.cleanupPending {
			return grantCleanupFailed, &res, nil
		}
		// Its outcome goes back to the caller: an OAuth grant GitHub did not confirm
		// revoked is still live there, and once the token replaces the row gofer can no
		// longer revoke it, so the user must be told.
		prior = &res
	}
	defer h.locks.markerLock(userID, p.name)()
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stepTimeout)
	defer cancel()
	ok, err := sessionValid(lctx)
	if err != nil {
		return 0, prior, fmt.Errorf("re-check the session: %w", err)
	}
	if !ok {
		return grantSessionEnded, prior, nil
	}
	// A disconnect or logout begun since (it takes only the marker lock to record).
	if h.locks.currentLogout(userID, p.name) != nil {
		return grantBusy, prior, nil
	}
	pending, err = h.ledger.LogoutPending(lctx, userID, p.name)
	if err != nil {
		return 0, prior, fmt.Errorf("read pending logout: %w", err)
	}
	if pending {
		return grantBusy, prior, nil
	}
	switch existing, err := h.vault.Get(lctx, userID, p.name); {
	case err == nil && !tokenExpired(existing, h.now()):
		return grantAlreadyConnected, prior, nil
	case err == nil:
		// An expired token works nowhere any more: a new one replaces it, so the user
		// can renew from the radar without disconnecting first.
	case !errors.Is(err, vault.ErrNotFound):
		return 0, prior, fmt.Errorf("read the write grant: %w", err)
	}
	cred := vault.Credential{
		Provider:    p.name,
		AccessToken: token,
		TokenType:   "bearer",
		Expiry:      expiry,
		ClientID:    PersonalTokenClient,
		Version:     newVersion(),
		Login:       login,
	}
	if err := h.vault.Put(lctx, userID, cred); err != nil {
		return 0, prior, fmt.Errorf("store the token: %w", err)
	}
	return grantStored, prior, nil
}

// sameOriginRequest reports whether a state-changing request came from gofer's own
// page, as webui's confirmations decide it: a browser that sends fetch metadata
// decides (only same-origin passes); without it, an Origin, when sent, must name
// this host, and "null" is refused.
func sameOriginRequest(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin":
		return true
	case "":
	default:
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return o != "null" && err == nil && u.Host != "" && u.Host == r.Host
}

// WriteScopesChanged handles a write GitHub refused before it was sent because the
// write grant at version now carries scopes other than exactly public_repo
// (ghcomment.ErrTokenScopes; GitHub lets a classic token's scopes be edited). A
// personal access token is deleted, so the radar asks the user to connect again with
// a narrow one; gofer cannot revoke it, and says so there. An OAuth grant gofer only
// ever asks public_repo for is left alone: every write with it is still refused while
// it has more, and the user disconnects it. It reports whether gofer's copy was
// deleted, which only the vault's confirmation says.
func (h *Handler) WriteScopesChanged(ctx context.Context, userID, version string) (deleted bool) {
	if _, ok := h.providers[WriteProvider]; !ok || h.vault == nil || version == "" {
		return false
	}
	unlock, err := h.locks.lock(ctx, userID, WriteProvider)
	if err != nil {
		return false
	}
	defer unlock()
	cred, err := h.vault.Get(ctx, userID, WriteProvider)
	if err != nil || cred.Version != version {
		return false
	}
	if !isPersonalToken(cred) {
		slog.WarnContext(ctx, "write grant reports scopes beyond public_repo; writes refused", "user", userID)
		return false
	}
	slog.InfoContext(ctx, "commenting token's scopes widened; deleting gofer's copy", "user", userID)
	return h.deleteIfVersion(ctx, userID, cred)
}
