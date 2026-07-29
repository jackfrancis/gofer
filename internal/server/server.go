// Package server wires together routing, middleware, and handlers for the web
// tier. It holds no substrate client and no launcher: the agent-runtime backend is
// injected as an ingest.Dispatcher (see internal/runtime), and the /agent/* plane is
// the runtime-facing contract a backend authenticates against.
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/jackfrancis/gofer/internal/api"
	"github.com/jackfrancis/gofer/internal/auth"
	"github.com/jackfrancis/gofer/internal/authn"
	"github.com/jackfrancis/gofer/internal/config"
	"github.com/jackfrancis/gofer/internal/identity"
	"github.com/jackfrancis/gofer/internal/ingest"
	"github.com/jackfrancis/gofer/internal/metrics"
	"github.com/jackfrancis/gofer/internal/principal"
	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/vault"
	"github.com/jackfrancis/gofer/internal/webui"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// New builds the fully wired HTTP handler for the web tier over injected
// dependencies. The agent-runtime backend is injected as an ingest.Dispatcher, so
// swapping backends (a no-op, AEI, agent-sandbox, …) never changes this package: the
// web tier speaks only gofer's neutral dispatch seam. The vault holds delegated
// provider tokens the auth handler writes at login. The returned cleanup is a no-op
// today (reserved for the staleness reconciler).
func New(cfg *config.Config, log *slog.Logger, dispatcher ingest.Dispatcher, vlt vault.Vault, store worklist.Store) (http.Handler, func()) {
	sessions := session.NewManager(cfg.SessionSecret, cfg.CookieSecure)
	authH := auth.NewHandler(cfg, sessions, vlt)
	// The web tier holds only the run-credential verification key: it authenticates a
	// runtime's bearer but cannot mint one. Minting is a runtime/control-plane concern;
	// gofer keeps only the verifier.
	authenticator := authn.New(sessions, authn.NewIdentityValidator(
		identity.NewEd25519Verifier(cfg.MintPublicKey, cfg.Audience)))

	// gofer brokers the chat model: the non-secret coordinates (endpoint, model) ride
	// to runtimes as run parameters, and the single shared token is vended per run as
	// provider "ai" — so the sandbox never holds a standing model secret. The default
	// connection (first configured; change the default by reordering) seeds backfill
	// ranking and the "Review all PRs" batch; an individual Discuss turn can route to
	// any configured connection via the thread's model picker.
	conn := cfg.DefaultConnection()
	aiEndpoint := conn.Endpoint
	aiModel := conn.Default()
	// The agent-runtime backend is injected (dispatcher); cmd/server selects it. The
	// default NoopDispatcher accepts and drops runs (worklist stays empty); a backend
	// that executes runs brings gofer to life without touching this package (see
	// internal/runtime for the contract).
	ingestor := ingest.New(dispatcher, cfg.Audience, cfg.SinkURL, log, aiEndpoint, aiModel)
	// The thread's model picker spans every configured connection's models (default
	// first). A model id offered by more than one connection is disambiguated with the
	// connection label; the selected option carries the connection endpoint the turn
	// routes to. Built only when the conversation is enabled.
	var options []webui.ModelOption
	if cfg.ConversationEnabled {
		for _, ch := range cfg.ModelChoices() {
			options = append(options, webui.ModelOption{
				Value:    ch.ConnID + "|" + ch.Model,
				Label:    ch.Label,
				Endpoint: ch.Endpoint,
				Model:    ch.Model,
			})
		}
	}
	// gofer's own Prometheus metrics (app-level aggregates). Batch wall-clock timing is
	// driven by run write-backs (NoteWriteback), so it is populated once a backend that
	// writes results back is selected.
	metric := metrics.New()
	worklistHandler := api.NewWorklistHandler(store, ingestor)
	// The runtime write-back sink also notifies the batch tracker (NoteWriteback), so
	// a Review-all batch is timed to the instant its last review lands.
	ingestHandler := api.NewIngestHandler(store, ingestor)
	// The broker vends the single shared chat-model token as provider "ai", per run,
	// so the sandbox never holds a standing model secret. Every model on this endpoint
	// uses the same token; the model id travels as a run parameter.
	credentialHandler := api.NewCredentialHandler(vlt, conn.Token)
	// The assistive conversation is offered when the model is fully configured; the
	// runtime (not the web tier) runs the model, vending the token from the broker.
	convEnabled := cfg.ConversationEnabled
	webHandler := webui.New(sessions, store, ingestor, authH, convEnabled, options)

	mux := http.NewServeMux()

	// Health check.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// Prometheus metrics (app-level aggregates; unauthenticated, protect by network).
	mux.Handle("GET /metrics", metric.Handler())

	// Landing page (server-rendered) and its static assets.
	mux.Handle("GET /{$}", http.HandlerFunc(webHandler.Index))
	mux.Handle("GET /static/", webHandler.Static())

	// Per-item assistive conversation (server-rendered, Post/Redirect/Get). The
	// handlers check the session themselves.
	mux.Handle("GET /items/thread", http.HandlerFunc(webHandler.Thread))
	mux.Handle("POST /items/thread", http.HandlerFunc(webHandler.ThreadPost))
	// Hide/unhide one turn of a thread — declutter a long review and drop it from context.
	mux.Handle("POST /items/thread/hide", http.HandlerFunc(webHandler.HideMessage))
	// Batch action: concurrently schedule a review turn for every PR on the radar.
	mux.Handle("POST /items/review-all", http.HandlerFunc(webHandler.ReviewAllPRs))
	// Force a re-ingest to pull newly created or updated work onto the radar.
	mux.Handle("POST /items/refresh", http.HandlerFunc(webHandler.Refresh))
	// Clear every conversation thread, so the review workflows can be demoed or
	// exercised again from scratch without rebuilding the environment.
	mux.Handle("POST /items/reset-conversations", http.HandlerFunc(webHandler.ResetConversations))
	// A review of one item by the configured second-opinion model.
	mux.Handle("POST /items/second-opinion", http.HandlerFunc(webHandler.SecondOpinion))
	// Batch action: run the review panel on every PR reviewed once but not yet given a
	// second opinion.
	mux.Handle("POST /items/second-opinion-all", http.HandlerFunc(webHandler.SecondOpinionAllPRs))

	// Auth lifecycle.
	mux.HandleFunc("GET /auth/providers", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"providers": authH.Providers()})
	})
	mux.HandleFunc("GET /auth/{provider}/login", authH.Login)
	mux.HandleFunc("GET /auth/{provider}/callback", authH.Callback)
	mux.HandleFunc("POST /auth/logout", authH.Logout)

	// Current principal (interactive user today; workloads once gofer mints its
	// own agent-plane tokens).
	mux.Handle("GET /api/me", authenticator.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := principal.FromContext(r.Context())
		resp := map[string]any{
			"kind":           p.Kind,
			"subject":        p.Subject,
			"acting_user_id": p.ActingUserID,
			"scopes":         p.Scopes,
		}
		if p.Kind == principal.KindUser {
			if u := sessions.CurrentUser(r); u != nil {
				resp["provider"] = u.Provider
				resp["email"] = u.Email
				resp["name"] = u.Name
			}
		}
		writeJSON(w, resp)
	})))

	// Worklist: the ordered set of work for the landing page.
	mux.Handle("GET /api/worklist", authenticator.RequireAuth(http.HandlerFunc(worklistHandler.List)))

	// Agent plane — the RUNTIME-FACING CONTRACT (workload tokens only, RequireScope).
	// An agent-runtime backend authenticates here with its audience-bound run
	// credential to: write the items it produced, read the acting user's persisted
	// work, and vend the user's delegated provider token. The two planes stay disjoint
	// (a browser session can never reach these; a run credential can never reach
	// /api/*).
	mux.Handle("POST /agent/worklist", authenticator.RequireScope(principal.ScopeMetadataWrite, http.HandlerFunc(ingestHandler.Ingest)))
	mux.Handle("GET /agent/worklist", authenticator.RequireScope(principal.ScopeSignalsRead, http.HandlerFunc(ingestHandler.List)))
	// The credential broker: a runtime vends the acting user's delegated provider
	// token here, so it never holds a standing secret.
	mux.Handle("GET /agent/credential", authenticator.RequireScope(principal.ScopeSignalsRead, http.HandlerFunc(credentialHandler.Vend)))

	// Global middleware chain (outermost first).
	var h http.Handler = mux
	h = cors(cfg.AllowedOrigins, h)
	h = securityHeaders(h)
	h = logRequests(log, h)
	h = recoverer(log, h)

	return h, func() {}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
