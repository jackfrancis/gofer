// Package ingest turns a user's intent — an empty worklist, a Discuss turn, a bulk
// review — into agent RUN REQUESTS and hands them to a Dispatcher.
//
// The Dispatcher is the seam an agent-runtime backend plugs into (see internal/runtime
// for the contract). gofer always constructs the full run intent (RunSpec) for each
// action; a backend implements Dispatcher to execute those RunSpecs — running the
// workload in internal/agent under the run's scoped identity and writing results back
// through the agent plane (POST /agent/worklist). The default backend is
// NoopDispatcher, which accepts and drops every run, so with it the worklist is never
// populated; selecting a backend that executes runs is what fetches and returns work.
package ingest

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// RunSpec is gofer's provider-neutral request to execute one agent run: which
// workload (TaskRef), its parameters (owner, item, non-secret model coordinates), the
// acting identity (Subject + Scopes + Audience), and a deadline. A concrete agent
// runtime consumes this; NoopDispatcher discards it.
type RunSpec struct {
	TaskRef    string            // github-ingest | github-converse
	Parameters map[string]string // owner, gofer_url, ai_endpoint, ai_model, item, independent
	Subject    string            // the acting user id
	Scopes     []string          // capability scopes the run is authorized for
	Audience   string            // binds the run credential to gofer's agent plane
	Deadline   time.Duration
}

// RunStatus is the lifecycle a Dispatcher reports for a submitted run.
type RunStatus struct {
	Phase   string // Succeeded | Failed | Running
	Message string
}

// Dispatcher executes agent runs — the seam an agent-runtime backend plugs into. An
// implementation dispatches each RunSpec to run gofer's agent workload (internal/agent:
// fetch GitHub work, enrich, rank, converse) under the run's scoped identity, and
// reports the run's lifecycle back through Status. The default implementation is
// NoopDispatcher; concrete backends live in internal/runtime/<name>.
type Dispatcher interface {
	Submit(ctx context.Context, spec RunSpec) (runID string, err error)
	Status(ctx context.Context, runID string) (RunStatus, error)
}

// NoopDispatcher is the default backend: it accepts every run and does nothing (no-op,
// always success). With it selected nothing executes, so no work is fetched and no
// reply is written back — selecting a concrete backend is what brings the worklist to
// life.
type NoopDispatcher struct{}

var _ Dispatcher = NoopDispatcher{}

// Submit accepts and drops the run, reporting success with an empty run id.
func (NoopDispatcher) Submit(context.Context, RunSpec) (string, error) { return "", nil }

// Status reports success for any run id (no run ever actually failed because none ran).
func (NoopDispatcher) Status(context.Context, string) (RunStatus, error) {
	return RunStatus{Phase: "Succeeded"}, nil
}

const (
	// ingestDeadline bounds a github-ingest run (a paginated GitHub walk, then enrich
	// and rank). converseDeadline bounds a slower model turn.
	ingestDeadline   = 10 * time.Minute
	converseDeadline = 15 * time.Minute
	// reviewConcurrency bounds how many review turns a bulk action dispatches at once.
	reviewConcurrency = 8
)

// Ingestor turns web-tier actions into agent run requests and submits them through a
// Dispatcher. It implements worklist.Ingestor and worklist.BackfillProber.
type Ingestor struct {
	d          Dispatcher
	audience   string
	sinkURL    string
	aiEndpoint string
	aiModel    string
	log        *slog.Logger
}

var (
	_ worklist.Ingestor       = (*Ingestor)(nil)
	_ worklist.BackfillProber = (*Ingestor)(nil)
)

// New builds an Ingestor that submits runs through d. audience binds a run's
// credential to gofer's agent plane; sinkURL is the in-cluster URL a runtime would
// call back on (carried to each run as gofer_url); aiEndpoint/aiModel are the
// non-secret default chat-model coordinates carried to each run.
func New(d Dispatcher, audience, sinkURL string, log *slog.Logger, aiEndpoint, aiModel string) *Ingestor {
	if log == nil {
		log = slog.Default()
	}
	return &Ingestor{d: d, audience: audience, sinkURL: sinkURL, aiEndpoint: aiEndpoint, aiModel: aiModel, log: log}
}

// runParams builds the parameters shared by every dispatched run: the owner, the sink
// URL a runtime calls back on (gofer_url), and — when configured — the non-secret
// chat-model coordinates. The model token is never a parameter; a runtime would vend
// it from the broker (GET /agent/credential) per run.
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

// EnsureBackfill requests a github-ingest run for ownerID so an empty worklist gets
// populated. The default NoopDispatcher accepts and drops it, so the worklist stays
// empty (Discovering…) until a backend that executes runs is selected.
func (i *Ingestor) EnsureBackfill(ctx context.Context, ownerID string) error {
	return i.dispatchIngest(ctx, ownerID)
}

// Refresh forces a github-ingest run for ownerID so a user who just created or updated
// work can pull it onto the radar on demand.
func (i *Ingestor) Refresh(ctx context.Context, ownerID string) error {
	return i.dispatchIngest(ctx, ownerID)
}

// dispatchIngest submits a github-ingest run for ownerID.
func (i *Ingestor) dispatchIngest(ctx context.Context, ownerID string) error {
	_, err := i.d.Submit(ctx, RunSpec{
		TaskRef:    "github-ingest",
		Parameters: i.runParams(ownerID),
		Subject:    ownerID,
		Scopes:     []string{"signals:read", "metadata:write"},
		Audience:   i.audience,
		Deadline:   ingestDeadline,
	})
	return err
}

// BackfillFailure reports whether ownerID's most recent backfill failed. The default
// NoopDispatcher runs nothing, so none can fail and it reports "not failed"; a backend
// that executes runs surfaces genuine failures through Dispatcher.Status.
func (i *Ingestor) BackfillFailure(ctx context.Context, ownerID string) (bool, string, error) {
	return false, "", nil
}

// Converse requests a github-converse run: an assistant answers the user's latest
// thread turn on itemID with the chosen model (empty = the default). Accepted and
// dropped by NoopDispatcher.
func (i *Ingestor) Converse(ctx context.Context, ownerID, itemID, endpoint, model string) error {
	return i.dispatchConverse(ctx, ownerID, itemID, endpoint, model, false)
}

// SecondOpinion requests an INDEPENDENT (blind) review of one item by an alternative
// model — the review-panel action. A backend answers without the prior thread; a
// consensus synthesis is chained once that review writes back.
func (i *Ingestor) SecondOpinion(ctx context.Context, ownerID, itemID, endpoint, model string) error {
	if model == "" {
		return errors.New("ingest: second opinion requires a model")
	}
	return i.dispatchConverse(ctx, ownerID, itemID, endpoint, model, true)
}

// SecondOpinionAll runs the review panel on each item in itemIDs as a bounded parallel
// batch of independent reviews.
func (i *Ingestor) SecondOpinionAll(ctx context.Context, ownerID string, itemIDs []string, endpoint, model string) error {
	if model == "" {
		return errors.New("ingest: second opinion requires a model")
	}
	return i.dispatchEach(ctx, ownerID, itemIDs, endpoint, model, true)
}

// ReviewAll concurrently dispatches a github-converse review run for each item in
// itemIDs as a bounded parallel batch.
func (i *Ingestor) ReviewAll(ctx context.Context, ownerID string, itemIDs []string, endpoint, model string) error {
	return i.dispatchEach(ctx, ownerID, itemIDs, endpoint, model, false)
}

// dispatchEach submits a github-converse run for every item, bounded by
// reviewConcurrency, and joins any errors.
func (i *Ingestor) dispatchEach(ctx context.Context, ownerID string, itemIDs []string, endpoint, model string, independent bool) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
		sem  = make(chan struct{}, reviewConcurrency)
	)
	for _, id := range itemIDs {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := i.dispatchConverse(ctx, ownerID, id, endpoint, model, independent); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(id)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// dispatchConverse submits one github-converse run for itemID. A non-empty
// endpoint/model overrides the default connection for this turn; independent carries a
// blind-review flag so a runtime answers without the prior thread.
func (i *Ingestor) dispatchConverse(ctx context.Context, ownerID, itemID, endpoint, model string, independent bool) error {
	params := i.runParams(ownerID)
	params["item"] = itemID
	if endpoint != "" {
		params["ai_endpoint"] = endpoint
	}
	if model != "" {
		params["ai_model"] = model
	}
	if independent {
		params["independent"] = "true"
	}
	_, err := i.d.Submit(ctx, RunSpec{
		TaskRef:    "github-converse",
		Parameters: params,
		Subject:    ownerID,
		Scopes:     []string{"signals:read", "metadata:write"},
		Audience:   i.audience,
		Deadline:   converseDeadline,
	})
	return err
}

// NoteWriteback records that a run wrote its result back to gofer's agent plane. The
// default NoopDispatcher never writes back, so this is a no-op; a backend's write-backs
// would drive bulk-action timing (the click-to-last-review wall-clock) and the
// review-panel consensus synthesis.
func (i *Ingestor) NoteWriteback(runID string) {}
