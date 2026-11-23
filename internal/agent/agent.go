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
// A Vendor that is also a CredentialVendor lets a run re-vend once when GitHub
// rejects the token (tokenHolder).
type Vendor interface {
	Vend(ctx context.Context, provider string) (accessToken string, err error)
}

// Sink is the runtime's view of gofer's worklist store: read the acting user's
// persisted work, and write results back. In-process it is the store directly;
// out-of-process it is an HTTP client to gofer's agent sink.
//
// Every read is bounded, so a run completes however large the worklist grows: Item
// reads one item with a window of its thread, and Select reads a page of a
// selection without threads (worklist.Selection; readAll pages through it).
//
// A write-back is merged into the item as stored when it lands (see
// worklist.MergeRunItems): Ingest updates what a run derives — GitHub fields,
// signals, scoring — and never the conversation thread, so work the owner did
// while the run was busy survives. Reply is the only way a run adds to a thread,
// and Research writes a research adjustment without the rest of the item.
type Sink interface {
	// Item reads the acting user's item with id and a window of its thread that
	// keeps the turn named by turn (worklist.ThreadWindow); ok is false if it is gone
	// (or is not the acting user's).
	Item(ctx context.Context, id, turn string) (item worklist.WorkItem, ok bool, err error)
	// Select reads one page of the items sel names, without threads, and the cursor
	// of the next page (sel.After), empty on the last. A page may hold fewer items
	// than sel.Limit asks for (worklist.Select).
	Select(ctx context.Context, sel worklist.Selection) (items []worklist.WorkItem, next string, err error)
	Ingest(ctx context.Context, items []worklist.WorkItem) error
	// Reply appends an agent turn to itemID's thread, answering the user turn whose
	// Key is replyTo (see worklist.AppendReply). It is dropped, without error, if
	// that turn is gone or the item is.
	Reply(ctx context.Context, itemID, replyTo string, reply worklist.Message) error
	// Research sets itemID's research adjustment and nothing else (see
	// worklist.ApplyResearch), so a pass that read the item before a refresh never
	// writes back its older GitHub fields. It is dropped, without error, if the item
	// is gone.
	Research(ctx context.Context, itemID string, adj worklist.ResearchAdjustment) error
	// Turns sets items' turn readings and nothing else, in one write (see
	// worklist.ApplyTurns), so a reading the model took a while over never writes back
	// the item it read. A reading is dropped, without error, if its item is gone,
	// hidden, or its last word is no longer the one the reading names.
	Turns(ctx context.Context, turns []worklist.ItemTurn) error
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
	Turn          string                     // converse: Key of the user turn to answer; empty answers the latest unanswered one
	Model         string                     // chat-model identifier stamped on agent replies (converse)
	Converser     worklist.Conversationalist // github-converse assistant; required for that job
	Independent   bool                       // converse: answer blind — omit prior thread turns (independent review)
	Researcher    worklist.ResearchRanker    // github-research re-ranker; required for that job
	TurnReader    worklist.TurnReader        // reads the user's last word on waiting items; nil leaves the timeline to decide
	Bots          []string                   // logins whose comments are automated (BOT_REVIEWERS): never the last word
	EnrichLimit   int                        // optional cap on items to enrich; 0 enriches every item
	ReadPageSize  int                        // items per worklist page read (readAll); 0 uses the default
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
	// defaultReadPageSize is how many items one worklist page read asks for. Items
	// are read without their threads, and the server also ends a page by size
	// (worklist.MaxSelectBytes), so a page is far under the response limit.
	defaultReadPageSize = 500
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

// readAll reads every item sel names, a bounded page at a time, so no single read
// grows with the worklist: a paged read (p.ReadPageSize items a page) or the top N.
// An item a later page repeats (a top-N ranking can shift between pages) is dropped.
func readAll(ctx context.Context, p Params, sink Sink, sel worklist.Selection) ([]worklist.WorkItem, error) {
	sel.After = ""
	if sel.Top == 0 {
		sel.Limit = p.ReadPageSize
		if sel.Limit <= 0 {
			sel.Limit = defaultReadPageSize
		}
	}
	var all []worklist.WorkItem
	seen := map[string]bool{}
	for {
		page, next, err := sink.Select(ctx, sel)
		if err != nil {
			return nil, err
		}
		for _, it := range page {
			if !seen[it.ID] {
				seen[it.ID] = true
				all = append(all, it)
			}
		}
		if next == "" || (sel.Top > 0 && len(all) >= sel.Top) {
			return all, nil
		}
		if len(page) == 0 || next == sel.After {
			return nil, fmt.Errorf("list worklist: page cursor %q does not advance", next)
		}
		sel.After = next
	}
}

// reserveContext returns a context that ends reserve before ctx's deadline, so work
// started under it cannot eat the time kept for what follows (writing results back,
// ranking). A context without a deadline is returned as is.
func reserveContext(ctx context.Context, reserve time.Duration) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, deadline.Add(-reserve))
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
		if err := runEnrich(ctx, p, vendor, sink, nil); err != nil {
			return err
		}
		if err := runReadTurns(ctx, p, sink); err != nil {
			p.logger().Warn("turn reading failed", "err", err)
		}
		return nil
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
	// The holder re-vends once if GitHub rejects the token (ADR 0007), and turns the
	// broker's "sign-in required" into a ReconnectError.
	holder, err := newTokenHolder(ctx, vendor, provider, p.GitHubBaseURL)
	if err != nil {
		return fmt.Errorf("vend credential: %w", err)
	}
	gh := github.NewClient(holder.client(p.Client), p.GitHubBaseURL)
	fetch, err := gh.FetchWorklistDetail(ctx, holder.Token())
	items, complete := fetch.Items, fetch.Complete
	if rerr := holder.Reconnect(); rerr != nil {
		return fmt.Errorf("fetch github: %w", rerr)
	}
	if err != nil {
		return fmt.Errorf("fetch github: %w", err)
	}
	log.Info("ingest: fetched work items", "count", len(items), "complete", complete)
	// Reconcile with what gofer already persists so a re-ingest (a Refresh, or an
	// empty-worklist backfill) never clobbers per-item state GitHub cannot supply —
	// the conversation thread and its read cursor, and the user/agent metadata
	// overrides. The fresh GitHub fields and base signals come from the fetch; the
	// chained enrich+rank re-derive the score. If the existing items cannot be read,
	// the run fails before it writes anything: proceeding without them would skip
	// retirement and report success (and a web tier without the bounded reads answers
	// 404, which must not pass for an empty worklist).
	// Only IDs, completion and the per-item state above are needed, so the read omits
	// thread bodies (the agent plane keeps the stored thread on every write-back).
	existing, lerr := readAll(ctx, p, sink, worklist.Selection{})
	if lerr != nil {
		return fmt.Errorf("ingest: read existing items: %w", lerr)
	}
	items, carried := reconcileFetched(items, existing, fetch.ReviewRequestsComplete)
	// Retire what left the open set. Only a COMPLETE fetch proves an item is gone: a
	// partial walk (time budget, rate limit, result ceiling) simply may not have
	// reached it, and retiring it then would hide open work.
	if complete {
		retired := retireMissing(items, existing, time.Now().UTC())
		if len(retired) > 0 {
			log.Info("ingest: retired items no longer open on GitHub", "count", len(retired))
			items = append(items, retired...)
		}
	} else if len(existing) > 0 {
		log.Info("ingest: fetch was partial; not retiring items missing from it")
	}
	if len(items) == 0 {
		return nil
	}
	if err := writeBack(ctx, sink, items); err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	// The backfill continues into enrich (per-item timeline signals), the turn
	// reading (whether the user's last word leaves the next step to someone else),
	// then rank (the model proposal), so one pass yields a fully scored radar. Each
	// is best-effort: a failure leaves the baseline-scored items in place. The
	// separate JobEnrich / JobRank exist for an independent scheduled refresh.
	if err := runEnrich(ctx, p, vendor, sink, carried); err != nil {
		// The user's access is gone: stop here, and say so, rather than rank and
		// report success. The items already written keep their baseline scores.
		if isSignInRequired(err) {
			return fmt.Errorf("backfill enrich: %w", err)
		}
		log.Warn("backfill enrich failed", "err", err)
	}
	if err := runReadTurns(ctx, p, sink); err != nil {
		log.Warn("backfill turn reading failed", "err", err)
	}
	if err := runRank(ctx, p, sink); err != nil {
		log.Warn("backfill rank failed", "err", err)
	}
	log.Info("ingest: backfill complete", "items", len(items))
	return nil
}

// reconcileFetched carries gofer-owned per-item state that GitHub cannot supply — the
// conversation thread and its read cursor, the hidden flag, and the original
// creation time — from the persisted items onto the
// freshly fetched ones, keyed by ID. Fetched items with no persisted counterpart (new
// work) pass through unchanged. The agent plane also keeps the stored thread, read
// cursor, hidden flag, creation time and, when a write-back carries none, the model
// proposal and research adjustment on every write-back (worklist.MergeRunItems),
// which is what protects a change made after this run read the items; carrying them
// here keeps the run's own view of each item whole.
//
// reviewsComplete reports whether the review-request search was walked whole
// (github.Fetch.ReviewRequestsComplete); see carryPendingRequest. carried names the
// items whose review request was kept from the store although this fetch did not
// show it, so the chained enrich can drop it if the timeline shows it ended.
func reconcileFetched(fetched, existing []worklist.WorkItem, reviewsComplete bool) (_ []worklist.WorkItem, carried map[string]bool) {
	carried = make(map[string]bool)
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
		fetched[i].ThreadReadThrough = old.ThreadReadThrough
		// The fresh signals are sent even for an item hidden when this run read it: the
		// owner may unhide it before the write lands, and only the plane's merge
		// (worklist.MergeRunItems), inside the store's atomic update, knows whether the
		// item is hidden then and must be stripped. The flag is carried for the run's
		// own view; the merge takes the stored one.
		fetched[i].Meta.HiddenAt = old.Meta.HiddenAt
		// A fetch derives no model proposal or research adjustment, so it sends none:
		// the plane's merge then keeps whatever is stored when this write lands, not
		// the copy read above, which a discussion may have replaced since. The
		// chained enrich and rank read the store again.
		clearModelSignals(&fetched[i].Signals)
		carryTurnSignals(&fetched[i], old)
		if old.Meta.CompletedAt.IsZero() {
			// A retired item's request ended with it; a resurfaced item's request is new.
			if carryPendingRequest(&fetched[i].Signals, old.Signals, reviewsComplete) {
				carried[fetched[i].ID] = true
			}
		}
		// A search hit carries no CI summary, and none is copied from the read above,
		// which may be a projection: the plane's merge keeps the stored one
		// (worklist.KeepCI) until the chained enrich reads the head commit again.
		// CompletedAt is deliberately NOT carried over: the item was just fetched from
		// the open set, so it is open, and a reopened item resurfaces (see Metadata).
		if !old.CreatedAt.IsZero() {
			fetched[i].CreatedAt = old.CreatedAt
		}
	}
	return fetched, carried
}

// retireMissing returns the persisted GitHub items that are absent from a complete
// fetch and not yet marked completed, each stamped with CompletedAt = now. The fetch
// covers every open item that involves the user or awaits their review, so absence
// means the item was merged or closed, or no longer involves them. Retiring rather
// than deleting keeps the thread: resolve drops completed items from the radar, an
// agent can still see them, and a reopened item is fetched again and resurfaces.
func retireMissing(fetched, existing []worklist.WorkItem, now time.Time) []worklist.WorkItem {
	open := make(map[string]bool, len(fetched))
	for _, it := range fetched {
		open[it.ID] = true
	}
	var retired []worklist.WorkItem
	for _, it := range existing {
		if it.Source != "github" || open[it.ID] || !it.Meta.CompletedAt.IsZero() {
			continue
		}
		it.Meta.CompletedAt = now
		// Retiring derives no model signals, and reads no CI; leave both to the store
		// (see reconcileFetched and worklist.KeepCI).
		clearModelSignals(&it.Signals)
		it.GitHub.CI = nil
		retired = append(retired, it)
	}
	return retired
}

// carryPendingRequest keeps what a refresh cannot re-establish about a review the
// user was asked for. GitHub's search says a review is requested but not by whom;
// only enrich's timeline read names the requester. Without this a refresh would blank
// the requester, the bot flag would read "human", and a stored proposal the bot
// policy had set aside (Signals.CurrentProposal) would apply again until enrich ran.
// So whenever the fetch shows a request, the stored requester is kept (whether the
// store learned it with the request or from an earlier timeline read); enrich
// replaces it whenever it reads the whole timeline.
//
// Only a complete review-request search confirms that a request was removed: when
// that search was partial (reviewsComplete false), an item may arrive through
// involves: alone, so a request the store recorded is kept, and carryPendingRequest
// reports that it inferred it. The caller skips retired items, whose request ended
// with them.
func carryPendingRequest(fetched *worklist.Signals, stored worklist.Signals, reviewsComplete bool) (inferred bool) {
	if !slices.Contains(fetched.Reasons, worklist.ReasonReviewRequested) {
		if reviewsComplete || !slices.Contains(stored.Reasons, worklist.ReasonReviewRequested) {
			return false // no request is shown, and none may be inferred
		}
		fetched.Reasons = append(slices.Clone(fetched.Reasons), worklist.ReasonReviewRequested)
		inferred = true
	}
	if fetched.ReviewRequestedBy == "" && stored.ReviewRequestedBy != "" {
		fetched.ReviewRequestedBy = stored.ReviewRequestedBy
		fetched.ReviewRequestedByBot = stored.ReviewRequestedByBot
	}
	return inferred
}

// clearModelSignals drops the model proposal, research adjustment and turn reading
// from a write-back that did not derive them. A write-back that carries none leaves
// the stored values in place (worklist.MergeRunItems), so a pass never restores the
// copy it read over a newer one written meanwhile.
func clearModelSignals(s *worklist.Signals) {
	s.Proposed = nil
	s.Research = nil
	s.Turn = nil
}

// carryTurnSignals keeps what a refresh cannot re-derive about whose turn it is. The
// stored last word always carries over: it holds the head first seen with it
// (LastWord.Head), which the chained enrich compares with the head now to tell
// whether the author pushed since, and which a push (it updates the item) must not
// erase. The waiting timestamps carry over only for an item GitHub has not updated
// since: its timeline is unchanged, so the item keeps its place on the radar
// (worklist.WorkItem.Waiting) until enrich reads the timeline again. An item GitHub
// updated (a comment, a push, a review request) is not shown as waiting until then.
func carryTurnSignals(fetched *worklist.WorkItem, stored worklist.WorkItem) {
	fetched.Signals.LastWord = stored.Signals.LastWord
	if fetched.GitHub.UpdatedAt.IsZero() || !fetched.GitHub.UpdatedAt.Equal(stored.GitHub.UpdatedAt) {
		return
	}
	fetched.Signals.AwaitingMeSince = stored.Signals.AwaitingMeSince
	fetched.Signals.AwaitingOthersSince = stored.Signals.AwaitingOthersSince
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
	limit := p.RankLimit
	if limit <= 0 {
		limit = defaultRankLimit
	}
	// Spend the model budget on the open items the cheap, deterministic scorer already
	// rates highest rather than on an arbitrary slice: the agent plane selects them
	// (worklist.Select), so the read is the top N, not the worklist. Retired items are
	// off the radar; a model proposal for one is wasted spend. enrich has just run, so
	// those baselines are computed from complete signals. The baseline ignores the
	// item's model signals (worklist.BaselineScore): the previous proposal is what this
	// pass replaces, and it may rest on a classification since changed (e.g. a
	// requester removed from BOT_REVIEWERS), so neither it nor an old research
	// adjustment may keep the item out of the pass.
	items, err := readAll(ctx, p, sink, worklist.Selection{Open: true, Top: limit})
	if err != nil {
		return fmt.Errorf("list worklist: %w", err)
	}
	// The selection leaves out items waiting on someone else (WorkItem.Waiting) before
	// its cutoff: they are listed apart from the ranking, so their slots go to items
	// that are the user's turn.
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
			// Record the classification the ranker saw, so the proposal is dropped if
			// it changes (worklist.Signals.CurrentProposal).
			prop.ReviewRequestedByBot = items[i].Signals.ReviewRequestedByBot
			items[i].Signals.Proposed = &prop
			// Rank derives only the proposal; the stored research adjustment and turn
			// reading stay.
			items[i].Signals.Research = nil
			items[i].Signals.Turn = nil
			// Nor does it read CI: the summary it read may have been replaced by an
			// enrich that landed while the model worked (worklist.KeepCI keeps the
			// stored one).
			items[i].GitHub.CI = nil
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
