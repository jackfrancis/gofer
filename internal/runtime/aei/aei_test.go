package aei

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/agent-execution-interface/sdks/go/aeiapp"

	"github.com/jackfrancis/gofer/internal/ingest"
)

// dispatchBody is the wire shape the app SDK POSTs to the control plane: the app plus
// the run spec. The tests assert on it directly, because it is the contract gofer's
// run intent has to survive translation into.
type dispatchBody struct {
	App        string            `json:"app"`
	TaskRef    string            `json:"taskRef"`
	Parameters map[string]string `json:"parameters"`
	Identity   struct {
		Subject string   `json:"subject"`
		Scopes  []string `json:"scopes"`
	} `json:"identity"`
	TimeoutSeconds int64 `json:"timeoutSeconds"`
}

// controlPlane is a fake aei-controller dispatch API: it records what gofer dispatched
// and answers run lookups with a phase the test chooses.
type controlPlane struct {
	srv    *httptest.Server
	got    dispatchBody
	raw    []byte
	bearer string
	phase  string
	msg    string
	runs   int
}

func newControlPlane(t *testing.T) *controlPlane {
	t.Helper()
	c := &controlPlane{phase: "Running"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /aei/v1alpha1/dispatch", func(w http.ResponseWriter, r *http.Request) {
		c.bearer = r.Header.Get("Authorization")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.raw = body
		if err := json.Unmarshal(body, &c.got); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.runs++
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"runId": "run-1"})
	})
	mux.HandleFunc("GET /aei/v1alpha1/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"phase": c.phase, "message": c.msg})
	})
	c.srv = httptest.NewServer(mux)
	t.Cleanup(c.srv.Close)
	return c
}

// backend builds the dispatcher against the fake control plane.
func backend(t *testing.T, c *controlPlane) *Dispatcher {
	t.Helper()
	d, err := New(Config{Endpoint: c.srv.URL, App: "gofer", Token: "sa-token"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

// A run intent gofer builds must reach the control plane whole: the app it dispatches
// as, the workload, its parameters, the delegated identity, and the deadline.
func TestSubmitTranslatesTheRunIntent(t *testing.T) {
	c := newControlPlane(t)
	d := backend(t, c)

	runID, err := d.Submit(context.Background(), ingest.RunSpec{
		TaskRef:    "github-ingest",
		Parameters: map[string]string{"owner": "github:1", "gofer_url": "http://gofer:8080"},
		Subject:    "github:1",
		Scopes:     []string{"signals:read", "metadata:write"},
		Audience:   "gofer-agent",
		Deadline:   10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if runID != "run-1" {
		t.Fatalf("run id = %q, want run-1", runID)
	}
	if c.got.App != "gofer" {
		t.Errorf("app = %q, want gofer", c.got.App)
	}
	if c.got.TaskRef != "github-ingest" {
		t.Errorf("task ref = %q, want github-ingest", c.got.TaskRef)
	}
	if c.got.Parameters["owner"] != "github:1" || c.got.Parameters["gofer_url"] != "http://gofer:8080" {
		t.Errorf("parameters = %v", c.got.Parameters)
	}
	if c.got.Identity.Subject != "github:1" {
		t.Errorf("subject = %q, want github:1", c.got.Identity.Subject)
	}
	if len(c.got.Identity.Scopes) != 2 || c.got.Identity.Scopes[0] != "signals:read" {
		t.Errorf("scopes = %v", c.got.Identity.Scopes)
	}
	if c.got.TimeoutSeconds != 600 {
		t.Errorf("timeout = %ds, want 600", c.got.TimeoutSeconds)
	}
	// The caller authenticates as itself; in the cluster this is the web pod's
	// projected ServiceAccount token, which the controller TokenReviews.
	if c.bearer != "Bearer sa-token" {
		t.Errorf("authorization = %q", c.bearer)
	}
}

// The audience is NOT requested: the AgentApp fixes it server-side, so a dispatching
// app cannot widen the credential it will be handed.
func TestSubmitDoesNotRequestAnAudience(t *testing.T) {
	c := newControlPlane(t)
	d := backend(t, c)

	if _, err := d.Submit(context.Background(), ingest.RunSpec{TaskRef: "github-ingest", Audience: "gofer-agent"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if bytes.Contains(c.raw, []byte("audience")) || bytes.Contains(c.raw, []byte("gofer-agent")) {
		t.Errorf("dispatch body carries an audience; the AgentApp fixes it: %s", c.raw)
	}
}

// Run lifecycle comes back from the control plane, so a failed run can be surfaced in
// the web tier instead of leaving the worklist spinning.
func TestStatusReportsThePhase(t *testing.T) {
	c := newControlPlane(t)
	d := backend(t, c)
	c.phase, c.msg = "Failed", "vend github credential: status 401"

	st, err := d.Status(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Phase != "Failed" || st.Message != "vend github credential: status 401" {
		t.Fatalf("status = %+v", st)
	}
}

// A backend that cannot reach a control plane is a configuration error, not a runtime
// surprise: New fails fast rather than accepting runs it can never dispatch.
func TestNewRequiresEndpointAndApp(t *testing.T) {
	t.Setenv(aeiapp.TokenPathEnv, "")
	t.Setenv("AEI_DISPATCH_ENDPOINT", "")
	t.Setenv("AEI_APP", "")
	if _, err := New(Config{App: "gofer"}); err == nil {
		t.Error("New with no dispatch endpoint: want error")
	}
	if _, err := New(Config{Endpoint: "http://aei-controller:8080"}); err == nil {
		t.Error("New with no app: want error")
	}
}

// The backend owns its own configuration, so it reads its environment itself and
// gofer's wiring point names none of it.
func TestNewReadsItsOwnEnvironment(t *testing.T) {
	t.Setenv(aeiapp.TokenPathEnv, "")
	t.Setenv("AEI_DISPATCH_ENDPOINT", "http://aei-controller.aei-system.svc:8081")
	t.Setenv("AEI_APP", "gofer")
	if _, err := New(Config{}); err != nil {
		t.Fatalf("New: %v", err)
	}
}

// writeToken writes a token file the way the kubelet projects one.
func writeToken(t *testing.T, path, token string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// dispatchOnce submits a run and returns the bearer the control plane saw.
func dispatchOnce(t *testing.T, d *Dispatcher, c *controlPlane) string {
	t.Helper()
	if _, err := d.Submit(context.Background(), ingest.RunSpec{TaskRef: "github-ingest"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return c.bearer
}

// gofer dispatches with the token projected for AEI's dispatch audience, read from the
// configured path on every request: the kubelet rotates the projected token in place,
// and a cached one would expire under a long-lived web pod.
func TestDispatchUsesTheProjectedTokenAndItsRotation(t *testing.T) {
	t.Setenv(aeiapp.TokenPathEnv, "")
	c := newControlPlane(t)
	path := filepath.Join(t.TempDir(), "token")
	writeToken(t, path, "dispatch-token-1")
	d, err := New(Config{Endpoint: c.srv.URL, App: "gofer", TokenPath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := dispatchOnce(t, d, c); got != "Bearer dispatch-token-1" {
		t.Errorf("bearer = %q, want the projected token", got)
	}
	writeToken(t, path, "dispatch-token-2")
	if got := dispatchOnce(t, d, c); got != "Bearer dispatch-token-2" {
		t.Errorf("bearer after rotation = %q, want the rotated token", got)
	}
}

// The deployment configures the path through the environment (gofer-config), which the
// backend reads itself.
func TestTokenPathFromEnvironment(t *testing.T) {
	c := newControlPlane(t)
	path := filepath.Join(t.TempDir(), "token")
	writeToken(t, path, "from-env")
	t.Setenv(aeiapp.TokenPathEnv, path)
	d, err := New(Config{Endpoint: c.srv.URL, App: "gofer"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := dispatchOnce(t, d, c); got != "Bearer from-env" {
		t.Errorf("bearer = %q, want the token at %s", got, aeiapp.TokenPathEnv)
	}
}

// An explicit token (tests, out-of-cluster runs) wins over any configured path.
func TestExplicitTokenWinsOverTokenPath(t *testing.T) {
	c := newControlPlane(t)
	t.Setenv(aeiapp.TokenPathEnv, filepath.Join(t.TempDir(), "absent"))
	d, err := New(Config{Endpoint: c.srv.URL, App: "gofer", Token: "explicit"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := dispatchOnce(t, d, c); got != "Bearer explicit" {
		t.Errorf("bearer = %q, want the explicit token", got)
	}
}

// A configured token path that holds no token (the volume is not mounted, or names the
// wrong file) is a misconfiguration every dispatch would hit, so New refuses it, and
// its error names the path without quoting any token.
func TestNewRefusesAnUnusableTokenPath(t *testing.T) {
	t.Setenv(aeiapp.TokenPathEnv, "")
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	writeToken(t, empty, "  ")
	for name, path := range map[string]string{
		"missing": filepath.Join(dir, "missing"),
		"empty":   empty,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New(Config{Endpoint: "http://aei-controller:8081", App: "gofer", TokenPath: path})
			if err == nil {
				t.Fatal("New: want error")
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error %q does not name the path", err)
			}
		})
	}
	t.Run("from environment", func(t *testing.T) {
		t.Setenv(aeiapp.TokenPathEnv, filepath.Join(dir, "missing"))
		if _, err := New(Config{Endpoint: "http://aei-controller:8081", App: "gofer"}); err == nil {
			t.Fatal("New: want error")
		}
	})
}
