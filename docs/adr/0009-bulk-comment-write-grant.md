# ADR 0009 — An opt-in write grant for posting the user's own comments

Status: proposed — implemented; not yet run against real GitHub.
Amended by: ADR 0010 (draft review comments created as a pending review); ADR 0011 (the write grant can also be a classic personal access token); the bulk comment flow below (November 2026: Post in one step on the radar, replacing the preview and confirm pages).
Date: 2026-11-23

## Context

Users want to select several items on the radar and post the same comment to every
one of them, typically a Prow command such as `/assign @someone` or `/lgtm`. With an
`@`, they want suggestions of whom to mention.

gofer is read-only toward GitHub by design (AGENTS.md: "Source scopes are READ"). ADR
0007 moved sign-in to a read-only GitHub App so that no token gofer holds can write,
because that token goes to a workload that runs a model over untrusted content.

That app cannot simply be given write permissions. A GitHub App's user access token
reads public repositories in any organisation (ADR 0007's probe). It can *write* only
where the app is installed, and the repositories gofer's users work in (for example
kubernetes-sigs/*) will not install it.

## Decision

### A second, separate, opt-in grant

A deployment that wants commenting registers a second **classic OAuth app** and sets
`GITHUB_WRITE_CLIENT_ID` / `GITHUB_WRITE_CLIENT_SECRET`. Each user connects it from the
radar ("Connect GitHub to comment"). It asks for the `public_repo` scope, the narrowest
classic scope that can comment on a public repository.

- **It is a different client from sign-in.** Config refuses
  `GITHUB_WRITE_CLIENT_ID == GITHUB_CLIENT_ID`. A classic OAuth grant accumulates
  scopes per (user, app), so reusing the sign-in app would eventually put
  `public_repo` on the token the broker vends to runs.
- **It is stored apart.** It goes into the vault as provider `github-write`, through
  the same credential lock, versioning, and pending-logout ledger as sign-in
  (`internal/auth/credential.go`, `internal/auth/logout.go`).
- **No run can obtain it.** `auth.Handler.Credential`, the broker's only source,
  answers `vault.ErrNotFound` for a grant provider, and the broker
  (`api.CredentialHandler`) also refuses `github-write` by name. Two independent
  checks, so neither can be removed by accident.
- **The connect flow cannot sign anyone in.** It runs under its own cookie
  (`gofer_grant`, path `/auth/github-write`), so the user's session survives it. It
  requires a signed-in GitHub session. The callback refuses, and revokes, a grant
  from a GitHub account other than the session's (`github:<id>` must match), or from
  an account `ALLOWED_GITHUB_LOGINS` no longer admits. The generic `/auth/{provider}`
  routes and the sign-in page never offer it.
- **Logout revokes it too.** The grant is revoked at GitHub and deleted when the user
  logs out, or when they choose "Disconnect commenting". Logout records the write
  grant's pending revocation in the **same commit** as the session's end and the
  sign-in credential's marker (`MarkLogoutEndingSession` with both providers), so a
  restart right after logout still revokes it. A grant that cannot be read at that
  point is recorded anyway (fail closed), and a commit failure changes nothing for
  either credential (503). If GitHub does not confirm revocation, logout's existing
  "unconfirmed" answer links GitHub's OAuth-app settings page.
- **Logout does not depend on configuration.** A write grant stored while commenting
  was configured is deleted at logout even after the write client is removed, with
  its revocation reported unconfirmed, so configuring the client again never revives
  it.
- **A connection in flight cannot undo a logout.** The flow is bound to the session
  that began it (its hashed id), not only to the user, so a session signed in again
  in another tab cannot finish it. The callback stores the grant only after
  re-checking that same session, under the write grant's marker lock.
  Logout holds the same lock while it decides what to revoke and commits the
  session's end. Either the grant is stored first, and logout sees and revokes it, or
  the session has ended first, and the callback revokes the token it was issued and
  stores nothing.
- **Storing it is strict.** Unlike sign-in, where a vault failure is logged and login
  continues, a write grant gofer could not save is revoked, and the user is told,
  including when GitHub did not confirm the revocation. A grant gofer has not saved is
  one it could not revoke at logout. Every way out of the callback after the code
  exchange (profile unreadable, wrong account, refused login, save failure, session
  ended) revokes the token it was issued, detached from the browser and on its own
  deadline. That cleanup revokes **that token only**
  (`DELETE /applications/{client_id}/token`), never the whole authorization, so a
  connection that succeeded meanwhile in another tab keeps working.

### Posting happens in the web tier, never in a run

The comment is posted by the web tier (`internal/comment` over `internal/ghcomment`),
synchronously, from the user's own submission. No model is involved, and the write
token never leaves the process that already holds the vault. This amends AGENTS.md's
"gofer core packages import no provider client": the web tier now has one small,
purpose-built GitHub client. It makes exactly three kinds of call:

- `GET /repos/{repo}`, to check the repository is public;
- `POST /repos/{repo}/issues/{n}/comments`;
- `GET /repos/{repo}/assignees`.

The client keeps the workload's guard rules (ADR 0007, "public-only guard"):

- every call first checks `"private": false` and uses the canonical name GitHub
  returns;
- redirects are never followed: it calls the transport directly, so no `Location`
  reaches an error;
- the token is never logged or put in an error.

### Bulk comment flow

1. Each item on the radar gets a checkbox. The compose box takes the comment
   (at most 16 KiB) for up to 50 selected items.
2. **Post** posts at once; there is no preview step. The radar names the account it
   posts as and says a posted comment cannot be taken back. Each checkbox carries
   its item and the repository and number its row showed. The comment and selection
   are checked, then resolved against the owner's stored items at that moment: only
   items on the radar take the comment; any that are gone, hidden or closed since
   the page rendered are skipped and listed (bulk actions never act on items off the
   radar), and so is one whose item now names another repository or number (a
   write-back can change an item's GitHub fields under the same ID): the user checked
   a destination, so the comment is never redirected. Posts go in order, one at a time, at least a second apart: GitHub asks
   for serial content creation, spaced out.
3. Each radar render mints a **one-time form key**: this process's epoch (random at
   startup), the time it was issued, random bytes, and a MAC (under a per-process
   secret) binding them to the account the radar was rendered for. A form rendered
   for one account never posts as another (the browser signed in as someone else in
   another tab): it posts nothing and says so, and none of its text or selection is
   shown to the account now signed in. Only the canonical (lowercase hex) spelling of
   a key is accepted, so one key never names two batches. The first submission of a key
   claims it for its owner, atomically, and runs the batch; any later submission of
   the same key (a double click, a back-button resubmit) shows that batch's results
   and posts nothing, to its owner, with or without a live grant: only starting a
   batch needs one, so a user whose grant died mid-batch can still see which
   comments landed. A used key is remembered (in memory) until it expires, 12 hours
   after it was issued, and results at least an hour (an expired key of this account
   still shows its batch's results while they are kept). A key gofer cannot vouch for
   posts nothing, and the radar says gofer cannot tell what an earlier submission of
   that form did, and to check the items on GitHub before posting again. Only an
   expired key whose MAC verifies against this account gets its comment and
   selection offered again; a key whose account cannot be checked (from another
   epoch, i.e. before a web-pod restart; malformed; missing) or that names another
   account shows none of the form's text or selection, which may be someone else's.
   The request-size cap allows the full comment percent-encoded and 50 selections in
   long-named repositories; the decoded comment and target count are checked apart. Batches are
   bounded in all; only one that posted nothing is dropped early, and past the bound
   gofer refuses to post rather than forget a used key.
4. The request must be same-origin, in addition to SameSite=Lax. A browser
   that sends fetch metadata decides (`Sec-Fetch-Site: same-origin`): gofer's
   `Referrer-Policy: no-referrer` makes a form POST send `Origin: null`. Without
   fetch metadata, an `Origin` must name gofer's host; `null` is refused. A radar that
   carries the comment form sets `Referrer-Policy: same-origin`, so Post sends a
   real origin.
5. The request waits for the batch, then redirects to the radar (`/?comment=<key>`),
   which shows each target as posted (with a link to the comment, when
   GitHub's answer arrived whole), unknown, or not posted (with the reason). A 201 is
   posted even if its body was lost. A POST whose answer was lost after the request
   was written, or that got a server error (any 5xx, amended by ADR 0010: a 500 does not say the write was rolled back either), is **unknown**: it may
   have been posted, and the radar says to check before posting again. A batch that
   posted nothing (a refused form, no grant, the worklist unreadable, nothing
   eligible, every target refused) reopens the comment box with the same comment and
   selection, the reason, and each target's own reason (a rate limit says when to try
   again). A row is checked again only while it still names the repository and number
   it was checked for. The results show whether or not commenting is still connected:
   a grant lost mid-batch leaves the connect prompt and the receipt of what landed.
   An unsent comment is kept for after reconnecting (in memory, the newest per user,
   for two hours) in two cases only: a verified, unused form of this account posted
   without a grant, kept with its form's key; and a batch that put nothing on GitHub
   and left the grant gone (missing, or a 401 GitHub confirmed, which deletes it),
   kept without one, since its key is used. A partial or unknown batch is never kept.
   It comes back in the box on the next plain radar once commenting is connected (a
   page showing an outcome leaves it kept), to post with an explicit Post, under its
   form's key when it has one: the original tab and the restored form then share one
   batch, and a draft whose form posted meanwhile is dropped. Posting any comment
   drops the kept one. If the worklist cannot be read on the way back, the error
   page's Try again keeps the results URL. A 401 confirmed
   by GitHub's check-token deletes the stored grant. A rate limit (429, or 403 with
   GitHub's rate-limit signals) stops the batch, says when to try again, and leaves
   the rest unattempted. The grant is read again before every post after the first,
   so a disconnect or logout mid-batch stops it even before GitHub confirms the
   revocation, and so does a grant of another version (disconnected and connected
   again): a batch never continues under a grant it did not start with.

Posting is detached from the browser request, under an overall deadline, so a closed
tab does not leave a batch half-reported.

### Mention suggestions across repositories

Prow's `/assign` accepts only users who can be assigned in that repository, so
suggestions come from `GET /repos/{repo}/assignees`. The list is read with the user's
**read** credential, capped per repository, and cached in memory per repository for a
few hours. The browser asks `GET /items/mentions?repo=…` for the repositories of the
checked items.

Each suggestion is ranked by how many of those repositories can assign it, and every
count is against **all** the selected repositories, not only those that answered.
One that cannot be assigned everywhere says so ("assignable in 2 of 3"), so a
`/assign` that will fail in some repositories is visible before posting. Repositories
gofer could not check (a list that failed, a list cut at its bound that does not name
the login, or past the 20-repository lookup cap) are shown as unknown ("assignable in
1 of 3; 2 not checked"), never as assignable. The typeahead
(`static/comment.js`) is progressive enhancement: without script the box is a plain
textarea and the flow is unchanged.

## Consequences

- AGENTS.md's least-privilege invariant becomes: **tokens vended to runs are
  read-only; the only write-capable credential is the opt-in `github-write` grant, and
  it never leaves the web tier.**
- `public_repo` can write to every public repository the user can push to, not just
  the ones they comment on. The grant is opt-in per user, revocable from the radar,
  and revoked on logout. A web-tier compromise could use it; that was already true of
  every token in the vault, but now one of them can write.
- With the write client unset, nothing changes: no checkboxes, no compose box, no new
  routes do anything.
- `reload.js` does not reload the radar while items are checked or the comment box
  holds text, so a refresh never discards a selection.
