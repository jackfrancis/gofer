// Command server is gofer's web tier: it authenticates users, renders the
// worklist ordered by gofer metadata, and dispatches agentic backfills to the AEI
// control plane. gofer is an AEI *app*: it holds no dispatch engine, no launcher,
// and no provider client — it POSTs runs to the pre-installed aei-controller
// through the app SDK. The run credential it verifies is minted there with gofer's
// Ed25519 authority (ADR 0002), so the web tier holds only the public key and can
// never mint one.
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
	"github.com/jackfrancis/gofer/internal/dispatch"
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

	if cfg.DispatchEndpoint == "" {
		log.Error("AEI_DISPATCH_ENDPOINT must be set: gofer dispatches runs to the pre-installed AEI control plane")
		os.Exit(1)
	}

	vlt := vault.NewMemoryVault()
	store := worklist.NewMemoryStore()

	// gofer dispatches to the AEI control plane (the aei-controller data plane) as
	// its AgentApp, authenticating with the pod's projected ServiceAccount token.
	// The control plane mints the run credential with gofer's Ed25519 authority and
	// launches the runtime; the web tier only verifies that credential (with the
	// public key from config) on its agent plane — it never mints and never embeds
	// a control plane, launcher, or provider client.
	engine := dispatch.New(dispatch.Config{
		Endpoint: cfg.DispatchEndpoint,
		App:      cfg.App,
	})

	if cfg.ConversationEnabled {
		log.Info("assistive conversation enabled (the runtime runs the model)")
	} else {
		log.Info("assistive conversation disabled (set AI_ENDPOINT and AI_MODEL to offer Discuss)")
	}

	handler, cleanup := server.New(cfg, log, engine, vlt, store)
	defer cleanup()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Info("gofer web tier listening", "addr", cfg.Addr, "dispatch", cfg.DispatchEndpoint, "app", cfg.App, "audience", cfg.Audience)
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
