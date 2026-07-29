// Package agent is the agent-runtime WORKLOAD: the work an agent runtime executes on
// gofer's behalf. Its entrypoint Run dispatches on job type (github-ingest,
// github-enrich, llm-rank, github-converse, github-research) and works against small
// seams (Vendor, Sink), so it behaves identically no matter what substrate runs it —
// an agent-runtime backend calls Run behind its Dispatcher (see internal/runtime).
//
// It is deliberately provider-direct: gofer is a credential broker, not a data broker,
// so the workload vends the user's credential and connects to GitHub directly, then
// writes results back through the Sink — it never sees the user's raw token until it
// is vended, and never writes anywhere but gofer.
package agent

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/jackfrancis/gofer/internal/github"
	"github.com/jackfrancis/gofer/internal/httpretry"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// Runtime job types — the contract between dispatch (the run's TaskRef) and the
// runtime.
const (
	JobIngest   = "github-ingest"
	JobEnrich   = "github-enrich"
	JobRank     = "llm-rank"
	JobConverse = "github-converse"
	JobResearch = "github-research"
)

// Vendor exchanges a run's authorization for the acting user's delegated provider
// credential. In-process it reads the vault; out-of-process it calls gofer's
// credential broker (GET /agent/credential). The runtime never holds a standing secret.
type Vendor interface {
	Vend(ctx context.Context, provider string) (accessToken string, err error)
}

// Sink is the runtime's view of gofer's worklist store: read the acting user's
// persisted work, and write items back. In-process it is the store directly;
// out-of-process it is an HTTP client to gofer's agent sink.
type Sink interface {
	List(ctx context.Context) ([]worklist.WorkItem, error)
	Ingest(ctx context.Context, items []worklist.WorkItem) error
}

// Params configures a single runtime invocation.
type Params struct {
	JobType       string
	Provider      string                     // e.g. "github"
	GitHubBaseURL string                     // GitHub API base; empty uses the public API
	Client        *http.Client               // shared HTTP client
	Ranker        worklist.AxisRanker        // llm-rank ranker; nil uses the stub
	RankLimit     int                        // max items to rank; 0 uses the default
	ItemID        string                     // target item for a per-item job (github-converse/research)
	Model         string                     // chat-model identifier stamped on agent replies (converse)
	Converser     worklist.Conversationalist // github-converse assistant; required for that job
	Independent   bool                       // converse: answer blind — omit prior thread turns (independent review)
	Researcher    worklist.ResearchRanker    // github-research re-ranker; required for that job
	EnrichLimit   int                        // optional cap on items to enrich; 0 enriches every item
	Logger        *slog.Logger               // phase logger; nil uses slog.Default()
}

// logger returns p.Logger, or the process default when unset.
func (p Params) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

const (
	// defaultRankLimit is a SPEND ceiling, not a correctness bound: ranking costs one
	// model call per item, so a very large worklist stops short rather than running up
	// an unbounded bill. Unlike enrich it can afford to — it refines axes the
	// deterministic baseline already computed for every item.
	defaultRankLimit = 200
	rankConcurrency  = 8
	// rankReserve is the slice of the run's remaining time ranking leaves for writing
	// its proposals back.
	rankReserve = 30 * time.Second
	// writeBackChunk bounds how many items one write-back carries. The agent sink caps
	// a request body at 8 MiB and every item carries its whole conversation thread, so
	// a large worklist is written in batches: no single request approaches the limit,
	// and a failure part-way still lands the earlier batches.
	writeBackChunk = 25
)

// hasBudget reports whether the run has more than reserve left before its deadline; a
// context without a deadline always has budget. Every stage that fans out inside a
// deadline-bounded run consults it, because an expired run persists NOTHING — a
// partial pass that lands always beats a complete one that never returns.
func hasBudget(ctx context.Context, reserve time.Duration) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return time.Until(deadline) > reserve
}

// writeBack persists items through the sink in bounded batches (writeBackChunk).
func writeBack(ctx context.Context, sink Sink, items []worklist.WorkItem) error {
	for start := 0; start < len(items); start += writeBackChunk {
		if err := sink.Ingest(ctx, items[start:min(start+writeBackChunk, len(items))]); err != nil {
			return err
		}
	}
	return nil
}

// Run executes the job selected by p.JobType against the given seams. The
// in-process launcher and the standalone runtime both call it, so the runtime
// behaves identically regardless of substrate.
func Run(ctx context.Context, p Params, vendor Vendor, sink Sink) error {
	// Make the runtime's outbound HTTP resilient to transient connectivity blips
	// (flaky cluster DNS, a provider hiccup) with a bounded, conservative retry.
	if p.Client == nil {
		p.Client = &http.Client{Timeout: 30 * time.Second}
	}
	p.Client = httpretry.Wrap(p.Client)

	switch p.JobType {
	case JobIngest:
		return runIngest(ctx, p, vendor, sink)
	case JobEnrich:
		return runEnrich(ctx, p, vendor, sink)
	case JobRank:
		return runRank(ctx, p, sink)
	case JobConverse:
		return runConverse(ctx, p, vendor, sink)
	case JobResearch:
		return runResearch(ctx, p, sink)
	default:
		return fmt.Errorf("agent: unknown job type %q", p.JobType)
	}
}

// runIngest vends the provider credential, fetches the user's work directly from
// GitHub, and writes it to the sink. An empty result is a successful no-op.
func runIngest(ctx context.Context, p Params, vendor Vendor, sink Sink) error {
	log := p.logger()
	provider := p.Provider
	if provider == "" {
		provider = "github"
	}
	token, err := vendor.Vend(ctx, provider)
	if err != nil {
		return fmt.Errorf("vend credential: %w", err)
	}
	gh := github.NewClient(p.Client, p.GitHubBaseURL)
	items, err := gh.FetchWorklist(ctx, token)
	if err != nil {
		return fmt.Errorf("fetch github: %w", err)
	}
	log.Info("ingest: fetched work items", "count", len(items))
	if len(items) == 0 {
		return nil
	}
	// Reconcile with what gofer already persists so a re-ingest (a Refresh, or an
	// empty-worklist backfill) never clobbers per-item state GitHub cannot supply —
	// the conversation thread and its read cursor, and the user/agent metadata
	// overrides. The fresh GitHub fields and base signals come from the fetch; the
	// chained enrich+rank re-derive the score. If the existing items cannot be read we
	// proceed unreconciled rather than skip the refresh — a rare case, logged loudly.
	if existing, lerr := sink.List(ctx); lerr == nil {
		items = reconcileFetched(items, existing)
	} else {
		log.Warn("ingest: could not read existing items to reconcile; threads may reset", "err", lerr)
	}
	if err := writeBack(ctx, sink, items); err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	// The backfill continues into enrich (per-item timeline signals) then rank
	// (the model proposal), so one pass yields a fully scored radar. Both are
	// best-effort: a failure leaves the baseline-scored items in place. The
	// separate JobEnrich / JobRank exist for an independent scheduled refresh.
	if err := runEnrich(ctx, p, vendor, sink); err != nil {
		log.Warn("backfill enrich failed", "err", err)
	}
	if err := runRank(ctx, p, sink); err != nil {
		log.Warn("backfill rank failed", "err", err)
	}
	log.Info("ingest: backfill complete", "items", len(items))
	return nil
}

// reconcileFetched carries gofer-owned per-item state that GitHub cannot supply — the
// conversation thread and its read cursor, the user/agent metadata overrides (hidden,
// completed), and the original creation time — from the persisted items onto the
// freshly fetched ones, keyed by ID. Fetched items with no persisted counterpart (new
// work) pass through unchanged. This makes a re-ingest reconcile in place instead of
// wiping the conversation, which is what a github-ingest write-back would otherwise do
// (the fetch has no thread, and the store upsert replaces the whole item).
func reconcileFetched(fetched, existing []worklist.WorkItem) []worklist.WorkItem {
	prior := make(map[string]worklist.WorkItem, len(existing))
	for _, it := range existing {
		prior[it.ID] = it
	}
	for i := range fetched {
		old, ok := prior[fetched[i].ID]
		if !ok {
			continue
		}
		fetched[i].Thread = old.Thread
		fetched[i].ThreadReadAt = old.ThreadReadAt
		fetched[i].Meta.HiddenAt = old.Meta.HiddenAt
		fetched[i].Meta.CompletedAt = old.Meta.CompletedAt
		if !old.CreatedAt.IsZero() {
			fetched[i].CreatedAt = old.CreatedAt
		}
	}
	return fetched
}

// runRank reads the user's persisted work, asks the AxisRanker to propose the
// four axes per item (bounded concurrency), and writes back the items that
// received a proposal. gofer blends the proposal into Rank at read time. With the
// stub ranker this is a no-op over ordering; a real model is swapped in behind the
// AxisRanker seam. Per-item ranking is best-effort.
func runRank(ctx context.Context, p Params, sink Sink) error {
	log := p.logger()
	ranker := p.Ranker
	stub := ranker == nil
	if ranker == nil {
		ranker = worklist.NewStubRanker()
	}
	items, err := sink.List(ctx)
	if err != nil {
		return fmt.Errorf("list worklist: %w", err)
	}
	limit := p.RankLimit
	if limit <= 0 {
		limit = defaultRankLimit
	}
	if len(items) > limit {
		// Spend the model budget on the items the cheap, deterministic scorer already
		// rates highest rather than on an arbitrary slice. enrich has just run, so those
		// baselines are computed from complete signals.
		now := time.Now().UTC()
		baseline := make(map[string]float64, len(items))
		for _, it := range items {
			baseline[it.ID] = worklist.Score(it, now).Rank
		}
		slices.SortStableFunc(items, func(a, b worklist.WorkItem) int {
			return cmp.Compare(baseline[b.ID], baseline[a.ID])
		})
		items = items[:limit]
	}
	log.Info("rank: proposing axes", "items", len(items), "stub", stub)

	var (
		mu      sync.Mutex
		changed []worklist.WorkItem
		wg      sync.WaitGroup
		sem     = make(chan struct{}, rankConcurrency)
		skipped int
	)
	for i := range items {
		if !hasBudget(ctx, rankReserve) {
			skipped = len(items) - i
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			prop, err := ranker.Propose(ctx, items[i])
			if err != nil {
				return // best-effort: leave the item unchanged
			}
			items[i].Signals.Proposed = &prop
			mu.Lock()
			changed = append(changed, items[i])
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	log.Info("rank: applied proposals", "changed", len(changed), "skipped_out_of_time", skipped)
	if len(changed) == 0 {
		return nil
	}
	if err := writeBack(ctx, sink, changed); err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	return nil
}
