// Package agentsessions is a concrete agent-runtime backend for gofer, built on
// agentsessions (github.com/aramase/agentsessions).
//
// It implements ingest.Dispatcher, the seam gofer's web tier dispatches through (see
// internal/runtime for the full backend contract). Each gofer run becomes one
// agentsessions SESSION: the run intent is appended to a durable, hash-chained event
// log, an api.Harness executes gofer's workload (internal/agent) for that turn, and
// the turn's terminal event records the outcome. Run lifecycle (Status) is read back
// from the journal rather than tracked in memory, so it is durable and auditable.
//
// Nothing in gofer changes to select this backend: it consumes gofer's existing seams
// (ingest.RunSpec, agent.Run, agent.Vendor/Sink, worklist.Store, vault.Vault) and is
// wired by a single assignment in cmd/server.
package agentsessions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/aramase/agentsessions/api"
	"github.com/aramase/agentsessions/controller"
	"github.com/aramase/agentsessions/eventlog"
	"github.com/aramase/agentsessions/sqlitelog"

	"github.com/jackfrancis/gofer/internal/ingest"
	"github.com/jackfrancis/gofer/internal/vault"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// Config wires the backend to the resources a run needs. The workload reaches gofer
// IN-PROCESS here (the dispatcher runs inside the web tier), so the vault and store
// are passed directly; the same harness works unchanged against HTTP clients to
// gofer's /agent/* plane when a substrate runs it out-of-process.
type Config struct {
	// Vault vends the acting user's delegated provider credential for a run.
	Vault vault.Vault
	// Store is gofer's worklist: the workload reads and writes the owner's items.
	Store worklist.Store
	// AIToken is the shared chat-model token gofer brokers per run as provider "ai".
	// Empty disables the model (ranking falls back to the deterministic stub).
	AIToken string
	// JournalPath, when set, persists session logs to that SQLite file so runs survive
	// a restart. Empty falls back to the AGENTSESSIONS_JOURNAL environment variable,
	// and journals stay in memory when neither is set. This backend owns the knob, so
	// gofer's wiring point never names it.
	JournalPath string
	// GitHubBaseURL overrides the GitHub API base (tests); empty uses the public API.
	GitHubBaseURL string
	// Logger records run lifecycle; nil uses slog.Default().
	Logger *slog.Logger
}

// Dispatcher runs gofer's agent workload as agentsessions sessions.
type Dispatcher struct {
	cfg Config
	log *slog.Logger

	sqlite *sqlitelog.Store // nil when journals are in memory

	mu   sync.Mutex
	logs map[string]eventlog.Store // session uid -> journal (in-memory mode)
}

var _ ingest.Dispatcher = (*Dispatcher)(nil)

// New builds the backend. It fails fast when a run could not possibly execute.
func New(cfg Config) (*Dispatcher, error) {
	if cfg.Vault == nil {
		return nil, errors.New("agentsessions: a vault is required to vend run credentials")
	}
	if cfg.Store == nil {
		return nil, errors.New("agentsessions: a worklist store is required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	journal := cfg.JournalPath
	if journal == "" {
		journal = os.Getenv("AGENTSESSIONS_JOURNAL")
	}
	d := &Dispatcher{cfg: cfg, log: log, logs: map[string]eventlog.Store{}}
	if journal != "" {
		s, err := sqlitelog.Open(journal)
		if err != nil {
			return nil, fmt.Errorf("agentsessions: open journal %s: %w", journal, err)
		}
		d.sqlite = s
	}
	log.Info("agent runtime backend ready", "backend", "agentsessions", "journal", journalKind(journal))
	return d, nil
}

// journalKind describes where session logs live, for the startup log.
func journalKind(path string) string {
	if path == "" {
		return "memory"
	}
	return path
}

// Close releases the journal.
func (d *Dispatcher) Close() error {
	if d.sqlite != nil {
		return d.sqlite.Close()
	}
	return nil
}

// session returns the durable journal for a session uid, creating an empty one on
// first use.
func (d *Dispatcher) session(uid string) eventlog.Store {
	if d.sqlite != nil {
		return d.sqlite.Session(uid)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	l, ok := d.logs[uid]
	if !ok {
		l = eventlog.AsStore(eventlog.New())
		d.logs[uid] = l
	}
	return l
}

// runIntent is the record of what gofer asked for, appended as the session's INPUT so
// the journal is self-describing (the harness reads the spec from the Dispatcher, but
// an auditor reads it from the log).
type runIntent struct {
	TaskRef    string            `json:"task_ref"`
	Parameters map[string]string `json:"parameters,omitempty"`
	Subject    string            `json:"subject,omitempty"`
	Scopes     []string          `json:"scopes,omitempty"`
	Audience   string            `json:"audience,omitempty"`
}

// Submit starts one session for spec and returns its uid as the run id.
//
// gofer's dispatch is fire-and-forget — Submit returns as soon as the run is accepted,
// and the run outlives the HTTP request that triggered it — so the turn is executed on
// a detached context bounded by the spec's own deadline. Results reach gofer through
// the workload's Sink, exactly as they would from an out-of-process runtime.
func (d *Dispatcher) Submit(_ context.Context, spec ingest.RunSpec) (string, error) {
	if spec.TaskRef == "" {
		return "", errors.New("agentsessions: run spec has no task ref")
	}
	uid := newSessionUID()
	journal := d.session(uid)
	head, err := journal.Head()
	if err != nil {
		return "", fmt.Errorf("agentsessions: read journal head: %w", err)
	}
	// Every turn runs on a fresh incarnation (a new controller mints a new fence)
	// continuing the session's journal, so the log stays the single writer authority.
	ctrl, err := controller.New(journal, d.model)
	if err != nil {
		return "", fmt.Errorf("agentsessions: start incarnation: %w", err)
	}
	intent, err := json.Marshal(runIntent{
		TaskRef:    spec.TaskRef,
		Parameters: spec.Parameters,
		Subject:    spec.Subject,
		Scopes:     spec.Scopes,
		Audience:   spec.Audience,
	})
	if err != nil {
		return "", fmt.Errorf("agentsessions: encode run intent: %w", err)
	}

	har := &harness{spec: spec, cfg: d.cfg, log: d.log}
	deadline := spec.Deadline
	if deadline <= 0 {
		deadline = defaultRunDeadline
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()
		if err := ctrl.Exec(ctx, har, []api.Message{*api.TextMessage("user", string(intent))}, head); err != nil {
			// The controller has already journaled the failure as an ERROR event, which
			// Status reads back; log it too so a failing run is visible in the web tier.
			d.log.Warn("agentsessions: run failed", "run", uid, "task", spec.TaskRef, "error", err)
			return
		}
		d.log.Info("agentsessions: run completed", "run", uid, "task", spec.TaskRef)
	}()
	return uid, nil
}

// defaultRunDeadline bounds a run whose spec carries no deadline.
const defaultRunDeadline = 15 * time.Minute

// Status reports a run's lifecycle by reading its journal: the turn's terminal event
// decides the phase. A session with no terminal event yet is still Running; an unknown
// run id reads as an empty journal and is likewise reported Running (it is not an
// error to ask about a run this process has not seen).
func (d *Dispatcher) Status(_ context.Context, runID string) (ingest.RunStatus, error) {
	if runID == "" {
		return ingest.RunStatus{}, errors.New("agentsessions: empty run id")
	}
	recs, err := d.session(runID).Read(1)
	if err != nil {
		return ingest.RunStatus{}, fmt.Errorf("agentsessions: read journal: %w", err)
	}
	st := ingest.RunStatus{Phase: "Running"}
	for _, r := range recs {
		switch r.Event.Kind {
		case api.EventEnd:
			st.Phase = "Succeeded"
			if r.Event.End != nil {
				if r.Event.End.State != "" && r.Event.End.State != "COMPLETED" {
					st.Phase = "Failed"
				}
				if r.Event.End.Error != nil {
					st.Message = r.Event.End.Error.Description
				}
			}
		case api.EventError:
			st.Phase = "Failed"
			if r.Event.Err != nil {
				st.Message = r.Event.Err.Description
			}
		}
	}
	return st, nil
}

// model is the host-mediated model hook agentsessions offers a harness. gofer's
// workload owns its own model calls (the converse tool loop drives read-only GitHub
// lookups that cannot cross a serialized mediation boundary, and ranking fans out
// concurrently while the journal is single-writer), so the harness never calls
// sink.Model and this reports a clear error if that ever changes.
func (d *Dispatcher) model(api.ModelRequest) (api.ModelResponse, error) {
	return api.ModelResponse{}, errors.New("agentsessions: gofer's workload runs its own model calls; none are host-mediated")
}

// newSessionUID mints an opaque session identifier.
func newSessionUID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "sess-" + hex.EncodeToString(b[:])
}
