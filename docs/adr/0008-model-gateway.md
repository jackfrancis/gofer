# ADR 0008 — An optional model gateway holds the model key and a shared request budget

Status: accepted. Implemented, and the end-to-end check against agentgateway v1.7.0
(`internal/modelgateway`) passes. The profile has not yet been deployed to a cluster
(see Verification).
Date: 2026-11-19

## Context

By default gofer brokers the chat-model token. The web tier holds `AI_TOKEN`, and each
run fetches it once (`GET /agent/credential?provider=ai`), keeps it in memory for the
run, and calls the provider directly. Two things follow:

- **Every run holds the upstream key while it runs.** It is never a standing secret in
  the sandbox, but it is in the memory of every runtime pod. On a durable, snapshotted
  substrate it can be checkpointed along with the rest of the actor's memory. AEI's
  guidance (`docs/hello-world.md`, "agentgateway — the model egress") recommends a
  gateway in that case.
- **There is no shared budget.** Review-all dispatches up to eight reviews at once
  (`reviewConcurrency`), and the review panel does the same with a second model. Each
  run backs off on a 429 (`internal/httpretry`), but nothing paces runs against each
  other, so a burst reaches the provider all at once.

Zumble-Zay solved both with an agentgateway overlay
(`deploy/k8s/overlays/dev/agentgateway.yaml` at ZZ 40ac67d): the gateway held the key
and applied a local token bucket. It pinned one model (`params.model`), though, and
served only chat-completions. gofer lets a user pick the model per turn, sends second
opinions to a different model, and configures connections that speak both
chat-completions and the Responses API (`AI_CONNECTIONS`). A direct port would have
sent every request to one model.

## Decision

Offer a deployment profile, off by default, in which agentgateway alone holds the
upstream key and applies one budget to every run's model calls.

**The gateway routes by model.** One wildcard model entry with no `params.model`
passes each request's model through unchanged. A `custom` provider declares both
`completions` and `responses`, so a chat-completions request goes to
`<upstream>/chat/completions` and a Responses request to `<upstream>/responses`.
gofer still decides, per connection, which API a model is called over. The model
picker and second opinions work unchanged.

**Runs authenticate with their run credential, and gofer authorizes them.** gofer's
run credential is not a JWT (`internal/identity`), so the gateway cannot verify it on
its own. It forwards the caller's `Authorization` to gofer over HTTP `extAuthz`. gofer
answers 204 at `/agent/model-access`, behind the same `RequireScope` as the credential
broker. Exactly the runs that could vend the model token without the gateway can use
the model through it, and a browser session never can. The gateway then replaces the
`Authorization` header with the upstream key, so the run credential stops at the
gateway.

**gofer holds no model key in this profile.** `AI_GATEWAY_URL` enables the model
without `AI_TOKEN`. The web tier refuses to start if both are set, or if any
`AI_CONNECTIONS` endpoint is not on the gateway's origin: a run presents its
credential to a gateway connection, and that credential must never reach a provider.
Runs carry `ai_auth=gateway`. The runtime then skips the vend and uses its run
credential as the model bearer. An unknown `ai_auth` fails the run instead of
guessing. The broker answers `provider=ai` with 404, because it has nothing to vend.

**One budget.** `llm.policies.localRateLimit` has a single request bucket with no
key, so every run draws on it. The gateway runs one replica, so the bucket is global.
Over budget the gateway answers 429, and the workload's existing backoff applies.
`make model-gateway-apply` sets the bucket (`MODEL_GATEWAY_BURST`, `_REFILL`,
`_INTERVAL`; ZZ's 20 / 2 per second by default).

**Deployment.** `deploy/k8s/model-gateway/` holds the gateway, pinned at
agentgateway v1.7.0. Its config is a plain file (`config.yaml`) whose upstream,
budget and authorization address `make model-gateway-apply` fills in. That target
moves the key into the `gofer-model-gateway` Secret, which only the gateway mounts,
and removes it from `gofer-secrets`. It rewrites `AI_CONNECTIONS` to the gateway's
paths and sets `AI_GATEWAY_URL`. `make deploy` re-applies the base config, so it
repeats that rewrite whenever the gateway is installed. `make model-gateway-down`
reverses all of this.

## Consequences

- Per-run vending stays the default, and nothing changes for a deployment that does
  not apply the profile.
- One upstream origin per gateway. All connections must share the origin given as
  `MODEL_GATEWAY_UPSTREAM`; the rewrite refuses others. A second upstream would need
  a model entry per origin, matched by model name.
- The wildcard serves any model the upstream serves, not only those gofer lists. Only
  gofer's runs can call it, and the shared budget bounds them.
- Every model call costs gofer one signature check (`/agent/model-access`). That is
  cheap and is not cached, so a revoked or expired run credential stops working at
  once.
- The runtime's egress is still open (AEI), so a run could reach the provider
  directly. Without a key it gets nowhere.
- Token-based budgets (`type: tokens`) and per-user buckets (`key:`) are one config
  line each if needed later. They are not configured.

## Verification

`internal/modelgateway` runs the real agentgateway binary on the shipped
`config.yaml`, in front of a fake provider, authorizing against gofer's real router,
called by the workload's real model client (`make model-gateway-test` fetches the
pinned release). It checks the plan's three points:

1. Two concurrent review runs, each holding only its own run credential, get answers.
   The provider sees the gateway's key on every call and never a run credential, and
   no response carries the key.
2. With a burst of 3, the two reviews and one more call from run A succeed. Then both
   run A and run B get 429. Had each run its own bucket, B would have had two calls
   left. Refused calls never reach the provider.
3. The second opinion (model-b over the Responses API) reaches model-b at
   `/responses` while the first review reaches model-a at `/chat/completions`.

It also checks that a caller with no bearer or a forged one is refused, and the
provider never sees that call. The check was also run against three broken configs,
and each one fails it: per-caller buckets fail point 2, a pinned model fails point 3,
and an authorization path that is not gofer's blocks every call. The gateway forwards
each call's original method to gofer, which is why `/agent/model-access` accepts any
method. The unit tests cover the configuration rules, the run
parameter, the runtime's choice of bearer and the routes. `make model-gateway-check`
checks a deployed profile: the gateway is up, gofer holds no key and routes every
connection through the gateway, and the gateway refuses an unauthenticated caller. It
has not yet been run against a cluster.
