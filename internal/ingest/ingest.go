// Package ingest turns an empty worklist into dispatched AEI runs. It implements
// worklist.Ingestor by submitting a github-ingest run for the user, so page
// rendering stays decoupled from GitHub retrieval.
package ingest

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackfrancis/agent-execution-interface/aei"
	"github.com/jackfrancis/agent-execution-interface/sdks/go/aeiapp"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// Dispatcher submits an AEI run and reads its lifecycle back. dispatch.Engine
// satisfies it.
type Dispatcher interface {
	Submit(ctx context.Context, spec aei.RunSpec) (string, error)
	Status(ctx context.Context, runID string) (aeiapp.Result, error)
}

// retryAfter bounds how long a tracked backfill run is treated as in-flight before
// EnsureBackfill dispatches a fresh one. It matches the ingest run deadline, so a
// healthy run is never piled on while a lost or stuck run eventually retries.
const retryAfter = 5 * time.Minute

// backfill is the most recent ingest run tracked for one owner, so a run that
// fails can be surfaced (BackfillFailure) instead of leaving the worklist spinning.
type backfill struct {
	id      string
	at      time.Time
	failed  bool
	message string
	logged  bool
}

// Ingestor dispatches ingestion runs. It implements worklist.Ingestor and
// worklist.BackfillProber, so a backfill run that fails surfaces to the read model.
type Ingestor struct {
	d          Dispatcher
	audience   string
	sinkURL    string
	aiEndpoint string
	aiModel    string
	log        *slog.Logger

	mu   sync.Mutex
	runs map[string]*backfill // ownerID -> most recent ingest run
}

var (
	_ worklist.Ingestor       = (*Ingestor)(nil)
	_ worklist.BackfillProber = (*Ingestor)(nil)
)

// New builds an Ingestor that dispatches through d, binding run credentials to
// audience. sinkURL is the in-cluster URL a runtime reaches to vend its
// credential (GET /agent/credential) and write results back (POST /agent/worklist);
// it is carried to each run in its parameters as gofer_url. aiEndpoint and aiModel
// are the non-secret chat-model coordinates carried to each run (ai_endpoint,
// ai_model) when both are set; the model token is vended from the broker, never a
// parameter. log records backfill failures; a nil log uses slog.Default().
func New(d Dispatcher, audience, sinkURL string, log *slog.Logger, aiEndpoint, aiModel string) *Ingestor {
	if log == nil {
		log = slog.Default()
	}
	return &Ingestor{
		d:          d,
		audience:   audience,
		sinkURL:    sinkURL,
		aiEndpoint: aiEndpoint,
		aiModel:    aiModel,
		log:        log,
		runs:       map[string]*backfill{},
	}
}

// runParams builds the run parameters shared by every dispatched job: the owner,
// the sink URL a runtime calls back on (gofer_url), and — when configured — the
// non-secret chat-model coordinates (ai_endpoint, ai_model). The model TOKEN is
// never a parameter; a runtime vends it from the broker per run.
func (i *Ingestor) runParams(ownerID string) map[string]string {
	p := map[string]string{"owner": ownerID}
	if i.sinkURL != "" {
		p["gofer_url"] = i.sinkURL
	}
	if i.aiEndpoint != "" && i.aiModel != "" {
		p["ai_endpoint"] = i.aiEndpoint
		p["ai_model"] = i.aiModel
	}
	return p
}

// EnsureBackfill dispatches a github-ingest run for ownerID. It is idempotent at
// the domain level: re-ingesting reconciles rather than duplicates. The run's
// scopes authorize gofer's agent plane (read the user's work, write metadata) and
// its audience binds the credential to that plane (ADR 0002).
//
// It tracks the dispatched run so BackfillFailure can report its outcome, and
// dispatches at most one run at a time per owner: while a recent, non-failed run
// is still tracked this is a no-op, so the empty-worklist poll does not pile on
// runs. A failed or stale (older than retryAfter) run is (re)dispatched, so a
// reload retries after a failure or a lost run.
func (i *Ingestor) EnsureBackfill(ctx context.Context, ownerID string) error {
	i.mu.Lock()
	cur := i.runs[ownerID]
	inFlight := cur != nil && !cur.failed && time.Since(cur.at) < retryAfter
	i.mu.Unlock()
	if inFlight {
		return nil
	}
	id, err := i.d.Submit(ctx, aei.RunSpec{
		TaskRef:    "github-ingest",
		Parameters: i.runParams(ownerID),
		Identity: aei.IdentityRequest{
			Subject:  ownerID,
			Scopes:   []string{"signals:read", "metadata:write"},
			Audience: i.audience,
		},
		Deadline: 5 * time.Minute,
	})
	if err != nil {
		return err
	}
	i.mu.Lock()
	i.runs[ownerID] = &backfill{id: id, at: time.Now()}
	i.mu.Unlock()
	return nil
}

// BackfillFailure reports whether ownerID's most recent backfill run has failed,
// with the run's failure message, by reading the run's lifecycle from the control
// plane. The failure is logged once. A run still in flight, one that succeeded, or
// a transient status-read error reports failed=false so the caller keeps waiting.
func (i *Ingestor) BackfillFailure(ctx context.Context, ownerID string) (bool, string, error) {
	i.mu.Lock()
	cur := i.runs[ownerID]
	i.mu.Unlock()
	if cur == nil {
		return false, "", nil
	}
	if cur.failed {
		return true, cur.message, nil
	}
	res, err := i.d.Status(ctx, cur.id)
	if err != nil {
		return false, "", err // transient; keep waiting
	}
	if res.Phase != "Failed" {
		return false, "", nil
	}
	i.mu.Lock()
	// Record on the run we probed; a concurrent EnsureBackfill may have moved on.
	if latest := i.runs[ownerID]; latest != nil && latest.id == cur.id {
		latest.failed = true
		latest.message = res.Message
		if !latest.logged {
			i.log.Warn("backfill run failed", "owner", ownerID, "run_id", cur.id, "error", res.Message)
			latest.logged = true
		}
	}
	i.mu.Unlock()
	return true, res.Message, nil
}

// Converse dispatches a github-converse run for one item (itemID) owned by
// ownerID: an assistant answers the user's latest thread turn and writes the reply
// back. Like EnsureBackfill it authorizes gofer's agent plane and binds the
// credential to it; the deadline is larger because a model turn is slow.
func (i *Ingestor) Converse(ctx context.Context, ownerID, itemID string) error {
	params := i.runParams(ownerID)
	params["item"] = itemID
	_, err := i.d.Submit(ctx, aei.RunSpec{
		TaskRef:    "github-converse",
		Parameters: params,
		Identity: aei.IdentityRequest{
			Subject:  ownerID,
			Scopes:   []string{"signals:read", "metadata:write"},
			Audience: i.audience,
		},
		Deadline: 15 * time.Minute,
	})
	return err
}
