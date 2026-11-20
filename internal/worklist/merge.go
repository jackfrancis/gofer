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
// A run OWNS what it derives from the source: the GitHub reference, type, signals,
// the scoring metadata, and whether the item is completed. The OWNER's state always
// comes from the stored item: the conversation thread (its messages and their hidden
// flags), the read cursor, the hidden-item flag, and when gofer first saw the item.
// A run adds to the thread only through AppendReply. The one thing a run can do to
// the hidden flag is clear it: an item whose GitHub update time (a run-owned field)
// is later than when the owner hid it has changed since, so it resurfaces
// (HiddenAfter).
//
// The two model-derived signals are the exception to "the run owns its signals": a
// GitHub fetch cannot produce them, so a refresh sends none, and a scoring pass that
// failed for an item sends none either. A write-back that carries no model proposal
// (Signals.Proposed) or no research adjustment (Signals.Research) keeps the stored
// one; a write-back that carries one replaces it. A run that only re-derives the
// research adjustment writes it alone, through ApplyResearch, so it never carries the
// rest of an item it read before a refresh. Every workload pass sends only the
// model signal it derived (or none), so it never restores a stale copy it read.

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
			it.ThreadReadThrough = ""
			it.Meta.HiddenAt = time.Time{}
			out = append(out, it)
			continue
		}
		it.Thread = old.Thread
		it.ThreadReadAt = old.ThreadReadAt
		it.ThreadReadThrough = old.ThreadReadThrough
		it.Meta.HiddenAt = HiddenAfter(old.Meta.HiddenAt, it.GitHub.UpdatedAt)
		if !old.CreatedAt.IsZero() {
			it.CreatedAt = old.CreatedAt
		}
		KeepModelSignals(&it.Signals, old.Signals)
		out = append(out, it)
	}
	return out
}

// KeepModelSignals fills in the model-derived signals an incoming write-back does not
// carry — the model proposal and the research adjustment — from the stored signals.
// A value the write-back carries wins.
func KeepModelSignals(incoming *Signals, stored Signals) {
	if incoming.Proposed == nil {
		incoming.Proposed = stored.Proposed
	}
	if incoming.Research == nil {
		incoming.Research = stored.Research
	}
}

// ErrUnstampedResearch is returned by ApplyResearch for an adjustment with no
// AppliedAt: without it a replayed write could not be told from a newer one.
var ErrUnstampedResearch = errors.New("worklist: research adjustment has no applied_at")

// ApplyResearch sets ownerID's item itemID's research adjustment, and nothing else,
// in one Store.Update. A research pass reads the item, asks the model for minutes,
// and writes back only what it derived: the GitHub fields, the other signals and
// the thread are left as stored now, so a refresh that landed meanwhile stands.
//
// An adjustment is dropped if any turn it read (ResearchAdjustment.Turns) is no
// longer in the stored thread: the owner cleared the conversation, and research on
// the old one must not outlive it. Then AppliedAt orders adjustments: one older than
// the stored adjustment is dropped, so a resent write cannot undo a later result. It
// writes nothing, and returns no error, when the item is gone or the adjustment is
// dropped.
func ApplyResearch(ctx context.Context, store Store, ownerID, itemID string, adj ResearchAdjustment) error {
	if adj.AppliedAt.IsZero() {
		return ErrUnstampedResearch
	}
	return store.Update(ctx, ownerID, func(current []WorkItem) ([]WorkItem, error) {
		for i := range current {
			if current[i].ID != itemID {
				continue
			}
			changed, err := SetResearch(&current[i], adj)
			if err != nil || !changed {
				return nil, err
			}
			return current[i : i+1], nil
		}
		return nil, nil // item gone
	})
}

// SetResearch sets item's research adjustment unless a turn it read is gone from
// the thread or the stored adjustment is newer (by AppliedAt), and reports whether it
// did. An adjustment with no AppliedAt is ErrUnstampedResearch.
func SetResearch(item *WorkItem, adj ResearchAdjustment) (bool, error) {
	if adj.AppliedAt.IsZero() {
		return false, ErrUnstampedResearch
	}
	if !turnsInThread(*item, adj.Turns) {
		return false, nil // read a conversation since cleared
	}
	if old := item.Signals.Research; old != nil && adj.AppliedAt.Before(old.AppliedAt) {
		return false, nil // a newer adjustment is already stored
	}
	item.Signals.Research = &adj
	return true, nil
}

// turnsInThread reports whether every key in turns names a turn in item's thread,
// hidden or not: hiding a turn keeps it in the conversation (Discuss refuses research
// that read one at read time, ResearchReflectsVisibleThread), while clearing the
// conversation removes it.
func turnsInThread(item WorkItem, turns []string) bool {
	if len(turns) == 0 {
		return true
	}
	have := make(map[string]bool, len(item.Thread))
	for i := range item.Thread {
		have[item.TurnKey(i)] = true
	}
	for _, k := range turns {
		if !have[k] {
			return false
		}
	}
	return true
}

// ErrEmptyReply is returned by AppendReply for a reply with no content.
var ErrEmptyReply = errors.New("worklist: reply has no content")

// AppendReply adds an agent's reply to item's thread, answering the user turn
// whose key (WorkItem.TurnKey) is replyTo, and records that link on the reply (Message.ReplyTo). It
// reports whether the thread changed:
//
//   - The reply is always a visible agent turn with no request kind, whatever it
//     claims, so a run cannot put words in the owner's mouth. It gets an ID if it has
//     none. It keeps a Notice only if it is one gofer knows (NoticeReconnect).
//   - If the answered turn is no longer in the thread (the owner cleared the
//     conversation), the reply is dropped.
//   - A reply already in the thread (same ID, or for a reply without one the same
//     time and model — a resent request) is not added twice.
//
// The reply goes at the end of the thread, after any turns posted while the run
// worked; ReplyTo, not position, says which turn it answers.
func AppendReply(item *WorkItem, replyTo string, reply Message) (bool, error) {
	if reply.Content == "" {
		return false, ErrEmptyReply
	}
	reply.Role = RoleAgent
	reply.Kind = "" // request kinds mark user turns only
	reply.Hidden = false
	if reply.Notice != NoticeReconnect {
		reply.Notice = "" // only notices gofer knows
	}
	reply.ReplyTo = replyTo
	answered := false
	for i, m := range item.Thread {
		if reply.ID != "" && m.ID == reply.ID {
			return false, nil // already recorded
		}
		if reply.ID == "" && m.Role == RoleAgent && m.At.Equal(reply.At) && m.Model == reply.Model {
			return false, nil // already recorded
		}
		if m.Role == RoleUser && item.TurnKey(i) == replyTo {
			answered = true
		}
	}
	if !answered {
		return false, nil
	}
	if reply.ID == "" {
		reply.ID = NewMessageID()
	}
	item.Thread = append(item.Thread, reply)
	return true, nil
}

// ApplyReply appends reply to ownerID's item itemID in one Store.Update (see
// AppendReply). It writes nothing, and returns no error, when the item or the
// answered turn is gone. The agent plane's reply handler and the in-process sink
// both use it.
func ApplyReply(ctx context.Context, store Store, ownerID, itemID, replyTo string, reply Message) error {
	return store.Update(ctx, ownerID, func(current []WorkItem) ([]WorkItem, error) {
		for i := range current {
			if current[i].ID != itemID {
				continue
			}
			changed, err := AppendReply(&current[i], replyTo, reply)
			if err != nil || !changed {
				return nil, err
			}
			return current[i : i+1], nil
		}
		return nil, nil // item gone
	})
}
