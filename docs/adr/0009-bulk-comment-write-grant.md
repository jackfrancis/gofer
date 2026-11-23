# ADR 0009 — An opt-in write grant for posting the user's own comments

Status: proposed — implemented; not yet run against real GitHub.
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
  logs out, or when they choose "Disconnect commenting". If GitHub does not confirm
  revocation, logout's existing "unconfirmed" answer links GitHub's OAuth-app settings
  page.

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
2. Submitting renders a **preview**: the exact comment and every target, with any
   items it will skip (gone, hidden or closed since the page rendered, since bulk
   actions never act on items off the radar). Nothing is posted yet.
3. **Confirm** posts the comment to each target in order, one at a time. GitHub asks
   for serial content creation. The confirm request must be same-origin (`Origin` /
   `Sec-Fetch-Site`) in addition to SameSite=Lax.
4. The preview carries a one-time nonce. A confirm whose nonce was already used
   (double click, back button, refresh) shows the first submission's results again
   instead of posting a second time.
5. The results page shows each target as posted (with a link to the comment) or
   failed (with the reason). A 401 confirmed by GitHub's check-token deletes the
   stored grant, stops the batch, and asks the user to reconnect.

Posting is detached from the browser request, under an overall deadline, so a closed
tab does not leave a batch half-reported.

### Mention suggestions across repositories

Prow's `/assign` accepts only users who can be assigned in that repository, so
suggestions come from `GET /repos/{repo}/assignees`. The list is read with the user's
**read** credential, capped per repository, and cached in memory per repository for a
few hours. The browser asks `GET /items/mentions?repo=…` for the repositories of the
checked items.

Each suggestion is ranked by how many of those repositories can assign it. One that
cannot be assigned everywhere says so ("assignable in 2 of 3"), so a `/assign` that
will fail in some repositories is visible before posting. The typeahead
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
