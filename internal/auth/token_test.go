package auth

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/vault"
)

// patToken is a classic token for the tests (ghp_ + 36 characters).
const (
	patToken   = "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789"
	patToken2  = "ghp_" + "ZYXWVUTSRQPONMLKJIHGFEDCBA9876543210"
	tokenClock = "2026-11-24 12:00:00 UTC"
)

// patInfo is what the fake GitHub says about one token.
type patInfo struct {
	id     int64
	login  string
	scopes *string // the X-OAuth-Scopes header; nil = none sent (a fine-grained token)
	expiry string  // the expiration header; "" = none (never expires)
}

// patGitHub answers GET /user per token, with the headers GitHub sends, and passes
// the OAuth apps' /applications calls through to the main fake GitHub. It records
// whether any of those carried a personal access token: gofer must never send one to
// an OAuth app's revoke or check endpoint.
type patGitHub struct {
	srv *httptest.Server

	mu      sync.Mutex
	tokens  map[string]patInfo
	patToOA bool // a personal access token reached /applications
}

func newPATGitHub(t *testing.T, gh *fakeGitHub) *patGitHub {
	t.Helper()
	f := &patGitHub{tokens: map[string]patInfo{}}
	target, _ := url.Parse(gh.srv.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/applications/") {
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "ghp_") {
				f.mu.Lock()
				f.patToOA = true
				f.mu.Unlock()
			}
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			proxy.ServeHTTP(w, r)
			return
		}
		if r.URL.Path != "/user" {
			http.NotFound(w, r)
			return
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		info, ok := f.tokens[tok]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		if info.scopes != nil {
			w.Header().Set("X-OAuth-Scopes", *info.scopes)
		}
		if info.expiry != "" {
			w.Header().Set("GitHub-Authentication-Token-Expiration", info.expiry)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": info.id, "login": info.login})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *patGitHub) set(token string, info patInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[token] = info
}

func (f *patGitHub) kill(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tokens, token)
}

func scopes(s string) *string { return &s }

// goodPAT is a classic, public_repo-only token for octocat (id 42) expiring 30 days
// after tokenClock.
func goodPAT() patInfo {
	return patInfo{id: 42, login: "octocat", scopes: scopes("public_repo"), expiry: "2026-12-24 12:00:00 UTC"}
}

// newTokenHandler is a write-grant handler that allows tokens, signed in as octocat,
// with GitHub's /user answered per token from then on.
func newTokenHandler(t *testing.T) (*Handler, *http.Request, *patGitHub, *fakeGitHub, vault.Vault) {
	t.Helper()
	gh := newFakeGitHub(t)
	h, _, cfg, vlt := newWriteHandler(t, gh)
	cfg.Providers.GitHubWriteTokens = true
	h.providers[WriteProvider].tokens = true
	now, _ := time.Parse("2006-01-02 15:04:05 MST", tokenClock)
	h.now = func() time.Time { return now }
	_, signedIn := signIn(t, h)
	pat := newPATGitHub(t, gh)
	h.githubAPIBase = pat.srv.URL
	return h, signedIn, pat, gh, vlt
}

func postToken(h *Handler, signedIn *http.Request, token string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, writePath+"/token", strings.NewReader(url.Values{"token": {token}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	for _, c := range signedIn.Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ConnectWriteToken(rec, req)
	return rec
}

// A classic, public_repo-only token from the signed-in account, expiring within 90
// days, is stored as the write grant: the web tier posts with it, and the broker
// still never vends it to a run.
func TestConnectWriteTokenStoresAClassicToken(t *testing.T) {
	h, signedIn, pat, _, vlt := newTokenHandler(t)
	pat.set(patToken, goodPAT())
	rec := postToken(h, signedIn, patToken)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?commenting=token" {
		t.Fatalf("connect: %d %q %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	c, ok := writeStored(t, vlt)
	if !ok || c.ClientID != PersonalTokenClient || c.AccessToken != patToken || c.Login != "octocat" || c.Version == "" || c.RefreshToken != "" {
		t.Fatalf("stored = %+v (ok=%v)", c, ok)
	}
	if want, _ := time.Parse("2006-01-02 15:04:05 MST", "2026-12-24 12:00:00 UTC"); !c.Expiry.Equal(want) {
		t.Errorf("expiry = %v, want %v", c.Expiry, want)
	}
	st := h.CommentingState(bg, user)
	if !st.Connected || !st.Token || !st.TokensAllowed || st.Expires.IsZero() {
		t.Errorf("state = %+v", st)
	}
	if got, err := h.WriteCredential(bg, user); err != nil || got.AccessToken != patToken {
		t.Errorf("WriteCredential = %v", err)
	}
	if _, err := h.Credential(bg, user, WriteProvider, VendRequest{}); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("the broker vended the token: %v", err)
	}
}

// Every token gofer should not hold is refused, nothing is stored, and the token is
// never echoed back.
func TestConnectWriteTokenRefusals(t *testing.T) {
	h, signedIn, pat, _, vlt := newTokenHandler(t)
	for name, tc := range map[string]struct {
		token  string
		info   *patInfo
		header []string
		status int
		why    string
	}{
		"fine-grained prefix": {token: "github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz", status: 400, why: "fine-grained"},
		"not a token":         {token: "hunter2", status: 400, why: "does not look like"},
		"rejected":            {token: patToken, status: 400, why: "GitHub rejected"},
		"no scopes header":    {token: patToken, info: &patInfo{id: 42, login: "octocat", expiry: "2026-12-24 12:00:00 UTC"}, status: 400, why: "not a classic token"},
		"broader scopes":      {token: patToken, info: &patInfo{id: 42, login: "octocat", scopes: scopes("public_repo, repo"), expiry: "2026-12-24 12:00:00 UTC"}, status: 400, why: "exactly one scope"},
		"no scopes":           {token: patToken, info: &patInfo{id: 42, login: "octocat", scopes: scopes(""), expiry: "2026-12-24 12:00:00 UTC"}, status: 400, why: "it has none"},
		"never expires":       {token: patToken, info: &patInfo{id: 42, login: "octocat", scopes: scopes("public_repo")}, status: 400, why: "never expires"},
		"expires too late":    {token: patToken, info: &patInfo{id: 42, login: "octocat", scopes: scopes("public_repo"), expiry: "2027-06-24 12:00:00 UTC"}, status: 400, why: "more than 90 days"},
		"other account":       {token: patToken, info: &patInfo{id: 7, login: "mallory", scopes: scopes("public_repo"), expiry: "2026-12-24 12:00:00 UTC"}, status: 403, why: "different GitHub account"},
		"cross-site":          {token: patToken, info: func() *patInfo { p := goodPAT(); return &p }(), header: []string{"Sec-Fetch-Site", "cross-site"}, status: 403},
	} {
		t.Run(name, func(t *testing.T) {
			pat.kill(patToken)
			if tc.info != nil {
				pat.set(patToken, *tc.info)
			}
			rec := postToken(h, signedIn, tc.token, tc.header...)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.why) {
				t.Errorf("%d %s", rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), tc.token) {
				t.Error("the token was echoed back")
			}
			if _, ok := writeStored(t, vlt); ok {
				t.Error("a token was stored")
			}
		})
	}
}

// A token is refused while commenting is already connected (so an OAuth grant is never
// orphaned unrevoked), and when the deployment does not allow tokens.
func TestConnectWriteTokenNeedsRoomAndPermission(t *testing.T) {
	h, signedIn, pat, gh, vlt := newTokenHandler(t)
	h.githubAPIBase = gh.srv.URL // the OAuth connect re-checks with the main fake
	connectWrite(t, h, signedIn)
	h.githubAPIBase = pat.srv.URL
	oauth, _ := writeStored(t, vlt)
	pat.set(patToken, goodPAT())
	if rec := postToken(h, signedIn, patToken); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already connected") {
		t.Errorf("connected: %d %s", rec.Code, rec.Body)
	}
	if c, _ := writeStored(t, vlt); c.AccessToken != oauth.AccessToken {
		t.Error("the OAuth grant was replaced")
	}
	h.providers[WriteProvider].tokens = false
	if rec := postToken(h, signedIn, patToken); rec.Code != http.StatusNotFound {
		t.Errorf("tokens off: %d", rec.Code)
	}
}

// An expired token, or one stored while tokens were allowed and no longer are, is
// not used: the user connects again.
func TestStoredTokenStopsWorking(t *testing.T) {
	h, signedIn, pat, _, _ := newTokenHandler(t)
	pat.set(patToken, goodPAT())
	postToken(h, signedIn, patToken)
	later, _ := time.Parse("2006-01-02 15:04:05 MST", "2026-12-24 12:00:01 UTC")
	h.now = func() time.Time { return later }
	if st := h.CommentingState(bg, user); st.Connected {
		t.Errorf("an expired token reads as connected: %+v", st)
	}
	if _, err := h.WriteCredential(bg, user); !errors.Is(err, vault.ErrSignInRequired) {
		t.Errorf("an expired token was handed out: %v", err)
	}
	h.now = func() time.Time { t0, _ := time.Parse("2006-01-02 15:04:05 MST", tokenClock); return t0 }
	h.providers[WriteProvider].tokens = false
	if _, err := h.WriteCredential(bg, user); !errors.Is(err, vault.ErrSignInRequired) {
		t.Errorf("a token was handed out with tokens turned off: %v", err)
	}
}

// Disconnecting deletes gofer's copy without asking an OAuth app to revoke it (it
// cannot), and tells the user to delete the token on GitHub.
func TestDisconnectTokenDeletesWithoutRevoking(t *testing.T) {
	h, signedIn, pat, gh, vlt := newTokenHandler(t)
	pat.set(patToken, goodPAT())
	postToken(h, signedIn, patToken)
	req := httptest.NewRequest(http.MethodPost, writePath+"/disconnect", nil)
	for _, c := range signedIn.Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.DisconnectWrite(rec, req)
	if rec.Header().Get("Location") != "/?commenting=token-deleted" {
		t.Fatalf("disconnect: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if _, ok := writeStored(t, vlt); ok {
		t.Error("the token is still stored")
	}
	if pat.patToOA || gh.tokensRevoked.Load() != 0 || gh.revokedOther.Load() != 0 {
		t.Errorf("gofer tried to revoke the token through an OAuth app (sent=%v)", pat.patToOA)
	}
	if st := h.CommentingState(bg, user); st.Connected {
		t.Errorf("still connected: %+v", st)
	}
}

// Signing out deletes the token too, and says only the user can revoke it.
func TestLogoutWithTokenSaysDeleteIt(t *testing.T) {
	h, signedIn, pat, _, vlt := newTokenHandler(t)
	pat.set(patToken, goodPAT())
	postToken(h, signedIn, patToken)
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	for _, c := range signedIn.Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.Logout(rec, req)
	var body struct {
		Message  string   `json:"message"`
		Settings []string `json:"settings_urls"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusOK || !strings.Contains(body.Message, "commenting token") || len(body.Settings) != 1 || body.Settings[0] != tokensSettings {
		t.Fatalf("logout: %d %s", rec.Code, rec.Body)
	}
	if _, ok := writeStored(t, vlt); ok {
		t.Error("the token is still stored after logout")
	}
	if pat.patToOA {
		t.Error("gofer sent the token to an OAuth app endpoint")
	}
}

// A 401 on a token is confirmed with GitHub (GET /user) before the token is dropped;
// a token GitHub still accepts is kept.
func TestWriteRejectedToken(t *testing.T) {
	h, signedIn, pat, _, vlt := newTokenHandler(t)
	pat.set(patToken, goodPAT())
	postToken(h, signedIn, patToken)
	c, _ := writeStored(t, vlt)
	h.WriteRejected(bg, user, c.Version)
	if _, ok := writeStored(t, vlt); !ok {
		t.Fatal("a token GitHub still accepts was dropped")
	}
	pat.kill(patToken)
	h.WriteRejected(bg, user, c.Version)
	if _, ok := writeStored(t, vlt); ok {
		t.Error("a token GitHub rejects was kept")
	}
	if pat.patToOA {
		t.Error("gofer checked the token with an OAuth app endpoint")
	}
}

// GitHub's expiration header parses in both forms it is known to use.
func TestInspectTokenExpiryFormats(t *testing.T) {
	h, _, pat, _, _ := newTokenHandler(t)
	for _, exp := range []string{"2026-12-24 12:00:00 UTC", "2026-12-24 12:00:00 +0000"} {
		info := goodPAT()
		info.expiry = exp
		pat.set(patToken2, info)
		got, err := h.inspectToken(bg, patToken2)
		if err != nil || got.expiry.Format(time.RFC3339) != "2026-12-24T12:00:00Z" || !got.classic || strconv.Itoa(len(got.scopes)) != "1" {
			t.Errorf("%s: %+v %v", exp, got, err)
		}
	}
}
