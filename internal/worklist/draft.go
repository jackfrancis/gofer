package worklist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// This file is the draft review (ADR 0010): a converse run, asked by a
// KindDraftReviewRequest turn, reads the review conversation and the pull request's
// diff and answers with the comments the conversation agrees on, anchored to diff
// lines. The reply carries them as a DraftReview. Nothing is posted by the run: the
// web tier shows the draft under the reply, and only the owner's confirmation creates
// a PENDING review on GitHub, with the write grant, which never leaves the web tier.

// Bounds on a stored draft review. AppendReply enforces them, so whatever a run
// sends, a stored draft is at most about MaxDraftComments*(MaxDraftCommentBytes +
// MaxDraftPathBytes) + MaxDraftBodyBytes.
const (
	MaxDraftComments     = 30
	MaxDraftCommentBytes = 4 << 10
	MaxDraftPathBytes    = 1 << 10
	MaxDraftBodyBytes    = 8 << 10
	maxDraftRefBytes     = 255
)

// Diff sides a draft comment anchors to, as GitHub names them: RIGHT is the pull
// request's version of the file (an added or unchanged line), LEFT the base's (a
// deleted line).
const (
	SideRight = "RIGHT"
	SideLeft  = "LEFT"
)

// DraftReview is the structured result of a draft-review run: inline comments
// anchored to lines of one pull request's diff, and a summary Body for the agreed
// points no diff line carries. It rides on the agent reply (Message.Draft).
//
// Repo, Number, Commit and BaseRef pin what the comments were anchored to. They are
// part of the draft, so its Digest binds them: the web tier creates the review only
// on that pull request (still the item's), while its head is still Commit and its
// base branch still BaseRef, since a push or a retarget changes what the lines mean.
type DraftReview struct {
	Repo     string         `json:"repo"`
	Number   int            `json:"number"`
	Commit   string         `json:"commit"`
	BaseRef  string         `json:"base_ref"`
	Body     string         `json:"body,omitempty"`
	Comments []DraftComment `json:"comments,omitempty"`
	// Patches maps every commented file to the PatchDigest of the patch its comments
	// were anchored to. The web tier re-reads those patches before it creates the
	// review and refuses if one changed: a related change merged into the base can
	// move the merge base, and with it the lines, without moving the head or the base
	// branch.
	Patches map[string]string `json:"patches,omitempty"`
}

// PatchDigest identifies one file's patch as GitHub lists it on the pull request's
// files: hex SHA-256 of the patch text (ghcomment.PatchDigest computes the same).
func PatchDigest(patch string) string {
	sum := sha256.Sum256([]byte(patch))
	return hex.EncodeToString(sum[:])
}

// DraftComment is one inline comment: a single line of one file's diff.
type DraftComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

// DraftReceipt records the owner's confirmation of a draft (Message.DraftReceipt),
// written by the web tier only. It is claimed (DraftCreating) in the store before
// gofer asks GitHub for anything, so a replayed confirmation finds it and writes
// nothing, across a restart too; then settled to what happened, or removed when
// GitHub certainly created nothing, so the owner can try again.
type DraftReceipt struct {
	State string    `json:"state"` // DraftCreating | DraftCreated | DraftUnknown
	URL   string    `json:"url,omitempty"`
	At    time.Time `json:"at"`
}

// DraftReceipt states.
const (
	DraftCreating = "creating" // claimed; the request to GitHub may be under way, or was cut off
	DraftCreated  = "created"
	DraftUnknown  = "unknown" // the request reached GitHub, but its answer was lost
)

// Empty reports whether the draft has nothing to post.
func (d *DraftReview) Empty() bool {
	return d == nil || (strings.TrimSpace(d.Body) == "" && len(d.Comments) == 0)
}

// Digest identifies the draft: its content and the pull request, commit and base it
// is pinned to. The thread page posts it back with the owner's selection, and the
// web tier refuses a confirmation whose stored draft no longer matches.
func (d *DraftReview) Digest() string {
	if d == nil {
		return ""
	}
	b, _ := json.Marshal(d)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// For reports whether the draft is pinned to the item's pull request as it stands now.
func (d *DraftReview) For(it WorkItem) bool {
	return d != nil && it.Type == TypePullRequest && strings.EqualFold(d.Repo, it.GitHub.Repo) && d.Number == it.GitHub.Number
}

var draftRepo = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)

// BoundDraft returns d normalized to what may be stored: nil when its pin (repo,
// number, full commit id, base branch) is invalid or nothing is left to post;
// comments with an empty body, an invalid path, a line below 1, an unknown side, or
// no valid patch digest for their file dropped; text cut to its bound; at most
// MaxDraftComments comments; patch digests kept for commented files only. A side
// left empty is RIGHT.
func BoundDraft(d *DraftReview) *DraftReview {
	if d == nil || !validCommit(d.Commit) || !draftRepo.MatchString(d.Repo) || d.Number <= 0 ||
		d.BaseRef == "" || len(d.BaseRef) > maxDraftRefBytes || !utf8.ValidString(d.BaseRef) {
		return nil
	}
	out := &DraftReview{
		Repo: d.Repo, Number: d.Number, Commit: strings.ToLower(d.Commit), BaseRef: d.BaseRef,
		Body: cutText(strings.TrimSpace(d.Body), MaxDraftBodyBytes),
	}
	for _, c := range d.Comments {
		if len(out.Comments) == MaxDraftComments {
			break
		}
		side := strings.ToUpper(strings.TrimSpace(c.Side))
		if side == "" {
			side = SideRight
		}
		body := strings.TrimSpace(c.Body)
		digest := d.Patches[c.Path]
		if body == "" || c.Line < 1 || (side != SideRight && side != SideLeft) || !ValidDraftPath(c.Path) || len(digest) != 64 || !isHex(digest) {
			continue
		}
		out.Comments = append(out.Comments, DraftComment{Path: c.Path, Line: c.Line, Side: side, Body: cutText(body, MaxDraftCommentBytes)})
		if out.Patches == nil {
			out.Patches = map[string]string{}
		}
		out.Patches[c.Path] = strings.ToLower(digest)
	}
	if out.Empty() {
		return nil
	}
	return out
}

// ValidDraftPath reports whether p can name a file in a diff: a relative path
// within MaxDraftPathBytes, valid UTF-8, with no control characters.
func ValidDraftPath(p string) bool {
	if p == "" || len(p) > MaxDraftPathBytes || !utf8.ValidString(p) || strings.HasPrefix(p, "/") {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// validCommit reports whether s is a full commit id (SHA-1 or SHA-256, hex).
func validCommit(s string) bool {
	return (len(s) == 40 || len(s) == 64) && isHex(s)
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// cutText cuts s to at most max bytes without splitting a rune.
func cutText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const mark = "…"
	s = s[:max-len(mark)]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + mark
}

// IsDraftReviewRequest reports whether turn i is a draft-review request.
func (w WorkItem) IsDraftReviewRequest(i int) bool {
	return i >= 0 && i < len(w.Thread) && w.Thread[i].Role == RoleUser && w.Thread[i].Kind == KindDraftReviewRequest
}

// LatestDraft returns the position of the reply carrying the draft the thread
// offers, or -1. Only the newest draft request counts: its reply, if it has one, is
// not hidden, and carries a draft. So a redraft that found nothing to draft retires
// the draft before it, and a late reply to an older request never supersedes a newer
// result. A hidden request or reply offers nothing.
func (w WorkItem) LatestDraft() int {
	for i := len(w.Thread) - 1; i >= 0; i-- {
		if !w.IsDraftReviewRequest(i) {
			continue
		}
		if w.Thread[i].Hidden {
			return -1
		}
		j, ok := w.replies()[w.TurnKey(i)]
		if !ok || w.Thread[j].Hidden || w.Thread[j].Draft == nil {
			return -1
		}
		return j
	}
	return -1
}

// OfferedDraft returns the position of the draft turn key names if it is the draft
// the thread offers now (LatestDraft), its digest is digest, and it is pinned to the
// item's pull request as it stands (DraftReview.For). A receipt on it does not
// matter here: the caller decides what a confirmed draft means.
func (w WorkItem) OfferedDraft(key, digest string) (int, bool) {
	i := w.LatestDraft()
	if i < 0 || w.TurnKey(i) != key || w.Thread[i].Draft.Digest() != digest || !w.Thread[i].Draft.For(w) {
		return 0, false
	}
	return i, true
}

// ErrDraftNotOffered is a confirmation of a draft the thread no longer offers: the
// item is gone or off the radar, the draft was replaced, hidden or cleared, or the
// item's pull request is no longer the one the draft is pinned to.
var ErrDraftNotOffered = errors.New("the draft is no longer offered")

// ClaimDraft claims ownerID's draft (item itemID, turn key, digest) for creation at
// at, in one Store.Update: it records a DraftCreating receipt unless the draft
// already has a receipt, which it returns instead (prior), with nothing written. It
// returns the item as stored (with the claim when claimed) so the caller creates the
// review from the stored draft. ErrDraftNotOffered when the draft is not offered or
// the item is off the radar.
func ClaimDraft(ctx context.Context, store Store, ownerID, itemID, key, digest string, at time.Time) (item WorkItem, prior *DraftReceipt, err error) {
	err = store.Update(ctx, ownerID, func(current []WorkItem) ([]WorkItem, error) {
		item, prior = WorkItem{}, nil
		for n := range current {
			if current[n].ID != itemID {
				continue
			}
			if !current[n].OnRadar() {
				return nil, ErrDraftNotOffered
			}
			i, ok := current[n].OfferedDraft(key, digest)
			if !ok {
				return nil, ErrDraftNotOffered
			}
			if r := current[n].Thread[i].DraftReceipt; r != nil {
				cp := *r
				prior, item = &cp, current[n]
				return nil, nil
			}
			current[n].Thread[i].DraftReceipt = &DraftReceipt{State: DraftCreating, At: at}
			item = current[n]
			return current[n : n+1], nil
		}
		return nil, ErrDraftNotOffered
	})
	return item, prior, err
}

// SettleDraft records what happened to a claim ClaimDraft made at claimedAt:
// receipt (DraftCreated or DraftUnknown), or nil to release the claim when GitHub
// certainly created nothing. It changes nothing unless the turn still carries that
// very claim, so it never overwrites another confirmation's receipt.
func SettleDraft(ctx context.Context, store Store, ownerID, itemID, key string, claimedAt time.Time, receipt *DraftReceipt) error {
	return store.Update(ctx, ownerID, func(current []WorkItem) ([]WorkItem, error) {
		for n := range current {
			if current[n].ID != itemID {
				continue
			}
			i, ok := current[n].TurnIndex(key)
			if !ok {
				return nil, nil
			}
			r := current[n].Thread[i].DraftReceipt
			if r == nil || r.State != DraftCreating || !r.At.Equal(claimedAt) {
				return nil, nil
			}
			current[n].Thread[i].DraftReceipt = receipt
			return current[n : n+1], nil
		}
		return nil, nil
	})
}

// RedactDrafts returns items with every turn's draft and receipt removed, for a
// run's read: a run reads a draft's summary (its Content), never the structured
// draft. The items' threads are copied, never changed in place.
func RedactDrafts(items []WorkItem) []WorkItem {
	out := make([]WorkItem, len(items))
	for n, it := range items {
		if hasDraftData(it.Thread) {
			th := make([]Message, len(it.Thread))
			copy(th, it.Thread)
			for i := range th {
				th[i].Draft, th[i].DraftReceipt = nil, nil
			}
			it.Thread = th
		}
		out[n] = it
	}
	return out
}

func hasDraftData(th []Message) bool {
	for _, m := range th {
		if m.Draft != nil || m.DraftReceipt != nil {
			return true
		}
	}
	return false
}

// DraftInput is what a ReviewDrafter reads: the pull request, the review
// conversation (the visible turns before the request), and the pull request's diff,
// rendered with the line numbers a comment may anchor to. Diff is UNTRUSTED,
// attacker-influenceable data; so are the parts of the conversation that quote the
// pull request.
type DraftInput struct {
	Item        WorkItem
	ViewerLogin string
	History     []Message
	Diff        string
}

// DraftProposal is a ReviewDrafter's answer, before the run checks it against the
// diff: inline comments, agreed points that belong to no diff line (General), and
// the points it left out because the conversation does not agree on them (Excluded),
// one short line each, which the reply shows so the owner sees what was held back.
type DraftProposal struct {
	Comments []DraftComment
	General  []string
	Excluded []string
}

// ReviewDrafter drafts the review comments a conversation agrees on. It is
// read-only: the run stores its answer as a draft, and only the owner's confirmation
// posts anything, as a pending review (ADR 0010).
type ReviewDrafter interface {
	DraftReview(ctx context.Context, in DraftInput) (DraftProposal, error)
}
