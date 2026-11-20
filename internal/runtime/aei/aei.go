// Package aei is a concrete agent-runtime backend for gofer, built on the Agent
// Execution Interface (github.com/jackfrancis/agent-execution-interface).
//
// It implements ingest.Dispatcher, the seam gofer's web tier dispatches through (see
// internal/runtime for the full backend contract). gofer holds no dispatch engine of
// its own: it is an AEI *app* that POSTs each run to the pre-installed AEI control
// plane (the aei-controller dispatch API) through the app SDK and reads the run's
// lifecycle back. It imports no provider and no client-go, and creates no per-run
// Kubernetes object — the caller authenticates with a ServiceAccount token projected
// into the web pod for AEI's dispatch audience ("aei-dispatch"), bounded by gofer's
// AgentApp policy (dispatchers, maxScopes, audience).
//
// The substrate is OUT-OF-PROCESS: the control plane launches gofer's runtime image
// (cmd/runtime, whose workload lives in this package — see workload.go) on whatever
// provider the app's class names, and that runtime reaches gofer through the /agent/*
// plane with the run credential. The control plane is the sole minter of that
// credential; gofer's web tier holds only the public half and verifies it, so a
// web-tier compromise cannot forge a run credential.
//
// Nothing in gofer changes to select this backend: it consumes gofer's existing seams
// (ingest.RunSpec, agent.Run, agent.Vendor/Sink) and is wired by a single assignment
// in cmd/server.
package aei

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackfrancis/agent-execution-interface/sdks/go/aeiapp"

	"github.com/jackfrancis/gofer/internal/ingest"
)

// Config wires the backend to the AEI control plane. Every field has an environment
// fallback so the backend owns its own configuration and gofer's wiring point names
// none of it.
type Config struct {
	// Endpoint is the AEI dispatch API base URL: the aei-controller's dispatch
	// listener, e.g. "http://aei-controller.aei-system.svc:8081". Empty falls back to
	// AEI_DISPATCH_ENDPOINT.
	Endpoint string
	// App is the AgentApp gofer dispatches as. The control plane bounds every run by
	// that app's policy (permitted dispatchers, scope ceiling) and fixes the minted
	// credential's audience. Empty falls back to AEI_APP.
	App string
	// Token overrides the dispatch bearer (tests, out-of-cluster runs) and wins over
	// TokenPath.
	Token string
	// TokenPath is the file the dispatch bearer is read from: a ServiceAccount token
	// projected for AEI's dispatch audience, which the controller TokenReviews — the
	// production path, with no shared secret. The SDK re-reads it on every request,
	// so the kubelet's in-place rotation is picked up. Empty falls back to
	// AEI_DISPATCH_TOKEN_PATH; with neither, the SDK sends the pod's default
	// ServiceAccount token, which AEI admits only while it still accepts legacy
	// dispatch tokens.
	TokenPath string
	// HTTPClient overrides the transport (tests); nil uses a client bounded by
	// dispatchTimeout.
	HTTPClient *http.Client
	// Logger records dispatch lifecycle; nil uses slog.Default().
	Logger *slog.Logger
}

// dispatchTimeout bounds one call to the control plane. Dispatch is fire-and-forget —
// the controller accepts the run and answers immediately — so this only guards against
// a hung control plane, never the run itself.
const dispatchTimeout = 30 * time.Second

// Dispatcher submits gofer's runs to the AEI control plane and reads their lifecycle.
// It is a thin adapter over the app SDK: gofer embeds no control plane and no launcher.
type Dispatcher struct {
	client *aeiapp.Client
	app    string
	log    *slog.Logger
}

var _ ingest.Dispatcher = (*Dispatcher)(nil)

// New builds the backend. It fails fast when a run could not possibly be dispatched.
func New(cfg Config) (*Dispatcher, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = strings.TrimSpace(os.Getenv("AEI_DISPATCH_ENDPOINT"))
	}
	if endpoint == "" {
		return nil, errors.New("aei: no dispatch endpoint (set AEI_DISPATCH_ENDPOINT to the aei-controller)")
	}
	app := strings.TrimSpace(cfg.App)
	if app == "" {
		app = strings.TrimSpace(os.Getenv("AEI_APP"))
	}
	if app == "" {
		return nil, errors.New("aei: no app name (set AEI_APP to gofer's AgentApp)")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: dispatchTimeout}
	}
	// Watch each dispatch's exchange (see observingTransport) on a copy, so a caller's
	// client is never changed.
	watched := *client
	watched.Transport = observingTransport{base: client.Transport}
	opts := []aeiapp.Option{aeiapp.WithHTTPClient(&watched)}
	tokenSource := "default ServiceAccount token"
	switch tokenPath := resolveTokenPath(cfg.TokenPath); {
	case cfg.Token != "":
		opts = append(opts, aeiapp.WithToken(cfg.Token))
		tokenSource = "explicit token"
	case tokenPath != "":
		// A projected volume is mounted before the container starts, so a path that
		// cannot be read now is a misconfiguration every dispatch would hit. Fail at
		// startup rather than accept runs that can never be dispatched.
		if err := checkTokenFile(tokenPath); err != nil {
			return nil, err
		}
		opts = append(opts, aeiapp.WithTokenPath(tokenPath))
		tokenSource = tokenPath
	}

	log.Info("agent runtime backend ready", "backend", "aei", "dispatch", endpoint, "app", app, "token", tokenSource)
	return &Dispatcher{client: aeiapp.New(endpoint, app, opts...), app: app, log: log}, nil
}

// resolveTokenPath applies the AEI_DISPATCH_TOKEN_PATH fallback (the variable the app
// SDK reads too) so the backend can check and log the path it will use.
func resolveTokenPath(configured string) string {
	if p := strings.TrimSpace(configured); p != "" {
		return p
	}
	return strings.TrimSpace(os.Getenv(aeiapp.TokenPathEnv))
}

// checkTokenFile confirms the dispatch token file holds a token. It never logs or
// returns the token itself.
func checkTokenFile(path string) error {
	b, err := os.ReadFile(path) // #nosec G304 -- operator-configured token path
	if err != nil {
		return fmt.Errorf("aei: dispatch token %s (%s): %w", aeiapp.TokenPathEnv, path, err)
	}
	if strings.TrimSpace(string(b)) == "" {
		return fmt.Errorf("aei: dispatch token %s (%s) is empty", aeiapp.TokenPathEnv, path)
	}
	return nil
}

// Submit dispatches spec to the control plane and returns the run id. Dispatch is
// non-blocking: the run outlives the request that triggered it, and its results land
// in gofer's own store when the runtime writes them back through the agent plane.
//
// A failure wraps ingest.ErrNotAccepted only when the control plane certainly holds
// no run. That is judged on the request's final attempt (after any redirect): it was
// never sent (ctx already done, or no connection was made), or the answer was a
// refusal, a 4xx (an overload's 429 included) or a redirect not followed. Anything
// else leaves acceptance unknown: a 5xx (a gateway's 502/503/504 can come after the
// control plane accepted the POST), a failure after the request went out, or a 202
// whose run id could not be read.
func (d *Dispatcher) Submit(ctx context.Context, spec ingest.RunSpec) (string, error) {
	if spec.TaskRef == "" {
		return "", fmt.Errorf("aei: run spec has no task ref: %w", ingest.ErrNotAccepted)
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("aei: dispatch %s not sent: %w: %w", spec.TaskRef, ingest.ErrNotAccepted, err)
	}
	ex := &exchange{}
	runID, err := d.client.Dispatch(context.WithValue(ctx, exchangeKey{}, ex), toAppSpec(spec))
	if err != nil {
		if ex.refused() {
			return "", fmt.Errorf("aei: dispatch %s: %w: %w", spec.TaskRef, ingest.ErrNotAccepted, err)
		}
		return "", fmt.Errorf("aei: dispatch %s (acceptance unknown): %w", spec.TaskRef, err)
	}
	d.log.Info("aei: run dispatched", "run", runID, "task", spec.TaskRef, "app", d.app)
	return runID, nil
}

// Status reads a run's current lifecycle from the control plane. AEI's phases are
// gofer's, so the result maps across directly.
func (d *Dispatcher) Status(ctx context.Context, runID string) (ingest.RunStatus, error) {
	if runID == "" {
		return ingest.RunStatus{}, errors.New("aei: empty run id")
	}
	res, err := d.client.Get(ctx, runID)
	if err != nil {
		return ingest.RunStatus{}, fmt.Errorf("aei: read run %s: %w", runID, err)
	}
	return ingest.RunStatus{Phase: res.Phase, Message: res.Message}, nil
}

// exchange is what observingTransport saw of a dispatch request's latest attempt; a
// redirect starts a new attempt, which replaces what the earlier one saw.
type exchange struct {
	mu        sync.Mutex
	attempted bool // an attempt reached the transport
	status    int  // the latest attempt's response status, when one arrived
	noConn    bool // the latest attempt never had a connection, so it sent nothing
}

// refused reports whether the control plane certainly did not accept the run, judged
// on the final attempt: it sent nothing, or it was answered with a 3xx or 4xx.
func (e *exchange) refused() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case !e.attempted, e.noConn:
		return true
	case e.status != 0:
		return e.status >= 300 && e.status < 500
	}
	return false
}

type exchangeKey struct{}

// observingTransport records, for a request whose context carries an *exchange,
// whether it was attempted, whether a connection was ever made, and the response
// status. The app SDK reports a refusal other than 429 only as text, so this is how
// the backend tells a refusal from a lost answer without parsing errors.
type observingTransport struct{ base http.RoundTripper }

func (t observingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	ex, _ := req.Context().Value(exchangeKey{}).(*exchange)
	if ex == nil {
		return base.RoundTrip(req)
	}
	var connected atomic.Bool
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connected.Store(true) }}
	resp, err := base.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	ex.mu.Lock()
	ex.attempted = true
	ex.status = 0
	ex.noConn = false
	if resp != nil {
		ex.status = resp.StatusCode
	} else if err != nil {
		// A request is written only on a connection; without one it never left.
		ex.noConn = !connected.Load()
	}
	ex.mu.Unlock()
	return resp, err
}

// toAppSpec translates gofer's run intent into the app SDK's dispatch spec. The
// audience is deliberately dropped: in the dispatch plane the AgentApp fixes the
// minted credential's audience server-side, so gofer requests only the subject and
// the scopes — and the control plane bounds those by the app's ceiling.
func toAppSpec(spec ingest.RunSpec) aeiapp.Spec {
	return aeiapp.Spec{
		TaskRef:    spec.TaskRef,
		Parameters: spec.Parameters,
		Identity: aeiapp.Identity{
			Subject: spec.Subject,
			Scopes:  spec.Scopes,
		},
		TimeoutSeconds: int64(spec.Deadline / time.Second),
	}
}
