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
   It is not shown while drafting again would only repeat the offered draft
   (`WorkItem.RedraftRedundant`), because the drafter would read the conversation it
   read: the draft is unconfirmed and pinned to the item's pull request, no turn after
   its request but the draft itself is visible (the drafter read only the turns before
   its request, so a correction posted while it worked counts even if answered before
   the draft landed), and no turn before the request was hidden or unhidden since the
   request was saved. Stored revisions order the two, never times: each hide or unhide
   takes the thread's next visibility revision (`Message.HiddenRev`, assigned by
   `WorkItem.SetHidden` inside the store update that saves it), and the request records
   the revision current when it was saved (`Message.ReadRev`, read inside its own
   update and kept by a retry, which restamps only the time), marking it recorded
   (`Message.ReadRevKnown`). A request saved before revisions were recorded has no
   such mark, and toggles of its time left no revision, so its draft never counts as
   redundant: such a thread keeps the button, as it did before. Runs never set any of
   these: `AppendReply` clears them on a reply. The request handler refuses a
   redundant one from a stale page. The draft's head is not compared with the item's,
   since gofer has no observation of the head it can order against the drafter's own
   read: a confirmation GitHub refuses as out of date (a new head, or a moved base)
   offers "Draft Review Comments again" on its result page, bound to that draft (its
   turn key and digest, `redraft=1`). That request is judged by the draft it names
   alone: admitted while that draft is still the one offered, whatever its redundancy,
   and refused otherwise, never falling back to the thread button's check, so a stale
   result page cannot retire a newer draft, and the "draft again" it asks for is always
   reachable.
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

**An approval is of the revision the reviews read.** A revision is the head commit,
the base branch, and the digest of the whole diff GitHub shows
(`worklist.Revision`; `github.DiffDigest`: SHA-256 over the files sorted by path, one
line each of path, previous path, status, blob SHA and the patch's SHA-256). Every
converse reply on a pull request records the head and base it read (`Message.Head`
and `Base`, kept only as a valid pair), and a review also records the digest of the
whole diff (`Message.Diff`). A review reads one settled snapshot first
(`github.PullDiff`: the pull request, every file, then the pull request again, settled
only when its head and base are unchanged) and shows the model that snapshot's head
and file list, so what the model is told it reviews and what gofer records are the
same read. The model's tools read GitHub live, so after it answers the run reads the
pull request once more: if the head or base moved while it worked, the review vouches
for no diff. A diff gofer cannot identify has no digest either
(`PullDiff.Unverifiable`): a list not read whole, a snapshot that did not settle, or a
file other than a new one that GitHub shows no patch for (a binary change), since its
entry names only the head blob and a merge base that moved under it would leave the
digest unchanged. `WorkItem.ReviewedRevision` is the revision the latest visible review
read (the visible answer to a visible review request that landed last). An approval
stands only on a draft pinned to exactly that revision, read whole
(`WorkItem.Reviewed`): `AppendReply` clears the flag otherwise, whatever the run sent.
A push changes the head, a retarget the base, and a related change merged into the
base (a stacked pull request's dependency) changes the diff while leaving both alone;
an unrelated advance of the base changes none of them, since the merge base stays put.
The drafter is told the head and base, and the revision each assistant turn read.

An approving draft carries the diff digest it approves (`DraftReview.Diff`; without a
valid one it approves nothing). When the owner keeps the approval, the web tier sends
it with the review (`ghcomment.Review.Diff`), and `PendingReview` reads every file the
pull request lists and refuses (out of date, "draft again") unless they hash to that
digest (`ghcomment.DiffDigest`, the same formula; a test holds the two equal), every
commented path is among them, and the final read of the pull request reports as many
files as were read. Both sides read files the same way, 100 a page up to GitHub's 3000,
complete only when as many were read as the pull request reports (`changed_files`),
so a comparison the workload read whole is read whole here, and one it could not is
never offered. A withheld approval clears its digest with it, so the comments beside
it stay a current, confirmable draft. So
neither a push, a retarget nor a moved merge base, before the draft or after it,
turns an approval of one diff into one of another. A pull request whose diff gofer
cannot identify gets no approval, and the reply says why.

**A stale review is always flagged, and recoverable.** The run compares the latest
review's revision with the one it read whatever the drafter answered, so a drafter
that rightly declines to approve still leaves the owner a reason: the reply says the
latest review is out of date, and which part moved ("No approval drafted" when the
drafter had proposed one). The draft reply records the revision it read too, so the
thread can tell (`WorkItem.ReviewOutdated`) and offers "Review the current revision"
(`webui.ReviewAgain`, `POST /items/review-again`): a review request appended to the
conversation, which is kept, and sent to the default model. Review all skips a
reviewed pull request and a second opinion needs another model, so without this a
single-model deployment had no way back short of clearing the conversation. The
button shows only while the latest review is older than the draft: once a review
lands after it, that review is the newer reading even if the pull request moved again
in between, so the draft is what is stale, and drafting again (offered, since a turn
now follows the draft) is the way forward, never another review of the same revision.

**The checkbox is the only source of a command.** Model-written text is stored with
every line a bot would read as a command (`/word` at the start of a line: `/lgtm`,
`/approve`, `/hold`, `/retest`, ...) made inert, its command wrapped in backticks
(`worklist.NeutralizeCommands`, in `BoundDraft`, so the preview shows the stored
text). Unchecking the approval therefore leaves no approval command in what is sent.
A path such as `/usr/bin/env` is not a command line and stays as written.

Line endings are normalized to LF first: a bot splits lines at LF, and a browser turns
a bare CR into LF before the copy button reads the text, so a CR could otherwise hide
a command from the check and reveal it in what is pasted. The text stored, previewed
and copied is the normalized one.

A GitHub `suggestion` block is applied verbatim, so its text is never altered while it
stays a suggestion: one holding a command-shaped line (a `.gitignore`'s `/dist`, say)
becomes a plain `text` code block with a note after it
(`worklist.SuggestionNote`), so the author reads the point but cannot apply edited
code. A suggestion with no such line is kept exactly. Blocks are found as Markdown
parses them (goldmark), so a suggestion nested in a list, its fence indented past
three spaces, is found too, and the note is indented to stay in its list item. The
rewrite is a fixed point.

Drafts stored before a rule existed are held to it too: `DraftReview.Current` reports
whether `BoundDraft` would store the draft unchanged today. One that is not is not
offered (the thread says to draft again, and drafting again is never counted as
redundant over it) and a confirmation of it is refused (`OfferedDraft`), so an older
draft can never send text today's rules would change. A draft already confirmed still
shows its receipt.

**GitHub's submit dialog can drop the body.** GitHub's "Finish your review" dialog can
open with an empty summary, and submitting it replaces the body gofer created, while
inline comments survive: the review looks submitted, and the approval commands never
reach Prow. gofer keeps the review pending, records the body it is about to send on
the receipt (`DraftReceipt.Body`) in the claim itself, before it contacts GitHub, and
shows it, with a copy button and that warning, on the result page, on a replay, and in
the thread's receipt, to paste into the dialog's summary before submitting. Saving it
in the claim means a confirmation cut off after GitHub created the review (still
`creating`) shows it too. The same applies to any summary.

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
