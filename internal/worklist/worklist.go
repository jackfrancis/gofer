// Package worklist models the user's ordered set of work — GitHub issues and PRs
// enriched with gofer metadata — plus the persistence and ingestion seams behind
// it.
//
// Ordering depends on gofer metadata that cannot be inferred from the GitHub item
// alone, so sorting happens server-side (see sort.go). Scoring is a pure function
// of an item's Signals (see score.go), so the worklist can be re-scored at read
// time without re-fetching from the provider.
package worklist

import (
	"context"
	"time"
)

// Priority is a coarse, gofer-assigned importance band.
type Priority string

const (
	PriorityNone   Priority = ""
	PriorityLow    Priority = "low"
	PriorityMedium Priority = "medium"
	PriorityHigh   Priority = "high"
)

// ItemType distinguishes the kind of work item.
type ItemType string

const (
	TypeIssue       ItemType = "issue"
	TypePullRequest ItemType = "pull_request"
)

// Reason records why an item is on the user's radar. An item may surface for
// several reasons at once (e.g. authored and review-requested); they are the
// raw relationship facts that feed the Relevance and Urgency axes.
type Reason string

const (
	ReasonReviewRequested Reason = "review_requested"
	ReasonAssignee        Reason = "assignee"
	ReasonAuthor          Reason = "author"
	ReasonCommented       Reason = "commented"
	ReasonMentioned       Reason = "mentioned"
	ReasonTeamMentioned   Reason = "team_mentioned"
	ReasonCodeowner       Reason = "codeowner"
)

// Origin records who set the metadata, so human overrides outrank agent values.
const (
	OriginAgent = "agent"
	OriginUser  = "user"
)

// GitHubRef identifies the upstream GitHub item.
type GitHubRef struct {
	Number    int       `json:"number"`
	Repo      string    `json:"repo"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	State     string    `json:"state"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Metadata is gofer's judgment about an item, derived from its Signals. The four
// axes are orthogonal, each normalized 0..1 and independently sortable; Rank is
// their weighted blend (the default "most important first"). It cannot be derived
// from the GitHub item alone.
type Metadata struct {
	// Score axes (0..1).
	Relevance  float64 `json:"relevance"`  // closeness to me / my active work
	Impact     float64 `json:"impact"`     // strategic / org importance
	Engagement float64 `json:"engagement"` // social heat: level + velocity
	Urgency    float64 `json:"urgency"`    // time pressure / someone blocked on me
	Rank       float64 `json:"rank"`       // weighted blend of the axes

	// Priority is a coarse, human-facing band derived from Rank.
	Priority Priority `json:"priority"`

	// Contributions explain the score: which signals drove which axis, so the
	// Rationale is derived rather than authored.
	Contributions []Contribution `json:"contributions,omitempty"`
	Rationale     string         `json:"rationale,omitempty"`

	Origin    string    `json:"origin"` // OriginAgent | OriginUser
	ScoredAt  time.Time `json:"scored_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// HiddenAt is when the user hid this item; zero means visible. The user sets
	// it; an agent clears it (auto-unhide) once the underlying GitHub item is
	// updated after this time, so a changed item resurfaces.
	HiddenAt time.Time `json:"hidden_at,omitempty"`

	// CompletedAt is when the underlying GitHub item was closed or merged; zero
	// means still open. An agent sets it when the item leaves the open radar;
	// re-ingesting the item as open clears it, so a reopened item resurfaces.
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

// Contribution is one explainable factor in a score: a signal's signed weight
// toward an axis, with a human-readable detail for the UI.
type Contribution struct {
	Axis   string  `json:"axis"`   // relevance | impact | engagement | urgency
	Signal string  `json:"signal"` // e.g. "review_requested", "comment_accel"
	Weight float64 `json:"weight"` // signed contribution
	Detail string  `json:"detail,omitempty"`
}

// WorkItem is one unit of work owned by a user.
type WorkItem struct {
	ID      string    `json:"id"`
	OwnerID string    `json:"owner_id"`
	Source  string    `json:"source"` // "github"
	Type    ItemType  `json:"type"`
	GitHub  GitHubRef `json:"github"`
	Signals Signals   `json:"signals"`             // observed facts that feed scoring
	Meta    Metadata  `json:"gofer"`               // gofer's judgment derived from Signals
	Thread  []Message `json:"thread,omitempty"`    // assistive conversation (ported later)
	// ThreadReadAt is when the owner last read the thread; zero means never. An
	// agent reply newer than this is "unread", so the radar can flag threads with
	// a response the user hasn't seen yet.
	ThreadReadAt time.Time `json:"thread_read_at,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Signals are the observed FACTS about an item — measured, not judged. They are
// the inputs to scoring and are kept verbatim so an item can be re-scored when
// the weighting changes, and so any score can be explained. ObservedAt records
// the freshness of the measurement.
type Signals struct {
	// Relationship to the acting user (why it's on the radar).
	Reasons       []Reason `json:"reasons,omitempty"`        // review_requested | assignee | author | ...
	RelatedActive []string `json:"related_active,omitempty"` // IDs of my active items this overlaps
	// ReviewRequestedBy is the GitHub login that requested the user's review;
	// empty when none is pending. gofer derives ReviewRequestedByBot from it
	// against the configured bot list, so an automated assignment is not mistaken
	// for an explicit human request.
	ReviewRequestedBy    string `json:"review_requested_by,omitempty"`
	ReviewRequestedByBot bool   `json:"review_requested_by_bot,omitempty"`

	// Engagement / heat.
	Comments          int     `json:"comments"`
	Participants      int     `json:"participants"`
	Reactions         int     `json:"reactions"`
	InfluentialActors int     `json:"influential_actors"`
	InboundRefs       int     `json:"inbound_refs"`
	OtherReviewers    int     `json:"other_reviewers"`
	CommentVelocity   float64 `json:"comment_velocity"`
	CommentAccel      float64 `json:"comment_accel"`

	// Temporal.
	OpenedAt            time.Time `json:"opened_at"`
	LastActivityAt      time.Time `json:"last_activity_at"`
	AwaitingMeSince     time.Time `json:"awaiting_me_since"`
	AwaitingOthersSince time.Time `json:"awaiting_others_since"`
	DeadlineAt          time.Time `json:"deadline_at"`
	Reopened            bool      `json:"reopened"`

	// Strategic context.
	RepoTier      int      `json:"repo_tier"`
	Labels        []string `json:"labels,omitempty"`
	Blocking      int      `json:"blocking"`
	RoadmapThemes []string `json:"roadmap_themes,omitempty"`

	// Soft / external (probabilistic; never treated as ground truth).
	Topics          []string `json:"topics,omitempty"`
	TrendScore      float64  `json:"trend_score"`
	DiffuseInterest float64  `json:"diffuse_interest"`

	// Proposed holds an LLM-proposed set of axes. It is an INPUT gofer ratifies
	// against the deterministic baseline; nil when no ranker has run.
	Proposed *AxisProposal `json:"proposed,omitempty"`

	// Research holds the discussion-derived per-axis re-weighting. Score
	// multiplies the foundation axes by these factors; nil when no research pass
	// has run.
	Research *ResearchAdjustment `json:"research,omitempty"`

	ObservedAt time.Time `json:"observed_at"` // freshness of this measurement
}

// Store is the owner-scoped persistence contract for work items. The cloud
// persistence backend will implement this without changing callers.
type Store interface {
	List(ctx context.Context, ownerID string) ([]WorkItem, error)
	// Upsert adds or replaces an owner's items, keyed by WorkItem.ID.
	// Implementations scope every item to ownerID so an agent cannot write another
	// user's data.
	Upsert(ctx context.Context, ownerID string, items ...WorkItem) error
}

// Lister enumerates stored items across all owners. It is a cross-owner
// CONTROL-PLANE read used by the staleness reconciler; the request path never
// uses it.
type Lister interface {
	All(ctx context.Context) (map[string][]WorkItem, error)
}

// Ingestor starts the serialized agentic flow that backfills a user's work
// items. Implementations MUST be idempotent and MUST return promptly —
// EnsureBackfill is invoked from the request path when a worklist is empty, so it
// should enqueue work rather than block on it.
type Ingestor interface {
	EnsureBackfill(ctx context.Context, ownerID string) error
}
