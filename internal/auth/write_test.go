package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	"golang.org/x/oauth2"

	"github.com/jackfrancis/gofer/internal/config"
	"github.com/jackfrancis/gofer/internal/session"
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
	if gh.revokedOther.Load() != 1 {
		t.Errorf("write-grant revocations = %d, want 1", gh.revokedOther.Load())
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
