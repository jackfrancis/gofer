package llm

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// systemPrompt defines the task and the four axes so the model scores
// consistently with gofer's signal-based baseline. gofer blends the output into
// Rank, so the prompt aims for calibrated, honest scores rather than trusting the
// model blindly.
const systemPrompt = `You rank a software engineer's GitHub work items for a personal "what needs my attention" radar.

Score the item on four axes, each a number from 0.0 to 1.0:
- relevance: how much this item needs the user's OWN attention right now. High when someone has explicitly asked for the user's participation — a review explicitly requested of them, an ASSIGNMENT to them, their own PR, or a direct mention of their username. An explicit assignment stays high even when "others_reviewing" is set. Relevance drops to LOW only when "waiting_on_others" is set (the user has already acted, or progress is on the author or a third party). "others_reviewing" lowers relevance only for a bystander who was neither assigned nor explicitly asked. If "review_auto_assigned" is set a bot auto-requested the review, so treat that review-request as a weaker signal.
- impact: how consequential the underlying change is (release-blocking bugs, security, broad blast radius score high; trivial or cosmetic score low).
- engagement: how much active collaboration is happening (comments, distinct participants, reactions, cross-references).
- urgency: how time-sensitive it is FOR THE USER (someone is waiting on their participation, a deadline is near, or it is going stale). An explicit assignment or a human review request makes it time-sensitive. If "waiting_on_others" is set, urgency is ~0. "others_reviewing" lowers urgency only for a bystander. If "review_auto_assigned" is set, urgency stays low unless they are also an assignee.

Also return:
- confidence: 0.0 to 1.0, how sure you are given the limited signals. Be honest; low signal means low confidence.
- rationale: one short sentence explaining the scores, addressed directly to the reader in the SECOND PERSON ("you"/"your"). Never write "the user", "they", or "their".

Respond with ONLY a JSON object, no prose, with exactly these keys:
{"relevance":0.0,"impact":0.0,"engagement":0.0,"urgency":0.0,"confidence":0.0,"rationale":"..."}`

// userPrompt serializes the item's observable signals into a compact JSON object
// for the model. Only facts gofer already holds are sent; the runtime does not
// fetch anything extra for ranking.
func userPrompt(item worklist.WorkItem) string {
	now := time.Now().UTC()
	reasons := make([]string, 0, len(item.Signals.Reasons))
	for _, r := range item.Signals.Reasons {
		reasons = append(reasons, string(r))
	}

	summary := map[string]any{
		"repo":         item.GitHub.Repo,
		"type":         item.Type,
		"title":        item.GitHub.Title,
		"state":        item.GitHub.State,
		"reasons":      reasons,
		"labels":       item.Signals.Labels,
		"comments":     item.Signals.Comments,
		"participants": item.Signals.Participants,
		"reactions":    item.Signals.Reactions,
		"inbound_refs": item.Signals.InboundRefs,
		"age_days":     daysSince(item.GitHub.UpdatedAt, now),
	}
	if !item.Signals.AwaitingMeSince.IsZero() && !item.Signals.ReviewRequestedByBot {
		summary["awaiting_me_days"] = daysSince(item.Signals.AwaitingMeSince, now)
	}
	if !item.Signals.AwaitingOthersSince.IsZero() {
		summary["waiting_on_others"] = true
		summary["awaiting_others_days"] = daysSince(item.Signals.AwaitingOthersSince, now)
	}
	if item.Signals.OtherReviewers > 0 {
		summary["others_reviewing"] = item.Signals.OtherReviewers
	}
	if item.Signals.ReviewRequestedByBot {
		summary["review_auto_assigned"] = true
	}
	if !item.Signals.DeadlineAt.IsZero() {
		summary["deadline_in_days"] = daysSince(now, item.Signals.DeadlineAt)
	}

	b, err := json.Marshal(summary)
	if err != nil {
		return fmt.Sprintf("Score this GitHub %s in %s.", item.Type, item.GitHub.Repo)
	}
	return "Score this GitHub work item:\n" + string(b)
}

// daysSince returns whole days between two times, never negative.
func daysSince(from, to time.Time) int {
	d := to.Sub(from).Hours() / 24
	if d < 0 {
		return 0
	}
	return int(d)
}
