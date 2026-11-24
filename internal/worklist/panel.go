package worklist

import (
	"context"
	"time"
)

// IndependentReviewPrompt is the user turn the review panel posts for the
// alternative model's blind review (Kind KindReviewRequest). It asks for a thorough,
// independent assessment: the runtime answers without the prior thread, so the model
// forms its own view before the consensus synthesis weighs every review. A panel
// review is a review request with this text (Message.IsPanelReview).
const IndependentReviewPrompt = "Give an independent review of this PR. There may be other humans actively reviewing the PR. Your value is to thoroughly review in detail, especially areas of ambiguity, and to make a practical assessment of the PR quality's readiness for acceptance, or if not, some concrete suggestions for next steps."

// SynthesisPrompt is the consensus turn (Kind KindSynthesisRequest) chained once a
// panel review has another review to weigh it against: the default model weighs the
// reviews the turn names (Message.Reviews) and rules definitively on whether they
// agree. Its leading AGREEMENT/DISAGREEMENT token is lifted into Message.Verdict by
// the runtime (ParseVerdict), so the radar can show the outcome without re-reading the
// prose.
const SynthesisPrompt = "Do all the reviews of this PR in this conversation agree with one another? You may call out subtle differences, but it is your job to state definitively whether or not there is agreement, or disagreement. Begin your reply with a single line containing exactly one word — AGREEMENT if the reviews substantively agree, or DISAGREEMENT if they do not — and nothing else on that line. Then, on the following lines, explain your reasoning."

// IsPanelReview reports whether m is the review panel's independent-review request:
// a review request (Kind KindReviewRequest, which only the web tier's panel and
// review actions set) with the panel's text. The same text sent as an ordinary
// Discuss turn has no kind, and is not one. Only the web tier writes user turns
// (AppendReply makes every run's turn an agent turn), so a run cannot forge one.
func (m Message) IsPanelReview() bool {
	return m.Role == RoleUser && m.Kind == KindReviewRequest && m.Content == IndependentReviewPrompt
}

// ChainSynthesis appends the consensus synthesis request to item's thread when the
// review request whose key is answered has just got its answer, and returns the new
// turn's key. Either review of a panel can land last (an earlier review that ran long
// can finish after the panel's blind one), so any review's answer rechecks. It chains
// only when there is something new to weigh:
//
//   - the item is not hidden (a hidden item takes no new turn), and the answered turn
//     is a review request;
//   - some visible panel review (IsPanelReview) with a visible answer is not yet
//     covered by a synthesis request (one naming it in Reviews, or one stored before
//     Reviews was recorded that follows it);
//   - there are at least two visible answered reviews, and they are not all by the
//     same known model: a model compared with itself is no second opinion. A panel
//     run with no earlier review (the owner ran the panel first) has nothing to
//     compare, so its review stands as the item's first until another lands.
//
// The synthesis names every visible answered review (Message.Reviews), so the run
// compares exactly those (SynthesisReady). The caller runs it in the same store update
// as the answer (ApplyReplyAndChain), and a covered panel review never chains again,
// so each panel review chains at most one synthesis, across retries and restarts.
func ChainSynthesis(item *WorkItem, answered string, at time.Time) (string, bool) {
	if item.IsHidden() {
		return "", false
	}
	i, ok := item.TurnIndex(answered)
	if !ok || item.Thread[i].Role != RoleUser || item.Thread[i].Kind != KindReviewRequest {
		return "", false
	}
	replies := item.replies()
	covered := item.synthesized()
	var (
		reviews []string
		models  = map[string]bool{}
		fresh   bool // an answered panel review no synthesis covers yet
	)
	for k, m := range item.Thread {
		if m.Role != RoleUser || m.Kind != KindReviewRequest || m.Hidden {
			continue
		}
		key := item.TurnKey(k)
		j, ok := replies[key]
		if !ok || item.Thread[j].Hidden {
			continue
		}
		reviews = append(reviews, key)
		models[item.Thread[j].Model] = true
		if m.IsPanelReview() && !covered[key] {
			fresh = true
		}
	}
	if !fresh || len(reviews) < 2 || (len(models) == 1 && !models[""]) {
		return "", false
	}
	msg := Message{ID: NewMessageID(), Role: RoleUser, Content: SynthesisPrompt, Kind: KindSynthesisRequest, Reviews: reviews, At: at}
	item.Thread = append(item.Thread, msg)
	return msg.ID, true
}

// synthesized is the set of review requests a synthesis request already covers: the
// ones it names (Message.Reviews), or, for one stored before Reviews was recorded,
// every review request before it.
func (w WorkItem) synthesized() map[string]bool {
	out := make(map[string]bool)
	for i, m := range w.Thread {
		if m.Role != RoleUser || m.Kind != KindSynthesisRequest {
			continue
		}
		if len(m.Reviews) > 0 {
			for _, k := range m.Reviews {
				out[k] = true
			}
			continue
		}
		for k := 0; k < i; k++ {
			if w.Thread[k].Role == RoleUser && w.Thread[k].Kind == KindReviewRequest {
				out[w.TurnKey(k)] = true
			}
		}
	}
	return out
}

// ApplyReplyAndChain is ApplyReply for the agent plane: in the same Store.Update that
// stores the answer, it chains the review panel's consensus synthesis
// (ChainSynthesis), and returns the synthesis turn's key ("" when none was chained),
// which the caller dispatches to the default model. at stamps that turn.
func ApplyReplyAndChain(ctx context.Context, store Store, ownerID, itemID, replyTo string, reply Message, at time.Time) (string, error) {
	return applyReply(ctx, store, ownerID, itemID, replyTo, reply, true, at)
}
