// Package ingest turns an empty worklist into dispatched AEI runs. It implements
// worklist.Ingestor by submitting a github-ingest run for the user, so page
// rendering stays decoupled from GitHub retrieval.
package ingest

import (
	"context"
	"errors"
	"fmt"
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

// BatchObserver records the outcome of a batch action that fans out to many runs.
// It is gofer's app-level metrics seam: only the app knows the fanned-out runs are
// one action, so only gofer can time the aggregate (AEI owns the per-run timing
// beneath it). metrics.Metrics implements it; nil disables batch measurement.
type BatchObserver interface {
	// ObserveReviewAll reports a Review-all-PRs batch: its wall-clock (from the click
	// until the last PR's review wrote back), its size, and whether every dispatched
	// run wrote back before the batch timed out.
	ObserveReviewAll(d time.Duration, size int, completed bool)
}

// ingestDeadline bounds a github-ingest run. It has to cover the whole backfill: a
// paginated walk of every search query (the search API rations queries on a small
// per-minute budget, so a large worklist spends real time waiting for it), then the
// chained enrich and rank. It is generous because an expired ingest persists nothing
// at all — the run is the unit of work — and a worklist that never lands leaves the
// user staring at "Discovering your work…" forever.
const ingestDeadline = 10 * time.Minute

// retryAfter bounds how long a tracked backfill run is treated as in-flight before
// EnsureBackfill dispatches a fresh one. It matches the ingest run deadline, so a
// healthy run is never piled on while a lost or stuck run eventually retries.
const retryAfter = ingestDeadline

// defaultBatchTimeout bounds how long a Review-all batch waits for every dispatched
// review to write its result back before it is recorded as a timeout, so a run that
// fails and never writes back cannot leave the batch pending forever. It comfortably
// exceeds a github-converse deadline (~15m).
const defaultBatchTimeout = 30 * time.Minute

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

	obs          BatchObserver // app-level batch metrics; nil disables measurement
	batchTimeout time.Duration // Review-all give-up window (timeout if a run never lands)

	mu   sync.Mutex
	runs map[string]*backfill // ownerID -> most recent ingest run

	batchMu  sync.Mutex
	runIndex map[string]*reviewBatch // runID -> its Review-all batch (write-back correlation)

	// Review-panel synthesis: after an independent (blind) review writes back, a
	// consensus turn by the default model is chained onto the same thread. threads
	// appends that chained prompt to the item (nil disables chaining); synth maps an
	// independent-review run id to the item awaiting synthesis; spawn runs the chained
	// dispatch off the write-back path (overridable in tests for determinism).
	threads ThreadAppender
	synthMu sync.Mutex
	synth   map[string]synthesisPending
	spawn   func(func())
}

// reviewBatch tracks one "Review all PRs" action so its wall-clock — from the click
// until the last PR's review output writes back — is timed exactly, off the
// write-backs gofer already serves rather than by polling the control plane.
type reviewBatch struct {
	start   time.Time
	size    int
	pending map[string]struct{} // run ids whose review has not yet written back
	timer   *time.Timer
	done    bool
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
		d:            d,
		audience:     audience,
		sinkURL:      sinkURL,
		aiEndpoint:   aiEndpoint,
		aiModel:      aiModel,
		log:          log,
		batchTimeout: defaultBatchTimeout,
		runs:         map[string]*backfill{},
		runIndex:     map[string]*reviewBatch{},
		synth:        map[string]synthesisPending{},
		spawn:        func(f func()) { go f() },
	}
}

// SetBatchObserver installs the app-level batch metrics sink (the web tier wires
// metrics.Metrics). With none set, batch actions dispatch exactly as before and
// record nothing.
func (i *Ingestor) SetBatchObserver(o BatchObserver) { i.obs = o }

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
	return i.dispatchIngest(ctx, ownerID)
}

// Refresh forces a github-ingest run for ownerID regardless of any recent backfill,
// so a user who just created or updated work can pull it into the worklist on
// demand. github-ingest reconciles (upsert/merge), so the refresh updates the
// existing worklist in place — it never wipes it or duplicates items.
func (i *Ingestor) Refresh(ctx context.Context, ownerID string) error {
	return i.dispatchIngest(ctx, ownerID)
}

// dispatchIngest submits a github-ingest run for ownerID and tracks it so
// BackfillFailure can report its outcome. It is the shared body of EnsureBackfill
// (which first gates on a recent run) and Refresh (which forces one).
func (i *Ingestor) dispatchIngest(ctx context.Context, ownerID string) error {
	id, err := i.d.Submit(ctx, aei.RunSpec{
		TaskRef:    "github-ingest",
		Parameters: i.runParams(ownerID),
		Identity: aei.IdentityRequest{
			Subject:  ownerID,
			Scopes:   []string{"signals:read", "metadata:write"},
			Audience: i.audience,
		},
		Deadline: ingestDeadline,
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
// ownerID: an assistant answers the user's latest thread turn with the chosen model
// (empty = the default) and writes the reply back. Like EnsureBackfill it authorizes
// gofer's agent plane and binds the credential to it; the deadline is larger because
// a model turn is slow.
func (i *Ingestor) Converse(ctx context.Context, ownerID, itemID, endpoint, model string) error {
	_, err := i.submitConverse(ctx, ownerID, itemID, endpoint, model, false)
	return err
}

// submitConverse dispatches one github-converse run and returns its run id, so a
// batch caller (ReviewAll) can track the run to completion. It carries the item id
// and the owner's audience-bound agent-plane identity (ADR 0002). A non-empty
// endpoint/model overrides the default connection for this turn (the token is shared
// across connections); empty values use the default connection's coordinates carried
// in the run parameters. When independent, it carries independent=true so the runtime
// answers blind — omitting the prior thread — for a review-panel review.
func (i *Ingestor) submitConverse(ctx context.Context, ownerID, itemID, endpoint, model string, independent bool) (string, error) {
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
	return i.dispatchConverse(ctx, ownerID, params)
}

// SecondOpinion runs a "review panel" on one item: it dispatches an INDEPENDENT
// (blind) review by the given alternative connection+model — the runtime answers
// without the prior thread, so it forms its own view — then, once that review writes
// back, a consensus synthesis turn by the default model is chained onto the same
// thread (see NoteWriteback -> dispatchSynthesis). The runtime stamps each reply with
// its model so the UI can attribute them. It errors when no model is given.
func (i *Ingestor) SecondOpinion(ctx context.Context, ownerID, itemID, endpoint, model string) error {
	if model == "" {
		return errors.New("ingest: second opinion requires a model")
	}
	runID, err := i.submitConverse(ctx, ownerID, itemID, endpoint, model, true)
	if err != nil {
		return err
	}
	i.registerSynthesis(runID, ownerID, itemID)
	return nil
}

// SecondOpinionAll runs the review panel on each item in itemIDs (all owned by
// ownerID) as a bounded parallel batch: for each it dispatches an INDEPENDENT (blind)
// review by the given alternative connection+model and registers the consensus
// synthesis that chains once that review writes back (see SecondOpinion). The
// independent-review prompt rides each item's stored thread, so the caller appends it
// first. It errors when no model is given, and joins any dispatch failures.
func (i *Ingestor) SecondOpinionAll(ctx context.Context, ownerID string, itemIDs []string, endpoint, model string) error {
	if model == "" {
		return errors.New("ingest: second opinion requires a model")
	}
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
			runID, err := i.submitConverse(ctx, ownerID, id, endpoint, model, true)
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("second opinion %s: %w", id, err))
				mu.Unlock()
				return
			}
			i.registerSynthesis(runID, ownerID, id)
		}(id)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// dispatchConverse submits a github-converse run with the given parameters under
// the owner's audience-bound agent-plane identity (ADR 0002).
func (i *Ingestor) dispatchConverse(ctx context.Context, ownerID string, params map[string]string) (string, error) {
	return i.d.Submit(ctx, aei.RunSpec{
		TaskRef:    "github-converse",
		Parameters: params,
		Identity: aei.IdentityRequest{
			Subject:  ownerID,
			Scopes:   []string{"signals:read", "metadata:write"},
			Audience: i.audience,
		},
		Deadline: 15 * time.Minute,
	})
}

// reviewConcurrency bounds how many github-converse runs ReviewAll dispatches at
// once, so reviewing a large radar launches a parallel batch without opening an
// unbounded burst of submits to the control plane.
const reviewConcurrency = 8

// ReviewAll concurrently dispatches a github-converse run for each item in itemIDs
// (all owned by ownerID) as a bounded parallel batch, rather than a slow serial
// sweep. Each run reviews with the given connection endpoint+model (empty uses the
// default connection). Each dispatch is a quick, non-blocking control-plane submit;
// ReviewAll waits for them all and returns their failures joined, so one failed
// dispatch never stops the others. The per-item review prompt rides that item's
// stored thread, so the caller must append it before calling this.
func (i *Ingestor) ReviewAll(ctx context.Context, ownerID string, itemIDs []string, endpoint, model string) error {
	start := time.Now()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
		ids  []string
		sem  = make(chan struct{}, reviewConcurrency)
	)
	for _, id := range itemIDs {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			runID, err := i.submitConverse(ctx, ownerID, id, endpoint, model, false)
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("review %s: %w", id, err))
				mu.Unlock()
				return
			}
			if runID != "" {
				mu.Lock()
				ids = append(ids, runID)
				mu.Unlock()
			}
		}(id)
	}
	wg.Wait()
	// Register the batch so its wall-clock is timed off the runtimes' write-backs:
	// each github-converse run POSTs its review to gofer's agent plane, and when the
	// last run in this batch writes back (NoteWriteback), the exact click-to-last-review
	// duration is recorded. Only gofer knows these runs are one action, so only gofer
	// can time the aggregate (AEI owns the per-run timing beneath it).
	if i.obs != nil && len(ids) > 0 {
		i.registerReviewBatch(start, ids)
	}
	return errors.Join(errs...)
}

// registerReviewBatch begins timing a Review-all batch. It records the dispatched
// run ids so NoteWriteback can mark each review as it lands, and arms a timeout so a
// run that fails and never writes back cannot leave the batch pending forever. The
// timer is created before the batch is published to runIndex, so a write-back can
// never observe a nil timer.
func (i *Ingestor) registerReviewBatch(start time.Time, runIDs []string) {
	b := &reviewBatch{
		start:   start,
		size:    len(runIDs),
		pending: make(map[string]struct{}, len(runIDs)),
	}
	for _, id := range runIDs {
		b.pending[id] = struct{}{}
	}
	b.timer = time.AfterFunc(i.batchTimeout, func() { i.finishReviewBatch(b) })
	i.batchMu.Lock()
	for id := range b.pending {
		i.runIndex[id] = b
	}
	i.batchMu.Unlock()
}

// NoteWriteback records that the run runID wrote its result back to gofer's agent
// plane (a POST /agent/worklist). For a Review-all batch this is the exact instant
// that PR's review output landed; when the batch's last run writes back, its
// wall-clock (click -> last review returned) is recorded. It is idempotent per run —
// a run's later writes (e.g. a chained research adjustment) are no-ops once its first
// write-back is counted — and ignores runs that belong to no batch (a backfill, a
// single Discuss turn). Safe for concurrent use.
func (i *Ingestor) NoteWriteback(runID string) {
	if runID == "" {
		return
	}
	i.maybeSynthesize(runID)
	i.batchMu.Lock()
	b := i.runIndex[runID]
	if b == nil {
		i.batchMu.Unlock()
		return
	}
	delete(i.runIndex, runID)
	delete(b.pending, runID)
	last := len(b.pending) == 0
	i.batchMu.Unlock()
	if last && i.finishReviewBatch(b) {
		b.timer.Stop()
	}
}

// finishReviewBatch finalizes a batch exactly once (the done guard), recording its
// wall-clock, size, and outcome. It is called by the last write-back and by the
// timeout; whichever wins derives completed from whether every run has landed, so a
// batch that finishes at the timeout boundary is still recorded correctly. It
// returns whether this call was the one that finalized.
func (i *Ingestor) finishReviewBatch(b *reviewBatch) bool {
	i.batchMu.Lock()
	if b.done {
		i.batchMu.Unlock()
		return false
	}
	b.done = true
	completed := len(b.pending) == 0
	for id := range b.pending { // drop any stragglers so a late write-back routes nowhere
		delete(i.runIndex, id)
	}
	i.batchMu.Unlock()
	i.obs.ObserveReviewAll(time.Since(b.start), b.size, completed)
	return true
}

// ThreadAppender appends a user turn to an item's stored thread. The Ingestor uses it
// to chain the review-panel synthesis prompt after an independent review lands; a nil
// appender disables synthesis chaining. kind tags the turn (see worklist.Message.Kind).
// It returns false when the item is not found.
type ThreadAppender interface {
	AppendUserTurn(ctx context.Context, ownerID, itemID, content, kind string) (bool, error)
}

// SetThreadAppender installs the store-backed thread writer used to chain a
// review-panel synthesis turn after an independent review lands. With none set, the
// panel dispatches the independent review but does not chain the consensus synthesis.
func (i *Ingestor) SetThreadAppender(a ThreadAppender) { i.threads = a }

// synthesisPending is one independent review awaiting its consensus synthesis turn.
type synthesisPending struct {
	ownerID string
	itemID  string
}

// synthesisPrompt is the consensus turn dispatched to the default model once an
// independent review lands: it weighs every review already in the thread and rules
// definitively on whether they agree. Its leading AGREEMENT/DISAGREEMENT token is
// lifted into Message.Verdict by the runtime (see worklist.ParseVerdict), so the radar
// can show the outcome without re-reading the prose.
const synthesisPrompt = "Do all the reviews of this PR in this conversation agree with one another? You may call out subtle differences, but it is your job to state definitively whether or not there is agreement, or disagreement. Begin your reply with a single line containing exactly one word — AGREEMENT if the reviews substantively agree, or DISAGREEMENT if they do not — and nothing else on that line. Then, on the following lines, explain your reasoning."

// registerSynthesis records that the independent-review run runID, once it writes
// back, should chain a consensus synthesis for (ownerID, itemID). It no-ops without a
// thread appender (chaining disabled) or an empty run id.
func (i *Ingestor) registerSynthesis(runID, ownerID, itemID string) {
	if runID == "" || i.threads == nil {
		return
	}
	i.synthMu.Lock()
	i.synth[runID] = synthesisPending{ownerID: ownerID, itemID: itemID}
	i.synthMu.Unlock()
}

// maybeSynthesize fires the chained consensus synthesis when runID is a registered
// independent review. It clears the registration first (so a re-delivered write-back
// cannot double-fire, and the synthesis's own write-back \u2014 never registered \u2014 chains
// nothing), then runs the dispatch off the write-back path.
func (i *Ingestor) maybeSynthesize(runID string) {
	i.synthMu.Lock()
	p, ok := i.synth[runID]
	if ok {
		delete(i.synth, runID)
	}
	i.synthMu.Unlock()
	if ok {
		i.spawn(func() { i.dispatchSynthesis(p.ownerID, p.itemID) })
	}
}

// dispatchSynthesis appends the synthesis prompt to the item's thread and dispatches a
// consensus turn by the DEFAULT model (no per-turn endpoint/model override, not
// independent) so it weighs every review in the thread. It runs off the write-back
// path, so failures are logged rather than returned.
func (i *Ingestor) dispatchSynthesis(ownerID, itemID string) {
	ctx := context.Background()
	ok, err := i.threads.AppendUserTurn(ctx, ownerID, itemID, synthesisPrompt, worklist.KindSynthesisRequest)
	if err != nil {
		i.log.Warn("review-panel synthesis: append failed", "owner", ownerID, "item", itemID, "error", err)
		return
	}
	if !ok {
		return // item gone before synthesis could chain
	}
	if _, err := i.submitConverse(ctx, ownerID, itemID, "", "", false); err != nil {
		i.log.Warn("review-panel synthesis: dispatch failed", "owner", ownerID, "item", itemID, "error", err)
	}
}

// StoreThreadAppender adapts a worklist.Store to ThreadAppender, so the ingestor can
// chain a review-panel synthesis prompt onto an item's stored thread.
func StoreThreadAppender(store worklist.Store) ThreadAppender {
	return storeAppender{store: store, now: time.Now}
}

type storeAppender struct {
	store worklist.Store
	now   func() time.Time
}

func (s storeAppender) AppendUserTurn(ctx context.Context, ownerID, itemID, content, kind string) (bool, error) {
	items, err := s.store.List(ctx, ownerID)
	if err != nil {
		return false, err
	}
	for _, it := range items {
		if it.ID == itemID {
			it.Thread = append(it.Thread, worklist.Message{Role: worklist.RoleUser, Content: content, Kind: kind, At: s.now().UTC()})
			return true, s.store.Upsert(ctx, ownerID, it)
		}
	}
	return false, nil
}
