package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/oauth2"

	"github.com/jackfrancis/gofer/internal/config"
	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/vault"
)

// fakeGitHub is a stateful stand-in for the GitHub endpoints sign-in and the
// credential source touch: the token endpoint (code exchange and refresh, with
// single-use rotating refresh tokens when expiring), /user, check-token and grant
// revocation.
type fakeGitHub struct {
	srv   *httptest.Server
	login atomic.Value // string

	mu        sync.Mutex
	expiring  bool              // issue 8 h tokens with refresh tokens (a GitHub App)
	valid     map[string]bool   // live access tokens
	issuer    map[string]string // access token -> the client that issued it ("" = cid)
	userID    atomic.Int64      // the id /user answers with
	refresh   map[string]string // live refresh token -> the access token issued with it
	n         int
	killNext  bool // the next issued token is dead on arrival (a grant revoked mid-flow)
	revokeErr bool // grant revocation fails
	// checkStatus, when set, is check-token's answer whatever the token.
	checkStatus int
	// onCode, a func(), runs when an authorization code is exchanged, before the
	// token is issued.
	onCode atomic.Value
	// onRefresh runs when a refresh is served, after the pair has rotated.
	onRefresh func()
	// onRevoke runs when a grant revocation arrives, with the token it carries.
	onRevoke func(token string)

	refreshes    atomic.Int32
	checks       atomic.Int32
	revoked      atomic.Int32 // grant revocations
	revokedOther atomic.Int32 // grant revocations of a client other than cid (the write grant)
	revokedWith  atomic.Value // string: the access token the last revocation carried
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{valid: map[string]bool{}, refresh: map[string]string{}, issuer: map[string]string{}}
	f.login.Store("octocat")
	f.userID.Store(42)
	f.revokedWith.Store("")
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/login/oauth/access_token":
			_ = r.ParseForm()
			if hook, _ := f.onCode.Load().(func()); hook != nil && r.Form.Get("grant_type") == "authorization_code" {
				hook() // outside f.mu: the hook may call back into this server
			}
			w.Header().Set("Content-Type", "application/json")
			f.mu.Lock()
			defer f.mu.Unlock()
			if r.Form.Get("grant_type") == "refresh_token" {
				f.refreshes.Add(1)
				old, ok := f.refresh[r.Form.Get("refresh_token")]
				if !ok {
					// GitHub answers token-endpoint errors with 200 and an error body.
					_, _ = w.Write([]byte(`{"error":"bad_refresh_token","error_description":"The refresh token passed is incorrect or expired."}`))
					return
				}
				delete(f.refresh, r.Form.Get("refresh_token"))
				delete(f.valid, old) // rotation kills the old access token
				if f.onRefresh != nil {
					f.onRefresh()
				}
			}
			resp := f.issueLocked()
			if id := r.Form.Get("client_id"); id != "" && id != "cid" {
				f.issuer[resp["access_token"].(string)] = id
			}
			_ = json.NewEncoder(w).Encode(resp)
		case r.URL.Path == "/user":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":` + strconv.FormatInt(f.userID.Load(), 10) + `,"login":"` + f.login.Load().(string) + `","email":"u@example.test"}`))
		case appPath(r, "/token") != "" && r.Method == http.MethodPost:
			client := appPath(r, "/token")
			f.checks.Add(1)
			if !f.basicAuthOK(r) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			f.mu.Lock()
			forced := f.checkStatus
			f.mu.Unlock()
			if forced != 0 {
				w.WriteHeader(forced)
				return
			}
			if tok := bodyToken(r); f.isValid(tok) && f.issuedBy(tok) == client {
				_, _ = w.Write([]byte(`{"token":"echoed"}`))
				return
			}
			http.NotFound(w, r)
		case appPath(r, "/grant") != "" && r.Method == http.MethodDelete:
			client := appPath(r, "/grant")
			f.revoked.Add(1)
			if client != "cid" {
				f.revokedOther.Add(1)
			}
			tok := bodyToken(r)
			f.revokedWith.Store(tok)
			if f.onRevoke != nil {
				f.onRevoke(tok)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.revokeErr || !f.valid[tok] || !f.basicAuthOK(r) || f.issuedByLocked(tok) != client {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			// Revoking a grant kills every token of that client, and only of that one.
			for t := range f.valid {
				if f.issuedByLocked(t) == client {
					delete(f.valid, t)
				}
			}
			if client == "cid" {
				f.refresh = map[string]string{}
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// issueLocked mints a token response; f.mu is held.
func (f *fakeGitHub) issueLocked() map[string]any {
	f.n++
	tok := "tok-" + strconv.Itoa(f.n)
	if !f.killNext {
		f.valid[tok] = true
	}
	f.killNext = false
	out := map[string]any{"access_token": tok, "token_type": "bearer", "scope": ""}
	if f.expiring {
		ref := "ref-" + strconv.Itoa(f.n)
		f.refresh[ref] = tok
		out["refresh_token"] = ref
		out["expires_in"] = 28800
		out["refresh_token_expires_in"] = 15897600
	}
	return out
}

func (f *fakeGitHub) isValid(tok string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.valid[tok]
}

// seed registers a live access token (and refresh token, if any) issued earlier.
func (f *fakeGitHub) seed(access, refresh string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.valid[access] = true
	if refresh != "" {
		f.refresh[refresh] = access
	}
}

func (f *fakeGitHub) basicAuthOK(r *http.Request) bool {
	u, p, ok := r.BasicAuth()
	return ok && ((u == "cid" && p == "csec") || (u == "wcid" && p == "wsec"))
}

// appPath is the client id in /applications/{id}{suffix}, or "".
func appPath(r *http.Request, suffix string) string {
	rest, ok := strings.CutPrefix(r.URL.Path, "/applications/")
	if !ok {
		return ""
	}
	id, ok := strings.CutSuffix(rest, suffix)
	if !ok || strings.Contains(id, "/") {
		return ""
	}
	return id
}

func (f *fakeGitHub) issuedBy(tok string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issuedByLocked(tok)
}

func (f *fakeGitHub) issuedByLocked(tok string) string {
	if c := f.issuer[tok]; c != "" {
		return c
	}
	return "cid"
}

func bodyToken(r *http.Request) string {
	var b struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(r.Body).Decode(&b)
	return b.AccessToken
}

// newTestHandler wires a Handler and session manager the way server.New does, with the
// GitHub provider pointed at the fake, under the default OAuth-app kind.
func newTestHandler(t *testing.T, gh *fakeGitHub, allowed ...string) (*Handler, *session.Manager, *config.Config, vault.Vault) {
	return newKindHandler(t, gh, config.GitHubOAuthApp, allowed...)
}

func newKindHandler(t *testing.T, gh *fakeGitHub, kind string, allowed ...string) (*Handler, *session.Manager, *config.Config, vault.Vault) {
	t.Helper()
	gh.mu.Lock()
	gh.expiring = kind == config.GitHubApp
	gh.mu.Unlock()
	cfg := &config.Config{
		BaseURL:       "https://gofer.example.test",
		SessionSecret: []byte("test-session-secret-0123456789ab"),
		CookieSecure:  true,
		Providers: config.Providers{
			GitHub:     config.OAuthApp{ClientID: "cid", ClientSecret: "csec"},
			GitHubKind: kind,
		},
		AllowedGitHubLogins: allowed,
	}
	sessions := session.NewManager(cfg.SessionSecret, cfg.CookieSecure,
		session.WithAllow(func(u *session.User) bool { return cfg.SignInAllowed(u.Provider, u.Login) }))
	vlt := vault.NewMemoryVault()
	h := NewHandler(cfg, sessions, vlt)
	p := h.providers["github"]
	// github.Endpoint sends client credentials in the form; so does the fake's.
	p.oauth.Endpoint = oauth2.Endpoint{AuthURL: gh.srv.URL + "/login/oauth/authorize", TokenURL: gh.srv.URL + "/login/oauth/access_token", AuthStyle: oauth2.AuthStyleInParams}
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
		t.Errorf("grant revocations = %d, want 1 (the refused account's grant)", gh.revoked.Load())
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
