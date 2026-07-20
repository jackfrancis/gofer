package worklist

import (
	"context"
	"encoding/json"
	"time"
)

// Conversation roles. A thread alternates user and agent turns.
const (
	RoleUser  = "user"
	RoleAgent = "agent"
)

// Message is one turn in an item's assistive conversation thread, retained on
// the WorkItem. Role is RoleUser or RoleAgent.
type Message struct {
	Role    string    `json:"role"`
	Content string    `json:"content"`
	At      time.Time `json:"at"`
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
