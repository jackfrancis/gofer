// Package agent is the ephemeral agent runtime pipeline. Its entrypoint Run
// dispatches on job type (github-ingest, github-enrich, llm-rank, github-converse,
// github-research) and works against small seams (Vendor, Sink), so the runtime
// behaves identically no matter which AEI substrate runs it.
//
// It runs only in the agent runtime (cmd/runtime), never the web tier: gofer is a
// credential broker, not a data broker, so the agent vends the user's credential
// and connects to GitHub directly, then writes results back through the Sink — it
// never sees the user's raw token until it is vended, and never writes anywhere but
// gofer.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
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
// credential. In-process it reads the vault; out-of-process it calls the AEI
// broker (POST /vend). The runtime never holds a standing secret.
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
	Converser     worklist.Conversationalist // github-converse assistant; required for that job
	Researcher    worklist.ResearchRanker    // github-research re-ranker; required for that job
	EnrichLimit   int                        // max items to enrich/rank; 0 uses the default
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
	defaultRankLimit = 50
	rankConcurrency  = 8
)

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
	if err := sink.Ingest(ctx, items); err != nil {
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
		items = items[:limit]
	}
	log.Info("rank: proposing axes", "items", len(items), "stub", stub)

	var (
		mu      sync.Mutex
		changed []worklist.WorkItem
		wg      sync.WaitGroup
		sem     = make(chan struct{}, rankConcurrency)
	)
	for i := range items {
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
	log.Info("rank: applied proposals", "changed", len(changed))
	if len(changed) == 0 {
		return nil
	}
	if err := sink.Ingest(ctx, changed); err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	return nil
}
