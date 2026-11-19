package agent

import (
	"context"
	"fmt"
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
	// Read just the dispatched item, thread included, not the worklist.
	item, ok, err := sink.Item(ctx, p.ItemID)
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
	)
	provider := p.Provider
	if provider == "" {
		provider = "github"
	}
	holder, verr := newTokenHolder(ctx, vendor, provider, p.GitHubBaseURL)
	switch {
	case isSignInRequired(verr):
		return postReconnect(ctx, sink, item.ID, triggerKey)
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
		isPR := item.Type == worklist.TypePullRequest
		if disc, derr := gh.Discussion(ctx, token, item.GitHub.Repo, item.GitHub.Number, isPR); derr == nil {
			sourceContext = formatDiscussion(disc)
		}
		// The token was vended unexpired but GitHub refused it, and the broker
		// confirmed the user's access is gone.
		if holder.Reconnect() != nil {
			return postReconnect(ctx, sink, item.ID, triggerKey)
		}
		if ferr := holder.Failed(); ferr != nil {
			// GitHub refused the token and the broker could not be asked for a new
			// one: the same as a failed vend.
			p.logger().Warn("converse: GitHub unavailable for this turn; answering from stored context", "err", ferr)
			viewerLogin, sourceContext, noGitHub = "", "", true
		} else {
			box := newGitHubToolBox(gh, token, item.GitHub.Repo)
			// Repository checkouts live on the sandbox's scratch disk for this turn only.
			defer box.Close()
			tools = reconnectBox{ToolBox: box, h: holder}
		}
	}
	if noGitHub {
		userText += githubUnavailableNote
	}

	reply, err := p.Converser.Reply(ctx, item, viewerLogin, sourceContext, history, userText, tools)
	if isSignInRequired(err) || (holder != nil && holder.Reconnect() != nil) {
		// A tool call found the access gone: whatever the model produced is
		// discarded, never saved as a reply or review.
		return postReconnect(ctx, sink, item.ID, triggerKey)
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
		At:      time.Now().UTC(),
	}
	// Append the reply to the thread as stored now, not to the copy read above: the
	// owner may have posted, hidden or cleared turns while the model was answering.
	if err := sink.Reply(ctx, item.ID, triggerKey, msg); err != nil {
		return fmt.Errorf("reply: %w", err)
	}
	// A completed conversation turn is fresh evidence: re-weight the item's ranking
	// from the thread (best-effort). The GitHub-metadata foundation stays
	// authoritative — research only nuances it. The write-back carries only the
	// signal; the plane keeps the stored thread.
	if p.Researcher != nil {
		item.Thread = append(item.Thread, msg)
		if adj, rerr := p.Researcher.Research(ctx, item); rerr == nil {
			item.Signals.Research = &adj
			item.Thread = nil // the plane keeps the stored thread; don't resend it
			if err := sink.Ingest(ctx, []worklist.WorkItem{item}); err != nil {
				return fmt.Errorf("ingest: %w", err)
			}
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
func postReconnect(ctx context.Context, sink Sink, itemID, triggerKey string) error {
	msg := worklist.Message{
		ID:      worklist.NewMessageID(),
		ReplyTo: triggerKey,
		Role:    worklist.RoleAgent,
		Notice:  worklist.NoticeReconnect,
		Content: worklist.ReconnectNotice,
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
