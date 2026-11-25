package worklist

import (
	"context"
	"strings"
)

// RunPolicy holds the web tier's rules for a run's write-back that the workload
// does not decide: it reports facts, and gofer classifies them. Apply it to a run's
// items before they are merged into the store (MergeRunItems), on every path a
// write-back takes.
type RunPolicy struct {
	bots map[string]struct{}
}

// NewRunPolicy returns the policy for the given bot reviewers: GitHub logins whose
// review requests are automated rather than an explicit human ask (config
// BOT_REVIEWERS). Logins match without regard to case, as GitHub's do.
func NewRunPolicy(botReviewers []string) RunPolicy {
	bots := make(map[string]struct{}, len(botReviewers))
	for _, b := range botReviewers {
		if b = strings.ToLower(strings.TrimSpace(b)); b != "" {
			bots[b] = struct{}{}
		}
	}
	return RunPolicy{bots: bots}
}

// Bots returns the policy's bot logins, lowercased, in no particular order.
func (p RunPolicy) Bots() []string {
	out := make([]string, 0, len(p.bots))
	for b := range p.bots {
		out = append(out, b)
	}
	return out
}

// IsBot reports whether login is one of the policy's bot reviewers.
func (p RunPolicy) IsBot(login string) bool {
	if login == "" {
		return false
	}
	_, ok := p.bots[strings.ToLower(login)]
	return ok
}

// Apply stamps a run's items, in place, with what only the web tier may decide:
//
//   - Meta.Origin is always OriginAgent. A workload's metadata is agent-derived,
//     whatever the run sent, so a run cannot claim the human override (OriginUser)
//     that Resolve leaves unrescored.
//   - Signals.ReviewRequestedByBot is re-derived (Classify).
//   - The last word and turn reading are held to their bounds (BoundTurnSignals).
//   - GitHub.AuthorAvatar is kept only as an avatar path (NormalizeAvatarPath).
//     Projection does not bound it, so an oversized value stored verbatim would
//     take an item read's whole budget; the page checks it again when it renders.
func (p RunPolicy) Apply(items []WorkItem) {
	for i := range items {
		items[i].Meta.Origin = OriginAgent
		BoundTurnSignals(&items[i].Signals)
		items[i].GitHub.AuthorAvatar = NormalizeAvatarPath(items[i].GitHub.AuthorAvatar)
	}
	p.Classify(items)
}

// Classify sets Signals.ReviewRequestedByBot, in place, from
// Signals.ReviewRequestedBy against the bot list, replacing any stored or sent
// value. A review a bot requested reaches the ranker as an automated assignment; a
// human request keeps its urgency signal. The agent plane classifies what a run
// reads as well as what it writes, so a run ranks an item by the current bot list,
// not by the flag stored before an upgrade or a change to BOT_REVIEWERS.
func (p RunPolicy) Classify(items []WorkItem) {
	for i := range items {
		items[i].Signals.ReviewRequestedByBot = p.IsBot(items[i].Signals.ReviewRequestedBy)
	}
}

// ClassifyReads returns s with its reads (List, Get, Briefs) classified by the
// policy (Classify), so the user's own reads score an item by the same bot list as
// the agent plane's: the radar's rank and the one Discuss explains agree even for an item stored before an
// upgrade or a change to BOT_REVIEWERS. Writes pass through unchanged.
func (p RunPolicy) ClassifyReads(s Store) Store {
	return classifiedStore{Store: s, policy: p}
}

type classifiedStore struct {
	Store
	policy RunPolicy
}

func (c classifiedStore) List(ctx context.Context, ownerID string) ([]WorkItem, error) {
	items, err := c.Store.List(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	c.policy.Classify(items)
	return items, nil
}

// Get is the store's Get, classified: a run's single-item read (ReadItem).
func (c classifiedStore) Get(ctx context.Context, ownerID, id string) (WorkItem, bool, error) {
	it, ok, err := c.Store.Get(ctx, ownerID, id)
	if err != nil || !ok {
		return it, ok, err
	}
	one := []WorkItem{it}
	c.policy.Classify(one)
	return one[0], true, nil
}

// Briefs is the store's Briefs, classified: a run's bounded selection (SelectFrom),
// whose top-N ranking scores by the classification.
func (c classifiedStore) Briefs(ctx context.Context, ownerID string, q BriefQuery) ([]WorkItem, error) {
	items, err := c.Store.Briefs(ctx, ownerID, q)
	if err != nil {
		return nil, err
	}
	c.policy.Classify(items)
	return items, nil
}
