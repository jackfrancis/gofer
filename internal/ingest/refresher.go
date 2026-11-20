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
type Refresher struct {
	owners   worklist.OwnerLister
	ing      ScheduledRefresher
	eligible Eligibility // nil: every GitHub owner is refreshed (tests)
	interval time.Duration
	log      *slog.Logger

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
