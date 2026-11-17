package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"golang.org/x/oauth2"

	"github.com/jackfrancis/gofer/internal/config"
	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/vault"
)

// fakeGitHub serves the three GitHub endpoints a sign-in touches: the token
// exchange, /user (reporting *login), and token revocation (counted).
type fakeGitHub struct {
	srv     *httptest.Server
	login   atomic.Value // string
	revoked atomic.Int32
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{}
	f.login.Store("octocat")
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/login/oauth/access_token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"tok-abc","token_type":"bearer"}`))
		case r.URL.Path == "/user":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":42,"login":"` + f.login.Load().(string) + `","email":"u@example.test"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/applications/cid/token":
			f.revoked.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// newTestHandler wires a Handler and session manager the way server.New does, with the
// GitHub provider pointed at the fake.
func newTestHandler(t *testing.T, gh *fakeGitHub, allowed ...string) (*Handler, *session.Manager, *config.Config, vault.Vault) {
	t.Helper()
	cfg := &config.Config{
		BaseURL:             "https://gofer.example.test",
		SessionSecret:       []byte("test-session-secret-0123456789ab"),
		CookieSecure:        true,
		Providers:           config.Providers{GitHub: config.OAuthApp{ClientID: "cid", ClientSecret: "csec"}},
		AllowedGitHubLogins: allowed,
	}
	sessions := session.NewManager(cfg.SessionSecret, cfg.CookieSecure,
		session.WithAllow(func(u *session.User) bool { return cfg.SignInAllowed(u.Provider, u.Login) }))
	vlt := vault.NewMemoryVault()
	h := NewHandler(cfg, sessions, vlt)
	p := h.providers["github"]
	p.oauth.Endpoint = oauth2.Endpoint{AuthURL: gh.srv.URL + "/login/oauth/authorize", TokenURL: gh.srv.URL + "/login/oauth/access_token"}
	p.userURL = gh.srv.URL + "/user"
	h.githubAPIBase = gh.srv.URL
	h.client = gh.srv.Client()
	return h, sessions, cfg, vlt
}

// signIn runs Login then Callback as a browser would and returns the callback's
// response plus a request carrying whatever cookies it set.
func signIn(t *testing.T, h *Handler) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/auth/github/login", nil)
	req.SetPathValue("provider", "github")
	rec := httptest.NewRecorder()
	h.Login(rec, req)
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || rec.Code != http.StatusFound {
		t.Fatalf("login: status %d, location %q", rec.Code, rec.Header().Get("Location"))
	}

	cb := httptest.NewRequest(http.MethodGet, "/auth/github/callback?code=c&state="+url.QueryEscape(loc.Query().Get("state")), nil)
	cb.SetPathValue("provider", "github")
	for _, c := range rec.Result().Cookies() {
		cb.AddCookie(c)
	}
	out := httptest.NewRecorder()
	h.Callback(out, cb)

	next := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range out.Result().Cookies() {
		if c.MaxAge >= 0 && c.Value != "" {
			next.AddCookie(c)
		}
	}
	return out, next
}

// A listed login signs in, case-insensitively: a session, and the token in the vault.
func TestCallbackAllowsListedLogin(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.login.Store("OctoCat")
	h, sessions, _, vlt := newTestHandler(t, gh, "octocat")

	rec, next := signIn(t, h)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("callback: status %d, location %q; want 302 to /", rec.Code, rec.Header().Get("Location"))
	}
	if u := sessions.CurrentUser(next); u == nil || u.Login != "OctoCat" {
		t.Fatalf("no session for the allowed account: %+v", u)
	}
	if _, err := vlt.Get(context.Background(), "github:42", "github"); err != nil {
		t.Errorf("token not stored for the allowed account: %v", err)
	}
	if gh.revoked.Load() != 0 {
		t.Error("an allowed account's token was revoked")
	}
}

// An unlisted login is refused: 403, no session, nothing in the vault, and the token
// GitHub issued for it is revoked.
func TestCallbackRefusesUnlistedLogin(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.login.Store("mallory")
	h, sessions, _, vlt := newTestHandler(t, gh, "octocat")

	rec, next := signIn(t, h)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("callback status = %d, want 403", rec.Code)
	}
	if sessions.CurrentUser(next) != nil {
		t.Error("a refused account got a session")
	}
	if _, err := vlt.Get(context.Background(), "github:42", "github"); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("a refused account's token reached the vault (err=%v)", err)
	}
	if gh.revoked.Load() != 1 {
		t.Errorf("revocations = %d, want 1 (the refused account's token)", gh.revoked.Load())
	}
}

// With no allow-list, any account signs in (the behaviour before the allow-list).
func TestCallbackWithoutAllowListAllowsAnyone(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.login.Store("anyone")
	h, sessions, _, _ := newTestHandler(t, gh)

	rec, next := signIn(t, h)
	if rec.Code != http.StatusFound || sessions.CurrentUser(next) == nil {
		t.Fatalf("callback status %d; want a session for any account without an allow-list", rec.Code)
	}
}

// Tightening the allow-list ends an existing session on its next request; putting the
// account back restores it (the stored session is kept).
func TestAllowListAppliesToExistingSessions(t *testing.T) {
	gh := newFakeGitHub(t)
	h, sessions, cfg, _ := newTestHandler(t, gh, "octocat")

	_, next := signIn(t, h)
	if sessions.CurrentUser(next) == nil {
		t.Fatal("no session after sign-in")
	}
	cfg.AllowedGitHubLogins = []string{"someone-else"}
	if sessions.CurrentUser(next) != nil {
		t.Error("a session outlived its account's removal from the allow-list")
	}
	cfg.AllowedGitHubLogins = []string{"octocat"}
	if sessions.CurrentUser(next) == nil {
		t.Error("the session did not come back when the account was listed again")
	}
}
