package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/agent"
	"github.com/jackfrancis/gofer/internal/auth"
	"github.com/jackfrancis/gofer/internal/principal"
	"github.com/jackfrancis/gofer/internal/vault"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// fakeSource records what the broker asked for and answers with cred or err.
type fakeSource struct {
	cred           vault.Credential
	err            error
	user, provider string
	req            auth.VendRequest
}

func (f *fakeSource) Credential(_ context.Context, userID, provider string, req auth.VendRequest) (vault.Credential, error) {
	f.user, f.provider, f.req = userID, provider, req
	return f.cred, f.err
}

// The broker vends through the credential source: the acting user, the run's ID and
// any rejected version go to it, and the answer carries the token and the version
// but never the refresh token.
func TestCredentialVendsThroughSource(t *testing.T) {
	src := &fakeSource{cred: vault.Credential{
		Provider: "github", AccessToken: "ghu_new", RefreshToken: "ghr_secret", TokenType: "bearer",
		Expiry: time.Now().Add(8 * time.Hour), Version: "v2",
	}}
	h := NewCredentialHandler(src, "")
	rec := httptest.NewRecorder()
	h.Vend(rec, authedGet("/agent/credential?provider=github&rejected=v1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if src.user != "u1" || src.provider != "github" || src.req != (auth.VendRequest{Rejected: "v1", RunID: "run1"}) {
		t.Fatalf("source asked for user=%q provider=%q req=%+v", src.user, src.provider, src.req)
	}
	var out map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["access_token"] != "ghu_new" || out["credential_version"] != "v2" {
		t.Errorf("answer = %v", out)
	}
	if secs, _ := out["expires_in"].(float64); secs < 7*3600 {
		t.Errorf("expires_in = %v", out["expires_in"])
	}
	if _, leaked := out["refresh_token"]; leaked {
		t.Error("the refresh token left the web tier")
	}
}

// "Sign-in required" has its own status and code; any other failure is a 502.
func TestCredentialSignInRequired(t *testing.T) {
	rec := httptest.NewRecorder()
	NewCredentialHandler(&fakeSource{err: vault.ErrSignInRequired}, "").Vend(rec, authedGet("/agent/credential"))
	if rec.Code != StatusSignInRequired {
		t.Fatalf("status = %d, want %d", rec.Code, StatusSignInRequired)
	}
	var out map[string]string
	_ = json.NewDecoder(rec.Body).Decode(&out)
	if out["code"] != CodeSignInRequired {
		t.Errorf("code = %q", out["code"])
	}

	rec = httptest.NewRecorder()
	NewCredentialHandler(&fakeSource{err: errors.New("github down")}, "").Vend(rec, authedGet("/agent/credential"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("transient failure status = %d, want 502", rec.Code)
	}
}

// The broker vends the single shared chat-model token as provider "ai"; every model
// on the endpoint uses it (the model id travels as a run parameter, not the token).
func TestCredentialVendsSharedAIToken(t *testing.T) {
	h := NewCredentialHandler(&fakeSource{err: vault.ErrNotFound}, "tok-ai")

	if got := vendToken(t, h, "ai"); got != "tok-ai" {
		t.Errorf("provider ai -> %q, want tok-ai", got)
	}

	// Named ai:<name> providers are no longer brokered (one token per endpoint) -> 404.
	rec := httptest.NewRecorder()
	h.Vend(rec, authedGet("/agent/credential?provider=ai:second"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("ai:second status = %d, want 404", rec.Code)
	}

	// No token configured -> provider ai 404s.
	off := NewCredentialHandler(&fakeSource{}, "")
	rec2 := httptest.NewRecorder()
	off.Vend(rec2, authedGet("/agent/credential?provider=ai"))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("disabled ai status = %d, want 404", rec2.Code)
	}
}

func vendToken(t *testing.T, h *CredentialHandler, provider string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Vend(rec, authedGet("/agent/credential?provider="+provider))
	if rec.Code != http.StatusOK {
		t.Fatalf("provider %q status = %d", provider, rec.Code)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&out)
	return out.AccessToken
}

func authedGet(target string) *http.Request {
	p := &principal.Principal{
		Kind:         principal.KindWorkload,
		Subject:      "u1",
		ActingUserID: "u1",
		JobID:        "run1",
		Scopes:       []principal.Scope{principal.ScopeSignalsRead},
	}
	return httptest.NewRequest(http.MethodGet, target, nil).WithContext(principal.NewContext(context.Background(), p))
}

// The runtime's agent-plane client and this broker agree on "sign-in required" (the
// workload repeats the status and code rather than import the web tier), and on the
// credential version a run names in rejected=.
func TestCredentialContractWithRuntime(t *testing.T) {
	src := &fakeSource{err: vault.ErrSignInRequired}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		NewCredentialHandler(src, "").Vend(w, authedGet(r.URL.String()))
	}))
	defer srv.Close()
	plane := agent.NewPlaneClient(srv.URL, "run-cred", srv.Client())
	_, err := plane.VendCredential(context.Background(), "github", "")
	var re *agent.ReconnectError
	if !errors.As(err, &re) || !errors.Is(err, worklist.ErrSignInRequired) {
		t.Fatalf("runtime saw %v, want a ReconnectError", err)
	}

	src.err, src.cred = nil, vault.Credential{AccessToken: "ghu_x", TokenType: "bearer", Version: "v9"}
	vc, err := plane.VendCredential(context.Background(), "github", "v8")
	if err != nil || vc.AccessToken != "ghu_x" || vc.Version != "v9" || src.req.Rejected != "v8" {
		t.Fatalf("vend = %+v, %v; broker got %+v", vc, err, src.req)
	}
}

// The write grant (ADR 0009) never reaches a run: the broker refuses it by name,
// without even asking the source, which would answer anything.
func TestCredentialNeverVendsWriteGrant(t *testing.T) {
	src := &fakeSource{cred: vault.Credential{Provider: auth.WriteProvider, AccessToken: "gho_write"}}
	rec := httptest.NewRecorder()
	NewCredentialHandler(src, "").Vend(rec, authedGet("/agent/credential?provider="+auth.WriteProvider))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if src.provider != "" {
		t.Errorf("the broker asked the source for %q", src.provider)
	}
	if body := rec.Body.String(); strings.Contains(body, "gho_write") {
		t.Errorf("the write token left the web tier: %s", body)
	}
}
