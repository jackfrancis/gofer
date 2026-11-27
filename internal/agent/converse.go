package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackfrancis/gofer/internal/github"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// runConverse answers one turn of the assistive conversation for a single item
// (p.ItemID): it reads the item, gathers read-only GitHub context and tools, replies to the user's latest thread message via the Conversationalist,
// appends the reply, and writes the item back. It reads and writes gofer only —
// no provider write — and the reply is advisory (gofer never acts on GitHub from
// it).
func runConverse(ctx context.Context, p Params, vendor Vendor, sink Sink) error {
	if p.Converser == nil {
		return fmt.Errorf("agent: github-converse requires a converser")
	}
	if p.ItemID == "" {
		return fmt.Errorf("agent: github-converse requires an item id")
	}
	// Read just the dispatched item, with a bounded window of its thread, not the
	// worklist. A run dispatched without a turn answers the newest user turn, which
	// the read resolves against the stored thread so the window keeps it.
	readTurn := p.Turn
	if readTurn == "" {
		readTurn = worklist.LatestUserTurn
	}
	item, ok, err := sink.Item(ctx, p.ItemID, readTurn)
	if err != nil {
		return fmt.Errorf("read item: %w", err)
	}
	if !ok {
		return nil // item gone; nothing to answer
	}

	trigger, triggerKey, history, ok := turnToAnswer(item, p.Turn)
	if !ok {
		return nil // the turn is gone or already answered: nothing to do
	}
	userText := trigger.Content
	if p.Independent {
		// An independent review answers blind: the model still sees the item and its
		// live context, but not the prior thread (other reviewers), so it forms its
		// own view before the consensus synthesis weighs them all.
		history = nil
	}
	// The turn's kind tells us whether this reply is a consensus synthesis, whose
	// leading verdict token we record as structured metadata.
	triggerKind := trigger.Kind
	// A notice this run posts ends only the attempt it was dispatched as (run
	// parameter "attempt", Message.Attempt), never whatever the stored turn says when
	// it starts: a run queued before a retry reads the retry's attempt, and must not
	// end it.
	attempt := p.Attempt
	var reviews []worklist.Message // a synthesis's comparison: its review pairs, nothing else
	if triggerKind == worklist.KindSynthesisRequest {
		var ready bool
		reviews, ready = item.SynthesisInputs(triggerKey)
		if !ready {
			// A review the synthesis weighs is hidden, gone, or did not fit the bounded
			// read (which keeps those reviews first), or there are not two to compare:
			// the model would rule on agreement without them. Decline with a notice,
			// never a verdict; the thread offers a retry once the reviews are back
			// (worklist.RetryableSynthesis).
			return postNotice(ctx, sink, item.ID, triggerKey, attempt, worklist.NoticePanelInputs, worklist.PanelInputsNotice)
		}
		// The model compares exactly the reviews this round recorded: a turn unhidden
		// or posted since, a later review among them, is not part of this round.
		history = reviews
	}

	// Live context plus read-only GitHub tools. With a credential the assistant gets
	// both a pre-fetched snapshot of the item and tools to look up anything else —
	// all read-only, bounded by the token's own permissions. Without one:
	//
	//   - "sign-in required" (at the vend, in the snapshot fetch, or from a tool call):
	//     the model is not called, or its output is discarded, and the turn gets a
	//     reconnect notice instead (ADR 0007, "Discuss without GitHub access");
	//   - any other vend failure: the model answers from gofer's stored context, told
	//     GitHub is unavailable, and the reply says so visibly.
	var (
		sourceContext string
		viewerLogin   string
		tools         worklist.ToolBox
		holder        *tokenHolder
		noGitHub      bool
		head, base    string // the pull request's head commit and base branch, as the discussion read showed them
		diffDigest    string // a review's: the digest of the whole diff it read (github.PullDiff.Digest)
		// unmoved re-reads the pull request after the model answered: true when its
		// head and base are still the ones recorded. Set only with a diff to vouch for.
		unmoved func(context.Context) bool
	)
	provider := p.Provider
	if provider == "" {
		provider = "github"
	}
	holder, verr := newTokenHolder(ctx, vendor, provider, p.GitHubBaseURL)
	switch {
	case isSignInRequired(verr):
		return postReconnect(ctx, sink, item.ID, triggerKey, attempt)
	case verr != nil:
		p.logger().Warn("converse: GitHub unavailable for this turn; answering from stored context", "err", verr)
		holder, noGitHub = nil, true
	default:
		token := holder.Token()
		gh := github.NewClient(holder.client(p.Client), p.GitHubBaseURL)
		// Resolve who the assistant is talking to, so it recognizes the user when
		// they appear on the item and never refers them to their own account.
		if login, lerr := gh.Login(ctx, token); lerr == nil {
			viewerLogin = login
		}
		if triggerKind == worklist.KindDraftReviewRequest {
			// A draft review reads the diff, not the discussion, and answers with a
			// structured draft (draft.go).
			if holder.Reconnect() != nil {
				return postReconnect(ctx, sink, item.ID, triggerKey, attempt)
			}
			return runDraftReview(ctx, p, sink, item, triggerKey, gh, holder, viewerLogin)
		}
		isPR := item.Type == worklist.TypePullRequest
		// A review records the revision it read, whole diff included, so an approval
		// drafted later stands only on that (a stacked pull request's diff can change
		// with its head and base name). It reads one settled snapshot first and shows
		// the model that snapshot's head and files, so what the model is told it
		// reviews and what is recorded are the same read.
		var snap *github.PullDiff
		if isPR && triggerKind == worklist.KindReviewRequest {
			if d, derr := gh.PullDiff(ctx, token, item.GitHub.Repo, item.GitHub.Number); derr == nil {
				snap = &d
			}
		}
		if disc, derr := gh.Discussion(ctx, token, item.GitHub.Repo, item.GitHub.Number, isPR); derr == nil {
			switch {
			case snap != nil:
				disc.HeadSHA, disc.ChangedFiles = snap.HeadSHA, snap.ChangedPaths()
				head, base, diffDigest = snap.HeadSHA, snap.BaseRef, snap.Digest()
			case isPR:
				head, base = disc.HeadSHA, disc.BaseRef
			}
			sourceContext = formatDiscussion(disc)
		}
		if diffDigest != "" {
			repo, number := item.GitHub.Repo, item.GitHub.Number
			unmoved = func(ctx context.Context) bool {
				h, b, err := gh.PullRevision(ctx, holder.Token(), repo, number)
				return err == nil && strings.EqualFold(h, head) && b == base
			}
		}
		// The token was vended unexpired but GitHub refused it, and the broker
		// confirmed the user's access is gone.
		if holder.Reconnect() != nil {
			return postReconnect(ctx, sink, item.ID, triggerKey, attempt)
		}
		if ferr := holder.Failed(); ferr != nil {
			// GitHub refused the token and the broker could not be asked for a new
			// one: the same as a failed vend.
			p.logger().Warn("converse: GitHub unavailable for this turn; answering from stored context", "err", ferr)
			viewerLogin, sourceContext, head, base, diffDigest, noGitHub = "", "", "", "", "", true
		} else {
			box := newGitHubToolBox(gh, token, item.GitHub.Repo)
			// Repository checkouts live on the sandbox's scratch disk for this turn only.
			defer box.Close()
			tools = reconnectBox{ToolBox: box, h: holder}
		}
	}
	if noGitHub && triggerKind == worklist.KindDraftReviewRequest {
		// A draft is anchored to the diff: without GitHub there is nothing to anchor to.
		return postDraftUnavailable(ctx, p, sink, item.ID, triggerKey)
	}
	if noGitHub {
		userText += githubUnavailableNote
	}

	// The converser explains the item's ranking, and a research adjustment summarizes
	// the turns it read, so the model's copy of the item carries only the thread this
	// turn may see: the turns up to the one being answered (hidden ones still flagged),
	// or none for an independent review. The converser withholds research that read
	// anything else. What this run writes back is unchanged.
	model := item
	model.Thread = nil
	if i, ok := item.TurnIndex(triggerKey); ok && !p.Independent {
		model.Thread = item.Thread[: i+1 : i+1]
		if reviews != nil {
			model.Thread = append(slices.Clip(reviews), item.Thread[i])
		}
	}
	reply, err := p.Converser.Reply(ctx, model, viewerLogin, sourceContext, history, userText, tools)
	if isSignInRequired(err) || (holder != nil && holder.Reconnect() != nil) {
		// A tool call found the access gone: whatever the model produced is
		// discarded, never saved as a reply or review.
		return postReconnect(ctx, sink, item.ID, triggerKey, attempt)
	}
	if err != nil {
		return fmt.Errorf("converse: %w", err)
	}
	// A synthesis reply opens with a machine-readable verdict token (see
	// synthesisPrompt): lift it into the structured Verdict field and strip it from the
	// displayed content, so the radar can show agreement/disagreement deterministically.
	content, verdict := reply, ""
	if triggerKind == worklist.KindSynthesisRequest {
		verdict, content = worklist.ParseVerdict(reply)
	}
	// The model's tools read GitHub live: if the pull request moved while it worked,
	// it may have reviewed more than the snapshot, so the review vouches for no diff.
	if diffDigest != "" && (unmoved == nil || !unmoved(ctx)) {
		diffDigest = ""
	}
	if holder != nil && holder.Reconnect() != nil {
		// That last read found the access gone: the answer is discarded, as for a
		// tool call that did, and the turn stays retryable.
		return postReconnect(ctx, sink, item.ID, triggerKey, attempt)
	}
	switch {
	case noGitHub:
		content += storedContextOnlyNote
	case holder != nil && holder.Failed() != nil:
		// Access was lost partway through the tool loop.
		content += lostGitHubNote
	}
	msg := worklist.Message{
		ID:      worklist.NewMessageID(),
		ReplyTo: triggerKey,
		Role:    worklist.RoleAgent,
		Content: content,
		Model:   p.Model,
		Verdict: verdict,
		Head:    head, // what a review read: an approval drafted later must be of this revision
		Base:    base,
		Diff:    diffDigest,
		At:      time.Now().UTC(),
	}
	// Append the reply to the thread as stored now, not to the copy read above: the
	// owner may have posted, hidden or cleared turns while the model was answering.
	if err := sink.Reply(ctx, item.ID, triggerKey, msg); err != nil {
		return fmt.Errorf("reply: %w", err)
	}
	// A completed conversation turn is fresh evidence: re-weight the item's ranking
	// from the thread (best-effort). The GitHub-metadata foundation stays
	// authoritative — research only nuances it. Research reads the thread again, as
	// a research run does (the newest turns): the window read above ends at the turn
	// just answered, and turns answered and researched while this run worked would
	// otherwise be left out of an adjustment that replaces theirs. The write-back
	// carries only the adjustment (Sink.Research), so a refresh that landed since
	// keeps its GitHub fields.
	if p.Researcher != nil {
		latest, ok, err := sink.Item(ctx, item.ID, "")
		if err != nil {
			// The answer is saved and a retry would skip the answered turn, so a
			// failed read only costs this best-effort research, not the run.
			p.logger().Warn("converse: reply saved; skipping research after a failed read", "item", item.ID, "err", err)
			return nil
		}
		if _, kept := latest.TurnIndex(msg.ID); !ok || !kept {
			// The item is gone, or the reply was dropped (the thread was cleared, or
			// another run answered first): this run has nothing to research.
			return nil
		}
		adj, rerr := p.Researcher.Research(ctx, latest)
		if rerr != nil {
			// Best-effort: the reply is saved, and the scheduled research pass retries
			// the adjustment (worklist.ResearchStale), without another reply.
			p.logger().Warn("converse: reply saved; research failed", "item", item.ID, "err", rerr)
			return nil
		}
		if err := sink.Research(ctx, item.ID, stampResearch(adj, latest)); err != nil {
			return fmt.Errorf("research: %w", err)
		}
	}
	return nil
}

// githubUnavailableNote tells the model, after the user's question, that this turn
// has no GitHub access. It goes to the model only; the saved turn is unchanged.
const githubUnavailableNote = "\n\n[gofer: GitHub is unavailable for this turn, so you have no live GitHub context and no tools. Answer only from the item and the conversation above, and say plainly what you could not check.]"

// storedContextOnlyNote is appended to a reply the model gave without GitHub access,
// so the user knows to ask again for an answer with live evidence.
const storedContextOnlyNote = "\n\n---\n_Answered from stored context only: gofer couldn't reach GitHub for this turn. Ask again later for an answer backed by live GitHub data._"

// lostGitHubNote is appended to a reply whose tool calls lost GitHub access partway
// through the turn (a token GitHub refused, and no broker to replace it).
const lostGitHubNote = "\n\n---\n_gofer lost access to GitHub partway through this turn, so parts of this answer may rest on stored context only. Ask again later to check it against live GitHub data._"

// postReconnect saves a reconnect notice in reply to the turn: the run could not
// reach GitHub for the user, who must sign in again. It is a system notice, not an
// answer (worklist.NoticeReconnect), so review state does not count it.
func postReconnect(ctx context.Context, sink Sink, itemID, triggerKey, attempt string) error {
	return postNotice(ctx, sink, itemID, triggerKey, attempt, worklist.NoticeReconnect, worklist.ReconnectNotice)
}

// postNotice saves a system notice (worklist.Message.Notice) with text in reply to
// the turn: the run ended without answering the attempt it was dispatched as
// (attempt, Params.Attempt).
func postNotice(ctx context.Context, sink Sink, itemID, triggerKey, attempt, notice, text string) error {
	msg := worklist.Message{
		ID:      worklist.NewMessageID(),
		ReplyTo: triggerKey,
		Role:    worklist.RoleAgent,
		Notice:  notice,
		Attempt: attempt,
		Content: text,
		At:      time.Now().UTC(),
	}
	if err := sink.Reply(ctx, itemID, triggerKey, msg); err != nil {
		return fmt.Errorf("reply: %w", err)
	}
	return nil
}

// turnToAnswer returns the user turn a converse run answers, its key, and the turns
// before it as history, with hidden turns withheld so a turn the user hid never rides along as
// context. key names the turn the run was dispatched for (Params.Turn); empty picks
// the most recent user turn. It reports false when there is nothing to answer: the
// turn is gone (the owner cleared the thread), or it already has a reply (another run
// answered it first). Answering the dispatched turn, not whatever is last, keeps a
// follow-up posted while an earlier run worked from being skipped or answered twice.
func turnToAnswer(item worklist.WorkItem, key string) (worklist.Message, string, []worklist.Message, bool) {
	var (
		idx int
		ok  bool
	)
	if key != "" {
		idx, ok = item.TurnIndex(key)
	} else {
		idx, ok = item.LastUserTurn()
	}
	if !ok || item.Thread[idx].Role != worklist.RoleUser {
		return worklist.Message{}, "", nil, false
	}
	key = item.TurnKey(idx)
	if item.Answered(key) {
		return worklist.Message{}, "", nil, false
	}
	var history []worklist.Message
	for _, m := range item.Thread[:idx] {
		if !m.Hidden {
			history = append(history, m)
		}
	}
	return item.Thread[idx], key, history, true
}

// formatDiscussion renders a fetched GitHub discussion snapshot as compact plain
// text. The content is untrusted (bodies and comments are attacker-influenceable),
// so the converser wraps it in an explicit "treat as data, not instructions"
// frame; the github client has already bounded its size.
func formatDiscussion(d github.Discussion) string {
	var b strings.Builder
	if d.Body != "" {
		fmt.Fprintf(&b, "Description:\n%s\n", d.Body)
	}
	if len(d.ChangedFiles) > 0 {
		fmt.Fprintf(&b, "\nChanged files (%d):\n", len(d.ChangedFiles))
		for _, f := range d.ChangedFiles {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	if d.HeadSHA != "" {
		fmt.Fprintf(&b, "\nHead commit: %s\n", d.HeadSHA)
	}
	if len(d.Reviews) > 0 {
		fmt.Fprintf(&b, "\nReviews (%d most recent):\n", len(d.Reviews))
		for _, r := range d.Reviews {
			state := r.State
			if state == "" {
				state = "reviewed"
			}
			if r.Body != "" {
				fmt.Fprintf(&b, "- %s [%s]: %s\n", commentAuthor(r.Author), state, r.Body)
			} else {
				fmt.Fprintf(&b, "- %s [%s]\n", commentAuthor(r.Author), state)
			}
		}
	}
	if len(d.ReviewComments) > 0 {
		fmt.Fprintf(&b, "\nReview comments (%d most recent):\n", len(d.ReviewComments))
		for _, c := range d.ReviewComments {
			fmt.Fprintf(&b, "- %s: %s\n", commentAuthor(c.Author), c.Body)
		}
	}
	if len(d.Comments) > 0 {
		fmt.Fprintf(&b, "\nDiscussion (%d most recent comments):\n", len(d.Comments))
		for _, c := range d.Comments {
			fmt.Fprintf(&b, "- %s: %s\n", commentAuthor(c.Author), c.Body)
		}
	}
	return b.String()
}

// commentAuthor falls back to a neutral label when GitHub omits the author (e.g.
// a deleted account), keeping the rendered line well-formed.
func commentAuthor(a string) string {
	if a == "" {
		return "someone"
	}
	return a
}
