package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/jackfrancis/gofer/internal/config"
	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/storage"
	"github.com/jackfrancis/gofer/internal/vault"
)

// newWriteHandler is newKindHandler with the write grant configured (ADR 0009).
func newWriteHandler(t *testing.T, gh *fakeGitHub, allowed ...string) (*Handler, *session.Manager, *config.Config, vault.Vault) {
	t.Helper()
	gh.mu.Lock()
	gh.expiring = true
	gh.mu.Unlock()
	cfg := &config.Config{
		BaseURL:       "https://gofer.example.test",
		SessionSecret: []byte("test-session-secret-0123456789ab"),
		CookieSecure:  true,
		Providers: config.Providers{
			GitHub:      config.OAuthApp{ClientID: "cid", ClientSecret: "csec"},
			GitHubKind:  config.GitHubApp,
			GitHubWrite: config.OAuthApp{ClientID: "wcid", ClientSecret: "wsec"},
		},
		AllowedGitHubLogins: allowed,
	}
	sessions := session.NewManager(cfg.SessionSecret, cfg.CookieSecure,
		session.WithAllow(func(u *session.User) bool { return cfg.SignInAllowed(u.Provider, u.Login) }))
	vlt := vault.NewMemoryVault()
	h := NewHandler(cfg, sessions, vlt)
	for _, name := range []string{"github", WriteProvider} {
		p := h.providers[name]
		p.oauth.Endpoint = oauth2.Endpoint{AuthURL: gh.srv.URL + "/login/oauth/authorize", TokenURL: gh.srv.URL + "/login/oauth/access_token", AuthStyle: oauth2.AuthStyleInParams}
		p.userURL = gh.srv.URL + "/user"
	}
	h.githubAPIBase = gh.srv.URL
	h.client = gh.srv.Client()
	return h, sessions, cfg, vlt
}

// connectWrite runs ConnectWrite then ConnectWriteCallback for the signed-in browser
// whose cookies are on signedIn, and returns the callback's response, the authorize
// URL it was sent to, and a request carrying the browser's cookies afterwards.
func connectWrite(t *testing.T, h *Handler, signedIn *http.Request) (*httptest.ResponseRecorder, *url.URL, *http.Request) {
	t.Helper()
	return connectWriteWith(t, h, signedIn, nil)
}

// connectWriteWith is connectWrite, running between before the callback.
func connectWriteWith(t *testing.T, h *Handler, signedIn *http.Request, between func()) (*httptest.ResponseRecorder, *url.URL, *http.Request) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, writePath+"/login", nil)
	for _, c := range signedIn.Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ConnectWrite(rec, req)
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || rec.Code != http.StatusFound {
		t.Fatalf("connect: status %d, location %q", rec.Code, rec.Header().Get("Location"))
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "gofer_session" {
			t.Fatal("connecting the write grant touched the session cookie")
		}
	}
	cb := httptest.NewRequest(http.MethodGet, writePath+"/callback?code=c&state="+url.QueryEscape(loc.Query().Get("state")), nil)
	for _, c := range signedIn.Cookies() {
		cb.AddCookie(c)
	}
	for _, c := range rec.Result().Cookies() {
		cb.AddCookie(c)
	}
	if between != nil {
		between()
	}
	out := httptest.NewRecorder()
	h.ConnectWriteCallback(out, cb)
	next := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range signedIn.Cookies() {
		next.AddCookie(c)
	}
	return out, loc, next
}

func writeStored(t *testing.T, v vault.Vault) (vault.Credential, bool) {
	t.Helper()
	c, err := v.Get(bg, user, WriteProvider)
	if errors.Is(err, vault.ErrNotFound) {
		return vault.Credential{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return c, true
}

// Connecting stores the grant from the write client, asks for public_repo only, and
// leaves the session alone.
func TestConnectWriteStoresGrantAndKeepsSession(t *testing.T) {
	gh := newFakeGitHub(t)
	h, sessions, _, vlt := newWriteHandler(t, gh)
	_, signedIn := signIn(t, h)

	rec, authorize, next := connectWrite(t, h, signedIn)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("callback: %d %q", rec.Code, rec.Body)
	}
	if got := authorize.Query().Get("scope"); got != "public_repo" {
		t.Errorf("scope = %q, want public_repo", got)
	}
	if got := authorize.Query().Get("client_id"); got != "wcid" {
		t.Errorf("client_id = %q, want the write client", got)
	}
	if sessions.CurrentUser(next) == nil {
		t.Fatal("the session did not survive connecting the write grant")
	}
	c, ok := writeStored(t, vlt)
	if !ok || c.ClientID != "wcid" || c.Version == "" || c.Login != "octocat" {
		t.Fatalf("stored write grant = %+v (ok=%v)", c, ok)
	}
	if st := h.CommentingState(bg, user); !st.Enabled || !st.Connected {
		t.Errorf("state = %+v", st)
	}
	if got, err := h.WriteCredential(bg, user); err != nil || got.AccessToken != c.AccessToken {
		t.Errorf("WriteCredential = %+v, %v", got, err)
	}
	// The sign-in credential is untouched.
	if s := stored(t, vlt); s.ClientID != "cid" {
		t.Errorf("sign-in credential = %+v", s)
	}
}

// The write grant is never a way to sign in and never reaches a run.
func TestWriteGrantIsNeverVendedOrOffered(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newWriteHandler(t, gh)
	_, signedIn := signIn(t, h)
	connectWrite(t, h, signedIn)
	if _, ok := writeStored(t, vlt); !ok {
		t.Fatal("setup: no write grant")
	}

	if _, err := h.Credential(bg, user, WriteProvider, VendRequest{}); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("Credential(github-write) err = %v, want ErrNotFound", err)
	}
	if ok, _ := h.ScheduledRunAllowed(bg, user, WriteProvider); ok {
		t.Error("ScheduledRunAllowed(github-write) = true")
	}
	if slices.Contains(h.Providers(), WriteProvider) {
		t.Errorf("Providers() = %v offers the write grant as a sign-in", h.Providers())
	}
	for _, path := range []string{"login", "callback"} {
		req := httptest.NewRequest(http.MethodGet, "/auth/github-write/"+path, nil)
		req.SetPathValue("provider", WriteProvider)
		rec := httptest.NewRecorder()
		if path == "login" {
			h.Login(rec, req)
		} else {
			h.Callback(rec, req)
		}
		if rec.Code != http.StatusNotFound {
			t.Errorf("generic %s for the write grant: %d, want 404", path, rec.Code)
		}
	}
}

// A grant from a different GitHub account than the session's is revoked, not stored.
func TestConnectWriteRefusesAnotherAccount(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newWriteHandler(t, gh)
	_, signedIn := signIn(t, h)
	gh.userID.Store(43)
	rec, _, _ := connectWrite(t, h, signedIn)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("callback = %d, want 403", rec.Code)
	}
	if _, ok := writeStored(t, vlt); ok {
		t.Error("another account's grant was stored")
	}
	if gh.tokensRevoked.Load() != 1 || gh.revokedOther.Load() != 0 {
		t.Errorf("token revocations = %d, grant revocations = %d; want the one token only", gh.tokensRevoked.Load(), gh.revokedOther.Load())
	}
}

// An account the allow-list no longer admits cannot connect, and its grant is revoked.
func TestConnectWriteHonoursAllowList(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newWriteHandler(t, gh, "octocat")
	_, signedIn := signIn(t, h)
	// The session is checked against the allow-list too, so change only the login
	// GitHub reports for the grant.
	gh.login.Store("mallory")
	rec, _, _ := connectWrite(t, h, signedIn)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("callback = %d, want 403", rec.Code)
	}
	if _, ok := writeStored(t, vlt); ok {
		t.Error("a refused account's grant was stored")
	}
}

// A callback with no grant flow (or another user's) completes nothing.
func TestConnectWriteCallbackNeedsItsFlow(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newWriteHandler(t, gh)
	_, signedIn := signIn(t, h)
	cb := httptest.NewRequest(http.MethodGet, writePath+"/callback?code=c&state=x", nil)
	for _, c := range signedIn.Cookies() {
		cb.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ConnectWriteCallback(rec, cb)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("callback without a flow = %d, want 400", rec.Code)
	}
	if _, ok := writeStored(t, vlt); ok {
		t.Error("stored a grant without a flow")
	}
}

// Connecting needs a signed-in session.
func TestConnectWriteNeedsSession(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, _ := newWriteHandler(t, gh)
	rec := httptest.NewRecorder()
	h.ConnectWrite(rec, httptest.NewRequest(http.MethodGet, writePath+"/login", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("anonymous connect: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

// Logout revokes and deletes the write grant along with the sign-in credential.
func TestLogoutRevokesWriteGrant(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newWriteHandler(t, gh)
	_, signedIn := signIn(t, h)
	connectWrite(t, h, signedIn)

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	for _, c := range signedIn.Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.Logout(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout = %d %s", rec.Code, rec.Body)
	}
	if gh.revokedOther.Load() != 1 {
		t.Errorf("write-grant revocations = %d, want 1", gh.revokedOther.Load())
	}
	if _, ok := writeStored(t, vlt); ok {
		t.Error("write grant kept after logout")
	}
}

// A user who never connected the write grant still gets a confirmed logout.
func TestLogoutWithoutWriteGrantStaysConfirmed(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, _ := newWriteHandler(t, gh)
	if rec := logout(t, h); rec.Code != http.StatusNoContent {
		t.Fatalf("logout = %d %s, want 204", rec.Code, rec.Body)
	}
	if gh.revokedOther.Load() != 0 {
		t.Error("revoked a write grant nobody connected")
	}
}

// Disconnect revokes and deletes the write grant and keeps the session.
func TestDisconnectWrite(t *testing.T) {
	gh := newFakeGitHub(t)
	h, sessions, _, vlt := newWriteHandler(t, gh)
	_, signedIn := signIn(t, h)
	connectWrite(t, h, signedIn)

	req := httptest.NewRequest(http.MethodPost, writePath+"/disconnect", nil)
	for _, c := range signedIn.Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.DisconnectWrite(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("disconnect = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if _, ok := writeStored(t, vlt); ok {
		t.Error("write grant kept after disconnect")
	}
	if gh.revokedOther.Load() != 1 {
		t.Errorf("write-grant revocations = %d, want 1", gh.revokedOther.Load())
	}
	if sessions.CurrentUser(signedIn) == nil {
		t.Error("disconnect ended the session")
	}
	if _, err := h.WriteCredential(bg, user); !errors.Is(err, vault.ErrSignInRequired) {
		t.Errorf("WriteCredential after disconnect: %v", err)
	}
	// The sign-in credential still works.
	if _, err := h.Credential(bg, user, "github", VendRequest{}); err != nil {
		t.Errorf("sign-in credential after disconnect: %v", err)
	}
}

// A 401 is acted on only when check-token confirms the grant is dead.
func TestWriteRejected(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newWriteHandler(t, gh)
	_, signedIn := signIn(t, h)
	connectWrite(t, h, signedIn)
	c, _ := writeStored(t, vlt)

	h.WriteRejected(bg, user, c.Version) // still valid at GitHub
	if _, ok := writeStored(t, vlt); !ok {
		t.Fatal("deleted a grant check-token says is valid")
	}
	gh.mu.Lock()
	delete(gh.valid, c.AccessToken) // the user revoked gofer in GitHub's settings
	gh.mu.Unlock()
	h.WriteRejected(bg, user, "another-version")
	if _, ok := writeStored(t, vlt); !ok {
		t.Fatal("deleted a grant for a rejection of another version")
	}
	h.WriteRejected(bg, user, c.Version)
	if _, ok := writeStored(t, vlt); ok {
		t.Error("kept a grant GitHub confirmed dead")
	}
	if st := h.CommentingState(bg, user); st.Connected {
		t.Error("still connected after the grant died")
	}
}

// Without the write client configured, nothing about the grant exists.
func TestCommentingOffWithoutWriteClient(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, _ := newKindHandler(t, gh, config.GitHubApp)
	if st := h.CommentingState(bg, user); st.Enabled || st.Connected {
		t.Errorf("state = %+v", st)
	}
	rec := httptest.NewRecorder()
	h.ConnectWrite(rec, httptest.NewRequest(http.MethodGet, writePath+"/login", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("connect without a write client = %d, want 404", rec.Code)
	}
}

// Review P1: logout records the write grant's pending revocation in the same commit
// as the session's end, so a restart right after it still revokes the grant.
func TestLogoutCommitsWriteGrantMarkerWithSession(t *testing.T) {
	gh := newFakeGitHub(t)
	dir := t.TempDir()
	db, err := storage.Open(bg, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	keys, _ := storage.NewKeyring(make([]byte, storage.KeySize))
	h, _, cfg, _ := newWriteHandler(t, gh)
	h.vault, h.ledger = db.Vault(keys), db.Vault(keys)
	h.sessions = session.NewManager(cfg.SessionSecret, cfg.CookieSecure, session.WithStore(db.Sessions()))
	_, signedIn := signIn(t, h)
	connectWrite(t, h, signedIn)
	if _, err := db.Vault(keys).Get(bg, user, WriteProvider); err != nil {
		t.Fatalf("setup: no write grant: %v", err)
	}

	// Both credential locks are busy, so logout returns right after its commit.
	defer func(d time.Duration) { logoutLockWait = d }(logoutLockWait)
	logoutLockWait = 20 * time.Millisecond
	u1, _ := h.locks.lock(bg, user, "github")
	u2, _ := h.locks.lock(bg, user, WriteProvider)
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	for _, c := range signedIn.Cookies() {
		req.AddCookie(c)
	}
	h.Logout(httptest.NewRecorder(), req)
	oldLogouts := []*localLogout{h.locks.currentLogout(user, "github"), h.locks.currentLogout(user, WriteProvider)}

	for _, p := range []string{"github", WriteProvider} {
		if pending, err := db.Vault(keys).LogoutPending(bg, user, p); err != nil || !pending {
			t.Errorf("%s marker = %v, %v; want recorded with the session's end", p, pending, err)
		}
	}
	// A new process over the same database (the old one's locks stay held, as if
	// it had died) refuses the grant and finishes its revocation.
	defer func() {
		// Let the old process's deferred revocations wake and finish before the
		// database goes away.
		u1()
		u2()
		for _, lo := range oldLogouts {
			if lo == nil {
				continue
			}
			select {
			case <-lo.done:
			case <-time.After(5 * time.Second):
				t.Error("the old process's deferred logout did not finish")
			}
		}
	}()
	h2, _, _, _ := newWriteHandler(t, gh)
	h2.vault, h2.ledger = db.Vault(keys), db.Vault(keys)
	if _, err := h2.WriteCredential(bg, user); !errors.Is(err, vault.ErrSignInRequired) {
		t.Errorf("write grant usable after the logout commit: %v", err)
	}
	if gh.revokedOther.Load() != 1 {
		t.Errorf("write-grant revocations = %d, want 1", gh.revokedOther.Load())
	}
	if _, err := db.Vault(keys).Get(bg, user, WriteProvider); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("write grant kept after the new process finished its logout: %v", err)
	}
}

// Review P1: a write grant that cannot be read at logout is revoked anyway, and a
// commit failure changes nothing for either credential.
func TestLogoutWriteGrantFailsClosed(t *testing.T) {
	gh := newFakeGitHub(t)
	h, sessions, _, vlt := newWriteHandler(t, gh)
	_, signedIn := signIn(t, h)
	connectWrite(t, h, signedIn)

	h.vault = failingCommitVault{vlt}
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	for _, c := range signedIn.Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.Logout(rec, req)
	if rec.Code != http.StatusServiceUnavailable || sessions.CurrentUser(signedIn) == nil {
		t.Fatalf("logout = %d; want 503 with the session standing", rec.Code)
	}
	if gh.revoked.Load() != 0 {
		t.Error("revoked although nothing was recorded")
	}
}

// Review P2: a connection that completes after logout ended its session stores
// nothing and revokes the grant it was issued.
func TestConnectAfterLogoutStoresNothing(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newWriteHandler(t, gh)
	_, signedIn := signIn(t, h)
	ended := false
	tok := &oauth2.Token{AccessToken: "w-tok", TokenType: "bearer"}
	gh.seed("w-tok", "")
	gh.mu.Lock()
	gh.issuer["w-tok"] = "wcid"
	gh.mu.Unlock()
	out, err := h.storeWriteGrant(bg, h.providers[WriteProvider], user, "octocat", tok, func(context.Context) (bool, error) {
		ended = true
		return false, nil // logout ended the session during the exchange
	})
	if err != nil || out != grantSessionEnded || !ended {
		t.Fatalf("outcome = %v, %v (checked=%v)", out, err, ended)
	}
	if _, ok := writeStored(t, vlt); ok {
		t.Error("stored a grant for an ended session")
	}

	// Through the callback: logout lands while the code is being exchanged.
	gh.onCode.Store(func() {
		gh.onCode.Store(func() {})
		req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
		for _, c := range signedIn.Cookies() {
			req.AddCookie(c)
		}
		h.Logout(httptest.NewRecorder(), req)
	})
	rec, _, _ := connectWrite(t, h, signedIn)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "signed out") || !strings.Contains(rec.Body.String(), "confirmed") {
		t.Fatalf("callback after logout = %d %s", rec.Code, rec.Body)
	}
	if _, ok := writeStored(t, vlt); ok {
		t.Error("the callback stored a grant after logout")
	}
	if gh.tokensRevoked.Load() != 1 {
		t.Errorf("token revocations = %d, want 1 (the unsaved token)", gh.tokensRevoked.Load())
	}
}

// failingPutLedger is a vault whose writes fail.
type failingPutLedger struct{ *vault.MemoryVault }

func (failingPutLedger) PutClearingLogout(context.Context, string, vault.Credential) error {
	return errors.New("disk full")
}

// Review P2: a grant gofer could not save is reported, and revoked, not treated as
// connected.
func TestConnectWriteReportsVaultFailure(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newWriteHandler(t, gh)
	_, signedIn := signIn(t, h)
	h.vault = failingPutLedger{vlt.(*vault.MemoryVault)}
	rec, _, _ := connectWrite(t, h, signedIn)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "could not save") {
		t.Fatalf("callback = %d %s", rec.Code, rec.Body)
	}
	if gh.tokensRevoked.Load() != 1 {
		t.Errorf("token revocations = %d, want 1", gh.tokensRevoked.Load())
	}
	h.vault = vlt
	if st := h.CommentingState(bg, user); st.Connected {
		t.Error("connected after a failed save")
	}
}

// Disconnect with nothing connected changes nothing and reports no error.
func TestDisconnectWithoutGrant(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newWriteHandler(t, gh)
	_, signedIn := signIn(t, h)
	req := httptest.NewRequest(http.MethodPost, writePath+"/disconnect", nil)
	for _, c := range signedIn.Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.DisconnectWrite(rec, req)
	if rec.Header().Get("Location") != "/" || gh.revokedOther.Load() != 0 {
		t.Fatalf("disconnect = %q, revocations %d", rec.Header().Get("Location"), gh.revokedOther.Load())
	}
	if p, _ := vlt.(*vault.MemoryVault).LogoutPending(bg, user, WriteProvider); p {
		t.Error("recorded a pending revocation of nothing")
	}
}

// Review P2 (second round): a grant flow completes only from the session that began
// it, not from another session of the same user (one signed in again after a logout
// in another tab).
func TestConnectWriteBoundToStartingSession(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newWriteHandler(t, gh)
	_, first := signIn(t, h)
	req := httptest.NewRequest(http.MethodGet, writePath+"/login", nil)
	for _, c := range first.Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ConnectWrite(rec, req)
	loc, _ := url.Parse(rec.Header().Get("Location"))

	_, second := signIn(t, h) // the same account, a new session
	cb := httptest.NewRequest(http.MethodGet, writePath+"/callback?code=c&state="+url.QueryEscape(loc.Query().Get("state")), nil)
	for _, c := range second.Cookies() {
		cb.AddCookie(c)
	}
	for _, c := range rec.Result().Cookies() {
		cb.AddCookie(c)
	}
	out := httptest.NewRecorder()
	h.ConnectWriteCallback(out, cb)
	if out.Code != http.StatusBadRequest {
		t.Fatalf("callback from another session = %d, want 400", out.Code)
	}
	if _, ok := writeStored(t, vlt); ok {
		t.Error("another session completed the flow")
	}
}

// Review P2 (second round): a write grant stored while commenting was configured is
// deleted at logout even after the write client is removed, so configuring it again
// never revives the grant; its revocation is reported unconfirmed.
func TestLogoutDeletesWriteGrantWithoutConfig(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newWriteHandler(t, gh)
	_, signedIn := signIn(t, h)
	connectWrite(t, h, signedIn)
	delete(h.providers, WriteProvider) // the write client is no longer configured

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	for _, c := range signedIn.Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.Logout(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), oauthAppsSettings) {
		t.Fatalf("logout = %d %s; want unconfirmed, linking the OAuth-app settings", rec.Code, rec.Body)
	}
	if _, ok := writeStored(t, vlt); ok {
		t.Error("the write grant survived logout with commenting unconfigured")
	}
}

// Review P2 (second round): a token issued before the profile lookup failed is
// revoked, and the user is told.
func TestConnectWriteProfileFailureRevokesToken(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newWriteHandler(t, gh)
	_, signedIn := signIn(t, h)
	gh.userFails.Store(true)
	rec, _, _ := connectWrite(t, h, signedIn)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "confirmed the token it issued is revoked") {
		t.Fatalf("callback = %d %s", rec.Code, rec.Body)
	}
	if gh.tokensRevoked.Load() != 1 {
		t.Errorf("token revocations = %d, want 1", gh.tokensRevoked.Load())
	}
	if _, ok := writeStored(t, vlt); ok {
		t.Error("stored a grant whose profile could not be read")
	}
}

// Review P2 (second round): cleaning up a failed connection revokes only its own
// token, so a connection that succeeded (another tab) keeps working.
func TestFailedConnectKeepsOtherConnection(t *testing.T) {
	gh := newFakeGitHub(t)
	h, _, _, vlt := newWriteHandler(t, gh)
	_, signedIn := signIn(t, h)
	connectWrite(t, h, signedIn)
	good, _ := writeStored(t, vlt)

	gh.userID.Store(43) // the second attempt comes back from another account and fails
	if rec, _, _ := connectWrite(t, h, signedIn); rec.Code != http.StatusForbidden {
		t.Fatalf("second connect = %d", rec.Code)
	}
	if !gh.isValid(good.AccessToken) {
		t.Error("cleaning up the failed connection revoked the stored grant's token")
	}
	if c, ok := writeStored(t, vlt); !ok || c.AccessToken != good.AccessToken {
		t.Errorf("stored grant = %+v (ok=%v)", c, ok)
	}
}
