package worklist

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Scoring weights blend the four axes into Rank. They are deliberate, hand-set
// placeholders, moved to configuration once tuned.
const (
	wRelevance  = 0.35
	wUrgency    = 0.30
	wImpact     = 0.20
	wEngagement = 0.15
)

// Score computes an item's gofer Metadata from its Signals, as of now. It is a
// pure function of the item, so a worklist can be re-scored when the weights
// change without re-fetching from the provider. Origin is preserved; the caller
// owns persistence timestamps.
//
// When the item carries an LLM AxisProposal, that proposal is authoritative for
// the four axes it returns (bounded only to [0,1]); the deterministic baseline is
// the fallback used when no proposal is present. gofer still owns how the axes
// blend into Rank.
//
// Time-dependent axes (Engagement, Urgency) are evaluated against now, so a later
// read-time rescore keeps them fresh.
func Score(item WorkItem, now time.Time) Metadata {
	var contribs []Contribution

	rel := relevanceScore(item.Signals, &contribs)
	urg := urgencyScore(item.Signals, now, &contribs)
	eng := engagementScore(item.Signals, &contribs)
	imp := impactScore(item.Signals, &contribs)

	// An LLM proposal is authoritative for the four axes it returns: gofer uses
	// the proposed values directly, bounded only to [0,1]. The baseline above is
	// the fallback when no proposal is present.
	if p := item.Signals.Proposed; p != nil {
		rel = clampUnit(p.Relevance)
		imp = clampUnit(p.Impact)
		eng = clampUnit(p.Engagement)
		urg = clampUnit(p.Urgency)
		detail := p.Rationale
		if detail == "" {
			detail = "LLM-scored axes"
		}
		add(&contribs, "rank", "llm", p.Confidence, detail)
	}

	// The research layer re-weights the foundation axes from the conversation's
	// evidence: each axis is scaled by its multiplier (bounded to [0,2]), and the
	// product is bounded to [0,1]. The metadata remains the anchor: a multiplier
	// cannot manufacture an axis a near-zero foundation does not support.
	if rsh := item.Signals.Research; rsh != nil {
		rel = clampUnit(rel * clampMult(rsh.Relevance))
		imp = clampUnit(imp * clampMult(rsh.Impact))
		eng = clampUnit(eng * clampMult(rsh.Engagement))
		urg = clampUnit(urg * clampMult(rsh.Urgency))
		detail := rsh.Rationale
		if detail == "" {
			detail = "research re-weighting"
		}
		add(&contribs, "rank", "research", 0, detail)
	}

	rank := wRelevance*rel + wUrgency*urg + wImpact*imp + wEngagement*eng

	m := Metadata{
		Relevance:     rel,
		Impact:        imp,
		Engagement:    eng,
		Urgency:       urg,
		Rank:          rank,
		Priority:      band(rank),
		Contributions: contribs,
		Origin:        item.Meta.Origin,
		ScoredAt:      now,
	}
	switch {
	case item.Signals.Research != nil && item.Signals.Research.Rationale != "":
		m.Rationale = item.Signals.Research.Rationale
	case item.Signals.Proposed != nil && item.Signals.Proposed.Rationale != "":
		m.Rationale = item.Signals.Proposed.Rationale
	default:
		m.Rationale = rationale(contribs)
	}
	return m
}

// baselineAxes returns the four deterministic, signal-based axis values without
// recording contributions. It is the fallback when no proposal is ratified and
// the reference the StubRanker echoes.
func baselineAxes(s Signals, now time.Time) (rel, imp, eng, urg float64) {
	var ignore []Contribution
	rel = relevanceScore(s, &ignore)
	urg = urgencyScore(s, now, &ignore)
	eng = engagementScore(s, &ignore)
	imp = impactScore(s, &ignore)
	return rel, imp, eng, urg
}

// clampUnit bounds a value to [0,1], the valid range for an axis.
func clampUnit(f float64) float64 { return math.Max(0, math.Min(1, f)) }

// clampMult bounds a research multiplier to [0,2]; 1.0 is neutral.
func clampMult(f float64) float64 { return math.Max(0, math.Min(2, f)) }

// relevanceScore: the strongest relationship reason wins, with a small bump for
// corroborating reasons.
func relevanceScore(s Signals, c *[]Contribution) float64 {
	weights := map[Reason]float64{
		ReasonReviewRequested: 1.0,
		ReasonAssignee:        0.9,
		ReasonCodeowner:       0.8,
		ReasonAuthor:          0.6,
		ReasonCommented:       0.5,
		ReasonMentioned:       0.5,
		ReasonInvolved:        0.5,
		ReasonTeamMentioned:   0.4,
	}
	var best float64
	var bestReason Reason
	for _, r := range s.Reasons {
		if w := weights[r]; w > best {
			best, bestReason = w, r
		}
	}
	if best == 0 {
		return 0
	}
	if n := len(s.Reasons); n > 1 {
		best = math.Min(1, best+0.05*float64(n-1))
	}
	add(c, "relevance", string(bestReason), best, "on your radar as "+humanReason(bestReason))
	return best
}

// urgencyScore: an outstanding ask on the user, a review request, or an
// approaching deadline all create time pressure; the strongest wins.
func urgencyScore(s Signals, now time.Time, c *[]Contribution) float64 {
	var urg float64

	if !s.AwaitingMeSince.IsZero() {
		days := now.Sub(s.AwaitingMeSince).Hours() / 24
		u := math.Min(1, 0.6+0.1*days)
		if u > urg {
			urg = u
		}
		add(c, "urgency", "awaiting_me", u, "awaiting your response since "+s.AwaitingMeSince.Format("Jan 2"))
	}

	if containsReason(s.Reasons, ReasonReviewRequested) && urg < 0.6 {
		urg = 0.6
		add(c, "urgency", string(ReasonReviewRequested), 0.6, "review requested")
	}

	if !s.DeadlineAt.IsZero() {
		if u := deadlineUrgency(s.DeadlineAt, now); u > 0 {
			if u > urg {
				urg = u
			}
			add(c, "urgency", "deadline", u, "due "+s.DeadlineAt.Format("Jan 2"))
		}
	}
	return urg
}

// deadlineUrgency ramps from 0 at 30 days out to 1 at (or past) the due date.
func deadlineUrgency(due, now time.Time) float64 {
	d := due.Sub(now).Hours() / 24
	switch {
	case d <= 0:
		return 1
	case d >= 30:
		return 0
	default:
		return 1 - d/30
	}
}

// engagementScore: social heat from discussion level and velocity, each
// saturating so a runaway thread cannot dominate the blend.
func engagementScore(s Signals, c *[]Contribution) float64 {
	comments := saturate(float64(s.Comments), 10)
	participants := saturate(float64(s.Participants), 8)
	reactions := saturate(float64(s.Reactions), 20)
	velocity := saturate(s.CommentVelocity, 5)
	eng := math.Min(1, 0.4*comments+0.3*participants+0.15*reactions+0.15*velocity)
	if eng > 0 {
		add(c, "engagement", "discussion", eng,
			fmt.Sprintf("%d comments from %d people, %d reactions", s.Comments, s.Participants, s.Reactions))
	}
	return eng
}

// impactScore: strategic importance from labels, repo tier, and hub centrality;
// strongest wins.
func impactScore(s Signals, c *[]Contribution) float64 {
	var imp float64
	var detail, signal string
	for _, l := range s.Labels {
		if w, name := labelImpact(l); w > imp {
			imp, detail, signal = w, name, "label"
		}
	}
	if s.RepoTier > 0 {
		if t := math.Min(1, float64(s.RepoTier)/3); t > imp {
			imp, detail, signal = t, "strategic repo", "repo_tier"
		}
	}
	if s.InboundRefs > 0 {
		if r := saturate(float64(s.InboundRefs), 5); r > imp {
			imp, detail, signal = r, fmt.Sprintf("referenced by %d items", s.InboundRefs), "inbound_refs"
		}
	}
	if imp > 0 {
		add(c, "impact", signal, imp, detail)
	}
	return imp
}

// labelImpact maps a GitHub label to a strategic-importance weight.
func labelImpact(label string) (float64, string) {
	l := strings.ToLower(label)
	switch {
	case strings.Contains(l, "security"), strings.Contains(l, "vuln"):
		return 1.0, "security"
	case strings.Contains(l, "release-blocker"), strings.Contains(l, "critical"):
		return 0.9, l
	case strings.HasPrefix(l, "priority/p0"), strings.HasPrefix(l, "priority/p1"),
		strings.Contains(l, "priority/important"):
		return 0.75, l
	case strings.HasPrefix(l, "kind/bug"):
		return 0.6, "bug"
	default:
		return 0, ""
	}
}

func band(rank float64) Priority {
	switch {
	case rank >= 0.66:
		return PriorityHigh
	case rank >= 0.33:
		return PriorityMedium
	case rank > 0:
		return PriorityLow
	default:
		return PriorityNone
	}
}

func saturate(x, k float64) float64 {
	if x <= 0 {
		return 0
	}
	return x / (x + k)
}

func add(c *[]Contribution, axis, signal string, weight float64, detail string) {
	*c = append(*c, Contribution{Axis: axis, Signal: signal, Weight: weight, Detail: detail})
}

// rationale leads with the highest-weight contribution's human-readable detail.
func rationale(c []Contribution) string {
	if len(c) == 0 {
		return ""
	}
	top := c[0]
	for _, x := range c[1:] {
		if x.Weight > top.Weight {
			top = x
		}
	}
	return top.Detail
}

func containsReason(rs []Reason, r Reason) bool {
	for _, x := range rs {
		if x == r {
			return true
		}
	}
	return false
}

func humanReason(r Reason) string {
	switch r {
	case ReasonReviewRequested:
		return "a review request"
	case ReasonAssignee:
		return "assigned to you"
	case ReasonCodeowner:
		return "code you own"
	case ReasonAuthor:
		return "authored by you"
	case ReasonCommented:
		return "a comment you left"
	case ReasonMentioned:
		return "a mention"
	case ReasonInvolved:
		return "work you are involved in"
	case ReasonTeamMentioned:
		return "a team mention"
	default:
		return string(r)
	}
}
