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
	"fmt"
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
//
// Submit's error says whether the run might exist: an error wrapping ErrNotAccepted
// means the backend definitely did not accept it (it answered with a refusal, such as
// an overload, or the request never left gofer); any other error leaves acceptance
// unknown (a response lost after the request was sent), and gofer then treats an
// ingest run as possibly running until its deadline.
type Dispatcher interface {
	Submit(ctx context.Context, spec RunSpec) (runID string, err error)
	Status(ctx context.Context, runID string) (RunStatus, error)
}

// ErrNotAccepted marks a Submit error after which the backend certainly holds no run.
var ErrNotAccepted = errors.New("the backend did not accept the run")

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
	// retryAfter bounds how long a tracked backfill run is treated as in-flight before
	// EnsureBackfill dispatches a fresh one. It matches the ingest run deadline, so a
	// healthy run is never piled on while a lost or stuck run eventually retries.
	retryAfter = ingestDeadline
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

	mu   sync.Mutex
	runs map[runKey]*run // the most recent run per key: the in-flight gate (gate.go)
	// gens counts, per owner, the ingest runs the backend has accepted; each accepted
	// run takes the next generation (run.gen). dones is the highest generation known
	// to have ended (failed, succeeded, or past its deadline): the refresh version a
	// worklist page compares to notice results it does not show (RefreshState).
	gens, dones map[string]uint64
	ledger      RunLedger  // nil: all of this is lost on restart (ledger.go)
	persistMu   sync.Mutex // serializes ledger writes (persist)
}

var (
	_ worklist.Ingestor          = (*Ingestor)(nil)
	_ worklist.BackfillProber    = (*Ingestor)(nil)
	_ worklist.BackfillCompleter = (*Ingestor)(nil)
)

// New builds an Ingestor that submits runs through d. audience binds a run's
// credential to gofer's agent plane; sinkURL is the in-cluster URL a runtime would
// call back on (carried to each run as gofer_url); aiEndpoint/aiModel are the
// non-secret default chat-model coordinates carried to each run.
func New(d Dispatcher, audience, sinkURL string, log *slog.Logger, aiEndpoint, aiModel string) *Ingestor {
	if log == nil {
		log = slog.Default()
	}
	return &Ingestor{d: d, audience: audience, sinkURL: sinkURL, aiEndpoint: aiEndpoint, aiModel: aiModel, log: log, runs: map[runKey]*run{}, gens: map[string]uint64{}, dones: map[string]uint64{}}
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
// populated. It dispatches at most one run at a time per owner: while a recent,
// non-failed run is still tracked this is a no-op, so the empty-worklist poll that
// drives the "Discovering…" view cannot pile up runs against a backend that really
// executes them. A failed or stale run (older than retryAfter) is re-dispatched, so a
// reload retries after a failure or a lost run.
func (i *Ingestor) EnsureBackfill(ctx context.Context, ownerID string) error {
	_, err := i.dispatchIngest(ctx, ownerID, ingestGate)
	return err
}

// Refresh requests a github-ingest run for ownerID so a user who just created or
// updated work can pull it onto the radar on demand. Like a scheduled tick, it starts
// nothing while an ingest run for the owner is still running: two runs at once could
// leave the older one's GitHub fields stored. The radar reloads until the running
// pass lands (Active), and a click after it has ended dispatches at once.
func (i *Ingestor) Refresh(ctx context.Context, ownerID string) error {
	_, err := i.dispatchIngest(ctx, ownerID, refreshGate)
	return err
}

// ScheduledRefresh requests a github-ingest run for ownerID on behalf of the
// periodic refresh (Refresher), and reports whether it submitted one. It submits
// nothing while any ingest run for the owner is in flight, asking the backend about a
// run that still looks live, so overlapping ticks (and a tick during a backfill or a
// Refresh) share one run per owner.
func (i *Ingestor) ScheduledRefresh(ctx context.Context, ownerID string) (bool, error) {
	return i.dispatchIngest(ctx, ownerID, refreshGate)
}

// RefreshState reports ownerID's refresh version — the highest generation of an
// ingest run known to have ended — and whether an ingest run is in flight, asking the
// backend about one that still looks live (Active). The worklist polls it
// (webui.RefreshStatus) and reloads when the version passes the one it rendered: a
// run that ended since, even one that started and ended between two polls, or one
// that ended while the next run was already going. err is a failed status read;
// active is then still true until the run's deadline.
func (i *Ingestor) RefreshState(ctx context.Context, ownerID string) (done uint64, active bool, err error) {
	active, err = i.Active(ctx, ownerID)
	i.mu.Lock()
	done = i.dones[ownerID]
	i.mu.Unlock()
	return done, active, err
}

// LocalRefreshState is RefreshState from what gofer already knows, without asking the
// backend, so rendering the worklist never waits on it. A run gofer has not seen end
// counts as in flight (the page's first poll asks the backend), until its deadline,
// when it counts as ended. Every run up to done ended before this returned, so items
// read after it hold everything those runs wrote.
func (i *Ingestor) LocalRefreshState(ownerID string) (done uint64, active bool) {
	key := ingestKey(ownerID)
	now := time.Now()
	i.mu.Lock()
	cur := i.runs[key]
	expired := cur != nil && now.Sub(cur.at) >= cur.g.ttl
	changed := expired && i.markDoneLocked(key, cur)
	active = cur != nil && !expired &&
		(cur.pending || cur.uncertain || (cur.id != "" && !cur.failed && !cur.succeeded))
	done = i.dones[ownerID]
	i.mu.Unlock()
	if changed {
		_ = i.persist(ownerID)
	}
	return done, active
}

// Active reports whether a github-ingest run for ownerID is in flight: being
// submitted, submitted and not yet seen to end, or of unknown acceptance (uncertain).
// It asks the backend about a run that still looks live (once per call, until the run
// is seen to end). A run past its deadline counts as ended, and raises the refresh
// version, so a page that slept through a run learns it ended. One the backend gave
// no id to probe (NoopDispatcher) is not in flight. A failed status read reports
// true, with the error: the run may still be going, and the page should keep
// watching until it ends or its deadline passes.
func (i *Ingestor) Active(ctx context.Context, ownerID string) (bool, error) {
	key := ingestKey(ownerID)
	now := time.Now()
	i.mu.Lock()
	cur := i.runs[key]
	var pending, uncertain, stale, noID, changed bool
	if cur != nil {
		pending, uncertain = cur.pending, cur.uncertain
		stale = now.Sub(cur.at) >= cur.g.ttl
		noID = cur.id == ""
		if stale {
			changed = i.markDoneLocked(key, cur)
		}
	}
	i.mu.Unlock()
	if changed {
		_ = i.persist(ownerID)
	}
	switch {
	case cur == nil || stale:
		return false, nil
	case pending || uncertain:
		return true, nil
	case noID:
		return false, nil // NoopDispatcher: nothing to wait for
	}
	failed, succeeded, _, err := i.probeRun(ctx, key)
	if err != nil {
		return true, err // unknown: keep watching, bounded by the deadline above
	}
	return !failed && !succeeded, nil
}

// dispatchIngest submits a github-ingest run for ownerID under gate g and tracks it,
// so BackfillFailure can report its outcome and EnsureBackfill can gate on it, and
// reports whether it submitted one. It submits nothing while the owner's ingest run
// is in flight. With a ledger, the reservation is recorded before the backend sees
// the run, so a restart while the backend's answer is outstanding (or lost) still
// holds the owner; if it cannot be recorded, nothing is submitted.
func (i *Ingestor) dispatchIngest(ctx context.Context, ownerID string, g gate) (bool, error) {
	key := ingestKey(ownerID)
	mine, ok := i.reserve(ctx, key, g)
	if !ok {
		return false, nil
	}
	if err := i.persist(ownerID); err != nil {
		i.settle(key, mine, "", ErrNotAccepted)
		return false, fmt.Errorf("ingest: record the reservation before dispatch: %w", err)
	}
	id, err := i.d.Submit(ctx, RunSpec{
		TaskRef:    taskIngest,
		Parameters: i.runParams(ownerID),
		Subject:    ownerID,
		Scopes:     []string{"signals:read", "metadata:write"},
		Audience:   i.audience,
		Deadline:   ingestDeadline,
	})
	i.settle(key, mine, id, err)
	_ = i.persist(ownerID) // accepted, rolled back, or uncertain: record what the gate now holds
	return err == nil, err
}

// probeRun reports the lifecycle of the run tracked for key, reading it from the
// backend (Dispatcher.Status) only while the run is still in flight: once a run is
// observed terminal the outcome is cached on the run, so the empty-worklist poll costs
// one status read per RUN rather than one per page render. A transient read error is
// returned so the caller keeps waiting rather than declaring an outcome.
func (i *Ingestor) probeRun(ctx context.Context, key runKey) (failed, succeeded bool, message string, err error) {
	i.mu.Lock()
	cur := i.runs[key]
	var (
		id                      string
		curFailed, curSucceeded bool
		curMessage              string
	)
	if cur != nil {
		id, curFailed, curSucceeded, curMessage = cur.id, cur.failed, cur.succeeded, cur.message
	}
	i.mu.Unlock()
	switch {
	case cur == nil:
		return false, false, "", nil // nothing dispatched yet
	case curFailed:
		return true, false, curMessage, nil
	case curSucceeded:
		return false, true, "", nil
	case id == "":
		return false, false, "", nil // not submitted yet, or the backend reported no run to probe
	}
	res, err := i.d.Status(ctx, id)
	if err != nil {
		return false, false, "", err // transient; keep waiting
	}
	switch res.Phase {
	case "Failed":
		i.mu.Lock()
		// Record on the run we probed; a concurrent dispatch may have moved on.
		changed := i.markDoneLocked(key, cur)
		if i.runs[key] == cur {
			cur.failed = true
			cur.message = res.Message
			if !cur.logged {
				i.log.Warn("run failed", "task", key.task, "owner", key.owner, "item", key.item, "turn", key.turn, "run_id", id, "error", res.Message)
				cur.logged = true
			}
		}
		i.mu.Unlock()
		if changed {
			_ = i.persist(key.owner)
		}
		return true, false, res.Message, nil
	case "Succeeded":
		i.mu.Lock()
		changed := i.markDoneLocked(key, cur)
		if i.runs[key] == cur {
			cur.succeeded = true
		}
		i.mu.Unlock()
		if changed {
			_ = i.persist(key.owner)
		}
		return false, true, "", nil
	}
	return false, false, "", nil // still running
}

// BackfillFailure reports whether ownerID's most recent backfill run has failed, with
// the run's failure message, by reading its lifecycle from the backend. The failure is
// logged once. A run still in flight, one that succeeded, or a transient status-read
// error reports failed=false so the caller keeps waiting. With a backend that runs
// nothing (NoopDispatcher) no run can fail, so this always reports "not failed".
func (i *Ingestor) BackfillFailure(ctx context.Context, ownerID string) (bool, string, error) {
	failed, _, message, err := i.probeRun(ctx, ingestKey(ownerID))
	if err != nil {
		return false, "", err
	}
	return failed, message, nil
}

// BackfillSucceeded reports whether ownerID's most recent backfill run completed
// successfully. The read model uses it to tell "finished and found nothing" apart from
// "still discovering", so a user with no open work sees an empty radar rather than a
// permanent spinner. A backend that runs nothing (NoopDispatcher) reports no run id, so
// this stays false and the worklist keeps its "Discovering…" state.
func (i *Ingestor) BackfillSucceeded(ctx context.Context, ownerID string) (bool, error) {
	_, succeeded, _, err := i.probeRun(ctx, ingestKey(ownerID))
	if err != nil {
		return false, err
	}
	return succeeded, nil
}

// Converse requests a github-converse run: an assistant answers the user turn ref
// names with the chosen model (empty = the default). Accepted and dropped by
// NoopDispatcher.
func (i *Ingestor) Converse(ctx context.Context, ownerID string, ref worklist.TurnRef, endpoint, model string) error {
	return i.dispatchConverse(ctx, ownerID, ref, endpoint, model, false)
}

// SecondOpinion requests an INDEPENDENT (blind) review of one item by an alternative
// model — the review-panel action. A backend answers without the prior thread; a
// consensus synthesis is chained once that review writes back.
func (i *Ingestor) SecondOpinion(ctx context.Context, ownerID string, ref worklist.TurnRef, endpoint, model string) error {
	if model == "" {
		return errors.New("ingest: second opinion requires a model")
	}
	return i.dispatchConverse(ctx, ownerID, ref, endpoint, model, true)
}

// SecondOpinionAll runs the review panel on each turn in refs as a bounded parallel
// batch of independent reviews.
func (i *Ingestor) SecondOpinionAll(ctx context.Context, ownerID string, refs []worklist.TurnRef, endpoint, model string) error {
	if model == "" {
		return errors.New("ingest: second opinion requires a model")
	}
	return i.dispatchEach(ctx, ownerID, refs, endpoint, model, true)
}

// ReviewAll concurrently dispatches a github-converse review run for each turn in
// refs as a bounded parallel batch.
func (i *Ingestor) ReviewAll(ctx context.Context, ownerID string, refs []worklist.TurnRef, endpoint, model string) error {
	return i.dispatchEach(ctx, ownerID, refs, endpoint, model, false)
}

// dispatchEach submits a github-converse run for every turn, bounded by
// reviewConcurrency, and joins any errors.
func (i *Ingestor) dispatchEach(ctx context.Context, ownerID string, refs []worklist.TurnRef, endpoint, model string, independent bool) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
		sem  = make(chan struct{}, reviewConcurrency)
	)
	for _, ref := range refs {
		wg.Add(1)
		sem <- struct{}{}
		go func(ref worklist.TurnRef) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := i.dispatchConverse(ctx, ownerID, ref, endpoint, model, independent); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(ref)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// dispatchConverse submits one github-converse run answering ref, unless a run for
// the same owner, item and turn is already in flight (see runKey): a double submit,
// or a retry while the first run is still working, then shares that run. A non-empty
// endpoint/model overrides the default connection for this turn; independent carries a
// blind-review flag so a runtime answers without the prior thread.
func (i *Ingestor) dispatchConverse(ctx context.Context, ownerID string, ref worklist.TurnRef, endpoint, model string, independent bool) error {
	params := i.runParams(ownerID)
	params["item"] = ref.Item
	params["turn"] = ref.Turn
	if endpoint != "" {
		params["ai_endpoint"] = endpoint
	}
	if model != "" {
		params["ai_model"] = model
	}
	if independent {
		params["independent"] = "true"
	}
	key := runKey{owner: ownerID, task: taskConverse, item: ref.Item, turn: ref.Turn}
	mine, ok := i.reserve(ctx, key, converseGate)
	if !ok {
		return nil // a run for this turn is already in flight
	}
	id, err := i.d.Submit(ctx, RunSpec{
		TaskRef:    taskConverse,
		Parameters: params,
		Subject:    ownerID,
		Scopes:     []string{"signals:read", "metadata:write"},
		Audience:   i.audience,
		Deadline:   converseDeadline,
	})
	i.settle(key, mine, id, err)
	return err
}

// NoteWriteback records that a run wrote its result back to gofer's agent plane. The
// default NoopDispatcher never writes back, so this is a no-op; a backend's write-backs
// would drive bulk-action timing (the click-to-last-review wall-clock) and the
// review-panel consensus synthesis.
func (i *Ingestor) NoteWriteback(runID string) {}
