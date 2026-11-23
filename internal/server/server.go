// Package server wires together routing, middleware, and handlers for the web
// tier. It holds no substrate client and no launcher: the agent-runtime backend is
// injected as an ingest.Dispatcher (see internal/runtime), and the /agent/* plane is
// the runtime-facing contract a backend authenticates against.
package server

import (
	"context"
	"encoding/json"
	"fmt"
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
// provider tokens the auth handler writes at login. sessionStore holds authenticated
// sessions; nil keeps them in memory. The returned cleanup stops the scheduled refresh
// (REFRESH_INTERVAL), if it runs; call it before closing the store. It fails when the
// store keeps ingest runs (ingest.RunLedger) and they cannot be read: serving with an
// empty gate could start a second ingest while one is still writing, so the caller
// should exit and let the pod restart.
func New(cfg *config.Config, log *slog.Logger, dispatcher ingest.Dispatcher, vlt vault.Vault, store worklist.Store, sessionStore session.Store) (http.Handler, func(), error) {
	sessions := session.NewManager(cfg.SessionSecret, cfg.CookieSecure, session.WithStore(sessionStore),
		session.WithAllow(func(u *session.User) bool { return cfg.SignInAllowed(u.Provider, u.Login) }))
	authH := auth.NewHandler(cfg, sessions, vlt)
	// Finish any logout a previous process began and did not complete (ADR 0007):
	// its credential stays unvendable until then.
	authH.ResumePendingLogouts(context.Background())
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
	// With persistent storage, each owner's last accepted ingest run survives a
	// restart: the gate still holds a run the runtime is still executing, and the
	// radar's refresh version keeps counting (ingest.RunLedger).
	if ledger, ok := store.(ingest.RunLedger); ok {
		if err := ingestor.UseLedger(context.Background(), ledger); err != nil {
			return nil, nil, fmt.Errorf("load the last ingest runs: %w", err)
		}
	}
	// The model-gateway profile (AI_GATEWAY_URL): a gateway holds the upstream model
	// key and the shared request budget, so runs present their run credential to it
	// and gofer brokers no model token (conn.Token is empty).
	if conn.Gateway {
		ingestor.UseModelGateway()
	}
	// Runs read the timeline with the same bot list the web tier classifies review
	// requests by, so a bot's comment is never taken for the last human word.
	ingestor.UseBots(cfg.BotReviewers)
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
	// The user's reads (the JSON worklist and the radar) classify review requests by
	// the current BOT_REVIEWERS, as the agent plane does, so the rank shown and the
	// rank Discuss explains agree before the next refresh re-stamps stored items.
	runPolicy := worklist.NewRunPolicy(cfg.BotReviewers)
	readStore := runPolicy.ClassifyReads(store)
	worklistHandler := api.NewWorklistHandler(readStore, ingestor)
	// The runtime write-back sink also notifies the batch tracker (NoteWriteback), so
	// a Review-all batch is timed to the instant its last review lands. It applies the
	// write-back policy: workload metadata is agent-derived, and a review requested by
	// one of BOT_REVIEWERS is flagged as an automated assignment for the ranker.
	ingestHandler := api.NewIngestHandler(store, ingestor, runPolicy)
	// The broker vends the single shared chat-model token as provider "ai", per run,
	// so the sandbox never holds a standing model secret. Every model on this endpoint
	// uses the same token; the model id travels as a run parameter. Per-user provider
	// tokens come through the auth handler, which refreshes expiring ones and
	// serialises every credential write (ADR 0007).
	credentialHandler := api.NewCredentialHandler(authH, conn.Token)
	// The assistive conversation is offered when the model is fully configured; the
	// runtime (not the web tier) runs the model, vending the token from the broker.
	convEnabled := cfg.ConversationEnabled
	webHandler := webui.New(sessions, readStore, ingestor, authH, convEnabled, options)

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
	mux.Handle("GET "+webui.LabelStylesheetPath, http.HandlerFunc(webHandler.LabelCSS))

	// Per-item assistive conversation (server-rendered, Post/Redirect/Get). The
	// handlers check the session themselves.
	mux.Handle("GET /items/thread", http.HandlerFunc(webHandler.Thread))
	mux.Handle("POST /items/thread", http.HandlerFunc(webHandler.ThreadPost))
	// Retry a stalled turn under its existing ID, rather than resending it as a new one.
	mux.Handle("POST /items/thread/retry", http.HandlerFunc(webHandler.ThreadRetry))
	// Hide/unhide one turn of a thread — declutter a long review and drop it from context.
	mux.Handle("POST /items/thread/hide", http.HandlerFunc(webHandler.HideMessage))
	mux.Handle("POST /items/hide", http.HandlerFunc(webHandler.HideItem))
	// Batch action: concurrently schedule a review turn for every PR on the radar.
	mux.Handle("POST /items/review-all", http.HandlerFunc(webHandler.ReviewAllPRs))
	// Force a re-ingest to pull newly created or updated work onto the radar.
	mux.Handle("POST /items/refresh", http.HandlerFunc(webHandler.Refresh))
	// The radar polls this to notice a refresh that ran while it was open (reload.js).
	mux.Handle("GET /items/refresh-status", http.HandlerFunc(webHandler.RefreshStatus))
	// The radar's project filter: saves which repositories the radar shows.
	mux.Handle("POST /items/projects", http.HandlerFunc(webHandler.FilterProjects))
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
	mux.Handle("GET /agent/worklist/select", authenticator.RequireScope(principal.ScopeSignalsRead, http.HandlerFunc(ingestHandler.Select)))
	mux.Handle("GET /agent/worklist/item", authenticator.RequireScope(principal.ScopeSignalsRead, http.HandlerFunc(ingestHandler.Item)))
	// A converse run appends its answer to a thread here; it cannot rewrite the thread.
	mux.Handle("POST /agent/worklist/reply", authenticator.RequireScope(principal.ScopeMetadataWrite, http.HandlerFunc(ingestHandler.Reply)))
	// A converse or research run writes its research adjustment here, and nothing else.
	mux.Handle("POST /agent/worklist/research", authenticator.RequireScope(principal.ScopeMetadataWrite, http.HandlerFunc(ingestHandler.Research)))
	mux.Handle("POST /agent/worklist/turns", authenticator.RequireScope(principal.ScopeMetadataWrite, http.HandlerFunc(ingestHandler.Turns)))
	// The credential broker: a runtime vends the acting user's delegated provider
	// token here, so it never holds a standing secret.
	mux.Handle("GET /agent/credential", authenticator.RequireScope(principal.ScopeSignalsRead, http.HandlerFunc(credentialHandler.Vend)))
	// The model gateway's authorization check: it forwards a run's bearer here before
	// attaching the upstream key. Same scope as the broker; served only in the gateway
	// profile. Any method, since the gateway calls with the model request's.
	if conn.Gateway {
		mux.Handle("/agent/model-access", authenticator.RequireScope(principal.ScopeSignalsRead, http.HandlerFunc(api.ModelAccess)))
	}

	// Global middleware chain (outermost first).
	var h http.Handler = mux
	h = cors(cfg.AllowedOrigins, h)
	h = securityHeaders(h)
	h = logRequests(log, h)
	h = recoverer(log, h)

	// The scheduled refresh (REFRESH_INTERVAL, off by default): a github-ingest run per
	// owner with stored work, every interval, so a populated worklist keeps up with
	// GitHub without a click. It reads the raw store (owners only) and dispatches only
	// for owners the auth handler confirms (ScheduledRunAllowed: provider configured,
	// a vendable credential readable, no logout pending, login still allowed). cleanup
	// stops it before main closes the store.
	//
	// When a default chat model is configured, each tick also runs the research pass:
	// a github-research run for every item whose discussion-derived ranking is stale
	// (worklist.ResearchStale), for the same confirmed owners.
	stopRefresh := func() {}
	if cfg.RefreshInterval > 0 {
		if owners, ok := store.(worklist.OwnerLister); ok {
			r := ingest.NewRefresher(owners, ingestor, authH, cfg.RefreshInterval, log)
			if ingestor.CanResearch() {
				r.WithResearch(store, ingestor)
				log.Info("scheduled research enabled", "interval", cfg.RefreshInterval)
			} else {
				log.Info("scheduled research disabled: no default chat model (AI_CONNECTIONS)")
			}
			r.Start()
			stopRefresh = r.Stop
			log.Info("scheduled refresh enabled", "interval", cfg.RefreshInterval)
		} else {
			log.Warn("scheduled refresh disabled: the worklist store cannot list owners", "interval", cfg.RefreshInterval)
		}
	} else {
		log.Info("scheduled refresh disabled (set REFRESH_INTERVAL to turn it on)")
	}

	return h, stopRefresh, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
