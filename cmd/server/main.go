// Command server is gofer's web tier: it authenticates users and renders the
// worklist ordered by gofer metadata. It selects the agent-runtime backend (an
// ingest.Dispatcher) and wires the web handler; see internal/runtime for the backend
// contract. The web tier verifies a runtime's run credential on its agent plane with
// a public key only — it never mints one.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackfrancis/gofer/internal/config"
	"github.com/jackfrancis/gofer/internal/ingest"
	"github.com/jackfrancis/gofer/internal/runtime/aei"
	"github.com/jackfrancis/gofer/internal/server"
	"github.com/jackfrancis/gofer/internal/vault"
	"github.com/jackfrancis/gofer/internal/worklist"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	cfg, err := config.Load()
	if err != nil {
		log.Error("configuration error", "err", err)
		os.Exit(1)
	}

	vlt := vault.NewMemoryVault()
	store := worklist.NewMemoryStore()

	if cfg.ConversationEnabled {
		log.Info("assistive conversation enabled (the agent-runtime backend runs the model)")
	} else {
		log.Info("assistive conversation disabled (set AI_CONNECTIONS and AI_TOKEN to offer Discuss)")
	}

	// Select the agent-runtime backend. This is the ONE wiring point that changes to
	// swap runtimes: a concrete backend constructs its own ingest.Dispatcher
	// and is assigned here, and nothing in gofer's interfaces or the web tier changes.
	// A backend owns its own configuration, so nothing backend-specific belongs above
	// this line; see internal/runtime for the full backend contract.
	//
	// This build dispatches runs to AEI's control plane.
	runtimeBackend, err := aei.New(aei.Config{Logger: log})
	if err != nil {
		log.Error("agent runtime backend error", "err", err)
		os.Exit(1)
	}
	var dispatcher ingest.Dispatcher = runtimeBackend

	handler, cleanup := server.New(cfg, log, dispatcher, vlt, store)
	defer cleanup()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Info("gofer web tier listening", "addr", cfg.Addr, "audience", cfg.Audience)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Error("graceful shutdown failed", "err", err)
	}
}
