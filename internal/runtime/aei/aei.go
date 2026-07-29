// Package aei is a concrete agent-runtime backend for gofer, built on the Agent
// Execution Interface (github.com/jackfrancis/agent-execution-interface).
//
// It implements ingest.Dispatcher, the seam gofer's web tier dispatches through (see
// internal/runtime for the full backend contract). gofer holds no dispatch engine of
// its own: it is an AEI *app* that POSTs each run to the pre-installed AEI control
// plane (the aei-controller dispatch API) through the app SDK and reads the run's
// lifecycle back. It imports no provider and no client-go, and creates no per-run
// Kubernetes object — the caller authenticates with the web pod's projected
// ServiceAccount token, bounded by gofer's AgentApp policy (dispatchers, maxScopes,
// audience).
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
	"os"
	"strings"
	"time"

	"github.com/jackfrancis/agent-execution-interface/sdks/go/aeiapp"

	"github.com/jackfrancis/gofer/internal/ingest"
)

// Config wires the backend to the AEI control plane. Every field has an environment
// fallback so the backend owns its own configuration and gofer's wiring point names
// none of it.
type Config struct {
	// Endpoint is the AEI dispatch API base URL (the aei-controller), e.g.
	// "http://aei-controller.aei-system.svc:8080". Empty falls back to
	// AEI_DISPATCH_ENDPOINT.
	Endpoint string
	// App is the AgentApp gofer dispatches as. The control plane bounds every run by
	// that app's policy (permitted dispatchers, scope ceiling) and fixes the minted
	// credential's audience. Empty falls back to AEI_APP.
	App string
	// Token overrides the dispatch bearer (tests, out-of-cluster runs); empty uses the
	// pod's projected ServiceAccount token, which the controller TokenReviews — the
	// production path, with no shared secret.
	Token string
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
	opts := []aeiapp.Option{aeiapp.WithHTTPClient(client)}
	if cfg.Token != "" {
		opts = append(opts, aeiapp.WithToken(cfg.Token))
	}

	log.Info("agent runtime backend ready", "backend", "aei", "dispatch", endpoint, "app", app)
	return &Dispatcher{client: aeiapp.New(endpoint, app, opts...), app: app, log: log}, nil
}

// Submit dispatches spec to the control plane and returns the run id. Dispatch is
// non-blocking: the run outlives the request that triggered it, and its results land
// in gofer's own store when the runtime writes them back through the agent plane.
func (d *Dispatcher) Submit(ctx context.Context, spec ingest.RunSpec) (string, error) {
	if spec.TaskRef == "" {
		return "", errors.New("aei: run spec has no task ref")
	}
	runID, err := d.client.Dispatch(ctx, toAppSpec(spec))
	if err != nil {
		return "", fmt.Errorf("aei: dispatch %s: %w", spec.TaskRef, err)
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
