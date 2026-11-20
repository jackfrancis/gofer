package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackfrancis/gofer/internal/config"
	"github.com/jackfrancis/gofer/internal/identity"
	"github.com/jackfrancis/gofer/internal/ingest"
	"github.com/jackfrancis/gofer/internal/vault"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// gatewayRouter builds gofer's real router with the given chat-model connection and
// returns it with a minter for run credentials the router accepts.
func gatewayRouter(t *testing.T, conn config.AIConnection) (http.Handler, identity.Minter) {
	t.Helper()
	pub, priv, err := identity.GenerateEd25519()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		SessionSecret:       []byte(strings.Repeat("s", 32)),
		Audience:            "gofer-agent",
		MintPublicKey:       pub,
		ConversationEnabled: true,
		AIConnections:       []config.AIConnection{conn},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, _, err := New(cfg, log, ingest.NoopDispatcher{}, vault.NewMemoryVault(), worklist.NewMemoryStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return h, identity.NewEd25519Authority(priv, "gofer-agent", 0)
}

func call(t *testing.T, h http.Handler, method, path, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(`{"model":"m"}`))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The gateway asks gofer whether a model call may proceed. A run credential with the
// broker's scope is allowed; anything else is refused. And the broker has no model
// key to hand out: in this profile the gateway alone holds it.
func TestModelAccessBehindTheGateway(t *testing.T) {
	h, minter := gatewayRouter(t, config.AIConnection{
		Endpoint: "http://gw/v1/chat/completions", Models: []string{"a", "b"}, Gateway: true, ID: "x",
	})
	run, err := minter.Mint(identity.Claims{RunID: "r1", Subject: "github:1", Scopes: []string{"signals:read", "metadata:write"}})
	if err != nil {
		t.Fatal(err)
	}
	noScope, _ := minter.Mint(identity.Claims{RunID: "r2", Subject: "github:1", Scopes: []string{"metadata:write"}})
	otherAud, _ := minter.Mint(identity.Claims{RunID: "r3", Subject: "github:1", Scopes: []string{"signals:read"}, Audience: "someone-else"})

	for _, method := range []string{http.MethodPost, http.MethodGet} {
		if rec := call(t, h, method, "/agent/model-access", run); rec.Code != http.StatusNoContent {
			t.Errorf("%s with a run credential: %d %s, want 204", method, rec.Code, rec.Body)
		}
	}
	for name, bearer := range map[string]string{"no bearer": "", "garbage": "x.y", "missing scope": noScope, "other audience": otherAud} {
		if rec := call(t, h, http.MethodPost, "/agent/model-access", bearer); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 401 or 403", name, rec.Code)
		}
	}
	rec := call(t, h, http.MethodGet, "/agent/credential?provider=ai", run)
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "access_token") {
		t.Fatalf("vend ai behind the gateway: %d %s, want 404 with no token", rec.Code, rec.Body)
	}
}

// Without the gateway profile the authorization route does not exist, and the broker
// vends the model token as before.
func TestModelAccessAbsentWithoutTheGateway(t *testing.T) {
	h, minter := gatewayRouter(t, config.AIConnection{
		Endpoint: "https://api.example/chat/completions", Models: []string{"a"}, Token: "upstream-key", ID: "x",
	})
	run, _ := minter.Mint(identity.Claims{RunID: "r1", Subject: "github:1", Scopes: []string{"signals:read"}})
	if rec := call(t, h, http.MethodPost, "/agent/model-access", run); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("model-access without the gateway: %d, want it unrouted", rec.Code)
	}
	if rec := call(t, h, http.MethodGet, "/agent/credential?provider=ai", run); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "upstream-key") {
		t.Errorf("vend ai without the gateway: %d %s, want the brokered token", rec.Code, rec.Body)
	}
}
