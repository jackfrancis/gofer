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
// (p.ItemID): it reads the item, gathers best-effort read-only GitHub context and
// tools, replies to the user's latest thread message via the Conversationalist,
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
	items, err := sink.List(ctx)
	if err != nil {
		return fmt.Errorf("list worklist: %w", err)
	}
	idx := -1
	for i := range items {
		if items[i].ID == p.ItemID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil // item gone; nothing to answer
	}
	item := items[idx]

	userText, history := splitThread(item.Thread)
	if userText == "" {
		return nil // the thread does not end on a user turn: nothing to answer
	}

	// Best-effort live context plus read-only GitHub tools: a vend failure (e.g.
	// no credential) or an empty token leaves the assistant reasoning over the
	// item's gofer metadata and thread alone. With a credential it gets both a
	// pre-fetched snapshot of the item and tools to look up anything else — all
	// read-only, bounded by the token's own scopes.
	var (
		sourceContext string
		viewerLogin   string
		tools         worklist.ToolBox
	)
	provider := p.Provider
	if provider == "" {
		provider = "github"
	}
	if token, verr := vendor.Vend(ctx, provider); verr == nil && token != "" {
		gh := github.NewClient(p.Client, p.GitHubBaseURL)
		// Resolve who the assistant is talking to, so it recognizes the user when
		// they appear on the item and never refers them to their own account.
		if login, lerr := gh.Login(ctx, token); lerr == nil {
			viewerLogin = login
		}
		isPR := item.Type == worklist.TypePullRequest
		if disc, derr := gh.Discussion(ctx, token, item.GitHub.Repo, item.GitHub.Number, isPR); derr == nil {
			sourceContext = formatDiscussion(disc)
		}
		tools = newGitHubToolBox(gh, token, item.GitHub.Repo)
	}

	reply, err := p.Converser.Reply(ctx, item, viewerLogin, sourceContext, history, userText, tools)
	if err != nil {
		return fmt.Errorf("converse: %w", err)
	}
	item.Thread = append(item.Thread, worklist.Message{
		Role:    worklist.RoleAgent,
		Content: reply,
		At:      time.Now().UTC(),
	})
	// A completed conversation turn is fresh evidence: re-weight the item's ranking
	// from the thread (best-effort). The GitHub-metadata foundation stays
	// authoritative — research only nuances it.
	if p.Researcher != nil {
		if adj, rerr := p.Researcher.Research(ctx, item); rerr == nil {
			item.Signals.Research = &adj
		}
	}
	if err := sink.Ingest(ctx, []worklist.WorkItem{item}); err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	return nil
}

// splitThread returns the last user message's content (the prompt to answer) and
// the messages before it (the history). It returns "" when the thread does not
// end on a user turn — i.e. there is nothing new to answer.
func splitThread(thread []worklist.Message) (userText string, history []worklist.Message) {
	n := len(thread)
	if n == 0 || thread[n-1].Role != worklist.RoleUser {
		return "", nil
	}
	return thread[n-1].Content, thread[:n-1]
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
