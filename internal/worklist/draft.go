package worklist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
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
)

// Diff sides a draft comment anchors to, as GitHub names them: RIGHT is the pull
// request's version of the file (an added or unchanged line), LEFT the base's (a
// deleted line).
const (
	SideRight = "RIGHT"
	SideLeft  = "LEFT"
)

// DraftReview is the structured result of a draft-review run: inline comments
// anchored to lines of the pull request's diff at Commit, and a summary Body for the
// agreed points no diff line carries. It rides on the agent reply (Message.Draft).
type DraftReview struct {
	// Commit is the head commit the comments are anchored to. The web tier creates
	// the pending review only while the pull request's head is still this commit,
	// since a push moves the lines.
	Commit   string         `json:"commit"`
	Body     string         `json:"body,omitempty"`
	Comments []DraftComment `json:"comments,omitempty"`
}

// DraftComment is one inline comment: a single line of one file's diff.
type DraftComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

// Empty reports whether the draft has nothing to post.
func (d *DraftReview) Empty() bool {
	return d == nil || (strings.TrimSpace(d.Body) == "" && len(d.Comments) == 0)
}

// Digest identifies the draft's content. The thread page posts it back with the
// owner's selection, and the web tier refuses a confirmation whose stored draft no
// longer matches what the page showed.
func (d *DraftReview) Digest() string {
	if d == nil {
		return ""
	}
	b, _ := json.Marshal(d)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// BoundDraft returns d normalized to what may be stored: nil when it has no valid
// commit or nothing left to post; comments with an empty body, an invalid path, a
// line below 1 or an unknown side dropped; text cut to its bound; at most
// MaxDraftComments comments. A side left empty is RIGHT.
func BoundDraft(d *DraftReview) *DraftReview {
	if d == nil || !validCommit(d.Commit) {
		return nil
	}
	out := &DraftReview{Commit: strings.ToLower(d.Commit), Body: cutText(strings.TrimSpace(d.Body), MaxDraftBodyBytes)}
	for _, c := range d.Comments {
		if len(out.Comments) == MaxDraftComments {
			break
		}
		side := strings.ToUpper(strings.TrimSpace(c.Side))
		if side == "" {
			side = SideRight
		}
		body := strings.TrimSpace(c.Body)
		if body == "" || c.Line < 1 || (side != SideRight && side != SideLeft) || !ValidDraftPath(c.Path) {
			continue
		}
		out.Comments = append(out.Comments, DraftComment{Path: c.Path, Line: c.Line, Side: side, Body: cutText(body, MaxDraftCommentBytes)})
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
	if len(s) != 40 && len(s) != 64 {
		return false
	}
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

// LatestDraft returns the position of the newest agent turn carrying a draft review
// that is not hidden, or -1. The thread offers to create a pending review from that
// draft only: an older one was superseded.
func (w WorkItem) LatestDraft() int {
	for i := len(w.Thread) - 1; i >= 0; i-- {
		m := w.Thread[i]
		if m.Role == RoleAgent && m.Draft != nil {
			if m.Hidden {
				return -1
			}
			return i
		}
	}
	return -1
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
