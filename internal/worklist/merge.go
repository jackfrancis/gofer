package worklist

import (
	"context"
	"errors"
	"time"
)

// A run (an agent workload) reads the owner's items, works for seconds to minutes,
// and writes its results back. Anything the owner did in the meantime — a message
// posted, a turn hidden, conversations cleared — must survive that write. So a
// run's write-back is never applied as a whole item: the web tier merges it into
// the item as stored at write time, inside Store.Update, and these functions are
// the merge rules.
//
// A run OWNS what it derives from the source: the GitHub reference, type, signals
// (including model proposals and research adjustments), the scoring metadata, and
// whether the item is completed. The OWNER's state always comes from the stored
// item: the conversation thread (its messages and their hidden flags), the read
// cursor, the hidden-item flag, and when gofer first saw the item. A run adds to
// the thread only through AppendReply.

// MergeRunItems returns the items a run's write-back should store, given the
// owner's current items. An item that exists keeps the owner's state from its
// stored copy and takes everything else from the run. A new item carries no owner
// state: a run cannot create a thread.
func MergeRunItems(current, incoming []WorkItem) []WorkItem {
	stored := make(map[string]WorkItem, len(current))
	for _, it := range current {
		stored[it.ID] = it
	}
	out := make([]WorkItem, 0, len(incoming))
	for _, it := range incoming {
		old, ok := stored[it.ID]
		if !ok {
			it.Thread = nil
			it.ThreadReadAt = time.Time{}
			it.Meta.HiddenAt = time.Time{}
			out = append(out, it)
			continue
		}
		it.Thread = old.Thread
		it.ThreadReadAt = old.ThreadReadAt
		it.Meta.HiddenAt = old.Meta.HiddenAt
		if !old.CreatedAt.IsZero() {
			it.CreatedAt = old.CreatedAt
		}
		out = append(out, it)
	}
	return out
}

// ErrEmptyReply is returned by AppendReply for a reply with no content.
var ErrEmptyReply = errors.New("worklist: reply has no content")

// AppendReply adds an agent's reply to item's thread, answering the user turn
// stamped inReplyTo. It reports whether the thread changed:
//
//   - The reply is always a visible agent turn with no request kind, whatever it
//     claims, so a run cannot put words in the owner's mouth.
//   - If the answered turn is no longer in the thread (the owner cleared the
//     conversation, or a retry re-stamped the prompt and a newer run will answer
//     it), the reply is dropped.
//   - A reply already in the thread (same time and model — a retried request) is
//     not added twice.
//
// The reply goes at the end of the thread, so the positions of existing turns
// never change (hiding a turn addresses it by position).
func AppendReply(item *WorkItem, inReplyTo time.Time, reply Message) (bool, error) {
	if reply.Content == "" {
		return false, ErrEmptyReply
	}
	reply.Role = RoleAgent
	reply.Kind = "" // request kinds mark user turns only
	reply.Hidden = false
	answered := false
	for _, m := range item.Thread {
		if m.Role == RoleAgent && m.At.Equal(reply.At) && m.Model == reply.Model {
			return false, nil // already recorded
		}
		if m.Role == RoleUser && m.At.Equal(inReplyTo) {
			answered = true
		}
	}
	if !answered {
		return false, nil
	}
	item.Thread = append(item.Thread, reply)
	return true, nil
}

// ApplyReply appends reply to ownerID's item itemID in one Store.Update (see
// AppendReply). It writes nothing, and returns no error, when the item or the
// answered turn is gone. The agent plane's reply handler and the in-process sink
// both use it.
func ApplyReply(ctx context.Context, store Store, ownerID, itemID string, inReplyTo time.Time, reply Message) error {
	return store.Update(ctx, ownerID, func(current []WorkItem) ([]WorkItem, error) {
		for i := range current {
			if current[i].ID != itemID {
				continue
			}
			changed, err := AppendReply(&current[i], inReplyTo, reply)
			if err != nil || !changed {
				return nil, err
			}
			return current[i : i+1], nil
		}
		return nil, nil // item gone
	})
}
