package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackfrancis/gofer/internal/github"
	"github.com/jackfrancis/gofer/internal/worklist"
)

const (
	enrichConcurrency = 8
	// enrichReserve is the slice of the run's remaining time enrich leaves for the rest
	// of the backfill: writing its results back, then ranking them.
	enrichReserve = 60 * time.Second
)

// runEnrich reads the user's persisted work and augments each item with per-item
// timeline signals (AwaitingMeSince, participants, inbound refs, other reviewers,
// review-requested-by) fetched directly from GitHub with the vended credential.
// It writes back only the items it changed, so a read-time rescore reflects the
// fresher signals. Per-item enrichment is best-effort and bounded in concurrency
// so the fan-out does not blow the run deadline.
//
// It deliberately covers EVERY item. Enrich feeds the deterministic baseline score,
// so an item it skips is scored from missing signals and can never rise — a cap here
// would decide the ranking instead of the ranking deciding the cap. The fan-out is
// cheap (one timeline read per item, a few percent of the hourly core budget even for
// a large worklist); it is bounded by concurrency and by the run clock instead of by
// an arbitrary count. Params.EnrichLimit still caps it when a caller wants that.
func runEnrich(ctx context.Context, p Params, vendor Vendor, sink Sink) error {
	provider := p.Provider
	if provider == "" {
		provider = "github"
	}
	token, err := vendor.Vend(ctx, provider)
	if err != nil {
		return fmt.Errorf("vend credential: %w", err)
	}
	gh := github.NewClient(p.Client, p.GitHubBaseURL)
	login, err := gh.Login(ctx, token)
	if err != nil {
		return fmt.Errorf("github login: %w", err)
	}
	items, err := sink.List(ctx)
	if err != nil {
		return fmt.Errorf("list worklist: %w", err)
	}
	if p.EnrichLimit > 0 && len(items) > p.EnrichLimit {
		items = items[:p.EnrichLimit]
	}
	p.logger().Info("enrich: augmenting work items", "items", len(items))

	var (
		mu      sync.Mutex
		changed []worklist.WorkItem
		wg      sync.WaitGroup
		sem     = make(chan struct{}, enrichConcurrency)
		skipped int
	)
	for i := range items {
		if items[i].Source != "github" {
			continue
		}
		// Yield to the run clock: stop starting new lookups once what remains must be
		// kept for writing these results back and ranking them.
		if !hasBudget(ctx, enrichReserve) {
			skipped = len(items) - i
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			act, err := gh.ItemActivity(ctx, token, items[i].GitHub.Repo, items[i].GitHub.Number, login)
			if err != nil {
				return // best-effort: leave the item's signals unchanged
			}
			s := &items[i].Signals
			// A union membership query cannot say whether the user actually spoke, so the
			// timeline settles it: an item carrying only the generic "involved" reason is
			// upgraded to "commented" once participation is proven.
			reasons, reasonChanged := s.Reasons, false
			if act.Commented {
				if upgraded, ok := upgradeInvolved(reasons); ok {
					reasons, reasonChanged = upgraded, true
				}
			}
			if !reasonChanged &&
				act.Participants == s.Participants && act.InboundRefs == s.InboundRefs &&
				act.OtherReviewers == s.OtherReviewers && act.RequestedByLogin == s.ReviewRequestedBy &&
				act.AwaitingMeSince.Equal(s.AwaitingMeSince) && act.AwaitingOthersSince.Equal(s.AwaitingOthersSince) {
				return
			}
			s.Reasons = reasons
			s.Participants = act.Participants
			s.InboundRefs = act.InboundRefs
			s.OtherReviewers = act.OtherReviewers
			s.ReviewRequestedBy = act.RequestedByLogin
			s.AwaitingMeSince = act.AwaitingMeSince
			s.AwaitingOthersSince = act.AwaitingOthersSince
			mu.Lock()
			changed = append(changed, items[i])
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	p.logger().Info("enrich: updated work items", "changed", len(changed), "skipped_out_of_time", skipped)
	if len(changed) == 0 {
		return nil
	}
	if err := writeBack(ctx, sink, changed); err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	return nil
}

// upgradeInvolved replaces the generic ReasonInvolved with ReasonCommented, returning
// a NEW slice (the caller's may alias the store's backing array) and whether anything
// changed. It reports false when the item never carried the generic reason — an item
// already known to be authored, assigned or review-requested keeps that stronger
// relationship untouched.
func upgradeInvolved(reasons []worklist.Reason) ([]worklist.Reason, bool) {
	idx := -1
	for i, r := range reasons {
		if r == worklist.ReasonCommented {
			return reasons, false // already known
		}
		if r == worklist.ReasonInvolved {
			idx = i
		}
	}
	if idx < 0 {
		return reasons, false
	}
	out := make([]worklist.Reason, len(reasons))
	copy(out, reasons)
	out[idx] = worklist.ReasonCommented
	return out, true
}
