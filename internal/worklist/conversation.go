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
const PanelInputsNotice = "gofer didn't run the consensus: a review it compares is hidden or gone, or the reviews are too large to read together. Unhide the review and retry, or hide some reviews and run the review panel again for a smaller comparison."

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
	// before it was recorded.
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
	// Notice marks an agent turn that is a system notice (NoticeReconnect), not an
	// answer: it does not answer the turn it replies to. Empty on every other turn.
	Notice string    `json:"notice,omitempty"`
	At     time.Time `json:"at"`
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

// SynthesisInputs returns the turns the synthesis request at key compares, in
// thread order: each review request it weighs followed by its answer. Those are the
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
	pos := make([]int, 0, 2*len(keys))
	for _, k := range keys {
		j, ok := w.TurnIndex(k)
		if !ok || w.Thread[j].Role != RoleUser || w.Thread[j].Kind != KindReviewRequest || w.Thread[j].Hidden {
			return nil, false
		}
		a, ok := replies[k]
		if !ok || w.Thread[a].Hidden {
			return nil, false
		}
		pos = append(pos, j, a)
	}
	slices.Sort(pos)
	pos = slices.Compact(pos)
	out := make([]Message, len(pos))
	for n, p := range pos {
		out[n] = w.Thread[p]
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
// them, the visible answered review requests before it.
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
		if a, ok := replies[w.TurnKey(k)]; ok && !w.Thread[a].Hidden {
			keys = append(keys, w.TurnKey(k))
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
// A turn a system notice replied to is not pending: its run is done.
func (w WorkItem) ReplyPending(now time.Time, staleAfter time.Duration) bool {
	answered, noticed := w.replies(), w.noticed()
	for i, m := range w.Thread {
		if m.Role != RoleUser || now.Sub(m.At) >= staleAfter {
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
// staleAfter, so its run has almost certainly failed and the turn can be retried. A
// turn a system notice replied to has not stalled: the notice says what to do.
func (w WorkItem) ReplyStalled(now time.Time, staleAfter time.Duration) bool {
	i, ok := w.LastUserTurn()
	if !ok {
		return false
	}
	key := w.TurnKey(i)
	return now.Sub(w.Thread[i].At) >= staleAfter && !w.Answered(key) && !w.noticed()[key]
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
// now: it is on the radar and not waiting on someone else (Actionable), has no completed review, and none is currently
// in flight (a stale pending request is treated as failed, so it is eligible again).
func (w WorkItem) NeedsReview(now time.Time, staleAfter time.Duration) bool {
	return w.Type == TypePullRequest && w.Actionable() && !w.HasReview() && !w.ReplyPending(now, staleAfter)
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
