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
	draftUpdatingReply    = "gofer was being updated when this draft was asked for, so it could not check which review an approval would rest on, and nothing was drafted. Draft again once the update is done."
)

// runDraftReview answers a draft-review request (see the file comment). gh and
// holder carry the user's read credential; item is the run's read, whose visible
// turns before the request are the conversation drafted from (draftHistory).
func runDraftReview(ctx context.Context, p Params, sink Sink, item worklist.WorkItem, triggerKey string, gh *github.Client, holder *tokenHolder, viewer string) error {
	var rev worklist.Revision // the revision the draft read, recorded on its reply (WorkItem.ReviewOutdated)
	reply := func(content string, draft *worklist.DraftReview) error {
		msg := worklist.Message{
			ID:      worklist.NewMessageID(),
			ReplyTo: triggerKey,
			Role:    worklist.RoleAgent,
			Content: content,
			Model:   p.Model,
			Draft:   draft,
			Head:    rev.Head,
			Base:    rev.Base,
			Diff:    rev.Diff,
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
		return postReconnect(ctx, sink, item.ID, triggerKey, p.Attempt) // the attempt this run was dispatched as
	}
	if err != nil {
		p.logger().Warn("draft review: could not read the pull request diff", "item", item.ID, "err", err)
		return reply(draftNoGitHubReply, nil)
	}
	if !diff.Open {
		return reply(draftClosedReply, nil)
	}
	rev = worklist.Revision{Head: diff.HeadSHA, Base: diff.BaseRef, Diff: diff.Digest()}
	text, anchors, omitted := renderDiff(diff.Files, maxDiffPromptBytes)
	patches := map[string]string{} // every rendered file's patch digest; the draft keeps the commented ones
	for _, f := range diff.Files {
		if anchors[f.Path] != nil {
			patches[f.Path] = worklist.PatchDigest(f.Patch)
		}
	}
	at := triggerIndex(item, triggerKey)
	history, partial := draftHistory(item, at)
	prop, err := p.Drafter.DraftReview(ctx, worklist.DraftInput{Item: item, ViewerLogin: viewer, History: history, Partial: partial, Diff: text, Head: diff.HeadSHA, Base: diff.BaseRef})
	if err != nil {
		return fmt.Errorf("draft review: %w", err)
	}
	// The conversation's approval is of the revision its latest review read. If the
	// head was pushed, the base retargeted, or the diff moved since (or that review
	// recorded none), it approves nothing here, and the reply says so whatever the
	// model answered, so the owner knows to review again: the store applies the same
	// rule (worklist.AppendReply), and the thread offers a review of the current
	// revision (WorkItem.ReviewOutdated). A diff gofer cannot identify (too long to
	// read whole, moving while read, a binary change with no patch) is never vouched
	// for: an approval of it is withheld, and the reply says why. The review is the
	// latest before this request, which the read names with its revision whether or
	// not it fits (DraftBasis); an approval stands only on a read holding it and every
	// visible turn since it was asked for. A stale revision is reported first: hiding
	// turns to make the read fit cannot make an old commit current. A read from a web
	// tier older than the run names no review at all (DraftBasis.Named): nothing can
	// be checked, so a draft that would approve drafts nothing, and drafting again
	// once the update is done can. Replying with no draft is what keeps that offered
	// (RedraftRedundant), whichever tier stores the reply: an older one keeps no field
	// it does not know.
	basis := item.DraftBasis(at)
	var stale *staleReview
	switch {
	case diff.Unverifiable() != "":
		if prop.Approve {
			stale = &staleReview{unverifiable: diff.Unverifiable(), approved: true}
		}
		prop.Approve = false
	case !basis.Named:
		if prop.Approve {
			return reply(draftUpdatingReply, nil)
		}
	case !basis.Review:
		// Drafted from a discussion with no review in it: comments need none, but an
		// approval is pinned to the revision a review read, so it needs one.
		if prop.Approve {
			stale = &staleReview{noReview: true, approved: true}
		}
		prop.Approve = false
	case !basis.Covers(rev):
		stale = &staleReview{reviewed: basis.Revision, now: rev, approved: prop.Approve}
		prop.Approve = false
	case !basis.Read:
		// The conversation since that review was asked for is longer than a read:
		// a later word the drafter never saw (the owner's, even one posted while the
		// review stalled) may disagree, so what it read is no agreement to approve.
		if prop.Approve {
			stale = &staleReview{unread: true, approved: true}
		}
		prop.Approve = false
	}
	pin := worklist.DraftReview{Repo: item.GitHub.Repo, Number: item.GitHub.Number, Commit: diff.HeadSHA, BaseRef: diff.BaseRef, Patches: patches, Diff: rev.Diff}
	draft, moved := anchorDraft(prop, anchors, pin)
	return reply(draftContent(draft, prop.Excluded, moved, omitted, diff.Complete, stale), draft)
}

// draftHistory is the visible conversation before position at in item (the run's
// read) that the drafter is shown, and whether the read left earlier turns out
// (partial). An answer whose question the read left out (WorkItem.Unpaired) is not
// shown: alone it is no evidence of agreement, since the owner's word on it, even a
// rejection, may have been left out with the question. An approval then needs no
// such answer since its review was asked for (DraftBasis.Read). A read from an older
// web tier marks no omission, so one that drops an answer is called partial.
func draftHistory(item worklist.WorkItem, at int) (history []worklist.Message, partial bool) {
	unpaired := item.Unpaired(at)
	for k := range min(at, len(item.Thread)) {
		if !item.Thread[k].Hidden && !unpaired[k] {
			history = append(history, item.Thread[k])
		}
	}
	return history, item.OmitsTurns(-1, at) || len(unpaired) > 0
}

// triggerIndex is the position of the turn with key in item's thread, or the end
// of the thread when it is not there.
func triggerIndex(item worklist.WorkItem, key string) int {
	if i, ok := item.TurnIndex(key); ok {
		return i
	}
	return len(item.Thread)
}

// staleReview is a latest review that read another revision (reviewed; zero when
// it recorded none) than the one the draft is anchored to (now), or, with
// unverifiable (why), a diff gofer cannot identify. approved marks an approval the
// drafter proposed and the run withheld for it.
type staleReview struct {
	reviewed, now worklist.Revision
	unverifiable  string
	noReview      bool // the conversation has no visible review
	unread        bool // the read does not hold the latest review and every turn since it was asked for
	approved      bool
}

// anchor is one line a review comment may sit on.
type anchor struct {
	side string
	line int
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// patchAnchors returns the lines of a unified-diff patch a comment may anchor to,
// as GitHub defines them: added and unchanged context lines on the RIGHT (the pull
// request's version), removed lines on the LEFT (the base). It also returns the
// patch annotated with exactly those anchors for the model: "[R<n>]" before an added
// or context line, "[L<n>]" before a removed one. A context line's base number is
// not an anchor: the model is never shown it, and GitHub names context lines RIGHT.
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
			fmt.Fprintf(&b, "[R%d] %s\n", newLine, line)
			oldLine++
			newLine++
		case '\\':
			b.WriteString(line + "\n") // "\ No newline at end of file"
		}
	}
	return anchors, b.String()
}

// renderDiff annotates the files' patches for the model and returns each rendered
// file's anchors by path. Every write is checked against budget bytes, notices
// included: a file whose text does not fit is left out, with a notice only while the
// notice fits. omitted counts every file listed without its patch (GitHub sent none,
// or it did not fit), whether or not its notice was written.
func renderDiff(files []github.DiffFile, budget int) (string, map[string]map[anchor]bool, int) {
	var b strings.Builder
	anchors := map[string]map[anchor]bool{}
	omitted := 0
	write := func(s string) bool {
		if b.Len()+len(s) > budget {
			return false
		}
		b.WriteString(s)
		return true
	}
	for _, f := range files {
		if f.Patch == "" {
			omitted++
			write(fmt.Sprintf("### %s (%s) — no patch from GitHub (binary or too large); no comment can anchor here\n\n", f.Path, f.Status))
			continue
		}
		a, annotated := patchAnchors(f.Patch)
		if !write(fmt.Sprintf("### %s (%s)\n%s\n", f.Path, f.Status, annotated)) {
			omitted++
			write(fmt.Sprintf("### %s (%s) — patch left out to fit; no comment can anchor here\n\n", f.Path, f.Status))
			continue
		}
		anchors[f.Path] = a
	}
	return b.String(), anchors, omitted
}

// anchorDraft builds the draft, pinned to pin's pull request, head and base, from the
// model's proposal: a comment stays inline
// only if its line is one the diff showed it (anchors); any other, and any past
// MaxDraftComments, moves into the review summary with its location, so an agreed
// point is never lost to a bad anchor. moved counts those. An approving proposal
// marks the draft Approve (the review then carries worklist.ApprovalCommands). It
// returns nil when there is nothing to post.
func anchorDraft(prop worklist.DraftProposal, anchors map[string]map[anchor]bool, pin worklist.DraftReview) (*worklist.DraftReview, int) {
	d := &worklist.DraftReview{Repo: pin.Repo, Number: pin.Number, Commit: pin.Commit, BaseRef: pin.BaseRef, Patches: pin.Patches, Approve: prop.Approve}
	if prop.Approve {
		d.Diff = pin.Diff // the whole diff it approves, checked again before the review is created
	}
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
// not be placed, including an approval withheld because the reviews read another
// commit. It is also what later runs read of the draft.
func draftContent(d *worklist.DraftReview, excluded []string, moved, omitted int, complete bool, stale *staleReview) string {
	var b strings.Builder
	switch {
	case d.Empty() && stale != nil && stale.approved:
		// The withheld approval was all there was: its note below says why.
	case d.Empty():
		b.WriteString("**Nothing to draft.** The conversation has no agreed, actionable point to turn into a review comment yet.\n")
	case len(d.Comments) == 0 && d.Body == "":
		fmt.Fprintf(&b, "%s of commit `%s`: this conversation agrees the pull request is ready to approve, and has no comment for the author.", worklist.ApprovalLead, shortSHA(d.Commit))
		b.WriteString(" Nothing is posted until you create the pending review below, and on GitHub it stays a draft only you can see until you submit it there.\n")
	default:
		n := len(d.Comments)
		fmt.Fprintf(&b, "**Drafted %d inline review comment%s** from the points this conversation agrees on, anchored to commit `%s`.", n, plural(n), shortSHA(d.Commit))
		if d.Body != "" {
			b.WriteString(" Points no diff line carries are in the review summary.")
		}
		if d.Approve {
			b.WriteString(worklist.ApprovalAlso)
		}
		b.WriteString(" Nothing is posted until you create the pending review below, and on GitHub it stays a draft only you can see until you submit it there.\n")
		for i, c := range d.Comments {
			fmt.Fprintf(&b, "\n%d. `%s` line %d%s:\n\n%s\n", i+1, c.Path, c.Line, sideNote(c.Side), indent(c.Body))
		}
		if d.Body != "" {
			fmt.Fprintf(&b, "\n**Review summary**\n\n%s\n", d.Body)
		}
	}
	if d != nil && d.Approve {
		b.WriteString(worklist.ApprovalBlock)
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
	switch {
	case stale == nil:
	case stale.noReview:
		b.WriteString("\n**No approval drafted.** The conversation agrees on approving, but an approval is pinned to the revision a review read, and this conversation has no visible review. Use Review this pull request, then draft again.\n")
	case stale.unread:
		b.WriteString(worklist.NoApprovalUnread)
	case stale.unverifiable != "":
		fmt.Fprintf(&b, "\n**No approval drafted.** The conversation agrees on approving, but %s, so gofer cannot check that an approval covers exactly what was reviewed.\n", stale.unverifiable)
	default:
		lead := "**The latest review is out of date.** It"
		if stale.approved {
			lead = "**No approval drafted.** The conversation agrees on approving, but its latest review"
		}
		r, n := stale.reviewed, stale.now
		var why string
		switch {
		case r.Head == "" || r.Diff == "":
			why = "did not record the revision it read in full"
		case r.Head == n.Head && r.Base == n.Base:
			why = fmt.Sprintf("read commit `%s` against `%s`, which the pull request still is, but its changes are no longer the ones that review read (a related change merged into `%s` moved the comparison)", shortSHA(r.Head), r.Base, r.Base)
		default:
			why = fmt.Sprintf("read commit `%s` against `%s`, and the pull request is now commit `%s` against `%s`", shortSHA(r.Head), r.Base, shortSHA(n.Head), n.Base)
		}
		fmt.Fprintf(&b, "\n%s %s, so no approval can be drafted from it. Review the current revision, then draft again.\n", lead, why)
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
