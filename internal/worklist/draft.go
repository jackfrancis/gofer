package worklist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
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

// ApprovalCommands is the review text an approving draft adds (DraftReview.Approve):
// Prow's approval commands, each on its own line, as Prow's lgtm and approve plugins
// read them from a submitted review's body.
const ApprovalCommands = "/lgtm\n/approve"

// The parts of a draft reply's text that describe its approval. The draft run
// (internal/agent) writes them, and the store removes them when it withholds the
// approval (AppendReply), so a reply never claims an approval its stored draft does
// not carry.
const (
	// ApprovalLead starts the line that opens a reply whose draft is an approval and
	// nothing else.
	ApprovalLead = "**Drafted an approval**"
	// ApprovalAlso is the sentence a reply with comments adds for an approval.
	ApprovalAlso = " The conversation also agrees the pull request is ready to approve."
	// ApprovalBlock shows the approval's commands.
	ApprovalBlock = "\n**Approval** (a general comment, on lines of its own)\n\n```\n" + ApprovalCommands + "\n```\n"
)

// Why an approval the conversation agreed on was withheld, as a draft reply's text
// says it. The run writes NoApprovalUnread too; the store writes them all when it
// withholds an approval the run sent (AppendReply).
const (
	NoApprovalUnread  = "\n**No approval drafted.** The conversation agrees on approving, but it is longer than gofer reads at once, so some of it was not read, and a message there (a hold you placed, say) could disagree. Hide the turns you no longer need, then draft again.\n"
	noApprovalToggled = "\n**No approval drafted.** The conversation agrees on approving, but a turn before the request was hidden or unhidden after this draft was asked for, so gofer cannot confirm the conversation the draft read. Draft again.\n"
	noApprovalStale   = "\n**No approval drafted.** The conversation agrees on approving, but its latest review did not read the revision this draft is anchored to, so no approval can be drafted from it. Review the current revision, then draft again.\n"
)

// withholdApproval drops the approval from reply's draft (the whole draft, when that
// was all of it) and rewrites the text to match: the approval's parts go, and why
// is added, so the thread never describes an approval the stored draft does not
// carry. The comments and everything else the text says stand.
func withholdApproval(reply *Message, why string) {
	if d := reply.Draft; d != nil {
		d.Approve, d.Diff = false, "" // as BoundDraft stores a draft that does not approve, so it stays Current
		if d.Empty() {
			reply.Draft = nil
		}
	}
	c := reply.Content
	if strings.HasPrefix(c, ApprovalLead) {
		_, c, _ = strings.Cut(c, "\n") // the approval was the whole draft: its line goes
	}
	c = strings.Replace(c, ApprovalAlso, "", 1)
	c = strings.Replace(c, ApprovalBlock, "", 1)
	reply.Content = strings.TrimLeft(strings.TrimLeft(c, "\n")+why, "\n")
}

// DraftReview is the structured result of a draft-review run: inline comments
// anchored to lines of one pull request's diff, a summary Body for the agreed points
// no diff line carries, and Approve when the conversation agrees the pull request is
// ready to approve. It rides on the agent reply (Message.Draft).
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
	// Approve marks a draft whose conversation agrees the pull request is ready to
	// approve: the review then carries ApprovalCommands as a general comment (in its
	// body, after the summary; ReviewBody), which the owner may leave out like any
	// other part. It is kept apart from Body so the summary's bound never cuts it.
	Approve bool `json:"approve,omitempty"`
	// Diff is, on an approving draft, the digest of the whole diff it approves
	// (github.DiffDigest), the one the latest review read: the web tier creates the
	// review only while the pull request still shows exactly that diff. Empty on a
	// draft that does not approve; an approving draft without a valid one approves
	// nothing (BoundDraft).
	Diff string `json:"diff,omitempty"`
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
	State string `json:"state"` // DraftCreating | DraftCreated | DraftUnknown
	URL   string `json:"url,omitempty"`
	// Body is the review body sent (ReviewBody of what the owner kept), recorded in
	// the claim, before gofer contacts GitHub, so a confirmation interrupted after
	// GitHub created the review still shows it. GitHub's dialog for submitting a
	// pending review can open with an empty summary that replaces it, so the thread
	// shows it to copy into that dialog.
	Body string    `json:"body,omitempty"`
	At   time.Time `json:"at"`
}

// DraftReceipt states.
const (
	DraftCreating = "creating" // claimed; the request to GitHub may be under way, or was cut off
	DraftCreated  = "created"
	DraftUnknown  = "unknown" // the request reached GitHub, but its answer was lost
)

// Empty reports whether the draft has nothing to post.
func (d *DraftReview) Empty() bool {
	return d == nil || (strings.TrimSpace(d.Body) == "" && len(d.Comments) == 0 && !d.Approve)
}

// ReviewBody is the body of the review created from a draft: the summary, if kept,
// then ApprovalCommands, if kept, on lines of their own.
func ReviewBody(summary string, approve bool) string {
	switch {
	case !approve:
		return summary
	case summary == "":
		return ApprovalCommands
	}
	return summary + "\n\n" + ApprovalCommands
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
// no valid patch digest for their file dropped; command lines in the text
// neutralized (NeutralizeCommands); text cut to its bound; at most MaxDraftComments
// comments; patch digests kept for commented files only. A side left empty is RIGHT.
func BoundDraft(d *DraftReview) *DraftReview {
	if d == nil || !validCommit(d.Commit) || !draftRepo.MatchString(d.Repo) || d.Number <= 0 || !validRef(d.BaseRef) {
		return nil
	}
	out := &DraftReview{
		Repo: d.Repo, Number: d.Number, Commit: strings.ToLower(d.Commit), BaseRef: d.BaseRef,
		Body:    cutText(NeutralizeCommands(strings.TrimSpace(d.Body)), MaxDraftBodyBytes),
		Approve: d.Approve && validDigest(d.Diff),
	}
	if out.Approve {
		out.Diff = strings.ToLower(d.Diff)
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
		if body == "" || c.Line < 1 || (side != SideRight && side != SideLeft) || !ValidDraftPath(c.Path) || !validDigest(digest) {
			continue
		}
		out.Comments = append(out.Comments, DraftComment{Path: c.Path, Line: c.Line, Side: side, Body: cutText(NeutralizeCommands(body), MaxDraftCommentBytes)})
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

// validDigest reports whether s is a hex SHA-256.
func validDigest(s string) bool {
	return len(s) == 64 && isHex(s)
}

// validRef reports whether s can be a stored branch name: non-empty, bounded, UTF-8.
func validRef(s string) bool {
	return s != "" && len(s) <= maxDraftRefBytes && utf8.ValidString(s)
}

// Current reports whether d is what BoundDraft would store today. A draft stored
// under older rules (before command lines were neutralized, say) is not: the thread
// does not offer it and a confirmation of it is refused (OfferedDraft), so it can
// never send text today's rules would change. Drafting again replaces it.
func (d *DraftReview) Current() bool {
	b := BoundDraft(d)
	return b != nil && b.Digest() == d.Digest()
}

// commandLine matches a line a bot such as Prow reads as a command: a slash and a
// command word at the start of the line (after any indentation), then the end of
// the line or a space. A path such as /usr/bin/x is not one: its word runs into
// another slash.
var commandLine = regexp.MustCompile(`^([ \t]*)(/[A-Za-z][A-Za-z0-9_-]*)([ \t\r].*)?$`)

// SuggestionNote follows a suggestion block NeutralizeCommands turned into a plain
// code block.
const SuggestionNote = "_(gofer did not offer this as a suggested change: a line in it would act as a bot command on GitHub, so it is shown as plain code.)_"

// NeutralizeCommands returns model-written review text with its line endings
// normalized to LF and every command line (commandLine: /lgtm, /approve, /hold, ...)
// made inert by wrapping its command in backticks, so it reads the same but no
// longer starts with a slash. A drafted review acts on a pull request only through
// what the owner selects: its approval commands come from DraftReview.Approve alone,
// never from the text.
//
// Line endings come first: a bot splits lines at LF, and a browser turns a bare CR
// into LF before the page's copy button reads the text, so a CR could otherwise hide
// a command from this check and reveal it in the pasted text. The text stored, shown
// and copied is this one.
//
// A GitHub suggestion block is applied verbatim, so it is never altered and left a
// suggestion: one holding a command line (a .gitignore's /dist, say) becomes a plain
// code block, with SuggestionNote after it, so the author cannot apply the altered
// text. Blocks are found as Markdown parses them (goldmark), so a suggestion nested
// in a list, with its fence indented past three spaces, is one too. Lines outside
// any suggestion are judged as a bot reads them, raw. The result is a fixed point.
func NeutralizeCommands(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(s, "\n")
	hit := make([]bool, len(lines))
	found := false
	for i, l := range lines {
		hit[i] = commandLine.MatchString(l)
		found = found || hit[i]
	}
	if !found {
		return s
	}
	starts := make([]int, len(lines)) // each line's byte offset in s
	for i, off := 1, 0; i < len(lines); i++ {
		off += len(lines[i-1]) + 1
		starts[i] = off
	}
	lineOf := func(off int) int { return sort.Search(len(starts), func(i int) bool { return starts[i] > off }) - 1 }

	// Suggestion blocks holding a command line: the language to replace on the
	// opening line, and where the note goes.
	type demotion struct{ col, n int }
	opener := map[int]demotion{}  // opening line -> the language's column and length
	noteAfter := map[int]string{} // line -> the note, indented to the block's fence
	src := []byte(s)
	doc := goldmark.New().Parser().Parse(text.NewReader(src))
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		fc, ok := n.(*ast.FencedCodeBlock)
		if !entering || !ok || fc.Info == nil || fc.Lines().Len() == 0 {
			return ast.WalkContinue, nil
		}
		lang := fc.Language(src)
		if !strings.EqualFold(string(lang), "suggestion") {
			return ast.WalkContinue, nil
		}
		first, last := lineOf(fc.Lines().At(0).Start), lineOf(fc.Lines().At(fc.Lines().Len()-1).Start)
		if !slices.Contains(hit[first:last+1], true) {
			return ast.WalkContinue, nil
		}
		info := fc.Info.Segment.Start
		open := lineOf(info)
		opener[open] = demotion{col: info - starts[open], n: len(lang)}
		fence := strings.IndexAny(lines[open], "`~")
		indent := strings.Map(func(r rune) rune {
			if r == '\t' {
				return r
			}
			return ' '
		}, lines[open][:max(fence, 0)])
		end := len(lines) - 1 // the closing fence, or the last line of a block left open
		for k := last + 1; k < len(lines); k++ {
			if t := strings.TrimSpace(lines[k]); len(t) >= 3 && (strings.Trim(t, "`") == "" || strings.Trim(t, "~") == "") {
				end = k
				break
			}
		}
		noteAfter[end] = indent + SuggestionNote
		return ast.WalkContinue, nil
	})

	out := make([]string, 0, len(lines)+len(noteAfter))
	for i, l := range lines {
		switch d, ok := opener[i]; {
		case ok:
			l = l[:d.col] + "text" + l[d.col+d.n:]
		case hit[i]:
			l = commandLine.ReplaceAllString(l, "$1`$2`$3")
		}
		out = append(out, l)
		if note, ok := noteAfter[i]; ok {
			out = append(out, note)
		}
	}
	return strings.Join(out, "\n")
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
	_, j := w.latestDraft()
	return j
}

// latestDraft returns the positions of the newest draft request and of the reply
// carrying the draft the thread offers (LatestDraft), or -1, -1.
func (w WorkItem) latestDraft() (req, reply int) {
	for i := len(w.Thread) - 1; i >= 0; i-- {
		if !w.IsDraftReviewRequest(i) {
			continue
		}
		if w.Thread[i].Hidden {
			return -1, -1
		}
		j, ok := w.replies()[w.TurnKey(i)]
		if !ok || w.Thread[j].Hidden || w.Thread[j].Draft == nil {
			return -1, -1
		}
		return i, j
	}
	return -1, -1
}

// RedraftRedundant reports whether drafting again would only repeat the draft the
// thread offers (LatestDraft), because the conversation the drafter reads is the one
// it read: the draft is unconfirmed and still pinned to the item's pull request
// (DraftReview.For); no turn after its request but the draft itself is visible (the
// drafter read only the turns before its request, so a correction posted while it
// worked, even one answered before the draft landed, is new to it); and no turn
// before the request was hidden or unhidden since the request was saved (a HiddenRev
// above the request's ReadRev), so hiding or unhiding an earlier instruction opens
// drafting again. Revisions, not times, order the two: each is assigned inside the
// store update that saved it, and a retry of the request keeps its ReadRev, so a
// slow first run's draft is still judged by what it could have read. A request
// saved before revisions were recorded (no ReadRevKnown) cannot be judged so, and
// its draft never counts as redundant: the thread keeps the button, as it did then.
// A toggle after
// the request but before its run read the thread counts too, which errs toward
// offering the button. A confirmed draft (any receipt) is used up: the thread says to
// draft again to start over. A retired draft (an empty redraft, a hidden request or
// reply, another pull request) leaves drafting open.
//
// The head the draft is anchored to is not compared with the item's: gofer has no
// observation of the head it can order against the drafter's own read. A draft that
// GitHub finds out of date is refused when confirmed (comment.ReviewResult.Stale),
// and that result offers drafting again.
func (w WorkItem) RedraftRedundant() bool {
	req, i := w.latestDraft()
	if i < 0 {
		return false
	}
	if !w.Thread[i].Draft.For(w) || !w.Thread[i].Draft.Current() || w.Thread[i].DraftReceipt != nil {
		return false
	}
	if w.toggledSince(req) {
		return false
	}
	for j := req + 1; j < len(w.Thread); j++ {
		if j != i && !w.Thread[j].Hidden {
			return false
		}
	}
	return true
}

// toggledSince reports whether a turn before the draft-review request at req was
// hidden or unhidden since the request was saved (a HiddenRev above its ReadRev), or
// whether that cannot be told (a request saved before revisions, no ReadRevKnown):
// either way, the conversation its run read may not be the one the thread shows now.
func (w WorkItem) toggledSince(req int) bool {
	if !w.Thread[req].ReadRevKnown {
		return true
	}
	for j := range req {
		if w.Thread[j].HiddenRev > w.Thread[req].ReadRev {
			return true
		}
	}
	return false
}

// VisibilityRev is the thread's visibility revision: the highest HiddenRev in it, or
// 0. Turns are only appended (clearing removes them all), so it only grows while the
// thread stands. Read it inside the store update whose result depends on it.
func (w WorkItem) VisibilityRev() int {
	rev := 0
	for _, m := range w.Thread {
		rev = max(rev, m.HiddenRev)
	}
	return rev
}

// SetHidden hides (or unhides) turn i, reporting whether its visibility changed. A
// change takes the thread's next visibility revision (HiddenRev), so a draft whose
// request was saved before it no longer counts as reading the conversation as it
// stands (RedraftRedundant). Call it inside the store update that saves the change,
// so the revision follows the order of the saved writes.
func (w *WorkItem) SetHidden(i int, hidden bool) bool {
	if i < 0 || i >= len(w.Thread) || w.Thread[i].Hidden == hidden {
		return false
	}
	rev := w.VisibilityRev() + 1
	w.Thread[i].Hidden = hidden
	w.Thread[i].HiddenRev = rev
	return true
}

// OfferedDraft returns the position of the draft turn key names if it is the draft
// the thread offers now (LatestDraft), its digest is digest, and it is pinned to the
// item's pull request as it stands (DraftReview.For), and it is what today's rules
// would store (DraftReview.Current). A receipt on it does not matter here: the
// caller decides what a confirmed draft means.
func (w WorkItem) OfferedDraft(key, digest string) (int, bool) {
	i := w.LatestDraft()
	if i < 0 || w.TurnKey(i) != key || w.Thread[i].Draft.Digest() != digest || !w.Thread[i].Draft.For(w) || !w.Thread[i].Draft.Current() {
		return 0, false
	}
	return i, true
}

// ErrDraftNotOffered is a confirmation of a draft the thread no longer offers: the
// item is gone or off the radar, the draft was replaced, hidden or cleared, or the
// item's pull request is no longer the one the draft is pinned to.
var ErrDraftNotOffered = errors.New("the draft is no longer offered")

// ClaimDraft claims ownerID's draft (item itemID, turn key, digest) for creation at
// at, in one Store.Update: it records a DraftCreating receipt carrying body (the
// review body about to be sent, so an interrupted confirmation can still show it)
// unless the draft already has a receipt, which it returns instead (prior), with
// nothing written. It
// returns the item as stored (with the claim when claimed) so the caller creates the
// review from the stored draft. ErrDraftNotOffered when the draft is not offered or
// the item is off the radar.
func ClaimDraft(ctx context.Context, store Store, ownerID, itemID, key, digest, body string, at time.Time) (item WorkItem, prior *DraftReceipt, err error) {
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
			current[n].Thread[i].DraftReceipt = &DraftReceipt{State: DraftCreating, Body: body, At: at}
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
	// Partial is true when History is the newest part of a conversation too long to
	// read whole: earlier turns were left out.
	Partial bool
	Diff    string
	// Head and Base are the pull request's head commit and base branch the diff
	// shows, which the draft is anchored to; a history turn's Head and Base are the
	// revision it read.
	Head, Base string
}

// DraftProposal is a ReviewDrafter's answer, before the run checks it against the
// diff: inline comments, agreed points that belong to no diff line (General), the
// points it left out because the conversation does not agree on them (Excluded),
// one short line each, which the reply shows so the owner sees what was held back,
// and Approve when the conversation agrees the pull request is ready to approve.
type DraftProposal struct {
	Comments []DraftComment
	General  []string
	Excluded []string
	Approve  bool
}

// ReviewDrafter drafts the review comments a conversation agrees on. It is
// read-only: the run stores its answer as a draft, and only the owner's confirmation
// posts anything, as a pending review (ADR 0010).
type ReviewDrafter interface {
	DraftReview(ctx context.Context, in DraftInput) (DraftProposal, error)
}
