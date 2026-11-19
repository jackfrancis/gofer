package worklist

import "strings"

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
//   - Signals.ReviewRequestedByBot is derived from Signals.ReviewRequestedBy against
//     the bot list, replacing any value the run sent. A review a bot requested
//     reaches the ranker as an automated assignment; a human request keeps its
//     urgency signal.
func (p RunPolicy) Apply(items []WorkItem) {
	for i := range items {
		items[i].Meta.Origin = OriginAgent
		items[i].Signals.ReviewRequestedByBot = p.IsBot(items[i].Signals.ReviewRequestedBy)
	}
}
