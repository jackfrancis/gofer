package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/jackfrancis/gofer/internal/config"
	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/storage"
	"github.com/jackfrancis/gofer/internal/vault"
)

const user = "github:42"

var bg = context.Background()

func stored(t *testing.T, v vault.Vault) vault.Credential {
	t.Helper()
	c, err := v.Get(bg, user, "github")
	if err != nil {
		t.Fatalf("stored credential: %v", err)
	}
	return c
}

// put stores a credential as an earlier sign-in would have, and tells the fake
// GitHub it is live.
func put(t *testing.T, gh *fakeGitHub, v vault.Vault, c vault.Credential) vault.Credential {
	t.Helper()
	c.Provider = "github"
	if c.TokenType == "" {
		c.TokenType = "bearer"
	}
	if err := v.Put(bg, user, c); err != nil {
		t.Fatal(err)
	}
	gh.seed(c.AccessToken, c.RefreshToken)
	return c
}

func appCred(access, refresh, version string, left time.Duration) vault.Credential {
	return vault.Credential{AccessToken: access, RefreshToken: refresh, ClientID: "cid", Version: version, Expiry: time.Now().Add(left)}
}

func authURL(t *testing.T, h *Handler) *url.URL {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/auth/github/login", nil)
	req.SetPathValue("provider", "github")
	rec := httptest.NewRecorder()
	h.Login(rec, req)
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// Each kind asks for its own scopes: the OAuth app keeps read:user user:email, the
// GitHub App asks for none.
func TestScopesFollowClientKind(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, _ := newKindHandler(t, gh, config.GitHubOAuthApp)
	if got := authURL(t, h).Query().Get("scope"); got != "read:user user:email" {
		t.Errorf("oauth-app scope = %q", got)
	}
	h, _, _, _ = newKindHandler(t, gh, config.GitHubApp)
	if q := authURL(t, h).Query(); q.Has("scope") {
		t.Errorf("github-app sent scope %q", q.Get("scope"))
	}
}

// A GitHub App sign-in stores the whole expiring credential, tagged with the client
// and a version, after check-token confirms it.
func TestCallbackStoresTaggedCredential(t *testing.T) {
	gh := newFakeGitHub(t)
	h, sessions, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	rec, next := signIn(t, h)
	if rec.Code != http.StatusFound || sessions.CurrentUser(next) == nil {
		t.Fatalf("callback: %d", rec.Code)
	}
	c := stored(t, vlt)
	if c.ClientID != "cid" || c.Version == "" || c.RefreshToken == "" || time.Until(c.Expiry) < 7*time.Hour || time.Until(c.RefreshExpiry) < 150*24*time.Hour {
		t.Errorf("stored = %+v", c)
	}
	if gh.checks.Load() != 1 {
		t.Errorf("check-token calls = %d, want 1", gh.checks.Load())
	}
}

// A grant revoked between the token exchange and the callback's lock (a logout in
// another tab) is caught by check-token: nothing is stored, no session, and the user
// is offered a fresh sign-in (a link, not a redirect, so it can never loop).
func TestCallbackRestartsWhenTokenRevokedMidFlow(t *testing.T) {
	gh := newFakeGitHub(t)
	h, sessions, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	gh.mu.Lock()
	gh.killNext = true
	gh.mu.Unlock()
	rec, next := signIn(t, h)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `href="/auth/github/login"`) {
		t.Fatalf("callback: %d %q; want a page that restarts sign-in", rec.Code, rec.Body.String())
	}
	if sessions.CurrentUser(next) != nil {
		t.Error("a revoked token got a session")
	}
	if _, err := vlt.Get(bg, user, "github"); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("a revoked token was stored (err=%v)", err)
	}
}

// A legacy row (no client, no version) is tagged and versioned before its first vend
// under the OAuth app, and refused under the GitHub App.
func TestLegacyRows(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubOAuthApp)
	put(t, gh, vlt, vault.Credential{AccessToken: "gho_legacy"})
	c, err := h.Credential(bg, user, "github", VendRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessToken != "gho_legacy" || c.ClientID != "cid" || c.Version == "" {
		t.Fatalf("vended %+v", c)
	}
	if s := stored(t, vlt); s.Version != c.Version || s.ClientID != "cid" {
		t.Errorf("the tag was not written back before the vend: %+v", s)
	}
	if h.SignInRequired(bg, user, "github") {
		t.Error("a usable legacy row asks for sign-in")
	}

	h, _, _, vlt = newKindHandler(t, gh, config.GitHubApp)
	put(t, gh, vlt, vault.Credential{AccessToken: "gho_legacy"})
	if _, err := h.Credential(bg, user, "github", VendRequest{}); !errors.Is(err, vault.ErrSignInRequired) {
		t.Fatalf("github-app vended a legacy row (err=%v)", err)
	}
	if !h.SignInRequired(bg, user, "github") {
		t.Error("SignInRequired = false for a legacy row under the app")
	}
}

// Missing credentials and credentials from another client are "sign-in required"; the
// other client's row is left alone (a rollback may make it current again).
func TestSignInRequiredCases(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	if _, err := h.Credential(bg, user, "github", VendRequest{}); !errors.Is(err, vault.ErrSignInRequired) {
		t.Fatalf("missing: err=%v", err)
	}
	put(t, gh, vlt, vault.Credential{AccessToken: "gho_other", ClientID: "other-client", Version: "v1"})
	if _, err := h.Credential(bg, user, "github", VendRequest{}); !errors.Is(err, vault.ErrSignInRequired) {
		t.Fatalf("other client: err=%v", err)
	}
	if stored(t, vlt).AccessToken != "gho_other" {
		t.Error("another client's credential was changed")
	}
	if _, err := h.Credential(bg, user, "google", VendRequest{}); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("unconfigured provider: err=%v, want ErrNotFound", err)
	}
}

// With less than 30 minutes left the credential is refreshed, and the rotated pair is
// stored before the new token is vended. With more left, nothing happens.
func TestEarlyRefresh(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	put(t, gh, vlt, appCred("ghu_old", "ghr_old", "v1", 2*time.Hour))
	c, err := h.Credential(bg, user, "github", VendRequest{})
	if err != nil || c.AccessToken != "ghu_old" || gh.refreshes.Load() != 0 {
		t.Fatalf("2h left: vended %q (err=%v), refreshes=%d", c.AccessToken, err, gh.refreshes.Load())
	}

	put(t, gh, vlt, appCred("ghu_old", "ghr_old", "v1", 20*time.Minute))
	c, err = h.Credential(bg, user, "github", VendRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessToken == "ghu_old" || c.Version == "v1" || time.Until(c.Expiry) < 7*time.Hour {
		t.Fatalf("not refreshed: %+v", c)
	}
	s := stored(t, vlt)
	if s.AccessToken != c.AccessToken || s.RefreshToken == "ghr_old" || s.Version != c.Version || s.ClientID != "cid" {
		t.Errorf("rotated pair not persisted: %+v", s)
	}
}

// Concurrent vends of a credential due for refresh spend its single-use refresh
// token once: the others wait for the lock and see the rotated pair.
func TestConcurrentVendsRefreshOnce(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	put(t, gh, vlt, appCred("ghu_old", "ghr_old", "v1", time.Minute))
	var wg sync.WaitGroup
	tokens := make([]string, 8)
	errs := make([]error, 8)
	for i := range tokens {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := h.Credential(bg, user, "github", VendRequest{})
			tokens[i], errs[i] = c.AccessToken, err
		}(i)
	}
	wg.Wait()
	for i := range tokens {
		if errs[i] != nil || tokens[i] != tokens[0] {
			t.Fatalf("vend %d: %q, %v (first %q)", i, tokens[i], errs[i], tokens[0])
		}
	}
	if n := gh.refreshes.Load(); n != 1 {
		t.Errorf("refreshes = %d, want 1", n)
	}
}

// A dead refresh token deletes the credential and asks for sign-in.
func TestDeadRefreshToken(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	put(t, gh, vlt, appCred("ghu_old", "", "v1", time.Minute))
	c := stored(t, vlt)
	c.RefreshToken = "ghr_spent" // not live at the fake
	_ = vlt.Put(bg, user, c)
	if _, err := h.Credential(bg, user, "github", VendRequest{}); !errors.Is(err, vault.ErrSignInRequired) {
		t.Fatalf("err = %v, want sign-in required", err)
	}
	if _, err := vlt.Get(bg, user, "github"); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("the dead credential was kept (err=%v)", err)
	}
}

// A refresh that fails for a passing reason still vends a token that has not expired.
func TestTransientRefreshFailureVendsUnexpiredToken(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	put(t, gh, vlt, appCred("ghu_old", "ghr_old", "v1", 10*time.Minute))
	gh.srv.Close() // GitHub unreachable
	c, err := h.Credential(bg, user, "github", VendRequest{})
	if err != nil || c.AccessToken != "ghu_old" {
		t.Fatalf("vended %q, err=%v", c.AccessToken, err)
	}
	if stored(t, vlt).AccessToken != "ghu_old" {
		t.Error("credential changed")
	}
}

// rejected=<version>: an old version gets the stored token; the stored version is
// confirmed with check-token, and only a confirmed rejection refreshes, once per run.
func TestRejectedVersion(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	put(t, gh, vlt, appCred("ghu_a", "ghr_a", "v1", 4*time.Hour))

	// Rotation already happened: no check, the stored token.
	c, err := h.Credential(bg, user, "github", VendRequest{Rejected: "v0", RunID: "run1"})
	if err != nil || c.AccessToken != "ghu_a" || gh.checks.Load() != 0 {
		t.Fatalf("stale rejection: %q err=%v checks=%d", c.AccessToken, err, gh.checks.Load())
	}

	// GitHub says the token is fine: the 401 was something else.
	c, err = h.Credential(bg, user, "github", VendRequest{Rejected: "v1", RunID: "run1"})
	if err != nil || c.AccessToken != "ghu_a" || gh.checks.Load() != 1 || gh.refreshes.Load() != 0 {
		t.Fatalf("valid token: %q err=%v checks=%d refreshes=%d", c.AccessToken, err, gh.checks.Load(), gh.refreshes.Load())
	}

	// The user revoked gofer at GitHub... except the refresh token still works here,
	// so model only the access token dying.
	gh.mu.Lock()
	delete(gh.valid, "ghu_a")
	gh.mu.Unlock()
	c, err = h.Credential(bg, user, "github", VendRequest{Rejected: "v1", RunID: "run1"})
	if err != nil || c.AccessToken == "ghu_a" || c.Version == "v1" || gh.refreshes.Load() != 1 {
		t.Fatalf("confirmed rejection: %+v err=%v refreshes=%d", c, err, gh.refreshes.Load())
	}

	// The same run rejecting the new version gets it back, never another refresh.
	gh.mu.Lock()
	delete(gh.valid, c.AccessToken)
	gh.mu.Unlock()
	again, err := h.Credential(bg, user, "github", VendRequest{Rejected: c.Version, RunID: "run1"})
	if err != nil || again.AccessToken != c.AccessToken || gh.refreshes.Load() != 1 {
		t.Fatalf("second forced refresh in one run: %q err=%v refreshes=%d", again.AccessToken, err, gh.refreshes.Load())
	}
	// A run with no ID never forces a refresh.
	if _, err := h.Credential(bg, user, "github", VendRequest{Rejected: c.Version}); err != nil || gh.refreshes.Load() != 1 {
		t.Fatalf("run without id: err=%v refreshes=%d", err, gh.refreshes.Load())
	}
	// Another run may.
	if _, err := h.Credential(bg, user, "github", VendRequest{Rejected: c.Version, RunID: "run2"}); err != nil || gh.refreshes.Load() != 2 {
		t.Fatalf("other run: err=%v refreshes=%d", err, gh.refreshes.Load())
	}
}

// A confirmed rejection of a credential with no refresh token (a legacy OAuth-app
// token revoked at rollout) deletes it and asks for sign-in; so does a confirmed one
// whose refresh token is dead too (the user revoked the grant).
func TestConfirmedRejectionWithoutRenewal(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubOAuthApp)
	_ = vlt.Put(bg, user, vault.Credential{Provider: "github", AccessToken: "gho_revoked"}) // not live
	c, err := h.Credential(bg, user, "github", VendRequest{})                               // tags it
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Credential(bg, user, "github", VendRequest{Rejected: c.Version, RunID: "r"}); !errors.Is(err, vault.ErrSignInRequired) {
		t.Fatalf("err = %v", err)
	}
	if _, err := vlt.Get(bg, user, "github"); !errors.Is(err, vault.ErrNotFound) {
		t.Error("the revoked credential was kept")
	}

	h, _, _, vlt = newKindHandler(t, gh, config.GitHubApp)
	_ = vlt.Put(bg, user, vault.Credential{Provider: "github", AccessToken: "ghu_dead", RefreshToken: "ghr_dead", ClientID: "cid", Version: "v1", Expiry: time.Now().Add(5 * time.Hour)})
	if _, err := h.Credential(bg, user, "github", VendRequest{Rejected: "v1", RunID: "r"}); !errors.Is(err, vault.ErrSignInRequired) {
		t.Fatalf("revoked grant: err = %v", err)
	}
	if _, err := vlt.Get(bg, user, "github"); !errors.Is(err, vault.ErrNotFound) {
		t.Error("the revoked credential was kept")
	}
}

// A conditional write never overwrites a row someone else rewrote, and a delete
// never removes one: the version decides.
func TestVersionCheckedWrites(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	put(t, gh, vlt, appCred("ghu_new", "ghr_new", "v2", time.Hour))
	ok, err := h.putIfVersion(bg, user, "v1", appCred("ghu_x", "ghr_x", "v3", time.Hour))
	if err != nil || ok {
		t.Fatalf("stale put applied (ok=%v err=%v)", ok, err)
	}
	h.deleteIfVersion(bg, user, appCred("ghu_old", "", "v1", 0))
	if stored(t, vlt).Version != "v2" {
		t.Error("a stale write or delete changed the newer row")
	}
}

// The lock waits on its context: a vend never hangs behind a stuck writer.
func TestCredentialLockHonoursContext(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	put(t, gh, vlt, appCred("ghu_a", "ghr_a", "v1", 4*time.Hour))
	unlock, err := h.locks.lock(bg, user, "github")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	defer cancel()
	if _, err := h.Credential(ctx, user, "github", VendRequest{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	unlock()
	if _, err := h.Credential(bg, user, "github", VendRequest{}); err != nil {
		t.Fatalf("after unlock: %v", err)
	}
	if len(h.locks.m) != 0 {
		t.Errorf("lock entries leaked: %d", len(h.locks.m))
	}
}

// logout signs in, then posts /auth/logout with the session.
func logout(t *testing.T, h *Handler) *httptest.ResponseRecorder {
	t.Helper()
	_, next := signIn(t, h)
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	for _, c := range next.Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.Logout(rec, req)
	return rec
}

// Logout refreshes a token about to expire, revokes the grant with the fresh token,
// then deletes the row: 204, confirmed.
func TestLogoutRefreshesThenRevokes(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	// Sign in, then age the stored token to just short of expiry.
	_, next := signIn(t, h)
	c := stored(t, vlt)
	c.Expiry = time.Now().Add(time.Minute)
	_ = vlt.Put(bg, user, c)
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	for _, ck := range next.Cookies() {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	h.Logout(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if gh.refreshes.Load() != 1 || gh.revoked.Load() != 1 {
		t.Fatalf("refreshes=%d revocations=%d", gh.refreshes.Load(), gh.revoked.Load())
	}
	if with := gh.revokedWith.Load().(string); with == c.AccessToken || with == "" {
		t.Errorf("revoked with %q, not the refreshed token", with)
	}
	if _, err := vlt.Get(bg, user, "github"); !errors.Is(err, vault.ErrNotFound) {
		t.Error("row kept after logout")
	}
}

// When GitHub does not confirm revocation, logout still signs out and deletes the
// row, and its answer says so with a link to GitHub's settings.
func TestLogoutReportsUnconfirmedRevocation(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	gh.mu.Lock()
	gh.revokeErr = true
	gh.mu.Unlock()
	rec := logout(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	out := decodeLogout(t, rec)
	if out.Revocation != "unconfirmed" || out.SettingsURL != githubAppsSettings || len(out.SettingsURLs) != 1 {
		t.Errorf("answer = %+v", out)
	}
	if _, err := vlt.Get(bg, user, "github"); !errors.Is(err, vault.ErrNotFound) {
		t.Error("row kept after logout")
	}
	if c := rec.Result().Cookies(); len(c) == 0 {
		t.Error("session cookie not cleared")
	}
}

// A dead refresh token at logout leaves revocation unconfirmed (after a restore the
// grant may still be live), and nothing is revoked with a stale token.
func TestLogoutWithDeadRefreshTokenIsUnconfirmed(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	_, next := signIn(t, h)
	c := stored(t, vlt)
	c.Expiry, c.RefreshToken = time.Now().Add(time.Minute), "ghr_restored_spent"
	_ = vlt.Put(bg, user, c)
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	for _, ck := range next.Cookies() {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	h.Logout(rec, req)
	if rec.Code != http.StatusOK || gh.revoked.Load() != 0 {
		t.Fatalf("status = %d, revocations = %d", rec.Code, gh.revoked.Load())
	}
	if _, err := vlt.Get(bg, user, "github"); !errors.Is(err, vault.ErrNotFound) {
		t.Error("row kept after logout")
	}
}

// After a cutover, the old client's credential cannot be revoked with the new
// client's credentials: unconfirmed, row deleted, no call made with it.
func TestLogoutOtherClientIsUnconfirmed(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	_, next := signIn(t, h)
	_ = vlt.Put(bg, user, vault.Credential{Provider: "github", AccessToken: "gho_legacy"})
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	for _, ck := range next.Cookies() {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	h.Logout(rec, req)
	if rec.Code != http.StatusOK || gh.revoked.Load() != 0 {
		t.Fatalf("status = %d, revocations = %d", rec.Code, gh.revoked.Load())
	}
	// The legacy row came from the OAuth app, so its settings page is the one to use.
	if out := decodeLogout(t, rec); out.SettingsURL != oauthAppsSettings {
		t.Errorf("settings = %+v, want the OAuth app page", out)
	}
}

// A normal logout under the OAuth app revokes the grant and answers 204.
func TestLogoutConfirmed(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubOAuthApp)
	rec := logout(t, h)
	if rec.Code != http.StatusNoContent || gh.revoked.Load() != 1 || gh.refreshes.Load() != 0 {
		t.Fatalf("status = %d, revocations = %d, refreshes = %d", rec.Code, gh.revoked.Load(), gh.refreshes.Load())
	}
	if _, err := vlt.Get(bg, user, "github"); !errors.Is(err, vault.ErrNotFound) {
		t.Error("row kept after logout")
	}
}

type logoutAnswer struct {
	Revocation   string   `json:"revocation"`
	SettingsURL  string   `json:"settings_url"`
	SettingsURLs []string `json:"settings_urls"`
}

func decodeLogout(t *testing.T, rec *httptest.ResponseRecorder) logoutAnswer {
	t.Helper()
	var out logoutAnswer
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("logout body: %v", err)
	}
	return out
}

// Review P1 (both rounds): a logout that cannot take the lock in time, because a
// refresh holds it, never deletes outside the lock. The refresh still stores its
// pair, but the request that ran it gets "sign-in required", not a token; once the
// lock is free, the deferred logout revokes the grant with the newest token and
// deletes the row.
func TestLogoutDuringRefresh(t *testing.T) {
	defer func(d time.Duration) { logoutLockWait = d }(logoutLockWait)
	logoutLockWait = 50 * time.Millisecond
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	put(t, gh, vlt, appCred("ghu_old", "ghr_old", "v1", time.Minute))

	started, proceed := make(chan struct{}), make(chan struct{})
	gh.onRefresh = func() { close(started); <-proceed }
	vended := make(chan error, 1)
	go func() {
		_, err := h.Credential(bg, user, "github", VendRequest{})
		vended <- err
	}()
	<-started // the refresh holds the lock and GitHub is rotating the pair

	res := logoutNow(t, h)
	if res.confirmed || len(res.settings) == 0 {
		t.Fatalf("logout that could not take the lock = %+v, want unconfirmed", res)
	}
	if _, err := vlt.Get(bg, user, "github"); err != nil {
		t.Fatalf("the row was deleted outside the lock (err=%v)", err)
	}
	close(proceed)
	if err := <-vended; !errors.Is(err, vault.ErrSignInRequired) {
		t.Fatalf("the refresh running during logout vended a token (err=%v)", err)
	}

	// The deferred logout revokes with the rotated token and deletes the row.
	deadline := time.Now().Add(5 * time.Second)
	for logoutIsPending(h) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := vlt.Get(bg, user, "github"); !errors.Is(err, vault.ErrNotFound) {
		t.Fatalf("row kept after the deferred logout (err=%v)", err)
	}
	if gh.revoked.Load() != 1 || gh.revokedWith.Load().(string) == "ghu_old" {
		t.Errorf("revocations = %d with %q; want one, with the rotated token", gh.revoked.Load(), gh.revokedWith.Load())
	}
}

// Review P2: a vend that reaches the lock while a logout is pending refuses and
// leaves the credential alone, so logout can still revoke with it.
func TestPendingLogoutKeepsCredentialForRevocation(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	put(t, gh, vlt, appCred("ghu_a", "ghr_a", "v1", 4*time.Hour))
	op := mustMark(t, h)
	if _, err := h.Credential(bg, user, "github", VendRequest{}); !errors.Is(err, vault.ErrSignInRequired) {
		t.Fatalf("vend during logout: err = %v", err)
	}
	if stored(t, vlt).AccessToken != "ghu_a" {
		t.Fatal("the vend changed the credential logout needs")
	}
	// A confirmed rejection that would delete a dead token leaves it to logout too.
	h.deleteIfVersion(bg, user, stored(t, vlt))
	stored(t, vlt)

	if res := h.revokeAndDelete(bg, user, "github", op); !res.confirmed || gh.revoked.Load() != 1 {
		t.Fatalf("logout = %+v, revocations = %d; want confirmed", res, gh.revoked.Load())
	}
	if _, err := vlt.Get(bg, user, "github"); !errors.Is(err, vault.ErrNotFound) {
		t.Error("row kept after logout")
	}
}

// failingPutVault refuses every write.
type failingPutVault struct{ vault.Vault }

func (failingPutVault) Put(context.Context, string, vault.Credential) error {
	return errors.New("disk full")
}

// Review P2: a sign-in whose write fails leaves a pending logout pending, so the old
// credential is neither vended nor spared.
func TestFailedSignInWriteKeepsPendingLogout(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	put(t, gh, vlt, appCred("ghu_old", "ghr_old", "v1", 4*time.Hour))
	h.markLogout(bg, user, "github")
	h.vault = failingPutVault{vlt}
	signIn(t, h)
	if !logoutIsPending(h) {
		t.Fatal("a failed sign-in write cleared the pending logout")
	}
	if _, err := h.Credential(bg, user, "github", VendRequest{}); !errors.Is(err, vault.ErrSignInRequired) {
		t.Fatalf("the old credential was vended (err=%v)", err)
	}
}

// Review P2: revocation gets its own deadline. A logout that waited most of its
// lock budget, then refreshed slowly, still revokes with the rotated token.
func TestLogoutRevocationHasItsOwnDeadline(t *testing.T) {
	defer func(d time.Duration) { logoutLockWait = d }(logoutLockWait)
	logoutLockWait = 300 * time.Millisecond
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	put(t, gh, vlt, appCred("ghu_old", "ghr_old", "v1", time.Minute))
	unlock, err := h.locks.lock(bg, user, "github")
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(200 * time.Millisecond); unlock() }()
	gh.onRefresh = func() { time.Sleep(200 * time.Millisecond) } // past the lock budget
	if res := logoutNow(t, h); !res.confirmed {
		t.Fatalf("logout = %+v; want confirmed", res)
	}
	if gh.revokedWith.Load().(string) == "ghu_old" {
		t.Error("revoked with the stale token")
	}
}

// sessionSpy is a session store that records whether a delete's context was live.
type sessionSpy struct {
	session.Store
	mu        sync.Mutex
	deleteErr error
	deleted   bool
}

// Load fails on a done context, as the SQLite store does.
func (s *sessionSpy) Load(ctx context.Context, key string) (session.User, time.Time, error) {
	if err := ctx.Err(); err != nil {
		return session.User{}, time.Time{}, err
	}
	return s.Store.Load(ctx, key)
}

func (s *sessionSpy) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	s.deleted, s.deleteErr = true, ctx.Err()
	s.mu.Unlock()
	return s.Store.Delete(ctx, key)
}

// Review P2: a browser that disconnects during logout still has its stored session
// deleted.
func TestLogoutDeletesSessionWhenBrowserDisconnects(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, cfg, _ := newKindHandler(t, gh, config.GitHubApp)
	spy := &sessionSpy{Store: session.NewMemoryStore()}
	h.sessions = session.NewManager(cfg.SessionSecret, cfg.CookieSecure, session.WithStore(spy))
	_, next := signIn(t, h)
	ctx, cancel := context.WithCancel(bg)
	cancel() // the browser is already gone
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil).WithContext(ctx)
	for _, ck := range next.Cookies() {
		req.AddCookie(ck)
	}
	h.Logout(httptest.NewRecorder(), req)
	if !spy.deleted || spy.deleteErr != nil {
		t.Fatalf("session delete ran=%v with ctx err %v; want a live context", spy.deleted, spy.deleteErr)
	}
	if h.sessions.CurrentUser(next) != nil {
		t.Error("the session survived logout")
	}
	// Review P2 (third round): the owner was still found, so the credential was
	// revoked and deleted.
	if gh.revoked.Load() != 1 {
		t.Errorf("grant revocations = %d, want 1: the owner lookup was cancelled with the browser", gh.revoked.Load())
	}
	if _, err := h.vault.Get(bg, user, "github"); !errors.Is(err, vault.ErrNotFound) {
		t.Error("credential kept after logout")
	}
}

// A new sign-in supersedes a logout still pending: its credential is kept.
func TestSignInClearsPendingLogout(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	h.markLogout(bg, user, "github")
	signIn(t, h)
	if _, err := h.Credential(bg, user, "github", VendRequest{}); err != nil {
		t.Fatalf("vend after a new sign-in: %v", err)
	}
	stored(t, vlt)
}

// Review P2: logout with no stored GitHub credential (the broker deleted a dead
// one, say after a restore) is unconfirmed, with both settings pages since gofer
// cannot tell which client issued the grant.
func TestLogoutWithoutCredentialIsUnconfirmed(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	_, next := signIn(t, h)
	_ = vlt.Delete(bg, user, "github")
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	for _, ck := range next.Cookies() {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	h.Logout(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 unconfirmed", rec.Code)
	}
	if out := decodeLogout(t, rec); len(out.SettingsURLs) != 2 {
		t.Errorf("settings = %+v, want both pages", out)
	}
}

// Review P2: the settings page follows the credential's issuer, not the configured
// kind; an unknown issuer gets both pages.
func TestRevocationSettings(t *testing.T) {
	gh := newFakeGitHub(t)
	app, _, _, _ := newKindHandler(t, gh, config.GitHubApp)
	oauth, _, _, _ := newKindHandler(t, gh, config.GitHubOAuthApp)
	cases := []struct {
		name string
		h    *Handler
		cred *vault.Credential
		want []string
	}{
		{"app, own row", app, &vault.Credential{ClientID: "cid"}, []string{githubAppsSettings}},
		{"app, legacy OAuth row after cutover", app, &vault.Credential{}, []string{oauthAppsSettings}},
		{"oauth, app row after rollback", oauth, &vault.Credential{ClientID: "Iv1.app"}, []string{githubAppsSettings, oauthAppsSettings}},
		{"oauth, own row", oauth, &vault.Credential{ClientID: "cid"}, []string{oauthAppsSettings}},
		{"no credential", app, nil, []string{githubAppsSettings, oauthAppsSettings}},
	}
	for _, c := range cases {
		got := c.h.revocationSettings(c.h.providers["github"], c.cred)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// Review P2: a 422 from check-token is inconclusive. The credential is kept and not
// rotated, the vend fails as temporary, and a callback does not call the grant
// revoked.
func TestCheckToken422IsInconclusive(t *testing.T) {
	gh := newFakeGitHub(t)
	h, sessions, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	put(t, gh, vlt, appCred("ghu_a", "ghr_a", "v1", 4*time.Hour))
	gh.mu.Lock()
	gh.checkStatus = http.StatusUnprocessableEntity
	gh.mu.Unlock()
	_, err := h.Credential(bg, user, "github", VendRequest{Rejected: "v1", RunID: "r"})
	if err == nil || errors.Is(err, vault.ErrSignInRequired) {
		t.Fatalf("err = %v, want a temporary failure", err)
	}
	if s := stored(t, vlt); s.Version != "v1" || gh.refreshes.Load() != 0 {
		t.Fatalf("a 422 changed the credential: %+v, refreshes=%d", s, gh.refreshes.Load())
	}

	rec, next := signIn(t, h)
	if rec.Code != http.StatusBadGateway || sessions.CurrentUser(next) != nil {
		t.Fatalf("callback on a 422 check: status %d", rec.Code)
	}
}

// ctxVault fails a write whose context is done, as the SQLite vault does.
type ctxVault struct{ vault.Vault }

func (v ctxVault) Put(ctx context.Context, userID string, c vault.Credential) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return v.Vault.Put(ctx, userID, c)
}

// Review P2: once GitHub has rotated the pair, a run that disconnects does not
// abandon it: the exchange and the write finish on their own deadline.
func TestRefreshSurvivesCancelledRun(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	h.vault = ctxVault{vlt}
	put(t, gh, vlt, appCred("ghu_old", "ghr_old", "v1", time.Minute))
	ctx, cancel := context.WithCancel(bg)
	gh.onRefresh = cancel // the run goes away while GitHub is rotating the pair
	c, err := h.Credential(ctx, user, "github", VendRequest{})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if s := stored(t, vlt); s.AccessToken != c.AccessToken || s.AccessToken == "ghu_old" {
		t.Fatalf("the rotated pair was not stored: %+v", s)
	}
}

// Review P3: a form-encoded token response's refresh_token_expires_in (int64 in
// the pinned oauth2) is decoded like the JSON one.
func TestRefreshExpiryFormEncoded(t *testing.T) {
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	want := now.Add(15897600 * time.Second)
	form := (&oauth2.Token{}).WithExtra(url.Values{"refresh_token_expires_in": {"15897600"}})
	if got := refreshExpiry(form, now); !got.Equal(want) {
		t.Errorf("form: %v, want %v", got, want)
	}
	js := (&oauth2.Token{}).WithExtra(map[string]any{"refresh_token_expires_in": float64(15897600)})
	if got := refreshExpiry(js, now); !got.Equal(want) {
		t.Errorf("json: %v, want %v", got, want)
	}
}

func logoutIsPending(h *Handler) bool {
	pending, _, _ := h.logoutPending(bg, user, "github")
	return pending
}

// sqliteHandler is a GitHub App handler over the SQLite vault in dir, as the web tier
// runs with DATA_DIR set. Opening the same dir again is a restart.
func sqliteHandler(t *testing.T, gh *fakeGitHub, dir string) (*Handler, vault.Vault) {
	t.Helper()
	db, err := storage.Open(bg, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	keys, err := storage.NewKeyring(make([]byte, storage.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	vlt := db.Vault(keys)
	h, _, _, _ := newKindHandler(t, gh, config.GitHubApp)
	h.vault = vlt
	h.ledger = vlt
	return h, vlt
}

// Review P1 (third round): a pending logout survives a restart. The pod dies after
// logout recorded it and before the credential was revoked and deleted; the next
// process, over the same database, refuses to vend (the broker's sign_in_required),
// and finishes the logout: grant revoked, row deleted, marker cleared.
func TestPendingLogoutSurvivesRestart(t *testing.T) {
	gh := newFakeGitHub(t)
	dir := t.TempDir()
	before, vlt := sqliteHandler(t, gh, dir)
	put(t, gh, vlt, appCred("ghu_a", "ghr_a", "v1", 4*time.Hour))
	before.markLogout(bg, user, "github") // what Logout does before discarding the session
	// ...and the process is gone.

	after, vlt2 := sqliteHandler(t, gh, dir)
	if _, err := after.Credential(bg, user, "github", VendRequest{}); !errors.Is(err, vault.ErrSignInRequired) {
		t.Fatalf("vend after restart: err = %v, want sign-in required", err)
	}
	if _, err := vlt2.Get(bg, user, "github"); !errors.Is(err, vault.ErrNotFound) {
		t.Fatalf("the orphaned logout was not finished (err=%v)", err)
	}
	if gh.revoked.Load() != 1 || gh.revokedWith.Load().(string) != "ghu_a" {
		t.Errorf("revocations = %d with %q, want one with the stored token", gh.revoked.Load(), gh.revokedWith.Load())
	}
	if logoutIsPending(after) {
		t.Error("marker left after the logout finished")
	}
}

// At startup, the web tier finishes pending logouts without waiting for a vend.
func TestResumePendingLogoutsAtStartup(t *testing.T) {
	gh := newFakeGitHub(t)
	dir := t.TempDir()
	before, vlt := sqliteHandler(t, gh, dir)
	put(t, gh, vlt, appCred("ghu_a", "ghr_a", "v1", 4*time.Hour))
	before.markLogout(bg, user, "github")

	after, vlt2 := sqliteHandler(t, gh, dir)
	after.ResumePendingLogouts(bg)
	deadline := time.Now().Add(5 * time.Second)
	for logoutIsPending(after) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := vlt2.Get(bg, user, "github"); !errors.Is(err, vault.ErrNotFound) || gh.revoked.Load() != 1 {
		t.Fatalf("resumed logout: row err=%v, revocations=%d", err, gh.revoked.Load())
	}
}

// A sign-in after the restart replaces the credential and clears the durable marker.
func TestSignInAfterRestartClearsMarker(t *testing.T) {
	gh := newFakeGitHub(t)
	dir := t.TempDir()
	before, _ := sqliteHandler(t, gh, dir)
	before.markLogout(bg, user, "github")
	after, _ := sqliteHandler(t, gh, dir)
	signIn(t, after)
	if _, err := after.Credential(bg, user, "github", VendRequest{}); err != nil {
		t.Fatalf("vend after a new sign-in: %v", err)
	}
}

// mustMark records a pending logout with no session, as an operation would.
func mustMark(t *testing.T, h *Handler) string {
	t.Helper()
	op, err := h.markLogout(bg, user, "github")
	if err != nil {
		t.Fatal(err)
	}
	return op
}

// logoutNow runs one whole logout operation for user, without a browser session.
func logoutNow(t *testing.T, h *Handler) logoutResult {
	t.Helper()
	return h.revokeAndDelete(bg, user, "github", mustMark(t, h))
}

// failingCommitVault refuses to commit a logout.
type failingCommitVault struct{ vault.Vault }

func (failingCommitVault) MarkLogoutEndingSession(context.Context, string, string, string) error {
	return errors.New("database is locked")
}

// Review P1 (fourth round): when the pending logout and the end of the session
// cannot be committed, logout says so (503) and changes nothing: the session stands,
// the credential is still vendable, and nothing is revoked.
func TestLogoutCommitFailureChangesNothing(t *testing.T) {
	gh := newFakeGitHub(t)
	h, sessions, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	_, next := signIn(t, h)
	h.vault = failingCommitVault{vlt}
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	for _, ck := range next.Cookies() {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	h.Logout(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if sessions.CurrentUser(next) == nil {
		t.Error("the session was ended although logout was not recorded")
	}
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			t.Error("the cookie was cleared although logout was not recorded")
		}
	}
	if gh.revoked.Load() != 0 || logoutIsPending(h) {
		t.Errorf("revocations = %d, pending = %v; want nothing done", gh.revoked.Load(), logoutIsPending(h))
	}
	if _, err := h.Credential(bg, user, "github", VendRequest{}); err != nil {
		t.Errorf("credential no longer vendable after a failed logout: %v", err)
	}
}

// Review P1 (fourth round), SQLite: logout ends the session in the same commit as
// the pending marker, so after a restart the session is gone and the credential
// unvendable even if the process stopped right after the commit.
func TestLogoutCommitsSessionAndMarkerTogether(t *testing.T) {
	gh := newFakeGitHub(t)
	dir := t.TempDir()
	db, err := storage.Open(bg, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	keys, _ := storage.NewKeyring(make([]byte, storage.KeySize))
	h, _, cfg, _ := newKindHandler(t, gh, config.GitHubApp)
	h.vault, h.ledger = db.Vault(keys), db.Vault(keys)
	h.sessions = session.NewManager(cfg.SessionSecret, cfg.CookieSecure, session.WithStore(db.Sessions()))
	_, next := signIn(t, h)

	// The lock is busy, so logout returns after its commit, before revoking.
	defer func(d time.Duration) { logoutLockWait = d }(logoutLockWait)
	logoutLockWait = 20 * time.Millisecond
	unlock, err := h.locks.lock(bg, user, "github")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	for _, ck := range next.Cookies() {
		req.AddCookie(ck)
	}
	h.Logout(httptest.NewRecorder(), req)

	// A new process over the same database.
	restarted := session.NewManager(cfg.SessionSecret, cfg.CookieSecure, session.WithStore(db.Sessions()))
	if restarted.CurrentUser(next) != nil {
		t.Error("the session survived the logout commit")
	}
	if p, err := db.Vault(keys).LogoutPending(bg, user, "github"); err != nil || !p {
		t.Errorf("pending marker = %v, %v; want recorded with the session's end", p, err)
	}
	// Let the deferred revocation finish before the database goes away.
	unlock()
	deadline := time.Now().Add(5 * time.Second)
	for logoutIsPending(h) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if gh.revoked.Load() != 1 {
		t.Errorf("deferred revocations = %d, want 1", gh.revoked.Load())
	}
}

// Review P2 (fourth round): a logout operation never records itself twice. A sign-in
// that replaces the credential after logout began clears the marker, and the rest of
// the logout leaves the new credential alone.
func TestLogoutDoesNotRecreateClearedMarker(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	put(t, gh, vlt, appCred("ghu_old", "ghr_old", "v1", 4*time.Hour))
	op := mustMark(t, h)
	signIn(t, h) // the user signs in again in another tab
	fresh := stored(t, vlt)
	res := h.revokeAndDelete(bg, user, "github", op)
	if res.confirmed || gh.revoked.Load() != 0 {
		t.Fatalf("logout = %+v, revocations = %d; want the replacement left alone", res, gh.revoked.Load())
	}
	if logoutIsPending(h) {
		t.Error("the marker the sign-in cleared was recorded again")
	}
	if s := stored(t, vlt); s.Version != fresh.Version {
		t.Error("the replacement credential was changed")
	}
	if _, err := h.Credential(bg, user, "github", VendRequest{}); err != nil {
		t.Errorf("the replacement is not vendable: %v", err)
	}
}

// Review P2 (fourth round): logout stores the pair it rotated before it asks GitHub
// to revoke it, so a process that stops in between leaves recovery a live token.
func TestLogoutStoresRotatedPairBeforeRevoking(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
	put(t, gh, vlt, appCred("ghu_old", "ghr_old", "v1", time.Minute))
	var storedAtRevoke string
	gh.onRevoke = func(string) {
		if c, err := vlt.Get(bg, user, "github"); err == nil {
			storedAtRevoke = c.AccessToken
		}
	}
	if res := logoutNow(t, h); !res.confirmed {
		t.Fatalf("logout = %+v", res)
	}
	revokedWith := gh.revokedWith.Load().(string)
	if revokedWith == "ghu_old" || storedAtRevoke != revokedWith {
		t.Errorf("at revocation the vault held %q, revoking %q; want the rotated token stored first", storedAtRevoke, revokedWith)
	}
}

// flakyLedger fails LogoutPending on its nth call.
type flakyLedger struct {
	vault.LogoutLedger
	n, calls int
}

func (f *flakyLedger) LogoutPending(ctx context.Context, userID, provider string) (bool, error) {
	f.calls++
	if f.calls == f.n {
		return false, context.DeadlineExceeded
	}
	return f.LogoutLedger.LogoutPending(ctx, userID, provider)
}

// Review P2 (fourth round): a ledger that cannot be read withholds the credential as
// a temporary failure, never as "sign-in required", on either check.
func TestLedgerErrorIsTemporary(t *testing.T) {
	for _, n := range []int{1, 2} {
		gh := newFakeGitHub(t)
		h, _, _, vlt := newKindHandler(t, gh, config.GitHubApp)
		put(t, gh, vlt, appCred("ghu_a", "ghr_a", "v1", 4*time.Hour))
		h.ledger = &flakyLedger{LogoutLedger: h.ledger, n: n}
		_, err := h.Credential(bg, user, "github", VendRequest{})
		if err == nil || errors.Is(err, vault.ErrSignInRequired) {
			t.Errorf("ledger read %d failing: err = %v, want a temporary error", n, err)
		}
	}
}
