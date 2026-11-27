package worklist

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Conversation roles. A thread alternates user and agent turns.
const (
	RoleUser  = "user"
	RoleAgent = "agent"
)

// Message.Kind marks a request turn's purpose so gofer can reason about an item's
// review state deterministically — counting completed reviews without re-reading the
// model's prose. Ordinary Discuss turns and agent replies carry no kind.
const (
	KindReviewRequest    = "review_request"    // "review this PR" (Review-all, or an independent 2nd-opinion review)
	KindSynthesisRequest = "synthesis_request" // the consensus turn that weighs the reviews
	// KindDraftReviewRequest asks for the review comments the conversation agrees
	// on, drafted for a pending GitHub review (draft.go, ADR 0010).
	KindDraftReviewRequest = "draft_review_request"
)

// Message.Notice values: an agent turn that is a system notice rather than an
// answer. A notice does not answer the turn it replies to (review and pending state
// ignore it), and the thread shows it as a notice.
const (
	// NoticeReconnect: gofer could not reach GitHub for the user, who must sign in
	// again. The UI shows a sign-in prompt with it.
	NoticeReconnect = "reconnect"
	// NoticePanelInputs: the consensus synthesis did not run, because a review it
	// weighs (Message.Reviews) is hidden, gone, or did not fit the run's read. The
	// thread offers to retry the synthesis once the reviews are back (RetryableSynthesis).
	NoticePanelInputs = "panel_inputs"
)

// knownNotice reports whether n is a notice gofer knows, which AppendReply keeps.
func knownNotice(n string) bool { return n == NoticeReconnect || n == NoticePanelInputs }

// ReconnectNotice is the text of a NoticeReconnect turn.
const ReconnectNotice = "gofer can't reach GitHub for you: sign in again to reconnect, then ask again."

// PanelInputsNotice is the text of a NoticePanelInputs turn.
const PanelInputsNotice = "gofer didn't run the consensus: a review it compares is hidden or gone, or the reviews are too large to read together. Unhide the review and retry, or hide some reviews and get another 2nd opinion for a smaller comparison."

// ErrSignInRequired is the error a ToolBox returns (wrapped) when the user's
// provider credential is gone and only signing in again helps. A Conversationalist
// must not feed it to the model as a tool result: it ends the turn and returns it,
// and the run posts a NoticeReconnect turn instead of a reply.
var ErrSignInRequired = errors.New("sign-in required: gofer can't reach GitHub for this user")

// Message.Verdict values: the consensus outcome stamped on a synthesis agent reply.
const (
	VerdictAgree    = "agree"
	VerdictDisagree = "disagree"
)

// Message is one turn in an item's assistive conversation thread, retained on
// the WorkItem. Role is RoleUser or RoleAgent.
//
// Replies can land out of order: the owner may post a follow-up while a run is
// still answering the previous turn, and every reply is appended at the end. So
// position does not say which question an answer belongs to; ReplyTo does. Turns
// stored before turns had IDs have neither field, and are read by their old
// convention (see TurnKey and replies).
type Message struct {
	// ID identifies the turn for its lifetime (NewMessageID). Empty on turns stored
	// before turns had IDs; their position stands in for it (WorkItem.TurnKey).
	ID string `json:"id,omitempty"`
	// ReplyTo is the key (WorkItem.TurnKey) of the user turn an agent turn answers. Empty on user turns
	// and on agent turns stored before replies recorded it, which answer the user
	// turn directly before them.
	ReplyTo string `json:"reply_to,omitempty"`
	Role    string `json:"role"`
	Content string `json:"content"`
	// Model names the chat model that produced an agent turn (empty for user turns
	// and for legacy replies), so the UI can attribute a reply — e.g. a second
	// opinion from a different model.
	Model string `json:"model,omitempty"`
	// Kind marks a request turn's purpose (KindReviewRequest / KindSynthesisRequest);
	// empty for ordinary Discuss turns and agent replies. It lets gofer count review
	// state deterministically instead of matching prompt text.
	Kind string `json:"kind,omitempty"`
	// Verdict is the consensus outcome (VerdictAgree / VerdictDisagree) carried on a
	// synthesis agent reply, parsed from the model's leading verdict token; empty on
	// every other message.
	Verdict string `json:"verdict,omitempty"`
	// Reviews lists the keys (TurnKey) of the review requests a synthesis request
	// weighs, recorded when it is chained (ChainSynthesis): the run compares exactly
	// those reviews, and declines (NoticePanelInputs) if any is no longer visible to it
	// (SynthesisReady). Empty on every other turn, and on synthesis requests stored
	// before it was recorded, except on a draft-review request in a run's read
	// (ThreadWindow), where it names the latest review before it (WorkItem.DraftBasis).
	Reviews []string `json:"reviews,omitempty"`
	// Attempt identifies the current attempt at answering a user turn: a retry that
	// starts a new run gives the turn a new one (Restamp; a retry that shares a run
	// already in flight puts the earlier one back, RevertAttempt). A system notice
	// carries the attempt its run was dispatched as (TurnRef.Attempt, a run
	// parameter), and ends only that attempt (noticed), so a run of an earlier attempt
	// that finishes late cannot stop the page watching the retry. Empty on a turn
	// never retried, and on every other agent turn.
	Attempt string `json:"attempt,omitempty"`
	// Hidden marks a turn the user has hidden from the thread view. A hidden turn is
	// also withheld from the model as future conversation context (see splitThread), so
	// a long review thread can be pared down to its definitive turns without deleting
	// the history. It is reversible — the user can unhide it.
	Hidden bool `json:"hidden,omitempty"`
	// HiddenRev is the thread's visibility revision (WorkItem.VisibilityRev) the
	// user's last hide or unhide of this turn made: one more than any in the thread at
	// the time, assigned inside the store update that saved it (WorkItem.SetHidden).
	// Zero for a turn never toggled, or toggled before gofer recorded it.
	HiddenRev int `json:"hidden_rev,omitempty"`
	// ReadRev is, on a draft-review request, the thread's visibility revision when the
	// request was saved, read inside that store update: a turn before it with a higher
	// HiddenRev was hidden or unhidden since, so the draft may have read it otherwise
	// (RedraftRedundant). A retry (Restamp) leaves it as it is. Zero on other turns.
	// Both revisions are the web tier's alone: AppendReply clears them on a reply.
	ReadRev int `json:"read_rev,omitempty"`
	// Omitted marks, on a turn in a run's read (ThreadWindow), that visible turns
	// between it and the read's turn before it (or the start of the thread) were left
	// out to fit the read: there the read is not the whole conversation
	// (WorkItem.OmitsTurns). It is never stored: AppendReply clears it on a reply.
	Omitted bool `json:"omitted,omitempty"`
	// BasisNamed marks, on a draft-review request in a run's read (ThreadWindow), that
	// the web tier serving the read names the latest review before it (Reviews, Head,
	// Base, Diff), or that there is none. A read without it comes from a web tier
	// older than that, which names nothing, so the run cannot tell a conversation
	// with no review from one whose review it was not told about (DraftBasis.Named).
	// It is never stored: AppendReply clears it on a reply.
	BasisNamed bool `json:"basis_named,omitempty"`
	// RedraftHelps marks a draft reply whose run withheld something drafting again
	// may draft: an approval it could not check because the web tier serving its
	// read was older than the run (DraftBasis.Named). The thread keeps offering to
	// draft again while it is the offered draft (RedraftRedundant).
	RedraftHelps bool `json:"redraft_helps,omitempty"`
	// ReadRevKnown marks a draft-review request whose ReadRev was recorded. A request
	// saved before gofer recorded revisions has none, and toggles of that time carry
	// no HiddenRev either, so its ReadRev of 0 says nothing: such a draft never counts
	// as redundant (RedraftRedundant). Cleared on a reply, as the revisions are.
	ReadRevKnown bool `json:"read_rev_known,omitempty"`
	// Refused and RefusedAttempt record, on a user turn, that the backend certainly
	// refused the dispatch of attempt RefusedAttempt (ingest.ErrNotAccepted), so no
	// run exists for it: while that is still the turn's attempt it can no longer be
	// answered (WorkItem.Ended). A retry starts a new attempt, which they do not
	// cover. The web tier's alone: AppendReply clears them on a reply.
	Refused        bool   `json:"refused,omitempty"`
	RefusedAttempt string `json:"refused_attempt,omitempty"`
	// Notice marks an agent turn that is a system notice (NoticeReconnect), not an
	// answer: it does not answer the turn it replies to. Empty on every other turn.
	Notice string `json:"notice,omitempty"`
	// Draft is the draft review an agent turn answering a KindDraftReviewRequest
	// carries (draft.go). AppendReply bounds it, and drops it from a reply to any
	// other turn. Runs never read it back (ThreadWindow leaves it out).
	Draft *DraftReview `json:"draft,omitempty"`
	// DraftReceipt records the owner's confirmation of Draft (draft.go): written by
	// the web tier only (ClaimDraft, SettleDraft), never by a run.
	DraftReceipt *DraftReceipt `json:"draft_receipt,omitempty"`
	// Head and Base are the pull request's revision a converse run read before it
	// wrote this agent turn: its head commit (a full commit id) and its base branch.
	// AppendReply keeps them only as a valid pair. An approving draft stands only on
	// the revision its latest review read (ReviewedRevision), since a push or a
	// retarget changes what would merge. Empty on user turns, on non-pull-request
	// items, and on replies stored before they were recorded, except on a
	// draft-review request in a run's read (ThreadWindow), which carries there the
	// revision of the latest review before it (WorkItem.DraftBasis).
	Head string `json:"head,omitempty"`
	Base string `json:"base,omitempty"`
	// Diff is the digest of the whole diff the run read with Head and Base
	// (github.DiffDigest): recorded by a review, and by a draft, so a stacked pull
	// request whose dependency merged into its base (a new diff, same head and base
	// name) no longer counts as reviewed. Empty when the run did not read the diff
	// whole.
	Diff string    `json:"diff,omitempty"`
	At   time.Time `json:"at"`
}

// ParseVerdict extracts the leading verdict token the consensus synthesis is asked to
// emit — AGREEMENT or DISAGREEMENT on its own first line — returning the verdict
// (VerdictAgree, VerdictDisagree, or "" when absent) and the reply with that token
// line removed so the displayed message reads naturally. Matching is case-insensitive
// and tolerates surrounding Markdown emphasis or punctuation.
func ParseVerdict(reply string) (verdict, cleaned string) {
	rest := strings.TrimLeft(reply, " \t\r\n")
	line, after := rest, ""
	if i := strings.IndexByte(rest, '\n'); i >= 0 {
		line, after = rest[:i], rest[i+1:]
	}
	switch strings.ToUpper(strings.Trim(line, " \t\r*_#>`.:-")) {
	case "AGREEMENT":
		return VerdictAgree, strings.TrimLeft(after, " \t\r\n")
	case "DISAGREEMENT":
		return VerdictDisagree, strings.TrimLeft(after, " \t\r\n")
	}
	return "", reply
}

// HasUnreadReply reports whether the thread holds an agent turn the owner has not
// seen: one after the read cursor (ThreadReadThrough). It drives the radar's
// "unread" cue and the unread-first sort. Turns the user posted since do not
// count, so a thread whose newer turns are all the user's is not unread.
//
// An item with no cursor falls back to the legacy time-based one (ThreadReadAt).
func (w WorkItem) HasUnreadReply() bool {
	last := w.latestAgentTurn()
	if last < 0 {
		return false
	}
	if w.ThreadReadThrough != "" {
		// The cursor names an agent turn (MarkRead), so it is at the latest one or
		// before it, or names a turn no longer in the thread: unread unless it is
		// the latest.
		return w.TurnKey(last) != w.ThreadReadThrough
	}
	return w.Thread[last].At.After(w.ThreadReadAt)
}

// LatestReplyKey returns the key (TurnKey) of the newest agent turn in the thread —
// the last one by position, which is the last to land — or "" when there is none.
// The thread page acknowledges it as read (MarkRead) when it renders.
func (w WorkItem) LatestReplyKey() string {
	if i := w.latestAgentTurn(); i >= 0 {
		return w.TurnKey(i)
	}
	return ""
}

// MarkRead advances the read cursor to the agent turn whose key is seen, reporting
// whether it moved. The cursor only moves forward: a key that is not an agent turn
// in the thread, or one at or before the cursor (another page already showed a
// newer reply), changes nothing. A cursor whose turn is gone (the thread was
// cleared) is replaced.
func (w *WorkItem) MarkRead(seen string) bool {
	p := w.turnPosition(seen)
	if p < 0 || w.Thread[p].Role != RoleAgent {
		return false
	}
	if w.ThreadReadThrough != "" && w.turnPosition(w.ThreadReadThrough) >= p {
		return false
	}
	w.ThreadReadThrough = seen
	return true
}

// latestAgentTurn is the position of the thread's last agent turn, or -1.
func (w WorkItem) latestAgentTurn() int {
	for i := len(w.Thread) - 1; i >= 0; i-- {
		if w.Thread[i].Role == RoleAgent {
			return i
		}
	}
	return -1
}

// turnPosition is the position of the turn whose key (TurnKey) is key, or -1.
func (w WorkItem) turnPosition(key string) int {
	for i := range w.Thread {
		if w.TurnKey(i) == key {
			return i
		}
	}
	return -1
}

// TurnRef names the thread turn a converse run answers: the item, and the key
// (WorkItem.TurnKey) of the user turn on it. Dispatch carries it, so a run answers the turn it was
// started for even after newer turns land.
type TurnRef struct {
	Item string
	Turn string
	// Attempt is the attempt a retry dispatches (Message.Attempt, from Restamp),
	// carried to the run as a parameter so its notices name it; empty for a turn's
	// first attempt.
	Attempt string
}

// NewMessageID returns a random id for a new thread turn.
func NewMessageID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic("worklist: failed to read random bytes: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// TurnKey identifies the turn at position i: its ID, or, for a turn stored before
// turns had IDs, its position. Positions are stable because turns are only ever
// appended (clearing a thread removes all of them), and unlike timestamps they
// cannot collide. Dispatch, replies and the hide action all name a turn by its key.
func (w WorkItem) TurnKey(i int) string {
	if id := w.Thread[i].ID; id != "" {
		return id
	}
	return "pos:" + strconv.Itoa(i)
}

// replies maps each answered user turn's key to the position of the first agent
// turn answering it. An agent turn answers the turn its ReplyTo names; one stored
// before replies recorded that answers the user turn directly before it. A system
// notice answers nothing (see noticed).
func (w WorkItem) replies() map[string]int {
	out := make(map[string]int)
	for i, m := range w.Thread {
		if m.Role != RoleAgent || m.Notice != "" {
			continue
		}
		key := m.ReplyTo
		if key == "" {
			if i == 0 || w.Thread[i-1].Role != RoleUser {
				continue
			}
			key = w.TurnKey(i - 1)
		}
		if _, seen := out[key]; !seen {
			out[key] = i
		}
	}
	return out
}

// noticed is the set of user turns whose current attempt a system notice ended: the
// run that took the turn finished, without answering it. A notice ends only the
// attempt it read (Message.Attempt): a retry gives the turn a new attempt (Restamp),
// so the retried turn is pending again until its own run ends, even if the earlier
// attempt's run posts its notice late.
func (w WorkItem) noticed() map[string]bool {
	attempt := make(map[string]string)
	for i, m := range w.Thread {
		if m.Role == RoleUser {
			attempt[w.TurnKey(i)] = m.Attempt
		}
	}
	out := make(map[string]bool)
	for _, m := range w.Thread {
		if m.Role != RoleAgent || m.Notice == "" || m.ReplyTo == "" {
			continue
		}
		if a, ok := attempt[m.ReplyTo]; ok && a == m.Attempt {
			out[m.ReplyTo] = true
		}
	}
	return out
}

// NeedsReconnect reports whether the item's latest agent turn is a reconnect
// notice: the thread view then offers a sign-in prompt.
func (w WorkItem) NeedsReconnect() bool {
	for i := len(w.Thread) - 1; i >= 0; i-- {
		if m := w.Thread[i]; m.Role == RoleAgent {
			return m.Notice == NoticeReconnect
		}
	}
	return false
}

// Answered reports whether the user turn with key has a reply.
func (w WorkItem) Answered(key string) bool {
	_, ok := w.replies()[key]
	return ok
}

// Ended reports whether the current attempt at the user turn with key can no longer
// be answered: a system notice ended it (noticed), or the backend certainly refused
// its dispatch (Refused, for that very attempt).
func (w WorkItem) Ended(key string) bool {
	if w.noticed()[key] {
		return true
	}
	i, ok := w.TurnIndex(key)
	return ok && w.refused(i)
}

// refused reports whether the backend certainly refused the current attempt at turn
// i (MarkRefused): no run exists for it, so it is neither pending nor waiting out
// staleAfter before a retry.
func (w WorkItem) refused(i int) bool {
	return w.Thread[i].Refused && w.Thread[i].RefusedAttempt == w.Thread[i].Attempt
}

// LatestReviewAt returns the position of the latest review (the visible answer to a
// visible review request that landed last), or -1. Review requests before it are
// superseded by it: a review cycle starts there.
func (w WorkItem) LatestReviewAt() int { return w.latestReview() }

// MarkRefused records that the backend certainly refused the dispatch of attempt at
// the user turn with key, so no run exists for it (Ended). It applies only while that
// attempt is the turn's current one and the turn has no answer, and reports whether
// it changed anything. The web tier calls it inside the store update that saves it.
func (w *WorkItem) MarkRefused(key, attempt string) bool {
	i, ok := w.TurnIndex(key)
	if !ok || w.Thread[i].Role != RoleUser || w.Thread[i].Attempt != attempt || w.Answered(key) ||
		(w.Thread[i].Refused && w.Thread[i].RefusedAttempt == attempt) {
		return false
	}
	w.Thread = slices.Clone(w.Thread)
	w.Thread[i].Refused, w.Thread[i].RefusedAttempt = true, attempt
	return true
}

// TurnIndex returns the position of the turn with key.
func (w WorkItem) TurnIndex(key string) (int, bool) {
	for i := range w.Thread {
		if w.TurnKey(i) == key {
			return i, true
		}
	}
	return 0, false
}

// LastUserTurn returns the position of the most recent user turn.
func (w WorkItem) LastUserTurn() (int, bool) {
	for i := len(w.Thread) - 1; i >= 0; i-- {
		if w.Thread[i].Role == RoleUser {
			return i, true
		}
	}
	return 0, false
}

// hasCompletedRequest reports whether some user request turn of the given kind is
// answered by an agent reply — the deterministic "this review/synthesis actually ran"
// signal, keyed off Message.Kind rather than prompt text.
func (w WorkItem) hasCompletedRequest(kind string) bool {
	answered := w.replies()
	for i, m := range w.Thread {
		if m.Role == RoleUser && m.Kind == kind {
			if _, ok := answered[w.TurnKey(i)]; ok {
				return true
			}
		}
	}
	return false
}

// HasReview reports whether the item has at least one completed review (a review
// request answered by the assistant).
func (w WorkItem) HasReview() bool { return w.hasCompletedRequest(KindReviewRequest) }

// HasDiscussion reports whether the conversation has something to draft review
// comments from: a visible user turn (other than a draft-review request) answered by
// a visible reply. A review is one, and so is a Discuss question answered with a
// review in all but name; the drafter judges what the conversation agrees on.
func (w WorkItem) HasDiscussion() bool {
	replies := w.replies()
	for i, m := range w.Thread {
		if m.Role != RoleUser || m.Hidden || w.IsDraftReviewRequest(i) {
			continue
		}
		if j, ok := replies[w.TurnKey(i)]; ok && !w.Thread[j].Hidden {
			return true
		}
	}
	return false
}

// Revision is the pull request revision a run read: its head commit, base branch,
// and the digest of the whole diff GitHub showed (github.DiffDigest; "" when the
// run did not read it whole).
type Revision struct {
	Head, Base, Diff string
}

// revision is m's recorded revision.
func (m Message) revision() Revision { return Revision{m.Head, m.Base, m.Diff} }

// latestReview returns the position of the visible answer to a visible review
// request that landed last in the thread, or -1.
func (w WorkItem) latestReview() int { return w.latestReviewBefore(len(w.Thread)) }

// latestReviewBefore returns the position of the latest review among the turns
// before end (a visible review request answered by a visible reply, both before
// end), or -1: the review a draft requested at end read, whatever landed after the
// request. One pass over the thread.
func (w WorkItem) latestReviewBefore(end int) int {
	replies := w.replies()
	latest := -1
	for i := 0; i < end && i < len(w.Thread); i++ {
		m := w.Thread[i]
		if m.Role != RoleUser || m.Kind != KindReviewRequest || m.Hidden {
			continue
		}
		if j, ok := replies[w.TurnKey(i)]; ok && j < end && !w.Thread[j].Hidden {
			latest = max(latest, j)
		}
	}
	return latest
}

// ReviewedRevision returns the pull request revision the latest review read
// (latestReview): the zero Revision when there is no such review, or when that
// answer recorded none. An approval drafted from the conversation approves that
// revision, and nothing else.
func (w WorkItem) ReviewedRevision() Revision { return w.revisionAt(w.latestReview()) }

// revisionAt is the revision recorded on the turn at j, or the zero Revision for -1.
func (w WorkItem) revisionAt(j int) Revision {
	if j >= 0 {
		return w.Thread[j].revision()
	}
	return Revision{}
}

// requestOf returns the position of the user turn the agent turn at j answers (the
// one its ReplyTo names, or for a legacy reply the turn directly before it), or -1.
func (w WorkItem) requestOf(j int) int {
	key := w.Thread[j].ReplyTo
	if key == "" {
		if j > 0 && w.Thread[j-1].Role == RoleUser {
			return j - 1
		}
		return -1
	}
	if i, ok := w.TurnIndex(key); ok {
		return i
	}
	return -1
}

// DraftBasis is what a draft-review request's read (ThreadWindow) says about the
// review an approval would rest on: the latest review before the request.
type DraftBasis struct {
	// Named is true when the read says which review that is, or that there is none
	// (Message.BasisNamed). Without it the rest says nothing: the web tier serving
	// the read is older than the run.
	Named bool
	// Review is true when the conversation has a visible review before the request,
	// whether or not the read holds it.
	Review bool
	// Revision is the revision that review recorded (ReviewedRevision).
	Revision Revision
	// Read is true when the read holds that review, request and answer, and every
	// visible turn since its request: nothing the owner said after asking for it,
	// even while it stalled, is missing, so what the read shows is the whole
	// agreement since.
	Read bool
}

// Covers reports whether the review read exactly rev (as Reviewed).
func (b DraftBasis) Covers(rev Revision) bool { return b.Review && reviewedAs(b.Revision, rev) }

// DraftBasis reads, from a read of the draft-review request at anchor, the review
// an approval would rest on: the request names it and carries its revision
// (ThreadWindow), since it may not fit the read.
func (w WorkItem) DraftBasis(anchor int) DraftBasis {
	if anchor < 0 || anchor >= len(w.Thread) {
		return DraftBasis{}
	}
	ask := w.Thread[anchor]
	if len(ask.Reviews) != 1 {
		return DraftBasis{Named: ask.BasisNamed}
	}
	b := DraftBasis{Named: ask.BasisNamed, Review: true, Revision: Revision{ask.Head, ask.Base, ask.Diff}}
	q, ok := w.TurnIndex(ask.Reviews[0])
	if !ok || q >= anchor {
		return b
	}
	j, answered := w.replies()[w.TurnKey(q)]
	b.Read = answered && j < anchor && !w.OmitsTurns(q, anchor)
	return b
}

// Unpaired reports whether the agent turn at j, in a run's read (ThreadWindow), is
// an answer whose question the read left out: the read omits visible turns at or
// before it (OmitsTurns), and the user turn it answers is not in the read. Read
// alone, such an answer is no evidence of what the conversation agreed, since the
// owner's word on it may have been left out with the question. An answer to a
// hidden question, in a read that omits nothing before it, is not unpaired: the
// owner chose to keep it.
func (w WorkItem) Unpaired(j int) bool {
	return j >= 0 && j < len(w.Thread) && w.Thread[j].Role == RoleAgent && w.requestOf(j) < 0 && w.OmitsTurns(-1, j)
}

// OmitsTurns reports whether a run's read (ThreadWindow) left out visible turns
// after position from, up to and including position to: some turn in (from, to]
// is marked Omitted. Only a read carries the mark.
func (w WorkItem) OmitsTurns(from, to int) bool {
	for k := max(from+1, 0); k <= to && k < len(w.Thread); k++ {
		if w.Thread[k].Omitted {
			return true
		}
	}
	return false
}

// Reviewed reports whether the latest review read exactly rev (ReviewedRevision):
// the same head, against the same base, with the same diff, which it must have read
// whole. Only then does an approval of rev stand. A push changes the head, a
// retarget the base, and a related change merged into the base (a stacked pull
// request's dependency) the diff while leaving both alone; an unrelated advance of
// the base changes none of them.
func (w WorkItem) Reviewed(rev Revision) bool { return reviewedAs(w.ReviewedRevision(), rev) }

// reviewedAs reports whether a review that recorded r read exactly rev, which it
// must have read whole (Reviewed).
func reviewedAs(r, rev Revision) bool {
	return r.Head != "" && r.Diff != "" && r.Head == strings.ToLower(rev.Head) && r.Base == rev.Base && r.Diff == rev.Diff
}

// ReviewOutdated reports whether the latest review is older than the newest draft
// and read another pull request revision: the draft's reply (which records the
// revision its run read) landed after the latest review, and the revisions differ,
// so the head was pushed, the base retargeted, or the diff moved since that review,
// and no approval can be drafted from it until the pull request is reviewed again.
// A review that landed after the draft was asked for is the newer reading, even if
// the draft's reply landed later still: then the draft is what is stale, and
// drafting again (never counted redundant once a turn follows its request) is the
// way forward, not another review. A draft reply without a revision, a hidden
// one, or a notice says nothing.
func (w WorkItem) ReviewOutdated() bool { return w.reviewOutdatedAt() >= 0 }

// reviewOutdatedAt returns the position of the draft reply that makes the latest
// review outdated (ReviewOutdated), or -1.
func (w WorkItem) reviewOutdatedAt() int {
	for i := len(w.Thread) - 1; i >= 0; i-- {
		if !w.IsDraftReviewRequest(i) {
			continue
		}
		j, ok := w.replies()[w.TurnKey(i)]
		if w.Thread[i].Hidden || !ok || w.Thread[j].Hidden || w.Thread[j].Head == "" {
			return -1
		}
		latest := w.latestReview()
		if latest < 0 || latest > i || w.Thread[latest].revision() == w.Thread[j].revision() {
			// No review to be out of date; one that landed after the draft was asked
			// for, so the draft is what is stale (it read the pull request before that
			// review, and drafting again is the way forward); or the same revision.
			return -1
		}
		return j
	}
	return -1
}

// FirstReviewModel returns the chat model that produced the item's first visible
// completed review — the Model stamped on the answer to the first review request
// that, like its answer, the owner has not hidden. A hidden review is out of the
// conversation a synthesis compares, so it does not count. It is "" when there is no
// such review, or when the reply predates model stamping. It lets the review panel
// and the bulk 2nd-opinion action always pick a different model.
func (w WorkItem) FirstReviewModel() string {
	answered := w.replies()
	for i, m := range w.Thread {
		if m.Role == RoleUser && m.Kind == KindReviewRequest && !m.Hidden {
			if j, ok := answered[w.TurnKey(i)]; ok && !w.Thread[j].Hidden {
				return w.Thread[j].Model
			}
		}
	}
	return ""
}

// HasSecondOpinion reports whether the item has a completed consensus synthesis — the
// second-opinion round has finished and a verdict is available.
func (w WorkItem) HasSecondOpinion() bool { return w.hasCompletedRequest(KindSynthesisRequest) }

// ReviewVerdict returns the consensus outcome (VerdictAgree / VerdictDisagree) of the
// newest answered synthesis request, read from its answer (the reply its ReplyTo
// links), or "" when there is none or that answer carries no verdict. Replies land
// out of order, so the newest request decides, not the last reply to arrive: an
// older round's late answer never replaces a newer round's verdict.
func (w WorkItem) ReviewVerdict() string {
	replies := w.replies()
	for i := len(w.Thread) - 1; i >= 0; i-- {
		if m := w.Thread[i]; m.Role == RoleUser && m.Kind == KindSynthesisRequest {
			if j, ok := replies[w.TurnKey(i)]; ok {
				return w.Thread[j].Verdict
			}
		}
	}
	return ""
}

// SynthesisInputs returns the turns the synthesis request at key compares: each
// review request it weighs, in thread order, immediately followed by its answer. Those are the
// only turns its run sends the model. A request records its reviews
// (Message.Reviews); for one stored before that, the comparison is rebuilt from the
// visible answered review requests before it. ok is false, and the run declines
// without a verdict, unless every named review is a visible review request whose
// answer is there and visible too, and there are at least two of them: one review
// cannot agree or disagree with anything.
//
// The web tier checks it on the stored item, and on the bounded read the run would
// get (SynthesisFits); the run checks it on the read it got (ThreadWindow keeps
// these turns first).
func (w WorkItem) SynthesisInputs(key string) ([]Message, bool) {
	i, ok := w.TurnIndex(key)
	if !ok || w.Thread[i].Role != RoleUser || w.Thread[i].Kind != KindSynthesisRequest {
		return nil, false
	}
	replies := w.replies()
	keys := w.synthesisReviews(i, replies)
	if len(keys) < 2 {
		return nil, false
	}
	type pair struct{ ask, answer int }
	pairs := make([]pair, 0, len(keys))
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		if seen[k] {
			continue
		}
		seen[k] = true
		j, ok := w.TurnIndex(k)
		if !ok || w.Thread[j].Role != RoleUser || w.Thread[j].Kind != KindReviewRequest || w.Thread[j].Hidden {
			return nil, false
		}
		a, ok := replies[k]
		if !ok || w.Thread[a].Hidden {
			return nil, false
		}
		pairs = append(pairs, pair{j, a})
	}
	// Each request is followed by its own answer (its reply link), in request order:
	// reviews that overlapped answer out of position, and a chat endpoint refuses two
	// user turns in a row.
	slices.SortFunc(pairs, func(x, y pair) int { return x.ask - y.ask })
	out := make([]Message, 0, 2*len(pairs))
	for _, p := range pairs {
		out = append(out, w.Thread[p.ask], w.Thread[p.answer])
	}
	return out, true
}

// SynthesisReady reports whether the synthesis request at key has the reviews it
// compares (SynthesisInputs).
func (w WorkItem) SynthesisReady(key string) bool {
	_, ok := w.SynthesisInputs(key)
	return ok
}

// SynthesisFits reports whether the synthesis request at key is ready
// (SynthesisReady) in the bounded read its run would get (ThreadWindow at
// MaxItemBytes), not only in the stored thread: reviews that are each valid can still
// be too large to read together, and then no retry can succeed.
func (w WorkItem) SynthesisFits(key string) bool {
	return ThreadWindow(w, key, MaxItemBytes).SynthesisReady(key)
}

// synthesisReviews is the keys of the review requests the synthesis request at
// position i compares: the ones it names, or, for a request stored before it named
// them, the visible review requests before it whose answers landed before it too (a
// review answered later was not part of that round).
func (w WorkItem) synthesisReviews(i int, replies map[string]int) []string {
	if len(w.Thread[i].Reviews) > 0 {
		return w.Thread[i].Reviews
	}
	var keys []string
	for k := 0; k < i; k++ {
		m := w.Thread[k]
		if m.Role != RoleUser || m.Kind != KindReviewRequest || m.Hidden {
			continue
		}
		if a, ok := replies[w.TurnKey(k)]; ok && a < i && !w.Thread[a].Hidden {
			keys = append(keys, w.TurnKey(k)) // answered before this round was asked
		}
	}
	return keys
}

// RetryableSynthesis returns the position of the synthesis request the thread can
// retry, wherever it sits in the thread: the newest synthesis request
// (StalledSynthesis), if every review it weighs is still there and fits the read its
// run would get (SynthesisFits).
// Unlike RetryableTurn it does not need to be the last user turn, so a Discuss turn
// posted after a failed synthesis does not strand it: the verdict can be recovered
// without another blind review.
func (w WorkItem) RetryableSynthesis(now time.Time, staleAfter time.Duration) (int, bool) {
	i, ok := w.StalledSynthesis(now, staleAfter)
	if !ok || !w.SynthesisFits(w.TurnKey(i)) {
		return 0, false
	}
	return i, true
}

// StalledSynthesis returns the position of the newest synthesis request if it is not
// hidden, has no answer, and is no longer plausibly in flight (older than staleAfter,
// or a notice ended its current attempt), whether or not it can be retried.
func (w WorkItem) StalledSynthesis(now time.Time, staleAfter time.Duration) (int, bool) {
	for i := len(w.Thread) - 1; i >= 0; i-- {
		m := w.Thread[i]
		if m.Role != RoleUser || m.Kind != KindSynthesisRequest {
			continue
		}
		key := w.TurnKey(i)
		if m.Hidden || w.Answered(key) {
			return 0, false
		}
		if !w.noticed()[key] && now.Sub(m.At) < staleAfter {
			return 0, false // still plausibly in flight
		}
		return i, true
	}
	return 0, false
}

// hasSynthesisRequest reports whether a consensus synthesis has been requested at all
// (pending or completed), so a second opinion already under way is not offered again.
func (w WorkItem) hasSynthesisRequest() bool {
	for _, m := range w.Thread {
		if m.Role == RoleUser && m.Kind == KindSynthesisRequest {
			return true
		}
	}
	return false
}

// ReplyPending reports whether some user turn has no reply yet and is still
// plausibly in flight (younger than staleAfter). Past that window the run has almost
// certainly finished or failed, so the turn is eligible for a retry.
//
// A turn a system notice replied to is not pending: its run is done. Nor is one whose
// current attempt the backend certainly refused (MarkRefused): no run started.
func (w WorkItem) ReplyPending(now time.Time, staleAfter time.Duration) bool {
	answered, noticed := w.replies(), w.noticed()
	for i, m := range w.Thread {
		if m.Role != RoleUser || now.Sub(m.At) >= staleAfter || w.refused(i) {
			continue
		}
		key := w.TurnKey(i)
		if _, ok := answered[key]; !ok && !noticed[key] {
			return true
		}
	}
	return false
}

// ReplyStalled reports whether the most recent user turn has no reply and is past
// staleAfter, so its run has almost certainly failed and the turn can be retried, or
// its current attempt was certainly refused (no run started), so it can be retried at
// once. A turn a system notice replied to has not stalled: the notice says what to do.
func (w WorkItem) ReplyStalled(now time.Time, staleAfter time.Duration) bool {
	i, ok := w.LastUserTurn()
	if !ok {
		return false
	}
	key := w.TurnKey(i)
	return (now.Sub(w.Thread[i].At) >= staleAfter || w.refused(i)) && !w.Answered(key) && !w.noticed()[key]
}

// RetryableTurn returns the position of the turn RetryStalled would retry: the
// stalled most recent user turn (ReplyStalled), unless the user has hidden it. A
// hidden turn is out of the model's context, and a retry would send it back as the
// prompt, so it is not retried until the user unhides it.
func (w WorkItem) RetryableTurn(now time.Time, staleAfter time.Duration) (int, bool) {
	if !w.ReplyStalled(now, staleAfter) {
		return 0, false
	}
	i, _ := w.LastUserTurn()
	if w.Thread[i].Hidden {
		return 0, false
	}
	return i, true
}

// RetryStalled prepares the RetryableTurn for a retry under its existing key: it
// re-stamps the turn's time to now, so the turn is pending again and a second retry
// within staleAfter finds nothing to do, and returns the stamp (RetryStamp), whose key
// and attempt a dispatch must name.
// It changes nothing and reports false when there is no such turn. The caller
// persists the item in the same Store.Update that read it. A retry never adds a
// turn, and the store keeps one answer per turn (AppendReply), so a first run that
// answers late and its retry leave one reply.
func (w *WorkItem) RetryStalled(now time.Time, staleAfter time.Duration) (RetryStamp, bool) {
	i, ok := w.RetryableTurn(now, staleAfter)
	if !ok {
		return RetryStamp{}, false
	}
	return w.Restamp(i, now), true
}

// RetryStamp records one retry of a turn (Restamp): the turn, the time and attempt it
// was given, and the ones it had before, which UndoRetry puts back.
type RetryStamp struct {
	Item, Key            string
	At, PrevAt           time.Time
	Attempt, PrevAttempt string
}

// Restamp starts a new attempt at the user turn at position i: it re-stamps the turn's
// time to now and gives it a new attempt ID (Message.Attempt), so the turn is pending
// again and only this attempt's own notice can end it (noticed). The thread is copied
// first, so a caller's read of the store is not changed under it.
func (w *WorkItem) Restamp(i int, now time.Time) RetryStamp {
	w.Thread = slices.Clone(w.Thread)
	m := &w.Thread[i]
	s := RetryStamp{Item: w.ID, Key: w.TurnKey(i), At: now, PrevAt: m.At, Attempt: NewMessageID(), PrevAttempt: m.Attempt}
	m.At, m.Attempt = s.At, s.Attempt
	return s
}

// RevertAttempt puts back the attempt Restamp's stamp s replaced, keeping the new
// time, when the retry started no run of its own but shares one already in flight
// for the turn: that run was dispatched as the earlier attempt, so only its notice
// can end the wait. It changes nothing, and reports false, unless the turn still
// carries exactly that attempt and is unanswered.
func (w *WorkItem) RevertAttempt(s RetryStamp) bool {
	i, ok := w.TurnIndex(s.Key)
	if !ok || w.Thread[i].Role != RoleUser || w.Thread[i].Attempt != s.Attempt || w.Answered(s.Key) {
		return false
	}
	w.Thread = slices.Clone(w.Thread)
	w.Thread[i].Attempt = s.PrevAttempt
	return true
}

// UndoRetry reverses Restamp's stamp s, restoring the turn's earlier time and
// attempt, when the dispatch it prepared certainly started no run. It changes
// nothing, and reports false, unless the turn still carries exactly that attempt and
// is still unanswered: a later retry, or an answer, wins.
func (w *WorkItem) UndoRetry(s RetryStamp) bool {
	i, ok := w.TurnIndex(s.Key)
	if !ok || w.Thread[i].Role != RoleUser || w.Thread[i].Attempt != s.Attempt || !w.Thread[i].At.Equal(s.At) || w.Answered(s.Key) {
		return false
	}
	w.Thread = slices.Clone(w.Thread)
	w.Thread[i].At, w.Thread[i].Attempt = s.PrevAt, s.PrevAttempt
	return true
}

// NeedsReview reports whether the pull request still needs a first review dispatched
// now, by Review all PRs: it is on the radar and not waiting on someone else
// (Actionable), has no completed review, and none is currently in flight (a stale
// pending request is treated as failed, so it is eligible again). A 2nd opinion
// awaiting a retry is left to the thread, which retries it blind: the bulk action
// would retry it through its ordinary dispatch, letting it read the conversation
// while it still counts as independent. The radar's count and the action both use
// this, so the button shows only what it will review.
func (w WorkItem) NeedsReview(now time.Time, staleAfter time.Duration) bool {
	if w.Type != TypePullRequest || !w.Actionable() || w.HasReview() || w.ReplyPending(now, staleAfter) {
		return false
	}
	if i, ok := w.RetryableTurn(now, staleAfter); ok && w.Thread[i].IsPanelReview() {
		return false
	}
	return true
}

// NeedsSecondOpinion reports whether the pull request has been reviewed once but has
// no consensus synthesis yet (and none in flight), and is on the radar and not waiting
// on someone else (Actionable) — the pool the bulk "Get 2nd Opinion" action draws from.
func (w WorkItem) NeedsSecondOpinion(now time.Time, staleAfter time.Duration) bool {
	return w.Type == TypePullRequest && w.Actionable() && w.HasReview() && !w.hasSynthesisRequest() && !w.ReplyPending(now, staleAfter)
}

// ToolDef describes a read-only tool the assistant may call during a turn.
// Parameters is a JSON Schema object for the tool's arguments. It is
// provider-neutral: the runtime supplies concrete tools behind it, so gofer core
// imports no provider client.
type ToolDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ToolBox is the set of read-only tools available to a Conversationalist for one
// turn. The runtime implements it over a provider client and the user's vended
// credential; a nil ToolBox means no tools. Invoke must never perform a write.
type ToolBox interface {
	Definitions() []ToolDef
	Invoke(ctx context.Context, name string, args json.RawMessage) (string, error)
}

// Conversationalist produces an assistant reply for an item, given freshly
// gathered source context, the prior thread, the user's new message, and a set of
// read-only tools it may call. It is read-only and advisory: it may summarize,
// draft, or suggest, but gofer never acts on GitHub from it. The real
// implementation calls an LLM from outside gofer core, behind this interface.
//
// sourceContext is additional, freshly fetched provider content that the converse
// runtime gathered with a gofer-vended credential. It is UNTRUSTED,
// attacker-influenceable data; implementations must frame it as data, never
// instructions.
type Conversationalist interface {
	Reply(ctx context.Context, item WorkItem, viewerLogin string, sourceContext string, history []Message, userText string, tools ToolBox) (string, error)
}

// MarkThreadRead advances ownerID's item itemID's read cursor to the agent turn
// whose key is seen (see MarkRead), in one Store.Update. It writes nothing when the
// cursor does not move or the item is gone, so re-rendering an already-read thread
// costs no write.
func MarkThreadRead(ctx context.Context, store Store, ownerID, itemID, seen string) error {
	if seen == "" {
		return nil
	}
	return store.Update(ctx, ownerID, func(current []WorkItem) ([]WorkItem, error) {
		for i := range current {
			if current[i].ID != itemID {
				continue
			}
			if !current[i].MarkRead(seen) {
				return nil, nil
			}
			return current[i : i+1], nil
		}
		return nil, nil // item gone
	})
}
