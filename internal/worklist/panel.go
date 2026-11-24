package worklist

import (
	"context"
	"time"
)

// IndependentReviewPrompt is the user turn the review panel posts for the
// alternative model's blind review (Kind KindReviewRequest). It asks for a thorough,
// independent assessment: the runtime answers without the prior thread, so the model
// forms its own view before the consensus synthesis weighs every review. The turn is
// recognised by this text (Message.IsPanelReview), so a turn stored before the
// synthesis was chained is still a panel review.
const IndependentReviewPrompt = "Give an independent review of this PR. There may be other humans actively reviewing the PR. Your value is to thoroughly review in detail, especially areas of ambiguity, and to make a practical assessment of the PR quality's readiness for acceptance, or if not, some concrete suggestions for next steps."

// SynthesisPrompt is the consensus turn (Kind KindSynthesisRequest) chained once a
// panel review is answered: the default model weighs every review in the thread and
// rules definitively on whether they agree. Its leading AGREEMENT/DISAGREEMENT token
// is lifted into Message.Verdict by the runtime (ParseVerdict), so the radar can show
// the outcome without re-reading the prose.
const SynthesisPrompt = "Do all the reviews of this PR in this conversation agree with one another? You may call out subtle differences, but it is your job to state definitively whether or not there is agreement, or disagreement. Begin your reply with a single line containing exactly one word — AGREEMENT if the reviews substantively agree, or DISAGREEMENT if they do not — and nothing else on that line. Then, on the following lines, explain your reasoning."

// IsPanelReview reports whether m is the review panel's independent-review request.
// Only the web tier writes user turns (AppendReply makes every run's turn an agent
// turn), so a run cannot forge one.
func (m Message) IsPanelReview() bool {
	return m.Role == RoleUser && m.Content == IndependentReviewPrompt
}

// ChainSynthesis appends the consensus synthesis request to item's thread after the
// user turn whose key is answered got its answer, and returns the new turn's key. It
// chains only when there is something to weigh:
//
//   - the item is not hidden (a hidden item takes no new turn);
//   - the answered turn is a visible panel review (IsPanelReview) with a substantive
//     answer (a system notice is not one);
//   - the thread holds at least one other visible review: a review request, other
//     than this one, whose answer is visible too. A panel run with no earlier review
//     (the owner ran the panel first) has nothing to compare, so its review simply
//     stands as the item's first.
//
// The caller runs it in the same store update as the answer (ApplyReplyAndChain), and
// AppendReply stores only a turn's first answer, so each panel review chains at most
// one synthesis, across retries and restarts.
func ChainSynthesis(item *WorkItem, answered string, at time.Time) (string, bool) {
	if item.IsHidden() {
		return "", false
	}
	i, ok := item.TurnIndex(answered)
	if !ok || !item.Thread[i].IsPanelReview() || item.Thread[i].Hidden {
		return "", false
	}
	replies := item.replies()
	if j, ok := replies[answered]; !ok || item.Thread[j].Hidden {
		return "", false
	}
	other := false
	for k, m := range item.Thread {
		if k == i || m.Role != RoleUser || m.Kind != KindReviewRequest || m.Hidden {
			continue
		}
		if j, ok := replies[item.TurnKey(k)]; ok && !item.Thread[j].Hidden {
			other = true
			break
		}
	}
	if !other {
		return "", false
	}
	msg := Message{ID: NewMessageID(), Role: RoleUser, Content: SynthesisPrompt, Kind: KindSynthesisRequest, At: at}
	item.Thread = append(item.Thread, msg)
	return msg.ID, true
}

// ApplyReplyAndChain is ApplyReply for the agent plane: in the same Store.Update that
// stores the answer, it chains the review panel's consensus synthesis
// (ChainSynthesis), and returns the synthesis turn's key ("" when none was chained),
// which the caller dispatches to the default model. at stamps that turn.
func ApplyReplyAndChain(ctx context.Context, store Store, ownerID, itemID, replyTo string, reply Message, at time.Time) (string, error) {
	return applyReply(ctx, store, ownerID, itemID, replyTo, reply, true, at)
}
