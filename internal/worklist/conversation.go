package worklist

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	// Hidden marks a turn the user has hidden from the thread view. A hidden turn is
	// also withheld from the model as future conversation context (see splitThread), so
	// a long review thread can be pared down to its definitive turns without deleting
	// the history. It is reversible — the user can unhide it.
	Hidden bool      `json:"hidden,omitempty"`
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

// HasUnreadReply reports whether the item's most recent agent reply is newer than
// when the owner last read the thread (ThreadReadAt) — i.e. there is a response
// the user has not seen. It drives the radar's "unread" cue. A thread whose last
// turn is still the user's (reply pending) is not unread.
func (w WorkItem) HasUnreadReply() bool {
	for i := len(w.Thread) - 1; i >= 0; i-- {
		if w.Thread[i].Role == RoleAgent {
			return w.Thread[i].At.After(w.ThreadReadAt)
		}
	}
	return false
}

// TurnRef names the thread turn a converse run answers: the item, and the key
// (WorkItem.TurnKey) of the user turn on it. Dispatch carries it, so a run answers the turn it was
// started for even after newer turns land.
type TurnRef struct {
	Item string
	Turn string
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
// before replies recorded that answers the user turn directly before it.
func (w WorkItem) replies() map[string]int {
	out := make(map[string]int)
	for i, m := range w.Thread {
		if m.Role != RoleAgent {
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

// FirstReviewModel returns the chat model that produced the item's first completed
// review — the Model stamped on the first agent reply answering the first answered
// review request. It is "" when there is no completed review, or when the reply
// predates model stamping. It lets the bulk 2nd-opinion action always pick a
// different model.
func (w WorkItem) FirstReviewModel() string {
	answered := w.replies()
	for i, m := range w.Thread {
		if m.Role == RoleUser && m.Kind == KindReviewRequest {
			if j, ok := answered[w.TurnKey(i)]; ok {
				return w.Thread[j].Model
			}
		}
	}
	return ""
}

// HasSecondOpinion reports whether the item has a completed consensus synthesis — the
// second-opinion round has finished and a verdict is available.
func (w WorkItem) HasSecondOpinion() bool { return w.hasCompletedRequest(KindSynthesisRequest) }

// ReviewVerdict returns the consensus outcome (VerdictAgree / VerdictDisagree) from
// the most recent synthesis reply, or "" when there is none.
func (w WorkItem) ReviewVerdict() string {
	for i := len(w.Thread) - 1; i >= 0; i-- {
		if m := w.Thread[i]; m.Role == RoleAgent && m.Verdict != "" {
			return m.Verdict
		}
	}
	return ""
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
func (w WorkItem) ReplyPending(now time.Time, staleAfter time.Duration) bool {
	answered := w.replies()
	for i, m := range w.Thread {
		if m.Role != RoleUser || now.Sub(m.At) >= staleAfter {
			continue
		}
		if _, ok := answered[w.TurnKey(i)]; !ok {
			return true
		}
	}
	return false
}

// ReplyStalled reports whether the most recent user turn has no reply and is past
// staleAfter, so its run has almost certainly failed and the turn can be retried.
func (w WorkItem) ReplyStalled(now time.Time, staleAfter time.Duration) bool {
	i, ok := w.LastUserTurn()
	if !ok {
		return false
	}
	return now.Sub(w.Thread[i].At) >= staleAfter && !w.Answered(w.TurnKey(i))
}

// NeedsReview reports whether the pull request still needs a first review dispatched
// now: it has no completed review and none is currently in flight (a stale pending
// request is treated as failed, so it is eligible again).
func (w WorkItem) NeedsReview(now time.Time, staleAfter time.Duration) bool {
	return w.Type == TypePullRequest && !w.HasReview() && !w.ReplyPending(now, staleAfter)
}

// NeedsSecondOpinion reports whether the pull request has been reviewed once but has
// no consensus synthesis yet (and none in flight) — the pool the bulk "Get 2nd
// Opinion" action draws from.
func (w WorkItem) NeedsSecondOpinion(now time.Time, staleAfter time.Duration) bool {
	return w.Type == TypePullRequest && w.HasReview() && !w.hasSynthesisRequest() && !w.ReplyPending(now, staleAfter)
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
