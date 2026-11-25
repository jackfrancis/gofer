package worklist

import (
	"context"
	"errors"
	"strings"
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
// A run adds to the thread only through AppendReply, and never changes the
// hidden-item flag. While an item is hidden, a write-back keeps only the run's
// GitHub fields, completion and labels: its signals and scoring are stripped
// (StripScoring), so a hidden item never carries a score (see hide.go).
//
// A pull request's CI summary (GitHubRef.CI) is kept like the model signals below:
// only enrich reads it, so a write-back that carries none keeps the stored one
// (KeepCI), and every other pass (refresh, retire, rank) sends none, never the copy
// it read. It is a GitHub field, so a hidden item keeps it (enrich skips hidden
// items, so it is the last one read).
//
// The model-derived signals are the exception to "the run owns its signals": a
// GitHub fetch cannot produce them, so a refresh sends none, and a scoring pass that
// failed for an item sends none either. A write-back that carries no model proposal
// (Signals.Proposed), research adjustment (Signals.Research) or turn reading
// (Signals.Turn) keeps the stored one; a write-back that carries one replaces it. A run that only re-derives the
// research adjustment writes it alone, through ApplyResearch, so it never carries the
// rest of an item it read before a refresh. Every workload pass sends only the
// model signal it derived (or none), so it never restores a stale copy it read.

// MergeRunItems returns the items a run's write-back should store, given the
// owner's current items. An item that exists keeps the owner's state from its
// stored copy and takes everything else from the run. A new item carries no owner
// state: a run cannot create a thread. A field the run read cut to size (Project)
// and returned unchanged takes its stored value again.
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
			it.Projected = nil
			out = append(out, it)
			continue
		}
		restoreProjected(&it, old)
		it.Thread = old.Thread
		it.ThreadReadAt = old.ThreadReadAt
		it.ThreadReadThrough = old.ThreadReadThrough
		it.Meta.HiddenAt = old.Meta.HiddenAt
		if !old.CreatedAt.IsZero() {
			it.CreatedAt = old.CreatedAt
		}
		KeepModelSignals(&it.Signals, old.Signals)
		KeepCI(&it, old)
		if it.IsHidden() {
			StripScoring(&it) // GitHub fields, the CI summary among them, stay
		}
		if !it.Meta.CompletedAt.IsZero() {
			// A retired item awaits no refresh: none revisits it until it reopens, and
			// a retirement may echo the flag from the copy it read.
			it.Meta.AwaitingRefresh = false
		}
		out = append(out, it)
	}
	return out
}

// KeepCI fills in a pull request's CI summary from the stored item when the
// write-back carries none. Only enrich reads CI; a refresh's search hits carry none,
// and copying the stored summary into the refresh would copy whatever the run read,
// which may be a projection (cut, and without its marker once copied). A summary the
// write-back carries wins.
func KeepCI(incoming *WorkItem, stored WorkItem) {
	if incoming.GitHub.CI == nil && incoming.Type == TypePullRequest {
		incoming.GitHub.CI = cloneCI(stored.GitHub.CI)
	}
}

// KeepModelSignals fills in the model-derived signals an incoming write-back does not
// carry — the model proposal, the research adjustment and the turn reading — from the
// stored signals. A value the write-back carries wins.
func KeepModelSignals(incoming *Signals, stored Signals) {
	if incoming.Proposed == nil {
		incoming.Proposed = stored.Proposed
	}
	if incoming.Research == nil {
		incoming.Research = stored.Research
	}
	if incoming.Turn == nil {
		incoming.Turn = stored.Turn
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
// An adjustment is dropped if the item is hidden (a hidden item carries no score),
// or if any turn it read (ResearchAdjustment.Turns) is no longer in the stored
// thread: the owner cleared the conversation, and research on the old one must not
// outlive it. Then AppliedAt orders adjustments: one older than
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

// SetResearch sets item's research adjustment unless the item is hidden, a turn it
// read is gone from the thread, or the stored adjustment is newer (by AppliedAt), and
// reports whether it did. An adjustment with no AppliedAt is ErrUnstampedResearch.
func SetResearch(item *WorkItem, adj ResearchAdjustment) (bool, error) {
	if adj.AppliedAt.IsZero() {
		return false, ErrUnstampedResearch
	}
	if item.IsHidden() {
		return false, nil // a hidden item carries no score
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
//     none. It keeps a Notice only if it is one gofer knows (NoticeReconnect,
//     NoticePanelInputs). It never carries Reviews, which mark user turns only.
//   - If the answered turn is no longer in the thread (the owner cleared the
//     conversation), the reply is dropped.
//   - A reply already in the thread (same ID, or for a reply without one the same
//     time and model — a resent request) is not added twice.
//   - A user turn keeps at most one answer: once an answer to it is stored (see
//     replies), any later reply to it is dropped, answer or notice. The first answer
//     stored wins, so a retry is safe even when the first run finishes late or gofer
//     restarts between them. A system notice is not an answer (see noticed), so a
//     turn answered only by a notice still accepts its answer. A hidden answer still
//     counts: hiding does not reopen the turn.
//
// The reply goes at the end of the thread, after any turns posted while the run
// worked; ReplyTo, not position, says which turn it answers.
func AppendReply(item *WorkItem, replyTo string, reply Message) (bool, error) {
	if reply.Content == "" {
		return false, ErrEmptyReply
	}
	reply.Role = RoleAgent
	reply.Kind = "" // request kinds mark user turns only
	reply.Reviews = nil
	reply.Hidden = false
	if !knownNotice(reply.Notice) {
		reply.Notice = "" // only notices gofer knows
	}
	if reply.Notice == "" {
		reply.Attempt = "" // only a notice names the attempt it ended
	}
	reply.ReplyTo = replyTo
	if validCommit(reply.Head) {
		reply.Head = strings.ToLower(reply.Head)
	} else {
		reply.Head = ""
	}
	found := false // the answered turn is still in the thread
	for i, m := range item.Thread {
		if reply.ID != "" && m.ID == reply.ID {
			return false, nil // already recorded
		}
		if reply.ID == "" && m.Role == RoleAgent && m.At.Equal(reply.At) && m.Model == reply.Model {
			return false, nil // already recorded
		}
		if m.Role == RoleUser && item.TurnKey(i) == replyTo {
			found = true
		}
	}
	if !found {
		return false, nil
	}
	if _, done := item.replies()[replyTo]; done {
		return false, nil // the turn already has its answer
	}
	// Only a draft-review request is answered with a draft, pinned to this item's
	// pull request, and a stored draft is always within its bounds, whatever the run
	// sent. A run never writes a receipt: that is the owner's confirmation.
	reply.DraftReceipt = nil
	if i, _ := item.TurnIndex(replyTo); item.IsDraftReviewRequest(i) && reply.Notice == "" {
		if reply.Draft = BoundDraft(reply.Draft); !reply.Draft.For(*item) {
			reply.Draft = nil
		}
		// An approval approves the commit the latest review read, so it stands only
		// on a draft pinned to that commit, whatever the run decided.
		if d := reply.Draft; d != nil && d.Approve && item.ReviewedHead() != d.Commit {
			d.Approve = false
			if d.Empty() {
				reply.Draft = nil
			}
		}
	} else {
		reply.Draft = nil
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
	_, err := applyReply(ctx, store, ownerID, itemID, replyTo, reply, false, time.Time{})
	return err
}

// applyReply is ApplyReply, chaining the review panel's synthesis when chain is set
// (ApplyReplyAndChain).
func applyReply(ctx context.Context, store Store, ownerID, itemID, replyTo string, reply Message, chain bool, at time.Time) (string, error) {
	var synth string
	err := store.Update(ctx, ownerID, func(current []WorkItem) ([]WorkItem, error) {
		synth = ""
		for i := range current {
			if current[i].ID != itemID {
				continue
			}
			changed, err := AppendReply(&current[i], replyTo, reply)
			if err != nil || !changed {
				return nil, err
			}
			if chain {
				synth, _ = ChainSynthesis(&current[i], replyTo, at)
			}
			return current[i : i+1], nil
		}
		return nil, nil // item gone
	})
	if err != nil {
		return "", err
	}
	return synth, nil
}
