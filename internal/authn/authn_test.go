package authn

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/identity"
	"github.com/jackfrancis/gofer/internal/principal"
	"github.com/jackfrancis/gofer/internal/session"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

func newAuth(t *testing.T) (*Authenticator, identity.Authority) {
	t.Helper()
	pub, priv, err := identity.GenerateEd25519()
	if err != nil {
		t.Fatal(err)
	}
	authority := identity.NewEd25519Authority(priv, "gofer-agent", time.Minute)
	sessions := session.NewManager([]byte("test-session-secret-0123456789ab"), false)
	// The web tier links only a verify-only identity, so it can authenticate a
	// runtime token but never mint one (ADR 0002).
	auth := New(sessions, NewIdentityValidator(identity.NewEd25519Verifier(pub, "gofer-agent")))
	return auth, authority
}

func mint(t *testing.T, a identity.Authority, scopes ...string) string {
	t.Helper()
	tok, err := a.Mint(identity.Claims{RunID: "r1", Subject: "u1", Scopes: scopes})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRequireAuthRejectsAnonymous(t *testing.T) {
	auth, _ := newAuth(t)
	rec := serve(auth.RequireAuth(okHandler()), httptest.NewRequest(http.MethodGet, "/api/me", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

// A workload (agent) token must never authenticate on the interactive user plane.
func TestRequireAuthRejectsWorkloadToken(t *testing.T) {
	auth, authority := newAuth(t)
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+mint(t, authority, "signals:read"))
	rec := serve(auth.RequireAuth(okHandler()), req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("workload token must be rejected on the user plane, got %d", rec.Code)
	}
}

func TestRequireScopeAcceptsWorkload(t *testing.T) {
	auth, authority := newAuth(t)
	var got *principal.Principal
	h := auth.RequireScope(principal.ScopeSignalsRead, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = principal.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/agent/x", nil)
	req.Header.Set("Authorization", "Bearer "+mint(t, authority, "signals:read"))
	rec := serve(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if got == nil || got.Kind != principal.KindWorkload || got.JobID != "r1" || got.ActingUserID != "u1" {
		t.Fatalf("want workload principal (JobID r1, acting u1), got %+v", got)
	}
}

func TestRequireScopeInsufficient(t *testing.T) {
	auth, authority := newAuth(t)
	req := httptest.NewRequest(http.MethodPost, "/agent/x", nil)
	req.Header.Set("Authorization", "Bearer "+mint(t, authority, "signals:read"))
	rec := serve(auth.RequireScope(principal.ScopeMetadataWrite, okHandler()), req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 for insufficient scope, got %d", rec.Code)
	}
}

// A presented-but-invalid bearer returns 401 and never falls through to a cookie.
func TestBogusBearerRejected(t *testing.T) {
	auth, _ := newAuth(t)
	req := httptest.NewRequest(http.MethodPost, "/agent/x", nil)
	req.Header.Set("Authorization", "Bearer not.a.real.token")
	rec := serve(auth.RequireScope(principal.ScopeSignalsRead, okHandler()), req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}
