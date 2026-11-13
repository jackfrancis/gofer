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
	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/storage"
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

	// Web-tier state: the worklist, the vault of delegated provider tokens, and
	// sessions. With DATA_DIR set they live in one SQLite database there (tokens
	// encrypted with VAULT_ENCRYPTION_KEY) and survive a restart; without it they are
	// in memory. See docs/adr/0004-sqlite-persistent-storage.md.
	var (
		vlt          vault.Vault    = vault.NewMemoryVault()
		store        worklist.Store = worklist.NewMemoryStore()
		sessionStore session.Store  // nil: the session manager keeps sessions in memory
		closeStorage = func() {}
	)
	if cfg.DataDir == "" {
		log.Warn("persistent storage disabled: sessions, delegated tokens and the worklist are in memory and will be lost on restart (set DATA_DIR and VAULT_ENCRYPTION_KEY to persist them)")
	} else {
		openCtx, cancelOpen := context.WithTimeout(context.Background(), 30*time.Second)
		db, err := storage.Open(openCtx, cfg.DataDir)
		cancelOpen()
		if err != nil {
			log.Error("storage error", "err", err)
			os.Exit(1)
		}
		keys, err := storage.NewKeyring(cfg.VaultKey)
		if err != nil {
			log.Error("storage error", "err", err)
			os.Exit(1)
		}
		vlt, store, sessionStore = db.Vault(keys), db.Worklist(), db.Sessions()
		closeStorage = func() {
			if err := db.Close(); err != nil {
				log.Error("closing storage failed", "err", err)
				return
			}
			log.Info("storage closed")
		}
		log.Info("persistent storage enabled", "data_dir", cfg.DataDir, "vault_key_id", keys.PrimaryKeyID())
	}
	// Runs on the way out of main, after the HTTP server has drained (below), so no
	// request is mid-write when the database closes.
	defer closeStorage()

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

	handler, cleanup := server.New(cfg, log, dispatcher, vlt, store, sessionStore)
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
