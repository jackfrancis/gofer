// Package dispatch is gofer's agent-dispatch tier. gofer holds no dispatch engine
// of its own: it is an AEI *app* that submits runs to the pre-installed AEI
// control plane (the aei-controller data plane, docs/dispatch-plane.md) through
// the app SDK and reads their lifecycle back. It imports no provider and no
// client-go, and creates no per-run Kubernetes object — the caller authenticates
// with the pod's projected ServiceAccount token, bounded by gofer's AgentApp
// policy (dispatchers, maxScopes, audience).
//
// Identity (ADR 0002): the run credential is minted by the control plane, which is
// configured with gofer's asymmetric, audience-bound Ed25519 authority, so the
// token a runtime carries IS a gofer Ed25519 token. gofer's web tier holds only
// the public half and verifies it (internal/authn); it never mints, so a web-tier
// compromise cannot forge a run credential.
package dispatch

import (
	"context"
	"net/http"
	"time"

	"github.com/jackfrancis/agent-execution-interface/aei"
	"github.com/jackfrancis/agent-execution-interface/sdks/go/aeiapp"
)

// Config wires the dispatch engine to the AEI control plane.
type Config struct {
	// Endpoint is the AEI dispatch API base URL (the aei-controller), e.g.
	// "http://aei-controller.aei-system.svc:8080".
	Endpoint string
	// App is the AgentApp name gofer dispatches as; the control plane bounds every
	// run by that app's policy and fixes the minted credential's audience.
	App string
	// HTTPClient optionally overrides the transport (tests); nil uses the default.
	HTTPClient *http.Client
	// Token optionally overrides the dispatch bearer; empty uses the pod's
	// projected ServiceAccount token (the production path — TokenReview, no shared
	// secret).
	Token string
}

// Engine submits runs to the AEI control plane and reads their lifecycle. It is a
// thin adapter over the app SDK; gofer embeds no control plane and no launcher.
type Engine struct {
	client *aeiapp.Client
}

// New builds an Engine that dispatches to the AEI control plane at cfg.Endpoint as
// app cfg.App.
func New(cfg Config) *Engine {
	var opts []aeiapp.Option
	if cfg.HTTPClient != nil {
		opts = append(opts, aeiapp.WithHTTPClient(cfg.HTTPClient))
	}
	if cfg.Token != "" {
		opts = append(opts, aeiapp.WithToken(cfg.Token))
	}
	return &Engine{client: aeiapp.New(cfg.Endpoint, cfg.App, opts...)}
}

// Submit dispatches spec and returns the run id. Dispatch is non-blocking: the run
// outlives the request, and its result lands in gofer's own store (the runtime
// writes it back through the domain plane), so callers that only care that work
// landed can ignore the id.
func (e *Engine) Submit(ctx context.Context, spec aei.RunSpec) (string, error) {
	return e.client.Dispatch(ctx, toAppSpec(spec))
}

// Status returns the current lifecycle of a submitted run.
func (e *Engine) Status(ctx context.Context, runID string) (aeiapp.Result, error) {
	return e.client.Get(ctx, runID)
}

// toAppSpec translates gofer's internal run intent (aei.RunSpec) into the app
// SDK's dispatch spec. The audience is intentionally dropped here: in the dispatch
// plane the AgentApp fixes the minted credential's audience server-side, so gofer
// requests only the subject and scopes.
func toAppSpec(spec aei.RunSpec) aeiapp.Spec {
	return aeiapp.Spec{
		TaskRef:    spec.TaskRef,
		Parameters: spec.Parameters,
		Identity: aeiapp.Identity{
			Subject: spec.Identity.Subject,
			Scopes:  spec.Identity.Scopes,
		},
		TimeoutSeconds: int64(spec.Deadline / time.Second),
	}
}
