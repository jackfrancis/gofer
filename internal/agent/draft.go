package agent

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackfrancis/gofer/internal/github"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// This file is the draft review's workload (ADR 0010). A converse run dispatched for
// a KindDraftReviewRequest turn reads the pull request's diff, asks the drafter for
// the comments the conversation agrees on, keeps inline only the comments whose line
// the diff shows, and replies with the draft. It writes nothing to GitHub: the web
// tier creates the pending review, from the stored draft, when the owner confirms.

// maxDiffPromptBytes bounds the annotated diff sent to the model. A file whose patch
// does not fit is listed without it, and no comment can anchor to it.
const maxDiffPromptBytes = 256 << 10

// Reply texts for a draft request that drafts nothing.
const (
	draftNotPRReply       = "Draft review comments apply to pull requests only, so nothing was drafted."
	draftUnavailableReply = "gofer can't draft review comments here: no chat model is configured for drafting. Nothing was drafted."
	draftNoGitHubReply    = "gofer couldn't read this pull request's diff from GitHub, so nothing was drafted. Try again in a moment."
	draftClosedReply      = "This pull request is no longer open, so nothing was drafted."
)

// runDraftReview answers a draft-review request (see the file comment). gh and
// holder carry the user's read credential; history is the visible conversation
// before the request.
func runDraftReview(ctx context.Context, p Params, sink Sink, item worklist.WorkItem, triggerKey string, history []worklist.Message, gh *github.Client, holder *tokenHolder, viewer string) error {
	reply := func(content string, draft *worklist.DraftReview) error {
		msg := worklist.Message{
			ID:      worklist.NewMessageID(),
			ReplyTo: triggerKey,
			Role:    worklist.RoleAgent,
			Content: content,
			Model:   p.Model,
			Draft:   draft,
			At:      time.Now().UTC(),
		}
		if err := sink.Reply(ctx, item.ID, triggerKey, msg); err != nil {
			return fmt.Errorf("reply: %w", err)
		}
		return nil
	}
	if item.Type != worklist.TypePullRequest || item.GitHub.Number <= 0 {
		return reply(draftNotPRReply, nil)
	}
	if p.Drafter == nil {
		return reply(draftUnavailableReply, nil)
	}
	diff, err := gh.PullDiff(ctx, holder.Token(), item.GitHub.Repo, item.GitHub.Number)
	if holder.Reconnect() != nil {
		return postReconnect(ctx, sink, item.ID, triggerKey)
	}
	if err != nil {
		p.logger().Warn("draft review: could not read the pull request diff", "item", item.ID, "err", err)
		return reply(draftNoGitHubReply, nil)
	}
	if !diff.Open {
		return reply(draftClosedReply, nil)
	}
	text, anchors, omitted := renderDiff(diff.Files, maxDiffPromptBytes)
	prop, err := p.Drafter.DraftReview(ctx, worklist.DraftInput{Item: item, ViewerLogin: viewer, History: history, Diff: text})
	if err != nil {
		return fmt.Errorf("draft review: %w", err)
	}
	draft, moved := anchorDraft(prop, anchors, diff.HeadSHA)
	return reply(draftContent(draft, prop.Excluded, moved, omitted, diff.Complete), draft)
}

// anchor is one line a review comment may sit on.
type anchor struct {
	side string
	line int
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// patchAnchors returns the lines of a unified-diff patch a comment may anchor to:
// added and context lines on the RIGHT (the pull request's version), removed and
// context lines on the LEFT (the base). It also returns the patch annotated with
// those numbers for the model: "[R<n>]" before an added or context line, "[L<n>]"
// before a removed one.
func patchAnchors(patch string) (map[anchor]bool, string) {
	anchors := map[anchor]bool{}
	var b strings.Builder
	oldLine, newLine, inHunk := 0, 0, false
	for _, line := range strings.Split(patch, "\n") {
		if m := hunkHeader.FindStringSubmatch(line); m != nil {
			oldLine, _ = strconv.Atoi(m[1])
			newLine, _ = strconv.Atoi(m[2])
			inHunk = true
			b.WriteString(line + "\n")
			continue
		}
		if !inHunk || line == "" {
			continue
		}
		switch line[0] {
		case '+':
			anchors[anchor{worklist.SideRight, newLine}] = true
			fmt.Fprintf(&b, "[R%d] %s\n", newLine, line)
			newLine++
		case '-':
			anchors[anchor{worklist.SideLeft, oldLine}] = true
			fmt.Fprintf(&b, "[L%d] %s\n", oldLine, line)
			oldLine++
		case ' ':
			anchors[anchor{worklist.SideRight, newLine}] = true
			anchors[anchor{worklist.SideLeft, oldLine}] = true
			fmt.Fprintf(&b, "[R%d] %s\n", newLine, line)
			oldLine++
			newLine++
		case '\\':
			b.WriteString(line + "\n") // "\ No newline at end of file"
		}
	}
	return anchors, b.String()
}

// renderDiff annotates the files' patches for the model, within budget bytes, and
// returns each rendered file's anchors by path. omitted counts files listed without
// a patch: GitHub sent none, or it did not fit.
func renderDiff(files []github.DiffFile, budget int) (string, map[string]map[anchor]bool, int) {
	var b strings.Builder
	anchors := map[string]map[anchor]bool{}
	omitted := 0
	for _, f := range files {
		header := fmt.Sprintf("### %s (%s)\n", f.Path, f.Status)
		if f.Patch == "" {
			omitted++
			fmt.Fprintf(&b, "### %s (%s) — no patch from GitHub (binary or too large); no comment can anchor here\n\n", f.Path, f.Status)
			continue
		}
		a, annotated := patchAnchors(f.Patch)
		if b.Len()+len(header)+len(annotated) > budget {
			omitted++
			line := fmt.Sprintf("### %s (%s) — patch left out to fit; no comment can anchor here\n\n", f.Path, f.Status)
			if b.Len()+len(line) <= budget {
				b.WriteString(line)
			}
			continue
		}
		b.WriteString(header)
		b.WriteString(annotated)
		b.WriteString("\n")
		anchors[f.Path] = a
	}
	return b.String(), anchors, omitted
}

// anchorDraft builds the draft from the model's proposal: a comment stays inline
// only if its line is one the diff showed it (anchors); any other, and any past
// MaxDraftComments, moves into the review summary with its location, so an agreed
// point is never lost to a bad anchor. moved counts those. It returns nil when there
// is nothing to post.
func anchorDraft(prop worklist.DraftProposal, anchors map[string]map[anchor]bool, commit string) (*worklist.DraftReview, int) {
	d := &worklist.DraftReview{Commit: commit}
	general := append([]string(nil), prop.General...)
	moved := 0
	for _, c := range prop.Comments {
		body := strings.TrimSpace(c.Body)
		if body == "" {
			continue
		}
		side := strings.ToUpper(strings.TrimSpace(c.Side))
		if side == "" {
			side = worklist.SideRight
		}
		if anchors[c.Path][anchor{side, c.Line}] && len(d.Comments) < worklist.MaxDraftComments {
			d.Comments = append(d.Comments, worklist.DraftComment{Path: c.Path, Line: c.Line, Side: side, Body: body})
			continue
		}
		moved++
		switch {
		case c.Path != "" && c.Line > 0:
			general = append(general, fmt.Sprintf("`%s` line %d: %s", c.Path, c.Line, body))
		case c.Path != "":
			general = append(general, fmt.Sprintf("`%s`: %s", c.Path, body))
		default:
			general = append(general, body)
		}
	}
	if len(general) > 0 {
		d.Body = "- " + strings.Join(general, "\n- ")
	}
	bounded := worklist.BoundDraft(d)
	return bounded, moved
}

// draftContent is the reply's text: what was drafted (numbered, as the thread's
// selection names them), what was left out for lack of consensus, and what could
// not be placed. It is also what later runs read of the draft.
func draftContent(d *worklist.DraftReview, excluded []string, moved, omitted int, complete bool) string {
	var b strings.Builder
	if d.Empty() {
		b.WriteString("**Nothing to draft.** The conversation has no agreed, actionable point to turn into a review comment yet.\n")
	} else {
		n := len(d.Comments)
		fmt.Fprintf(&b, "**Drafted %d inline review comment%s** from the points this conversation agrees on, anchored to commit `%s`.", n, plural(n), shortSHA(d.Commit))
		if d.Body != "" {
			b.WriteString(" Points no diff line carries are in the review summary.")
		}
		b.WriteString(" Nothing is posted until you create the pending review below, and on GitHub it stays a draft only you can see until you submit it there.\n")
		for i, c := range d.Comments {
			fmt.Fprintf(&b, "\n%d. `%s` line %d%s:\n\n%s\n", i+1, c.Path, c.Line, sideNote(c.Side), indent(c.Body))
		}
		if d.Body != "" {
			fmt.Fprintf(&b, "\n**Review summary**\n\n%s\n", d.Body)
		}
	}
	if len(excluded) > 0 {
		b.WriteString("\n**Left out — no consensus yet**\n\n")
		for _, e := range excluded {
			fmt.Fprintf(&b, "- %s\n", e)
		}
	}
	var notes []string
	if moved > 0 {
		notes = append(notes, fmt.Sprintf("%d comment%s pointed at a line the diff does not show, so %s in the summary instead.", moved, plural(moved), itThey(moved)))
	}
	if omitted > 0 {
		notes = append(notes, fmt.Sprintf("%d file%s had no patch gofer could show, so no comment is anchored there.", omitted, plural(omitted)))
	}
	if !complete {
		notes = append(notes, "The pull request has more files than gofer reads, so some files were not considered.")
	}
	if len(notes) > 0 {
		b.WriteString("\n_" + strings.Join(notes, " ") + "_\n")
	}
	return b.String()
}

// indent makes body one Markdown list-item paragraph block, so a multi-line comment
// (a suggestion block included) stays under its number.
func indent(body string) string {
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = "   " + l
		}
	}
	return strings.Join(lines, "\n")
}

func sideNote(side string) string {
	if side == worklist.SideLeft {
		return " (removed line)"
	}
	return ""
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func itThey(n int) string {
	if n == 1 {
		return "it is"
	}
	return "they are"
}

// postDraftUnavailable answers a draft request the run could not reach GitHub for.
func postDraftUnavailable(ctx context.Context, p Params, sink Sink, itemID, triggerKey string) error {
	msg := worklist.Message{
		ID:      worklist.NewMessageID(),
		ReplyTo: triggerKey,
		Role:    worklist.RoleAgent,
		Content: draftNoGitHubReply,
		Model:   p.Model,
		At:      time.Now().UTC(),
	}
	if err := sink.Reply(ctx, itemID, triggerKey, msg); err != nil {
		return fmt.Errorf("reply: %w", err)
	}
	return nil
}
