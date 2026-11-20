package ingest

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// refreshProvider is the provider a github-ingest run vends its token for. Only its
// owners ("github:<id>") are refreshed, and only those the Eligibility confirms.
const refreshProvider = "github"

// ScheduledRefresher is the dispatch side of the Refresher: Ingestor.ScheduledRefresh.
// It must submit nothing while the owner's ingest run is in flight, which is what
// makes overlapping ticks produce one run per owner.
type ScheduledRefresher interface {
	ScheduledRefresh(ctx context.Context, ownerID string) (bool, error)
}

// ScheduledResearcher is the dispatch side of the Refresher's research pass:
// Ingestor.ScheduledResearch. It must submit nothing while a research run for the
// item is in flight, which is what makes overlapping ticks produce one run per item.
type ScheduledResearcher interface {
	ScheduledResearch(ctx context.Context, ownerID, itemID string) (bool, error)
}

// ItemLister reads one owner's stored items: the research pass's view of the
// worklist (worklist.Store satisfies it).
type ItemLister interface {
	List(ctx context.Context, ownerID string) ([]worklist.WorkItem, error)
}

// Eligibility confirms that gofer may start a run nobody requested on the user's
// behalf (auth.Handler.ScheduledRunAllowed): the provider is configured, a credential
// it could vend is stored and readable, no logout is pending, and the account is
// still allowed to sign in. It only reads. An owner it does not confirm, including
// on a read error, is skipped: a signed-out or removed user, or a vault gofer cannot
// read, must not cost a run on every tick, nor act for an account gofer no longer
// serves.
type Eligibility interface {
	ScheduledRunAllowed(ctx context.Context, userID, provider string) (bool, error)
}

// Refresher periodically requests a GitHub refresh (a github-ingest run) for every
// owner with stored work, so a populated worklist stays in sync with GitHub without a
// click: new work appears, signals refresh, and closed or merged work is retired.
// Without it ingest runs only on an empty worklist or the Refresh button.
//
// Each tick asks the Ingestor, whose per-owner gate refuses while any ingest run for
// the owner is in flight, so a tick that overlaps a running refresh, a backfill or a
// Refresh adds nothing. The web tier is a single replica, so one Refresher runs; a
// second replica would need a leader.
//
// With WithResearch, each tick then runs the research pass (ResearchOnce): a
// github-research run for every item whose discussion-derived ranking is stale
// (worklist.ResearchStale), so a failed post-reply adjustment is retried and a later
// GitHub update re-ranks the discussion once.
type Refresher struct {
	owners   worklist.OwnerLister
	ing      ScheduledRefresher
	eligible Eligibility // nil: every GitHub owner is refreshed (tests)
	interval time.Duration
	log      *slog.Logger

	// The research pass, when set (WithResearch): items reads an owner's items, and
	// research dispatches a run for one. passes rotates the owner the pass starts
	// with, so when dispatch stops early (the runtime's queue full), the same owner
	// does not take the first slot every time.
	items    ItemLister
	research ScheduledResearcher
	passes   uint64

	// served records, per owner, when a pass last settled its refresh (submitted, or
	// shared with a run already in flight), as a sequence number. Each pass goes
	// least recently served first, so an owner whose submission failed (the
	// runtime's queue full, or unreachable) goes first on the next pass, and owners
	// listed early cannot take every slot a capacity limit leaves.
	mu     sync.Mutex
	served map[string]uint64
	seq    uint64

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// NewRefresher builds a Refresher that ticks every interval. eligible may be nil only
// in tests.
func NewRefresher(owners worklist.OwnerLister, ing ScheduledRefresher, eligible Eligibility, interval time.Duration, log *slog.Logger) *Refresher {
	if log == nil {
		log = slog.Default()
	}
	return &Refresher{
		owners: owners, ing: ing, eligible: eligible, interval: interval, log: log,
		stop: make(chan struct{}), done: make(chan struct{}), served: map[string]uint64{},
	}
}

// WithResearch adds the research pass to every tick (ResearchOnce). Call it before
// Start. It returns r.
func (r *Refresher) WithResearch(items ItemLister, research ScheduledResearcher) *Refresher {
	r.items, r.research = items, research
	return r
}

// Start launches the loop. The first pass runs one interval after Start, not at
// once, so a restart does not refresh every owner together. Call Stop to end it.
func (r *Refresher) Start() { go r.loop() }

func (r *Refresher) loop() {
	defer close(r.done)
	t := time.NewTicker(r.interval)
	defer t.Stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-r.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			r.RefreshOnce(ctx)
			if r.research != nil {
				r.ResearchOnce(ctx)
			}
		}
	}
}

// Stop ends the loop, cancelling a pass in progress, and waits for it to return. It
// is safe to call more than once.
func (r *Refresher) Stop() {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
}

// RefreshOnce requests a refresh for every GitHub owner with stored work and reports
// how many runs it submitted. An owner whose ingest run is still in flight, or whom
// the Eligibility does not confirm, is skipped. Owners least recently served go first
// (see Refresher.served). Exported for tests; safe to call concurrently.
func (r *Refresher) RefreshOnce(ctx context.Context) int {
	owners, err := r.owners.Owners(ctx)
	if err != nil {
		r.log.Warn("scheduled refresh: list owners", "err", err)
		return 0
	}
	owners = r.leastRecentlyServed(owners)
	var submitted int
	for _, owner := range owners {
		if ctx.Err() != nil {
			break
		}
		if ownerProvider(owner) != refreshProvider {
			continue
		}
		if r.eligible != nil {
			ok, err := r.eligible.ScheduledRunAllowed(ctx, owner, refreshProvider)
			if err != nil {
				r.log.Warn("scheduled refresh: eligibility", "owner", owner, "err", err)
				continue
			}
			if !ok {
				continue
			}
		}
		ok, err := r.ing.ScheduledRefresh(ctx, owner)
		if err != nil {
			r.log.Warn("scheduled refresh: dispatch failed; the owner goes first next pass", "owner", owner, "err", err)
			continue
		}
		r.markServed(owner) // submitted, or sharing a run already in flight
		if ok {
			submitted++
		}
	}
	if submitted > 0 {
		r.log.Info("scheduled refresh: dispatched worklist refreshes", "runs", submitted, "owners", len(owners))
	}
	return submitted
}

// ResearchOnce requests a github-research run for every stale item
// (worklist.ResearchStale) of every GitHub owner with stored work whom the Eligibility
// confirms, and reports how many runs it submitted. Only items with a visible
// discussion are eligible, so an item with current research or no discussion costs
// nothing. Within an owner the item researched longest ago goes first, and owners
// take turns, one item each, so a pass that stops early is shared out. A dispatch
// error (the runtime's queue full, or unreachable) ends the pass; the next tick
// carries on. It does nothing without WithResearch. Exported for tests; safe to call
// concurrently (the Ingestor's gate holds one run per item).
func (r *Refresher) ResearchOnce(ctx context.Context) int {
	if r.research == nil || r.items == nil {
		return 0
	}
	owners, err := r.owners.Owners(ctx)
	if err != nil {
		r.log.Warn("scheduled research: list owners", "err", err)
		return 0
	}
	type ownerQueue struct {
		owner string
		items []string // stale item IDs, researched longest ago first
	}
	var queues []ownerQueue
	for _, owner := range owners {
		if ctx.Err() != nil {
			return 0
		}
		if ownerProvider(owner) != refreshProvider {
			continue
		}
		if r.eligible != nil {
			ok, err := r.eligible.ScheduledRunAllowed(ctx, owner, refreshProvider)
			if err != nil {
				r.log.Warn("scheduled research: eligibility", "owner", owner, "err", err)
				continue
			}
			if !ok {
				continue
			}
		}
		items, err := r.items.List(ctx, owner)
		if err != nil {
			r.log.Warn("scheduled research: list items", "owner", owner, "err", err)
			continue
		}
		var stale []worklist.WorkItem
		for _, it := range items {
			if worklist.ResearchStale(it) {
				stale = append(stale, it)
			}
		}
		if len(stale) == 0 {
			continue
		}
		sort.SliceStable(stale, func(a, b int) bool { return researchedAt(stale[a]).Before(researchedAt(stale[b])) })
		ids := make([]string, len(stale))
		for n := range stale {
			ids[n] = stale[n].ID
		}
		queues = append(queues, ownerQueue{owner: owner, items: ids})
	}
	if len(queues) == 0 {
		return 0
	}
	r.mu.Lock()
	start := int(r.passes % uint64(len(queues)))
	r.passes++
	r.mu.Unlock()
	queues = append(queues[start:], queues[:start]...)

	var submitted int
	for round := 0; ; round++ {
		more := false
		for _, q := range queues {
			if round >= len(q.items) {
				continue
			}
			more = true
			if ctx.Err() != nil {
				return submitted
			}
			ok, err := r.research.ScheduledResearch(ctx, q.owner, q.items[round])
			if err != nil {
				r.log.Warn("scheduled research: dispatch failed; the next tick carries on", "owner", q.owner, "item", q.items[round], "err", err)
				r.logResearch(submitted)
				return submitted
			}
			if ok {
				submitted++
			}
		}
		if !more {
			break
		}
	}
	r.logResearch(submitted)
	return submitted
}

func (r *Refresher) logResearch(submitted int) {
	if submitted > 0 {
		r.log.Info("scheduled research: dispatched discussion re-ranks", "runs", submitted)
	}
}

// researchedAt is when item's research was last applied; zero if never, so an item
// never researched goes first.
func researchedAt(item worklist.WorkItem) time.Time {
	if r := item.Signals.Research; r != nil {
		return r.AppliedAt
	}
	return time.Time{}
}

// leastRecentlyServed orders owners for a pass, least recently served first (never
// served first of all), keeping the listed order among equals. Owners no longer
// listed are forgotten.
func (r *Refresher) leastRecentlyServed(owners []string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	listed := make(map[string]bool, len(owners))
	for _, o := range owners {
		listed[o] = true
	}
	for o := range r.served {
		if !listed[o] {
			delete(r.served, o)
		}
	}
	out := append([]string(nil), owners...)
	sort.SliceStable(out, func(a, b int) bool { return r.served[out[a]] < r.served[out[b]] })
	return out
}

// markServed records that this pass settled owner's refresh.
func (r *Refresher) markServed(owner string) {
	r.mu.Lock()
	r.seq++
	r.served[owner] = r.seq
	r.mu.Unlock()
}

// ownerProvider is the provider prefix of an owner id ("github:123" -> "github").
func ownerProvider(ownerID string) string {
	p, _, _ := strings.Cut(ownerID, ":")
	return p
}
