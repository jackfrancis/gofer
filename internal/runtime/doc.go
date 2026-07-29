// Package runtime documents the contract an AGENT-RUNTIME BACKEND implements, in one
// place, so that backends are interchangeable and their footprints are comparable.
//
// gofer's domain (the worklist model, scoring, sort, web tier, UI, OAuth) is
// substrate-agnostic and never changes to add a backend. A backend plugs into gofer's
// stable, gofer-native seams — it does NOT alter them. Adding one (AEI,
// agent-sandbox, a local launcher, …) is therefore purely additive: a package under
// internal/runtime/<name> plus one wiring line. Two backends can be diffed directly.
//
// This package holds no code today; it is the checklist. Concrete backends live in
// subpackages (e.g. internal/runtime/aei) and are wired in cmd/server.
//
// # The contract (four seams)
//
//  1. DISPATCH (web-side) — implement ingest.Dispatcher:
//
//     Submit(ctx, ingest.RunSpec) (runID string, err error)
//     Status(ctx, runID string) (ingest.RunStatus, error)
//
//     ingest.RunSpec is the provider-neutral run intent gofer already constructs
//     (task ref, parameters, scoped identity, deadline). This is the ONE dependency
//     the web tier has on a backend, and it is INJECTED: cmd/server assigns the chosen
//     ingest.Dispatcher and passes it to server.New. The default is ingest.NoopDispatcher
//     (accept + drop). Swapping backends changes only that one line — server.New and
//     every gofer interface are untouched.
//
//  2. WORKLOAD (runtime-side) — run agent.Run behind the dispatcher:
//
//     agent.Run executes the job named by RunSpec.TaskRef (github-ingest, enrich,
//     llm-rank, github-converse, github-research) against two small seams:
//     - agent.Vendor: vend the acting user's delegated provider token for the run.
//     - agent.Sink:   read and write the user's worklist.
//     gofer SUPPLIES both shapes, so a backend writes no plumbing of its own
//     (internal/agent/adapters.go):
//     - in-process:     agent.NewVaultVendor + agent.NewStoreSink (vault + store).
//     - out-of-process: agent.NewPlaneClient (one client that is both Vendor and
//     Sink over /agent/credential and /agent/worklist, authenticating
//     with the run credential).
//     A backend supplies whatever launches agent.Run under its substrate (a binary, a
//     pod, an in-process goroutine); the workload code is gofer's and is reused as-is.
//
//  3. IDENTITY (control-plane side) — supply the MINT half of identity.Authority:
//
//     A run credential is an audience-bound, capability-scoped, short-TTL token the
//     workload carries to authenticate gofer's /agent/* plane. gofer's web tier is
//     VERIFY-ONLY: it holds just the public key (identity.NewEd25519Verifier, from
//     config.MintPublicKey) and can never mint. A backend's control plane owns the
//     minter — so a web-tier compromise cannot forge a job token. gofer wires only the
//     verifier; the minter comes with a backend.
//
//  4. CREDENTIAL VENDING (web-side, already provided) — the /agent/credential broker
//     (internal/api) vends the user's delegated provider token (and the chat-model
//     token) to an authenticated workload, per run, never as a standing secret. A
//     backend's workload consumes it via agent.Vendor; gofer already serves it.
//
// # Where a backend touches gofer
//
// Additive only:
//   - a new package internal/runtime/<name> implementing ingest.Dispatcher (and
//     whatever launches agent.Run under its substrate);
//   - one line in cmd/server assigning that dispatcher.
//
// A backend reads its OWN configuration (env vars, files, credentials) inside its
// package, never in cmd/server, so the wiring point stays backend-agnostic: selecting
// a backend is a code change there, not a comment change.
//
// Never:
//   - gofer's interfaces (ingest.Dispatcher, agent.Vendor/Sink, identity.Authority,
//     worklist.Store/Ingestor) or its domain packages.
package runtime
