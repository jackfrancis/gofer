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
	"net/url"
	"slices"
	"strings"
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
	// ReasonInvolved is the honest fallback when the provider can only prove that the
	// user is involved, not how: GitHub's involves: qualifier is a union (authored,
	// assigned, mentioned, commented) and does not report which member matched. The
	// specific relationships that the item payload proves are used instead where
	// possible, and enrich upgrades this to ReasonCommented once the timeline shows the
	// user actually spoke.
	ReasonInvolved Reason = "involved"
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

	// Draft marks a draft pull request, Merged a closed pull request that was
	// merged, and StateReason is GitHub's reason a closed issue was closed
	// ("completed", "not_planned"). Only the radar's state icon reads them; an
	// item stored before they were recorded reads as plain open/closed.
	Draft       bool   `json:"draft,omitempty"`
	Merged      bool   `json:"merged,omitempty"`
	StateReason string `json:"state_reason,omitempty"`

	// LabelColors maps each of the item's labels (Signals.Labels) to its GitHub
	// color, six hex digits without '#', for display only. Read a color through
	// LabelColor, which refuses anything but six hex digits.
	LabelColors map[string]string `json:"label_colors,omitempty"`

	// CI summarizes the CI jobs on a pull request's head commit, for display only;
	// nil when unknown (an issue, a pull request enrich has not read yet, or one
	// whose CI could not be read). Enrich sets it; a refresh keeps the stored one.
	CI *CISummary `json:"ci,omitempty"`
}

// CISummary is the CI picture of one pull request's head commit: how many jobs
// succeeded and are still running, and every failed job, by name and link.
type CISummary struct {
	SHA     string `json:"sha,omitempty"` // the commit the jobs ran on
	Passed  int    `json:"passed"`
	Pending int    `json:"pending"`
	// Failed names each failed job, in the order GitHub's checks were read (by name).
	// At most MaxCIFailedNames are kept, each at most MaxCINameBytes; FailedOmitted
	// counts the failed jobs past that bound.
	Failed []string `json:"failed,omitempty"`
	// FailedURLs holds, index for index with Failed, the link GitHub shows for that
	// job on the pull request (a status's target URL, such as a Prow job page, or a
	// check run's details URL); "" for none. It is a list beside Failed rather than
	// a field of each entry so the stored shape of Failed stays a list of names,
	// which an older web tier still reads. Read the pairs through FailedJobs, which
	// admits only http(s) links.
	FailedURLs    []string `json:"failed_urls,omitempty"`
	FailedOmitted int      `json:"failed_omitted,omitempty"`
	// Partial is set when part of the CI could not be read (check runs refused,
	// more jobs than were paged), so the counts may be short.
	Partial bool `json:"partial,omitempty"`
}

// CIJob is one failed job as FailedJobs returns it.
type CIJob struct {
	Name string
	URL  string // an absolute http(s) link, or "" for none
}

// Bounds on a stored CISummary.
const (
	MaxCIFailedNames = 50
	MaxCINameBytes   = 256
	MaxCIURLBytes    = 2048
)

// AddFailed records a failed job's name and link within the bounds. A link that is
// not an absolute http(s) URL, or is longer than MaxCIURLBytes, is stored as none.
func (c *CISummary) AddFailed(name, link string) {
	if len(c.Failed) >= MaxCIFailedNames {
		c.FailedOmitted++
		return
	}
	c.Failed = append(c.Failed, truncateUTF8(name, MaxCINameBytes))
	c.FailedURLs = append(c.FailedURLs, CIJobURL(link))
}

// FailedJobs returns each failed job with its link. The link is re-checked here,
// since the store holds whatever a run wrote, and a list misaligned with Failed
// leaves the unmatched jobs without one.
func (c *CISummary) FailedJobs() []CIJob {
	out := make([]CIJob, len(c.Failed))
	for i, name := range c.Failed {
		out[i].Name = name
		if i < len(c.FailedURLs) {
			out[i].URL = CIJobURL(c.FailedURLs[i])
		}
	}
	return out
}

// CIJobURL returns link if it is an absolute http or https URL with a host and at
// most MaxCIURLBytes, and "" otherwise. A CI link reaches a page's href.
func CIJobURL(link string) string {
	if link == "" || len(link) > MaxCIURLBytes {
		return ""
	}
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return ""
	}
	return link
}

// Equal reports whether c and o summarize the same CI (nil only equals nil).
func (c *CISummary) Equal(o *CISummary) bool {
	if c == nil || o == nil {
		return c == o
	}
	return c.SHA == o.SHA && c.Passed == o.Passed && c.Pending == o.Pending &&
		c.FailedOmitted == o.FailedOmitted && c.Partial == o.Partial &&
		slices.Equal(c.Failed, o.Failed) && slices.Equal(c.FailedURLs, o.FailedURLs)
}

// LabelColor returns the GitHub color of the item's label name, normalized to six
// lowercase hex digits, or "" if none is stored or the stored value is not a color.
// The value reaches a stylesheet, so nothing else may pass.
func (g GitHubRef) LabelColor(name string) string {
	return NormalizeLabelColor(g.LabelColors[name])
}

// NormalizeLabelColor returns c as six lowercase hex digits (a leading '#' is
// allowed), or "" if c is not a color in that form.
func NormalizeLabelColor(c string) string {
	c = strings.TrimPrefix(c, "#")
	if len(c) != 6 {
		return ""
	}
	for i := 0; i < len(c); i++ {
		switch b := c[i]; {
		case '0' <= b && b <= '9', 'a' <= b && b <= 'f', 'A' <= b && b <= 'F':
		default:
			return ""
		}
	}
	return strings.ToLower(c)
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
	Signals Signals   `json:"signals"`          // observed facts that feed scoring
	Meta    Metadata  `json:"gofer"`            // gofer's judgment derived from Signals
	Thread  []Message `json:"thread,omitempty"` // assistive conversation (ported later)
	// ThreadReadThrough is the read cursor: the key (TurnKey) of the newest agent
	// turn the owner has seen, set when the thread page renders it (MarkRead). An
	// agent turn after it in the thread is "unread", so the radar can flag threads
	// with a response the user hasn't seen yet. Empty means never read; a key no
	// longer in the thread (it was cleared) reads as nothing seen.
	//
	// The cursor names a turn rather than a time because a reply's time is stamped
	// by the run that wrote it, and replies land out of order: a reply that arrives
	// after the page was built can carry an older time than the one shown.
	ThreadReadThrough string `json:"thread_read_through,omitempty"`
	// ThreadReadAt is the legacy, time-based read cursor: an agent reply newer than
	// it is unread. It applies only while ThreadReadThrough is empty, and gofer no
	// longer sets it.
	ThreadReadAt time.Time `json:"thread_read_at,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	// Projected is set only on an item a run read cut to size (Project): it names
	// each cut field with a fingerprint of the value the run received, so the
	// write-back restores the stored value (MergeRunItems). It is never stored.
	Projected map[string]string `json:"projected,omitempty"`
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
	// user's data. It is a BLIND write: it replaces whatever is stored, so a caller
	// that read the items first can overwrite a change made in between. Use Update
	// to change items that already exist.
	Upsert(ctx context.Context, ownerID string, items ...WorkItem) error
	// Update atomically reads, changes and writes an owner's items. fn receives a
	// copy of the owner's current items and returns the items to write (added or
	// replaced by ID, as Upsert); no other write to the store interleaves between
	// the read and the write. fn must be quick and do no I/O. If fn returns an
	// error, nothing is written and Update returns it.
	Update(ctx context.Context, ownerID string, fn func(current []WorkItem) ([]WorkItem, error)) error
	// Get returns the owner's item with id, thread included; ok is false if the
	// owner has none. It reads that one item, never another owner's.
	Get(ctx context.Context, ownerID, id string) (item WorkItem, ok bool, err error)
	// Briefs returns the owner's items that q names, without their threads
	// (ApplyBriefQuery is the reference), reading no more than it returns. It backs
	// the bounded reads a run makes (SelectFrom).
	Briefs(ctx context.Context, ownerID string, q BriefQuery) ([]WorkItem, error)
}

// Lister enumerates stored items across all owners. It is a cross-owner
// CONTROL-PLANE read used by the staleness reconciler; the request path never
// uses it.
type Lister interface {
	All(ctx context.Context) (map[string][]WorkItem, error)
}

// OwnerLister names every owner with at least one stored item. It is a cross-owner
// CONTROL-PLANE read used by the scheduled refresh (ingest.Refresher), which needs
// only the owners, not their items; the request path never uses it.
type OwnerLister interface {
	Owners(ctx context.Context) ([]string, error)
}

// Ingestor starts the serialized agentic flow that backfills a user's work
// items. Implementations MUST be idempotent and MUST return promptly —
// EnsureBackfill is invoked from the request path when a worklist is empty, so it
// should enqueue work rather than block on it.
type Ingestor interface {
	EnsureBackfill(ctx context.Context, ownerID string) error
}
