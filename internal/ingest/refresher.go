package ingest

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// refreshProvider is the provider a github-ingest run vends its token for. Only its
// owners ("github:<id>") are refreshed, and one gofer could not vend a token for is
// skipped, so a user who signed out (or whose grant died) does not cost a failed run
// on every tick.
const refreshProvider = "github"

// ScheduledRefresher is the dispatch side of the Refresher: Ingestor.ScheduledRefresh.
// It must submit nothing while the owner's ingest run is in flight, which is what
// makes overlapping ticks produce one run per owner.
type ScheduledRefresher interface {
	ScheduledRefresh(ctx context.Context, ownerID string) (bool, error)
}

// SignInChecker reports whether gofer holds no credential it could vend for the user
// and provider (auth.Handler.SignInRequired). It only reads.
type SignInChecker interface {
	SignInRequired(ctx context.Context, userID, provider string) bool
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
	signIn   SignInChecker // nil: every owner is refreshed
	interval time.Duration
	log      *slog.Logger

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// NewRefresher builds a Refresher that ticks every interval. signIn may be nil.
func NewRefresher(owners worklist.OwnerLister, ing ScheduledRefresher, signIn SignInChecker, interval time.Duration, log *slog.Logger) *Refresher {
	if log == nil {
		log = slog.Default()
	}
	return &Refresher{
		owners: owners, ing: ing, signIn: signIn, interval: interval, log: log,
		stop: make(chan struct{}), done: make(chan struct{}),
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
// how many runs it submitted. An owner whose ingest run is still in flight, or who
// must sign in again, is skipped. Exported for tests; safe to call concurrently.
func (r *Refresher) RefreshOnce(ctx context.Context) int {
	owners, err := r.owners.Owners(ctx)
	if err != nil {
		r.log.Warn("scheduled refresh: list owners", "err", err)
		return 0
	}
	var submitted int
	for _, owner := range owners {
		if ctx.Err() != nil {
			break
		}
		if ownerProvider(owner) != refreshProvider ||
			(r.signIn != nil && r.signIn.SignInRequired(ctx, owner, refreshProvider)) {
			continue
		}
		ok, err := r.ing.ScheduledRefresh(ctx, owner)
		if err != nil {
			r.log.Warn("scheduled refresh: dispatch", "owner", owner, "err", err)
			continue
		}
		if ok {
			submitted++
		}
	}
	if submitted > 0 {
		r.log.Info("scheduled refresh: dispatched worklist refreshes", "runs", submitted, "owners", len(owners))
	}
	return submitted
}

// ownerProvider is the provider prefix of an owner id ("github:123" -> "github").
func ownerProvider(ownerID string) string {
	p, _, _ := strings.Cut(ownerID, ":")
	return p
}
