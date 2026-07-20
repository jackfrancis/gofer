package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackfrancis/gofer/internal/github"
	"github.com/jackfrancis/gofer/internal/worklist"
)

const (
	defaultEnrichLimit = 50
	enrichConcurrency  = 8
)

func enrichLimit(n int) int {
	if n <= 0 {
		return defaultEnrichLimit
	}
	return n
}

// runEnrich reads the user's persisted work and augments each item with per-item
// timeline signals (AwaitingMeSince, participants, inbound refs, other reviewers,
// review-requested-by) fetched directly from GitHub with the vended credential.
// It writes back only the items it changed, so a read-time rescore reflects the
// fresher signals. Per-item enrichment is best-effort and bounded in concurrency
// so the fan-out does not blow the run deadline.
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
	if limit := enrichLimit(p.EnrichLimit); len(items) > limit {
		items = items[:limit]
	}
	p.logger().Info("enrich: augmenting work items", "items", len(items))

	var (
		mu      sync.Mutex
		changed []worklist.WorkItem
		wg      sync.WaitGroup
		sem     = make(chan struct{}, enrichConcurrency)
	)
	for i := range items {
		if items[i].Source != "github" {
			continue
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
			if act.Participants == s.Participants && act.InboundRefs == s.InboundRefs &&
				act.OtherReviewers == s.OtherReviewers && act.RequestedByLogin == s.ReviewRequestedBy &&
				act.AwaitingMeSince.Equal(s.AwaitingMeSince) && act.AwaitingOthersSince.Equal(s.AwaitingOthersSince) {
				return
			}
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
	p.logger().Info("enrich: updated work items", "changed", len(changed))
	if len(changed) == 0 {
		return nil
	}
	if err := sink.Ingest(ctx, changed); err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	return nil
}
