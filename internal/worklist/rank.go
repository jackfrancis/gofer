package worklist

import (
	"context"
	"time"
)

// AxisProposal is an LLM-proposed set of the four ranking axes for one item, with
// a confidence and rationale. It is an INPUT to gofer's deterministic blend: gofer
// uses it for the axes it returns (bounded to [0,1]) and blends into Rank itself,
// so attacker-influenced content cannot fully hijack ordering.
type AxisProposal struct {
	Relevance  float64 `json:"relevance"`
	Impact     float64 `json:"impact"`
	Engagement float64 `json:"engagement"`
	Urgency    float64 `json:"urgency"`
	Confidence float64 `json:"confidence"`
	Rationale  string  `json:"rationale,omitempty"`
	// ReviewRequestedByBot is the item's Signals.ReviewRequestedByBot when the
	// proposal was made. A proposal made under another classification (the requester
	// was added to or removed from BOT_REVIEWERS since) is stale: CurrentProposal
	// drops it, so the item falls back to its baseline until it is ranked again.
	// Proposals from before this field were all made with the flag unset, which is
	// what its zero value records.
	ReviewRequestedByBot bool `json:"review_requested_by_bot,omitempty"`
}

// CurrentProposal returns the item's model proposal, or nil when there is none or it
// was made under a different bot classification than the item has now. Everything
// that uses a proposal reads it through here.
func (s Signals) CurrentProposal() *AxisProposal {
	if s.Proposed == nil || s.Proposed.ReviewRequestedByBot != s.ReviewRequestedByBot {
		return nil
	}
	return s.Proposed
}

// AxisRanker proposes the four axes for an item. The real implementation calls an
// LLM from an agent runtime; gofer core depends only on this interface so it never
// imports a model client.
type AxisRanker interface {
	Propose(ctx context.Context, item WorkItem) (AxisProposal, error)
}

// StubRanker is a deterministic AxisRanker that proposes the signal-based baseline
// axes with full confidence. It makes the ranking pipeline exercisable before a
// real model is attached; its proposal echoes the baseline, so it never changes
// ordering.
type StubRanker struct {
	now func() time.Time
}

// NewStubRanker returns a StubRanker using the wall clock.
func NewStubRanker() *StubRanker { return &StubRanker{now: time.Now} }

// Propose returns the deterministic baseline axes as the proposal.
func (s *StubRanker) Propose(_ context.Context, item WorkItem) (AxisProposal, error) {
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	rel, imp, eng, urg := baselineAxes(item.Signals, now().UTC())
	return AxisProposal{
		Relevance:  rel,
		Impact:     imp,
		Engagement: eng,
		Urgency:    urg,
		Confidence: 1,
		Rationale:  "baseline (stub ranker)",
	}, nil
}

// ResearchAdjustment is the discussion-derived re-weighting of the four foundation
// axes. Each multiplier scales the corresponding axis: 1.0 leaves it unchanged, <1
// dampens, >1 amplifies. Multipliers are bounded to [0,2] and the product to [0,1]
// when Score applies them. The zero value is NOT neutral — 1.0 is; producers must
// set all four.
type ResearchAdjustment struct {
	Relevance  float64   `json:"relevance"`
	Impact     float64   `json:"impact"`
	Engagement float64   `json:"engagement"`
	Urgency    float64   `json:"urgency"`
	Rationale  string    `json:"rationale,omitempty"`
	AppliedAt  time.Time `json:"applied_at"`
	// Turns are the keys (WorkItem.TurnKey) of the thread turns the research read,
	// so a reader can tell whether they still belong to the conversation it is
	// shown alongside (ResearchReflectsVisibleThread). Empty on research stored
	// before it recorded them, and on research that read no turns.
	Turns []string `json:"turns,omitempty"`
	// GitHubUpdatedAt is the item's GitHub UpdatedAt as the research read it, so a
	// GitHub update stored after the research read the item (a refresh landing while
	// the model worked) still makes the research stale (ResearchStale), however late
	// the research was applied. Zero on research stored before it recorded it.
	GitHubUpdatedAt time.Time `json:"github_updated_at,omitzero"`
}

// Multipliers returns the per-axis factors Score actually applies: each bounded to
// [0,2]. An explanation of a score must use these, not the raw fields.
func (r ResearchAdjustment) Multipliers() (rel, imp, eng, urg float64) {
	return clampMult(r.Relevance), clampMult(r.Impact), clampMult(r.Engagement), clampMult(r.Urgency)
}

// ResearchRanker proposes the research re-weighting for an item from its
// conversation thread, layered on the foundation proposal. gofer core depends only
// on this interface so it never imports a model client.
type ResearchRanker interface {
	Research(ctx context.Context, item WorkItem) (ResearchAdjustment, error)
}
