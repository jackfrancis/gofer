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
   pull request's head commit and changed files with their patches
   (`github.Client.PullDiff`, public-only like every workload call), annotates each
   patch with the line numbers a comment may anchor to (`[R<n>]` for the pull request's
   version, `[L<n>]` for the base), and asks the drafter (`llm.ReviewDrafter`) for the
   agreed comments. The drafter sees the visible conversation (never a hidden turn) and
   the diff, both framed as data.
3. **Anchoring is checked, not trusted.** A comment stays inline only if its file and
   line are in the diff the run read; any other moves into the review summary with its
   location, so an agreed point is never lost to a bad anchor. The draft records the
   head commit it was anchored to.
4. **Reply.** The run replies with a readable account (the numbered comments, the
   summary, and the points left out for lack of consensus) and a structured
   `worklist.DraftReview` on the reply (`Message.Draft`). `worklist.AppendReply`
   bounds it (`BoundDraft`: at most 30 comments, 4 KiB each, an 8 KiB summary, a full
   commit id) and drops a draft from a reply to any other kind of turn. Runs never read
   a stored draft back (`ThreadWindow` leaves it out); later turns see its text.
5. **Confirm (the web tier).** Under the thread's newest draft, the user unchecks what
   they do not want and chooses "Create pending review on GitHub". The handler
   (`webui.CreateDraftReview`) requires a same-origin request (as ADR 0009's confirm)
   and reads the draft **from the store**: the form only names the turn, the draft's
   digest and the positions to keep. A turn that is gone, hidden, no longer the newest
   draft, or whose draft does not match the digest is refused, so selection can only
   leave comments out, never add or change one.
6. **Create.** `comment.Service.PendingReview` creates the review with the write grant
   through `internal/ghcomment` (three new calls, public repositories only, no
   redirects followed): read the pull request (it must be open, and its head must still
   be the draft's commit, since a push moves lines), read its reviews (refuse if the
   user already has a pending one), then `POST /repos/{repo}/pulls/{n}/reviews` with
   `commit_id`, the summary and the comments and **no `event`**. gofer never submits a
   review.

### Consensus

The drafter is told to draft only points the conversation agrees on: a concrete,
actionable point no turn disputes, or whose dispute was settled (conceded, resolved
with evidence, or decided by the user). The user's own messages decide: an endorsed
point is agreed, a rejected one is never drafted. An unsettled disagreement is left
out and listed under "Left out — no consensus yet". A synthesis verdict of agreement
is evidence, but each point is judged on its own. With a single undisputed review,
its actionable points are agreed. This is a model judgment; the user's review of the
draft before anything is created is the check on it.

### No preview nonce

ADR 0009's confirm uses a one-time nonce so a resubmit cannot post a comment twice. A
pending review does not need one: the content comes from an immutable stored reply,
not the form, and GitHub's one-pending-review rule, checked before creating, makes a
resubmit fail rather than duplicate. A POST whose answer was lost is reported as
unknown, and the user is told to check the pull request before trying again (a retry
then finds the pending review and refuses).

## Consequences

- ADR 0009's rule becomes: **the write grant never leaves the web tier, and posts only
  what the user confirmed: their own comment, or a draft review they selected, created
  pending.** A model's text can now reach GitHub under the user's name, but only as a
  pending review only they can see, after they chose it, and only through the web
  tier. The workload still holds read-only tokens and never the write grant.
- `internal/ghcomment` makes four kinds of call instead of three.
- A draft is as good as the model's reading of the conversation. The reply lists what
  was left out, so a missed disagreement is visible before anything is created.
- Comments are anchored to the head the run read. After a push the review is refused
  ("draft again"), never anchored to moved lines.
- Long threads are read through the bounded thread window (`MaxItemBytes`), as every
  converse run reads them; the oldest turns of a very long thread may be left out.
- The web and runtime images must ship together: an older runtime answers a draft
  request as an ordinary Discuss turn, with no draft.
