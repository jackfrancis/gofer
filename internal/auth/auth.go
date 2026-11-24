// Package auth wires OAuth2 login with trusted identity providers (Google,
// GitHub, Microsoft) on top of the session manager. It retains each user's
// delegated provider token in the vault so an agent runtime can later be vended a
// short-lived credential to act on their behalf.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"
	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/microsoft"

	"github.com/jackfrancis/gofer/internal/config"
	"github.com/jackfrancis/gofer/internal/httpretry"
	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/vault"
)

// provider describes how to authenticate with a single identity provider and how
// to map its user-info response onto a session.User.
type provider struct {
	name      string
	oauth     *oauth2.Config
	userURL   string
	userAgent string // some providers (GitHub) require a User-Agent header
	mapUser   func(raw map[string]any) (*session.User, error)
	// legacyRows: a stored credential with no client recorded may be from this
	// client, so it is tagged and vended. False for GitHub under a GitHub App, which
	// never wrote such a row.
	legacyRows bool
	// grant marks a provider that is not a way to sign in but an extra grant a
	// signed-in user connects (the write grant, ADR 0009). It is never offered on the
	// sign-in page, never served by Login/Callback, and never vended by Credential.
	grant bool
}

// github reports whether p is one of gofer's GitHub clients (sign-in or the write
// grant): both are revoked at GitHub and re-checked with check-token.
func (p *provider) github() bool { return isGitHub(p.name) }

// isGitHub reports whether a provider name is one of gofer's GitHub clients.
func isGitHub(name string) bool { return name == "github" || name == WriteProvider }

// Handler exposes HTTP handlers for the OAuth login lifecycle.
type Handler struct {
	cfg           *config.Config
	sessions      *session.Manager
	providers     map[string]*provider
	client        *http.Client
	vault         vault.Vault
	githubAPIBase string // GitHub REST base URL; overridable in tests
	now           func() time.Time

	// locks serialises every write of one (user, provider) credential: the
	// callback's Put, logout, and the broker's refresh (credential.go).
	locks credLocks
	// ledger records pending logouts durably: the vault's own, when it keeps one.
	ledger vault.LogoutLedger
	// forced records the runs that have used their one forced refresh.
	forcedMu sync.Mutex
	forced   map[string]time.Time
}

// NewHandler builds the auth handler from configuration. Only providers with
// credentials configured are registered.
func NewHandler(cfg *config.Config, sessions *session.Manager, vlt vault.Vault) *Handler {
	h := &Handler{
		cfg:           cfg,
		sessions:      sessions,
		providers:     make(map[string]*provider),
		client:        loginHTTPClient(),
		vault:         vlt,
		githubAPIBase: "https://api.github.com",
		now:           time.Now,
	}
	if l, ok := vlt.(vault.LogoutLedger); ok {
		h.ledger = l
	} else {
		h.ledger = vault.NewMemoryVault() // in memory, like the vault itself
	}

	redirect := func(name string) string {
		return cfg.BaseURL + "/auth/" + name + "/callback"
	}

	if app := cfg.Providers.Google; app.Enabled() {
		h.providers["google"] = &provider{
			name:       "google",
			legacyRows: true,
			oauth: &oauth2.Config{
				ClientID:     app.ClientID,
				ClientSecret: app.ClientSecret,
				Endpoint:     google.Endpoint,
				RedirectURL:  redirect("google"),
				Scopes:       []string{"openid", "email", "profile"},
			},
			userURL: "https://openidconnect.googleapis.com/v1/userinfo",
			mapUser: func(raw map[string]any) (*session.User, error) {
				sub, _ := raw["sub"].(string)
				if sub == "" {
					return nil, fmt.Errorf("google: missing subject")
				}
				email, _ := raw["email"].(string)
				name, _ := raw["name"].(string)
				return &session.User{ID: "google:" + sub, Provider: "google", Email: email, Name: name}, nil
			},
		}
	}

	if app := cfg.Providers.GitHub; app.Enabled() {
		// GITHUB_CLIENT_KIND decides the scopes. An OAuth app keeps read:user
		// user:email, so a sign-in through it (during rollout, or after a rollback)
		// still reads hidden primary emails. A GitHub App takes none: its permissions
		// decide, and Email addresses takes user:email's place (ADR 0007).
		scopes := []string{"read:user", "user:email"}
		if cfg.Providers.IsGitHubApp() {
			scopes = nil
		}
		h.providers["github"] = &provider{
			name:       "github",
			legacyRows: !cfg.Providers.IsGitHubApp(),
			mapUser:    mapGitHubUser,
			oauth: &oauth2.Config{
				ClientID:     app.ClientID,
				ClientSecret: app.ClientSecret,
				Endpoint:     github.Endpoint,
				RedirectURL:  redirect("github"),
				Scopes:       scopes,
			},
			userURL:   "https://api.github.com/user",
			userAgent: "gofer",
		}
		// The optional write grant (ADR 0009): a separate classic OAuth app with
		// public_repo, connected by a signed-in GitHub user so gofer can post their
		// comments. Config guarantees it is a different client from sign-in.
		if w := cfg.Providers.GitHubWrite; cfg.Providers.CommentingEnabled() {
			h.providers[WriteProvider] = &provider{
				name:  WriteProvider,
				grant: true,
				oauth: &oauth2.Config{
					ClientID:     w.ClientID,
					ClientSecret: w.ClientSecret,
					Endpoint:     github.Endpoint,
					RedirectURL:  cfg.BaseURL + writePath + "/callback",
					Scopes:       []string{"public_repo"},
				},
				userURL:   "https://api.github.com/user",
				userAgent: "gofer",
				mapUser:   mapGitHubUser,
			}
		}
	}

	if app := cfg.Providers.Microsoft; app.Enabled() {
		h.providers["microsoft"] = &provider{
			name:       "microsoft",
			legacyRows: true,
			oauth: &oauth2.Config{
				ClientID:     app.ClientID,
				ClientSecret: app.ClientSecret,
				Endpoint:     microsoft.AzureADEndpoint(cfg.Providers.MicrosoftTenant),
				RedirectURL:  redirect("microsoft"),
				Scopes:       []string{"openid", "email", "profile", "User.Read"},
			},
			userURL: "https://graph.microsoft.com/v1.0/me",
			mapUser: func(raw map[string]any) (*session.User, error) {
				id, _ := raw["id"].(string)
				if id == "" {
					return nil, fmt.Errorf("microsoft: missing id")
				}
				email, _ := raw["mail"].(string)
				if email == "" {
					email, _ = raw["userPrincipalName"].(string)
				}
				name, _ := raw["displayName"].(string)
				return &session.User{ID: "microsoft:" + id, Provider: "microsoft", Email: email, Name: name}, nil
			},
		}
	}

	return h
}

// mapGitHubUser maps GitHub's /user response onto a session.User.
func mapGitHubUser(raw map[string]any) (*session.User, error) {
	id, ok := raw["id"].(float64)
	if !ok {
		return nil, fmt.Errorf("github: missing id")
	}
	email, _ := raw["email"].(string)
	login, _ := raw["login"].(string)
	name, _ := raw["name"].(string)
	if name == "" {
		name = login
	}
	return &session.User{
		ID:       "github:" + strconv.FormatInt(int64(id), 10),
		Provider: "github",
		Email:    email,
		Name:     name,
		Login:    login,
	}, nil
}

// loginHTTPClient builds the HTTP client for the OAuth token exchange and the
// provider user-info calls. Interactive login runs on a tight budget, so each
// attempt is bounded and a transient DNS/egress blip is retried.
func loginHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 4 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 4 * time.Second
	t.ResponseHeaderTimeout = 5 * time.Second
	return httpretry.WrapN(&http.Client{Timeout: 12 * time.Second, Transport: t},
		httpretry.DefaultAttempts, httpretry.DefaultBaseBackoff, httpretry.DefaultMaxBackoff)
}

// Providers returns the names of the enabled providers.
func (h *Handler) Providers() []string {
	names := make([]string, 0, len(h.providers))
	for name, p := range h.providers {
		if p.grant {
			continue // not a way to sign in
		}
		names = append(names, name)
	}
	return names
}

// Login starts the OAuth flow for the provider named in the path value
// "provider", redirecting the user to the provider's consent screen.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	p, ok := h.providers[r.PathValue("provider")]
	if !ok || p.grant {
		http.Error(w, "unknown provider", http.StatusNotFound)
		return
	}

	state := session.NewToken()
	verifier := oauth2.GenerateVerifier()
	h.sessions.StartOAuth(w, &session.OAuthFlow{
		Provider: p.name,
		State:    state,
		Verifier: verifier,
	})

	url := p.oauth.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.S256ChallengeOption(verifier),
	)
	http.Redirect(w, r, url, http.StatusFound)
}

// Callback completes the OAuth flow: it validates state, exchanges the code,
// fetches the user profile, and establishes an authenticated session.
func (h *Handler) Callback(w http.ResponseWriter, r *http.Request) {
	p, ok := h.providers[r.PathValue("provider")]
	if !ok || p.grant {
		http.Error(w, "unknown provider", http.StatusNotFound)
		return
	}

	flow := h.sessions.OAuthFlow(r)
	if flow == nil || flow.Provider != p.name {
		http.Error(w, "no login in progress", http.StatusBadRequest)
		return
	}
	if state := r.URL.Query().Get("state"); state == "" || state != flow.State {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing authorization code", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, h.client)

	token, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(flow.Verifier))
	if err != nil {
		slog.Default().Warn("oauth token exchange failed", "provider", p.name, "err", err)
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}

	raw, err := h.fetchUser(ctx, p, token)
	if err != nil {
		slog.Default().Warn("oauth user fetch failed", "provider", p.name, "err", err)
		http.Error(w, "failed to fetch user profile", http.StatusBadGateway)
		return
	}
	user, err := p.mapUser(raw)
	if err != nil {
		slog.Default().Warn("oauth user profile invalid", "provider", p.name, "err", err)
		http.Error(w, "invalid user profile", http.StatusBadGateway)
		return
	}

	// The sign-in allow-list (ALLOWED_GITHUB_LOGINS). A refused account gets no
	// session and nothing in the vault, and the grant GitHub just gave this client
	// is revoked (the whole grant, so a refresh token dies too), so gofer is left
	// holding no credential of someone it refused.
	if h.cfg != nil && !h.cfg.SignInAllowed(user.Provider, user.Login) {
		slog.Default().Warn("sign-in refused: account not in ALLOWED_GITHUB_LOGINS",
			"provider", p.name, "login", user.Login, "user", user.ID)
		if p.name == "github" && token.AccessToken != "" {
			if rerr := h.revokeGrant(ctx, p, token.AccessToken); rerr != nil {
				slog.Default().Warn("revoking a refused account's github grant failed", "user", user.ID, "err", rerr)
			}
		}
		h.sessions.Destroy(w, r) // drop the pre-auth flow
		http.Error(w, "This account is not allowed to sign in to this gofer.", http.StatusForbidden)
		return
	}

	// Retain the delegated provider token in the vault so agent runtimes can be
	// vended a credential to act on the user's behalf, under the credential lock and
	// after re-checking it with GitHub (storeSignIn). A grant revoked mid-flow (a
	// logout in another tab) stores nothing and asks the user to sign in again, so
	// they consent again.
	if h.vault != nil {
		restart, err := h.storeSignIn(ctx, p, user.ID, user.Login, token)
		if err != nil {
			slog.Default().Warn("could not verify the new token", "provider", p.name, "user", user.ID, "err", err)
			http.Error(w, "could not verify the sign-in with "+p.name, http.StatusBadGateway)
			return
		}
		if restart {
			// Not an automatic redirect: if GitHub ever answered check-token "invalid"
			// for a good token, that would loop. The user restarts sign-in themselves.
			slog.Default().Info("sign-in token was revoked mid-flow; asking the user to sign in again", "provider", p.name, "user", user.ID)
			h.sessions.Destroy(w, r) // drop the pre-auth flow
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>gofer</title>`+
				`<p>gofer's access was revoked while you were signing in, so the sign-in did not complete.</p>`+
				`<p><a href="/auth/`+p.name+`/login">Sign in again</a></p>`)
			return
		}
	}

	if err := h.sessions.Authenticate(w, r, user); err != nil {
		slog.Default().Error("session could not be stored", "provider", p.name, "err", err)
		http.Error(w, "could not start session", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// Logout ends the interactive session and discards the delegated provider
// credential the login left in the vault. For GitHub it first revokes the grant at
// GitHub, refreshing the token first if it has expired or is about to (see
// revokeAndDelete). The pending logout and the end of the session are committed
// together first; if that commit fails, logout answers 503 and changes nothing.
// After it, logout is never blocked: the credential is revoked and deleted, now or
// in the background.
//
// It answers 204 when revocation is confirmed (or there was nothing to revoke).
// When it is not — no stored GitHub credential, a dead refresh token, a failed
// revoke call, or a credential from another client — it answers 200 with a JSON body
// that says so and links the user to the GitHub settings page(s) where they can
// revoke gofer themselves (settings_urls; settings_url is the first).
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	res := logoutResult{confirmed: true}
	// Find the owner on a deadline of its own: a browser that disconnects must not
	// hide whose credential to revoke.
	lctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	// LookupOwner, not CurrentUser: an account dropped from ALLOWED_GITHUB_LOGINS
	// gets no interactive access, but must still be able to revoke its grant.
	u, lerr := h.sessions.LookupOwner(r.WithContext(lctx))
	cancel()
	if lerr != nil {
		// The store could not say whose session this is. Ending it now could leave
		// that user's credential vendable: change nothing, and say so.
		slog.ErrorContext(r.Context(), "logout: looking up the session failed; the session stands", "err", lerr)
		writeLogoutError(w)
		return
	}
	// Record the pending logout and end the session together (one commit with the
	// SQLite vault), again detached from the browser. If that fails nothing changed:
	// the session stands, nothing is revoked, and the user is told to try again.
	var lo, writeLo *localLogout
	cctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	err := h.sessions.EndWith(w, r.WithContext(cctx), func(ctx context.Context, sessionKey string) error {
		if u == nil || h.vault == nil {
			return nil
		}
		var err error
		lo, writeLo, err = h.beginSessionLogout(ctx, sessionKey, u.ID, u.Provider)
		return err
	})
	cancel()
	switch {
	case errors.Is(err, session.ErrNotFound):
		// Another logout of this same session consumed it first; that one owns the
		// revocation. Clear this browser's cookie and record nothing.
		dctx, dcancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		h.sessions.Destroy(w, r.WithContext(dctx))
		dcancel()
		res = h.unconfirmed(u.Provider, h.providers[u.Provider], nil)
	case err != nil:
		slog.ErrorContext(r.Context(), "logout could not be recorded; the session stands", "user", u.ID, "err", err)
		writeLogoutError(w)
		return
	}
	if lo != nil {
		res = h.revokeAndDelete(r.Context(), u.ID, u.Provider, lo)
		// The write grant (ADR 0009) goes with the session that connected it; its
		// pending logout was recorded in the same commit.
		if writeLo != nil {
			res = mergeLogout(res, h.revokeAndDelete(r.Context(), u.ID, WriteProvider, writeLo))
		}
	}
	if res.confirmed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"revocation":    "unconfirmed",
		"message":       "You are signed out of gofer, but GitHub did not confirm that gofer's access was revoked. Revoke gofer in GitHub's settings to be sure.",
		"settings_url":  res.settings[0],
		"settings_urls": res.settings,
	})
}

// writeLogoutError answers a logout that changed nothing.
func writeLogoutError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": "gofer could not sign you out right now; you are still signed in. Try again.",
	})
}

// fetchUser calls the provider's user-info endpoint with the access token.
func (h *Handler) fetchUser(ctx context.Context, p *provider, token *oauth2.Token) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.userURL, nil)
	if err != nil {
		return nil, err
	}
	token.SetAuthHeader(req)
	req.Header.Set("Accept", "application/json")
	if p.userAgent != "" {
		req.Header.Set("User-Agent", p.userAgent)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s userinfo: status %d", p.name, resp.StatusCode)
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	// GitHub may omit the email from /user when it is private; fall back to the
	// dedicated emails endpoint to find the primary verified address.
	if p.name == "github" {
		if email, _ := raw["email"].(string); email == "" {
			if email := h.githubPrimaryEmail(ctx, token); email != "" {
				raw["email"] = email
			}
		}
	}
	return raw, nil
}

// githubPrimaryEmail looks up the user's primary, verified email address.
func (h *Handler) githubPrimaryEmail(ctx context.Context, token *oauth2.Token) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user/emails", nil)
	if err != nil {
		return ""
	}
	token.SetAuthHeader(req)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "gofer")

	resp, err := h.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(body, &emails); err != nil {
		return ""
	}
	for _, e := range emails {
		if e.Primary && e.Verified && !strings.HasSuffix(e.Email, "@users.noreply.github.com") {
			return e.Email
		}
	}
	return ""
}
