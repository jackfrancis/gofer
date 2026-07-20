// Package authn resolves a principal.Principal for each request from either an
// interactive session cookie or a workload bearer token, and provides
// authorization middleware (RequireAuth, RequireScope).
package authn

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jackfrancis/gofer/internal/identity"
	"github.com/jackfrancis/gofer/internal/principal"
	"github.com/jackfrancis/gofer/internal/session"
)

// ErrNoToken indicates a bearer token could not be validated.
var ErrNoToken = errors.New("authn: no valid token")

// TokenValidator validates a workload bearer token and returns its principal. It
// is the seam for gofer-issued, short-lived runtime tokens.
type TokenValidator interface {
	Validate(r *http.Request, token string) (*principal.Principal, error)
}

// noTokenValidator rejects every token; the default when none is wired.
type noTokenValidator struct{}

func (noTokenValidator) Validate(*http.Request, string) (*principal.Principal, error) {
	return nil, ErrNoToken
}

// identityValidator authenticates a workload bearer as a gofer-minted job token
// via the verify-only identity seam (ADR 0002): it can authenticate a runtime
// token but, holding only a public key, can never mint one. The web tier links
// this, never a Minter.
type identityValidator struct {
	v identity.Verifier
}

// NewIdentityValidator builds a TokenValidator over a verify-only identity. It
// maps the token's claims onto a workload principal (the run id becomes JobID,
// the acting subject becomes ActingUserID, the scopes carry through).
func NewIdentityValidator(v identity.Verifier) TokenValidator {
	return identityValidator{v: v}
}

func (iv identityValidator) Validate(_ *http.Request, token string) (*principal.Principal, error) {
	c, ok := iv.v.Verify(token)
	if !ok {
		return nil, ErrNoToken
	}
	scopes := make([]principal.Scope, len(c.Scopes))
	for i, s := range c.Scopes {
		scopes[i] = principal.Scope(s)
	}
	return &principal.Principal{
		Kind:         principal.KindWorkload,
		Subject:      c.Subject,
		ActingUserID: c.Subject,
		JobID:        c.RunID,
		Scopes:       scopes,
	}, nil
}

// Authenticator resolves principals and enforces authorization.
type Authenticator struct {
	sessions *session.Manager
	tokens   TokenValidator
}

// New builds an Authenticator. If tokens is nil, bearer tokens are rejected
// (cookie sessions still work).
func New(sessions *session.Manager, tokens TokenValidator) *Authenticator {
	if tokens == nil {
		tokens = noTokenValidator{}
	}
	return &Authenticator{sessions: sessions, tokens: tokens}
}

// resolve returns the principal for a request, or nil if unauthenticated.
func (a *Authenticator) resolve(r *http.Request) *principal.Principal {
	// Workload path: a presented bearer token is authoritative. An invalid token
	// must not silently fall through to the cookie session.
	if tok := bearerToken(r); tok != "" {
		p, err := a.tokens.Validate(r, tok)
		if err != nil || p == nil {
			return nil
		}
		return p
	}

	// Interactive path: OAuth session cookie. Users act on their own data with
	// full scope within their namespace.
	if u := a.sessions.CurrentUser(r); u != nil {
		return &principal.Principal{
			Kind:         principal.KindUser,
			Subject:      u.ID,
			ActingUserID: u.ID,
			Scopes:       []principal.Scope{principal.ScopeAll},
		}
	}
	return nil
}

// RequireAuth gates the interactive user plane (the /api/* routes). It accepts
// only an interactive user principal (an OAuth session); a workload/agent token is
// rejected here so a runtime credential minted for the agent plane cannot be
// replayed on the user API. It injects the principal into the request context.
func (a *Authenticator) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := a.resolve(r)
		if p == nil || p.Kind != principal.KindUser {
			writeJSONError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next.ServeHTTP(w, r.WithContext(principal.NewContext(r.Context(), p)))
	})
}

// RequireScope gates the agent plane. It accepts only a workload principal — a
// runtime authenticated by an agent-audience job token — that holds the given
// scope. An interactive session is rejected: the agent plane is for runtimes, not
// browsers. It injects the principal into the request context.
func (a *Authenticator) RequireScope(s principal.Scope, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := a.resolve(r)
		if p == nil {
			writeJSONError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if p.Kind != principal.KindWorkload {
			writeJSONError(w, http.StatusForbidden, "this endpoint is for agent runtimes")
			return
		}
		if !p.HasScope(s) {
			writeJSONError(w, http.StatusForbidden, "insufficient scope")
			return
		}
		next.ServeHTTP(w, r.WithContext(principal.NewContext(r.Context(), p)))
	})
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header.
// Tokens are accepted from this header only — never query strings.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"` + msg + `"}`))
}
