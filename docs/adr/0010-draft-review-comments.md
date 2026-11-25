# ADR 0010 — Draft review comments as a pending GitHub review

Status: proposed — implemented; not yet run against real GitHub.
Date: 2026-11-24
Amends: ADR 0009 ("Posting happens in the web tier, never in a run").

## Context

A pull request's Discuss thread can hold a review, several reviews by different
models (the review panel), and the user's back-and-forth with them. Users want to turn
what that conversation agreed on into review comments on GitHub without copying them
by hand, while keeping the final say: nothing should reach the pull request's author
until the user decides.

GitHub's REST API creates a review with no `event` as **PENDING**: a draft visible only
to its author, who can edit, submit or discard it on GitHub. One pending review per user
per pull request is allowed, and the REST API cannot add comments to an existing one.

ADR 0009 introduced the opt-in write grant (`github-write`) for the bulk comment, under
the rule that it posts only the user's own comment, with no model involved. A drafted
review's text is written by a model, so this feature needs that rule amended.

## Decision

### The model drafts in a run; the web tier creates the review

1. **Request.** At the bottom of a pull request's thread, "Draft Review Comments" posts
   a user turn of kind `draft_review_request` and dispatches a `github-converse` run by
   the default model, as any Discuss turn. It is offered only when commenting is
   configured, on a pull request on the radar with a completed review and no reply in
   flight; without a connected write grant the thread links to connecting it instead.
2. **Draft (the run, read-only).** The workload (`internal/agent/draft.go`) reads the
   pull request's head commit, base branch and changed files with their patches
   (`github.Client.PullDiff`, public-only like every workload call). It annotates each
   patch with exactly the anchors GitHub accepts: `[R<n>]` for an added or unchanged
   context line, `[L<n>]` for a removed one (a context line's base number is no
   anchor). The annotated diff is bounded (256 KiB), notices for files without a patch
   included. The drafter (`llm.ReviewDrafter`) sees the visible conversation (never a
   hidden turn) and the diff, both framed as data.
3. **Anchoring is checked, not trusted.** A comment stays inline only if its file, line
   and side are an anchor the run showed the model; any other moves into the review
   summary with its location, so an agreed point is never lost to a bad anchor. The
   draft is **pinned** to what it was anchored to: the repository and pull request
   number, the head commit, the base branch, and a digest of each commented file's
   patch (`DraftReview.Patches`, SHA-256 of the patch text GitHub lists). A comment
   on a file with no recorded digest is not stored.
4. **Reply.** The run replies with a readable account (the numbered comments, the
   summary, and the points left out for lack of consensus) and a structured
   `worklist.DraftReview` on the reply (`Message.Draft`). `worklist.AppendReply`
   bounds it (`BoundDraft`: at most 30 comments, 4 KiB each, an 8 KiB summary, a valid
   pin) and drops a draft from a reply to any other kind of turn, or one pinned to a
   pull request other than the item's. Runs never read a stored draft back: neither the
   bounded reads (`ThreadWindow`) nor the whole-list read kept for older runtimes
   (`RedactDrafts`) carry it; later turns see its text.
5. **Offer.** The thread offers only the draft of the **newest** draft request
   (`WorkItem.LatestDraft`): a redraft that found nothing retires the draft before it,
   and a late reply to an older request never supersedes a newer one. It is offered
   only while the item still names the pull request the draft is pinned to. Beside each
   checkbox the page shows the exact stored text it sends (each comment's body, the
   summary) and the pull request, commit and base branch it goes to; the reply's prose
   above is only an account of it.
6. **Confirm (the web tier).** The user unchecks what they do not want and chooses
   "Create pending review on GitHub". The handler (`webui.CreateDraftReview`) requires
   a same-origin request (as ADR 0009's confirm) and reads the draft **from the
   store**: the form only names the turn, the draft's digest (which covers the pin) and
   the positions to keep. A draft that is not the one offered, whose digest differs, or
   whose pin no longer matches the item is refused, so selection can only leave
   comments out, never add or change one or move the destination.
7. **Claim.** Before gofer asks GitHub for anything, the confirmation is claimed in the
   store (`worklist.ClaimDraft`: a `DraftReceipt` in state `creating` on the draft
   turn, written in the same transaction that re-checks the draft). It is then settled
   to `created` (with the review's link) or `unknown` (the answer was lost), or
   released when GitHub certainly created nothing (`SettleDraft`), so the owner can try
   again. Settling has its own short deadline, apart from the request's, which the
   GitHub calls may have used up: a release must land even when nothing was sent
   because time ran out. A confirmation of a draft that already has a receipt sends
   nothing to GitHub and shows what the earlier one did; the thread shows the receipt
   instead of the form. A POST answered with any server error (5xx) is `unknown`, never
   released: a 500 does not say the write was rolled back.
8. **Create.** `comment.Service.PendingReview` creates the review with the write grant
   through `internal/ghcomment` (four new calls, public repositories only, no
   redirects followed). It reads the reviews (and refuses if the user already has a
   pending one). It pages the pull request's files until it has every commented file,
   and refuses if one left the diff, lost its patch, or has a patch whose digest
   differs. Then, last, it reads the pull request: it must be open, with its head still
   the draft's commit and its base still the draft's branch. Then it sends
   `POST /repos/{repo}/pulls/{n}/reviews` with `commit_id`, the summary and the
   comments and **no `event`**. GitHub treats `commit_id` as an anchor, not a
   precondition (it accepts an older commit), so these checks are gofer's, made as close
   to the write as the API allows. gofer never submits a review.

### Consensus

The drafter is told to draft only points the conversation agrees on: a concrete,
actionable point no turn disputes, or whose dispute was settled (conceded, resolved
with evidence, or decided by the user). The user's own messages decide: an endorsed
point is agreed, a rejected one is never drafted. An unsettled disagreement is left
out and listed under "Left out — no consensus yet". A synthesis verdict of agreement
is evidence, but each point is judged on its own. With a single undisputed review,
its actionable points are agreed. This is a model judgment; the user's review of the
exact text before anything is created is the check on it.

### Approval

When the conversation agrees the pull request is ready to approve as it stands, the
drafter says so (`"approve": true`; `DraftProposal.Approve`), and the draft is marked
`DraftReview.Approve`. The review then carries `worklist.ApprovalCommands`,
`/lgtm` and `/approve` each on a line of their own, as a general comment: in the
review body, after the summary (`worklist.ReviewBody`), never on a file and line.
Those are Prow's commands, which its lgtm and approve plugins read from a submitted
review's body; on a repository without Prow they are only text.

The bar is the same as for a comment: the reviews (or the settled outcome of their
disagreement) recommend approving, no agreed point asks the author for a change before
it merges, nothing about whether it should merge is unsettled, and the user's own
messages decide. Non-blocking nits may still be drafted beside an approval; when in
doubt, the drafter does not approve. An approving draft with no comment and no summary
is still a draft (`DraftReview.Empty` counts the approval), so an approved pull request
no longer answers "nothing to draft".

The approval is a flag, not text in `Body`: the summary's bound (8 KiB, cut at the end)
can never cut the commands, and the thread offers the approval as its own checkbox,
showing exactly the text it sends, so the owner can keep the comments and leave the
approval out, or the reverse. The flag is part of the draft, so its `Digest` covers it.
The review is still created pending: the commands take effect only when the owner
submits it on GitHub.

**An approval is of the commit the reviews read.** Every converse reply on a pull
request records the head commit its run read (`Message.Head`, a full commit id or
nothing). `WorkItem.ReviewedHead` is the head the latest visible review read (the
visible answer to a visible review request that landed last). An approval stands only
on a draft pinned to that commit: the run withholds one when the head moved since, or
the review recorded none, and says so ("review again"), and `AppendReply` clears the
flag on the same rule, whatever the run sent. The drafter is also told the head and
the commit each assistant turn read. So a push between the review and the draft never
turns an approval of the old commit into one of the new; the draft's own pin (head
checked against GitHub before creating) covers a push after it.

**The checkbox is the only source of a command.** Model-written text is stored with
every line a bot would read as a command (`/word` at the start of a line: `/lgtm`,
`/approve`, `/hold`, `/retest`, ...) made inert, its command wrapped in backticks
(`worklist.NeutralizeCommands`, in `BoundDraft`, so the preview shows the stored
text). Unchecking the approval therefore leaves no approval command in what is sent.
A path such as `/usr/bin/env` is not a command line and stays as written.

**GitHub's submit dialog can drop the body.** GitHub's "Finish your review" dialog can
open with an empty summary, and submitting it replaces the body gofer created, while
inline comments survive: the review looks submitted, and the approval commands never
reach Prow. gofer keeps the review pending, records the body it sent on the receipt
(`DraftReceipt.Body`), and shows it, with a copy button and that warning, on the
result page, on a replay, and in the thread's receipt, to paste into the dialog's
summary before submitting. The same applies to any summary.

### Once per draft

ADR 0009's confirm consumes a one-time nonce held in memory. GitHub's
one-pending-review rule alone is not enough here: it protects only while the first
review is still pending, so a replayed confirmation after the user submitted or
discarded the review on GitHub would create another. The receipt (step 7) makes a draft
confirmable once, durably: it lives on the stored turn, so it survives a restart, and a
replay finds it. A claim whose request was cut off (the process died between claim and
settle) stays `creating`; after the request deadline the thread reads it as possibly
created, never as not created. To create another review, the user drafts again.

### Revalidating the hunks, not pinning the base commit

The diff is taken against the merge base of the pull request's head and its base
branch. With the head unchanged, that merge base moves when the pull request is
retargeted, or when a related change (one sharing history with this pull request) is
merged into the base branch; either can change what a line number means, or drop a
line from the diff. The base branch's tip (`base.sha`) moves whenever anything at all
is merged into it, which usually leaves the diff unchanged, so pinning it would refuse
nearly every confirmation on a busy repository; and the API offers the merge base
itself only through the compare endpoint, whose answer carries the whole comparison.

So gofer pins the base branch (a retarget refuses outright) and checks the hunks the
review actually touches: the patch of every commented file, compared by digest with
the one the comments were anchored to. A related merge that changed those patches
refuses the review; one that left them alone, like any unrelated advance of the base,
does not.

## Consequences

- ADR 0009's rule becomes: **the write grant never leaves the web tier, and posts only
  what the user confirmed: their own comment, or the text of a draft review they
  selected, created pending.** A model's text can now reach GitHub under the user's
  name, but only as a pending review only they can see, after they saw and chose that
  exact text, and only through the web tier. The workload still holds read-only tokens
  and never the write grant.
- `internal/ghcomment` makes five kinds of call instead of three.
- A draft is as good as the model's reading of the conversation. The reply lists what
  was left out, so a missed disagreement is visible before anything is created.
- Comments are anchored to the head, base branch and patches the run read. After a
  push, a retarget, or a merge into the base that changed a commented file's patch,
  the review is refused ("draft again"), never anchored to moved lines.
- Each draft creates at most one review; starting over means drafting again.
- Long threads are read through the bounded thread window (`MaxItemBytes`), as every
  converse run reads them; the oldest turns of a very long thread may be left out.
- The web and runtime images must ship together: an older runtime answers a draft
  request as an ordinary Discuss turn, with no draft.
