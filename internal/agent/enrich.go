package agent

import (
	"context"
	"fmt"
	"reflect"
	"slices"
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
// timeline signals (AwaitingMeSince, AwaitingOthersSince and the user's last word,
// participants, inbound refs, other reviewers, review-requested-by), and each pull
// request with a summary of its head commit's CI (GitHubRef.CI), fetched directly
// from GitHub with the vended credential.
// It writes back only the items it changed, so a read-time rescore reflects the
// fresher signals. Per-item enrichment is best-effort and bounded in concurrency
// so the fan-out does not blow the run deadline.
//
// It deliberately covers EVERY item. Enrich feeds the deterministic baseline score,
// so an item it skips is scored from missing signals and can never rise — a cap here
// would decide the ranking instead of the ranking deciding the cap. The fan-out is
// cheap (one timeline read per item, and a few CI reads per pull request: a small
// share of the hourly core budget even for a large worklist); it is bounded by
// concurrency and by the run clock instead of by an arbitrary count. Params.EnrichLimit still caps it when a caller wants that.
//
// The read pages through the open items without their threads (readAll), so it
// stays bounded however large the worklist grows without dropping any item.
//
// carried (optional) names the items whose review request the refresh that chains
// into this pass kept although its search did not show it (see carryPendingRequest):
// for those, a timeline that shows the user's request ended removes the request
// (Activity.RequestEnded: no personal or team request still outstanding, since the
// user may be on any requested team). A request the search itself showed is never
// removed here: the timeline cannot tell whether a team request includes the user.
func runEnrich(ctx context.Context, p Params, vendor Vendor, sink Sink, carried map[string]bool) error {
	provider := p.Provider
	if provider == "" {
		provider = "github"
	}
	holder, err := newTokenHolder(ctx, vendor, provider, p.GitHubBaseURL)
	if err != nil {
		return fmt.Errorf("vend credential: %w", err)
	}
	token := holder.Token()
	gh := github.NewClient(holder.client(p.Client), p.GitHubBaseURL)
	login, err := gh.Login(ctx, token)
	if err != nil {
		if rerr := holder.Reconnect(); rerr != nil {
			return fmt.Errorf("github login: %w", rerr)
		}
		return fmt.Errorf("github login: %w", err)
	}
	items, err := readAll(ctx, p, sink, worklist.Selection{Open: true})
	if err != nil {
		return fmt.Errorf("list worklist: %w", err)
	}
	if p.EnrichLimit > 0 && len(items) > p.EnrichLimit {
		items = items[:p.EnrichLimit]
	}
	p.logger().Info("enrich: augmenting work items", "items", len(items))

	// Two passes, timeline first: the timeline feeds the score, the CI summary is
	// display-only, so slow or failing CI reads must never cost an item its ranking
	// signals. The CI pass spends only the time the timeline pass left.
	// changed marks an item either pass changed; ciChanged one whose CI the CI pass
	// read and found changed. Only those carry CI in the write-back: any other
	// carries the summary this run read, which an overlapping enrich may have
	// replaced since, so it sends none and worklist.KeepCI keeps the stored one.
	changed := make([]bool, len(items))
	ciChanged := make([]bool, len(items))
	open := func(it worklist.WorkItem) bool {
		// Retired items (closed, merged, no longer involving the user) are off the radar:
		// spending a call on each would grow with every item ever seen. Hidden items
		// carry no signals to enrich, and their CI is the last one read (the selection
		// leaves both out already).
		return it.Source == "github" && it.OnRadar()
	}
	skipped := fanOut(ctx, items, open, func(i int) {
		// The timeline can take several page reads; they must finish inside the
		// reserve-adjusted deadline too, not just start before it, or one slow item
		// holds the batch past the time kept for writing every result back.
		actx, cancel := reserveContext(ctx, enrichReserve)
		opts := github.ActivityOptions{Author: items[i].GitHub.Author, Bots: p.Bots,
			// The review-requested:@me search shows the item: a team request may be ours.
			ReviewRequested: slices.Contains(items[i].Signals.Reasons, worklist.ReasonReviewRequested),
			PullRequest:     items[i].Type == worklist.TypePullRequest,
			Prior:           items[i].Signals.LastWord}
		if slices.Contains(items[i].Signals.Reasons, worklist.ReasonAuthor) {
			opts.Author = login // an item stored before its author was recorded
		}
		act, err := gh.ItemActivityFor(actx, token, items[i].GitHub.Repo, items[i].GitHub.Number, login, opts)
		cancel()
		// best-effort: an unreadable timeline leaves the item's signals unchanged
		if err == nil && applyActivity(&items[i].Signals, act, carried[items[i].ID]) {
			changed[i] = true
		}
	})
	// A worker that found the user's access gone stops the pass: its results are
	// partial, and the run should say why rather than write them.
	if rerr := holder.Reconnect(); rerr != nil {
		return fmt.Errorf("enrich: %w", rerr)
	}
	ciSkipped := fanOut(ctx, items, func(it worklist.WorkItem) bool {
		return open(it) && it.Type == worklist.TypePullRequest
	}, func(i int) {
		ci, ok := readCI(ctx, gh, token, items[i].GitHub)
		if !ok {
			return // best-effort: the stored summary stands
		}
		// A projected read holds a cut summary, so an equal fresh one may still differ
		// from what is stored: write it whenever the read was cut.
		if items[i].CIProjected() || !ci.Equal(items[i].GitHub.CI) {
			items[i].SetCI(ci)
			changed[i], ciChanged[i] = true, true
		}
	})
	if rerr := holder.Reconnect(); rerr != nil {
		return fmt.Errorf("enrich: %w", rerr)
	}
	var out []worklist.WorkItem
	for i := range items {
		if changed[i] {
			// Enrich derives no model signals; leave them to the store.
			clearModelSignals(&items[i].Signals)
			if !ciChanged[i] {
				items[i].GitHub.CI = nil
			}
			out = append(out, items[i])
		}
	}
	p.logger().Info("enrich: updated work items", "changed", len(out),
		"skipped_out_of_time", skipped, "ci_skipped_out_of_time", ciSkipped)
	if len(out) == 0 {
		return nil
	}
	if err := writeBack(ctx, sink, out); err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	return nil
}

// fanOut runs work(i) for every item want accepts, enrichConcurrency at a time, and
// returns how many accepted items it did not start because the run clock reached
// the time kept for writing the results back and ranking them. Each work call
// writes only items[i] (and its own index elsewhere), so calls never share state.
func fanOut(ctx context.Context, items []worklist.WorkItem, want func(worklist.WorkItem) bool, work func(i int)) (skipped int) {
	var (
		wg  sync.WaitGroup
		sem = make(chan struct{}, enrichConcurrency)
	)
	for i := range items {
		if !want(items[i]) {
			continue
		}
		// Yield to the run clock: stop starting new lookups once what remains must be
		// kept for writing these results back and ranking them.
		if !hasBudget(ctx, enrichReserve) {
			for _, it := range items[i:] {
				if want(it) {
					skipped++
				}
			}
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			work(i)
		}(i)
	}
	wg.Wait()
	return skipped
}

// applyActivity sets the timeline-derived signals from act and reports whether any
// changed. carried says the refresh chained into this pass kept the item's review
// request although its search did not show it (see runEnrich).
func applyActivity(s *worklist.Signals, act github.Activity, carried bool) bool {
	// A union membership query cannot say whether the user actually spoke, so the
	// timeline settles it: an item carrying only the generic "involved" reason is
	// upgraded to "commented" once participation is proven.
	reasons, reasonChanged := s.Reasons, false
	if act.Commented {
		if upgraded, ok := upgradeInvolved(reasons); ok {
			reasons, reasonChanged = upgraded, true
		}
	}
	if carried && act.RequestEnded && slices.Contains(reasons, worklist.ReasonReviewRequested) {
		// The refresh kept a request its partial search did not show; the whole
		// timeline shows it withdrawn or fulfilled.
		reasons = slices.DeleteFunc(slices.Clone(reasons), func(r worklist.Reason) bool { return r == worklist.ReasonReviewRequested })
		reasonChanged = true
	}
	// Only a whole timeline says who requested the review; a partial one leaves
	// what the store knows in place rather than reading as "nobody".
	requester := act.RequestedByLogin
	if !act.TimelineComplete {
		requester = s.ReviewRequestedBy
	}
	if !reasonChanged &&
		act.Participants == s.Participants && act.InboundRefs == s.InboundRefs &&
		act.OtherReviewers == s.OtherReviewers && requester == s.ReviewRequestedBy &&
		act.AwaitingMeSince.Equal(s.AwaitingMeSince) && act.AwaitingOthersSince.Equal(s.AwaitingOthersSince) &&
		reflect.DeepEqual(act.LastWord, s.LastWord) {
		return false
	}
	s.Reasons = reasons
	s.Participants = act.Participants
	s.InboundRefs = act.InboundRefs
	s.OtherReviewers = act.OtherReviewers
	s.ReviewRequestedBy = requester
	s.AwaitingMeSince = act.AwaitingMeSince
	s.AwaitingOthersSince = act.AwaitingOthersSince
	// A turn reading of a replaced last word no longer applies (CurrentTurnReading),
	// and the reading pass that follows enrich reads the new one.
	s.LastWord = act.LastWord
	return true
}

// readCI reads the CI on a pull request's head commit, inside the reserve-adjusted
// deadline as the timeline read is. ok is false when it could not be read, and the
// stored summary then stands: a failed read is not "no CI".
func readCI(ctx context.Context, gh *github.Client, token string, ref worklist.GitHubRef) (*worklist.CISummary, bool) {
	cctx, cancel := reserveContext(ctx, enrichReserve)
	defer cancel()
	st, err := gh.CIStatusForPR(cctx, token, ref.Repo, ref.Number)
	if err != nil {
		return nil, false
	}
	sum := st.Summary()
	return &sum, true
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
