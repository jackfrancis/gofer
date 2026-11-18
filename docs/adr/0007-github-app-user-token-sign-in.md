# ADR 0007 — Sign in through a read-only GitHub App instead of an OAuth app

Status: proposed — not implemented; revised through three rounds of review. The
question it rests on is verified: a GitHub App user access token with only read
permissions gets the job log's 302 in an organisation where the app is not installed
(see "The question").
Date: 2026-11-18

## Context

ADR 0006 gave the conversation GitHub Actions log tools (`gha_*`). In prod, GitHub
reads gofer's token the job's metadata but refuses the log download: "Must have admin
rights to Repository." gofer signs in through a GitHub OAuth app that requests
`read:user user:email`. The tokens tried so far on one public job
(`kubernetes-sigs/cluster-api-addon-provider-helm`, job 134438618532), against
`GET /repos/{repo}/actions/jobs/{job_id}/logs`:

| Token | Result |
| --- | --- |
| none | 403, but the address had used up its anonymous rate limit, so this says nothing |
| classic, `read:user` only | 403, "Must have admin rights to Repository." |
| classic, `public_repo` only | 302 |
| fine-grained personal token, "Public repositories (read-only)" | 302 |
| GitHub App user token, read-only permissions, app installed nowhere | **302** (below) |

`public_repo` reads the log, but it also grants write access to every public
repository the user can push to: issues, comments, code. AGENTS.md's invariant is
that source scopes are READ, and the token goes to a workload that runs a model over
untrusted content. A classic OAuth scope cannot be both enough and read-only.

A fine-grained personal token is read-only and enough, but it is a standing secret
each user would have to paste in, and its expiry is manual. A GitHub App's *user*
access token (`ghu_…`) has the same permission model as a fine-grained token, comes
from the same browser sign-in gofer already runs, and expires after 8 hours.

## The question

Can a GitHub App user access token with no write permissions download the job log of
a public repository in an organisation where the app is NOT installed?

GitHub's documentation does not settle it. "Authenticating with a GitHub App on behalf
of a user" says the app "can only access resources in an account where it is
installed". The permissions reference says the permissions it lists "are required to
access private resources" and that "some endpoints can also be used to access public
resources without these permissions". The log endpoint's own page says only "Anyone
with read access to the repository can use this endpoint", and asks for the `repo`
scope only for private repositories (a classic `read:user` token is refused anyway).

**Probe, 2026-11-18.** The repository owner registered a throwaway GitHub App on their
personal account:

- **Permissions:** read-only, Repository: Actions, Checks, Commit statuses, Contents,
  Issues, Pull requests, Metadata. Account: Email addresses.
- **Settings:** expiring user tokens on, device flow on, no webhook, private.
- **Installed:** nowhere, so `GET /user/installations` returned `total_count=0`.

They authorised it by the device flow. A script kept the token in memory, made GET
requests only and followed no redirect. The token was `ghu_`, with
`expires_in=28800`, a refresh token, and `scope: ''`. The app has since been deleted.

| Request (all on the repository above unless noted) | Status | `X-Accepted-GitHub-Permissions` |
| --- | --- | --- |
| `GET /user` | 200 | `allows_permissionless_access=true` |
| `GET …/actions/jobs/134438618532` | 200 | `actions=read` |
| **`GET …/actions/jobs/134438618532/logs`** | **302** to a blob host | — |
| **`GET …/actions/jobs/134438618532/steps/4/logs`** | **302** to a blob host | — |
| `GET …/actions/runs/{run}/jobs` | 200 | `actions=read` |
| `GET …/contents/README.md` | 200 | `contents=read` |
| `GET …/tarball/main` | 302 to codeload | — |
| `GET …/pulls/618`, `/files`, `/reviews`, `/comments` | 200 | `pull_requests=read` (`; contents=read` on the PR) |
| `GET …/issues/618`, `/comments`, `/timeline` | 200 | `issues=read` (`; pull_requests=read` on comments, timeline) |
| `GET …/commits/{sha}/check-runs` | 200 | `checks=read` |
| `GET …/commits/{sha}/status` | 200 | `statuses=read` |
| `GET /search/issues?q=is:open involves:@me archived:false` | **422** "Query must include 'is:issue' or 'is:pull-request'" | `allows_permissionless_access=true` |
| `GET /search/issues?q=is:pr is:open review-requested:@me archived:false` | 200 | `allows_permissionless_access=true` |

**Answer: yes.** A read-only GitHub App user token, from an app installed nowhere, gets
the 302 for both the job log and the step log, in the same organisation and job where
gofer's `read:user` token is refused. Every other read gofer's tools make succeeded.
Only one thing broke: an issue search without a type qualifier, which GitHub documents
(below).

## Decision (proposed)

Replace the GitHub OAuth app with a GitHub App that gofer uses only to sign users in
and get their user access tokens. gofer never authenticates as the app or an
installation, so it needs no app private key. Do not generate one.

### The app's permissions

All read-only, the least that today's tools need. A user token holds only what both
the app and the user have, so it stays read-only whatever the user's own rights.

| Permission | Used by | Endpoints (`internal/github`, `internal/auth`) |
| --- | --- | --- |
| *(none: permissionless)* | sign-in, ingest, enrich, converse (viewer login), `github_search` | `GET /user`, `GET /search/issues` |
| Email addresses (account) | sign-in, when the profile hides the email | `GET /user/emails` |
| Metadata | required by GitHub with any repository permission; the public-only visibility gate | `GET /repos/{repo}` |
| Contents | `github_read_file`; `repo_checkout`/`repo_search`/`repo_list`/`repo_read`; `github_get_pull_request` | `…/contents/{path}`, `…/tarball/{ref}`, `…/pulls/{n}` |
| Issues | `github_get_issue`; enrich (`ItemActivity`); Discussion context | `…/issues/{n}`, `…/issues/{n}/comments`, `…/issues/{n}/timeline` |
| Pull requests | `github_get_pull_request`; enrich; Discussion context | `…/pulls/{n}`, `/files`, `/reviews`, `/comments` |
| Checks | `github_get_ci_status`; `gha_job_summary` by PR number | `…/commits/{sha}/check-runs` |
| Commit statuses | `github_get_ci_status` | `…/commits/{sha}/status` |
| Actions | `gha_job_summary`, `gha_search`, `gha_read` | `…/actions/jobs/{id}`, `…/actions/runs/{id}[/attempts/{n}]/jobs`, `…/jobs/{id}/logs`, `…/jobs/{id}/steps/{n}/logs` |

The Prow tools (`ci_*`) read public GCS anonymously and use no GitHub permission.

Registration: callback URL `<BASE_URL>/auth/github/callback`, expiring user tokens on,
device flow off, no webhook, "Request user authorization (OAuth) during installation"
off. The client ID and secret are deployment parameters, like today's OAuth app
credentials, and never go in this repository.

**Public or private.** GitHub says a private app "can only be installed on the account
that owns the app. Only members of the organization that owns it can authorize it."
If anyone besides the owning account signs in to gofer, register the app as public, or
under an organisation whose members are gofer's users. Public only means anyone can
authorise or install the app. `ALLOWED_GITHUB_LOGINS` still decides who gets a
session, and the token of a refused account is revoked, as today. An installation
grants gofer nothing it uses, but it does let that user's token read the installed
account's private repositories, still read-only. An org owner, not gofer's operator,
decides that, so gofer has to enforce its own boundary (next section).

### Private repositories: public only, unless the deployment opts in

Today's `read:user` token cannot read private repositories, so gofer has never had
to hold private data. A user token can, as soon as anyone installs the app on an
account the user belongs to. Without a guard, that user's next ingest would pick up
private issues and PRs: gofer searches with no visibility filter and persists what it
finds. The ranking job would then send their repository names and titles to the
configured model, unprompted. Processing private data is a deployment decision, not a
side effect of someone else's installation.

- **New setting `GITHUB_PRIVATE_REPOS`, default off.** The web tier reads it from its
  own configuration; the user and the model cannot change it. When it is on, every
  run the web tier dispatches gets an extra scope, `github:private`, in its RunSpec.
  AEI mints that scope into the run credential, so every agent-plane request shows
  the web tier whether its run was allowed private access. It needs no record of its
  own and survives a restart.
  - **AEI must allow the scope first.** gofer's AgentApp caps the scopes a run may
    request (`identity.maxScopes` in `deploy/k8s/overlays/dev/agentapp.yaml`, today
    `signals:read` and `metadata:write`). AEI answers a request outside that ceiling
    with 403 before it creates the run (`boundScopes`, in AEI's
    `controller/internal/dispatch/dispatch.go`). Turning the setting on without
    widening the ceiling would break every dispatch.
  - So `github:private` goes into `maxScopes`, in the repository's AgentApp and in
    every cluster's applied copy, before the setting is turned on. The ceiling grants
    nothing by itself: gofer still asks for the scope only when the setting is on.
  - Within the ceiling, AEI copies the requested scopes into the AgentRun and mints
    them unchanged (`agentrun_controller.go`). Turning the setting on still needs a
    check that a dispatched run's credential actually carries `github:private`.
- **Off: gofer enforces public-only at two points.** The live check is in the
  workload's GitHub client (`internal/github`), so every tool and job inherits it.
  The stored check is in the web tier, for model inputs taken from what gofer has
  already kept.
- **Live: search.** Every search, ingest's and `github_search`'s alike, gets
  `is:public` appended.
- **Live: a visibility gate before every per-repository call.**
  - `GET /repos/{repo}` (Metadata: read) must answer `"private": false`. The gate
    reads the final response, after any rename redirect, and returns the
    repository's canonical `full_name` and ID. Later calls use that name, and the
    answer is cached per run.
  - Gated calls: contents, tarball, issues, pulls, checks, statuses, Actions jobs and
    logs.
  - A private repository answers "gofer is configured for public repositories only",
    and nothing from it reaches the model or the sink.
- **Live: redirects are checked by destination.** GitHub answers a transferred issue
  with a redirect to its new repository, which may be private, and today's client
  follows API redirects on its own. In public-only mode the GitHub client follows no
  API-host redirect itself (`CheckRedirect` stops it). It resolves where the redirect
  points (`/repos/{owner}/{repo}/…` or `/repositories/{id}/…`), runs the gate on that
  repository, and follows only if it is public and still the gated repository or
  another public one. Anything else is refused like a private repository.
  - Redirects off the API host are unchanged. The tarball's to codeload and the logs'
    to the blob host carry the gated repository's own content, and ADR 0006's blob
    client already handles them.
  - As a backstop, an issue or PR body whose `repository_url` names a repository
    other than the gated one is dropped unless that repository passes the gate.
- **Items and verdicts are bound to the repository's numeric ID, not its name.**
  - A repository name is not an identity. A private repository can be renamed and a
    new public one created under its old name; GitHub then stops redirecting the old
    name. Keyed by name, the new repository's `public` verdict would apply to the old
    repository's stored items.
  - Threads have the same problem. Today an item's ID is `github:<owner>/<repo>#<n>`
    (`internal/github/github.go`), and write-backs merge onto the stored item by that
    ID, so an unrelated issue with the same number in the new repository would
    inherit the old conversation.
  - `GitHubRef` therefore gains `RepoID`, the repository's numeric ID, and `IssueID`,
    the issue's or PR's own global numeric ID. Search hits carry the issue ID; the
    repository ID comes from the visibility check, which already fetches the
    repository. Ingest writes both on every item.
  - **Matching.** An incoming item merges onto a stored one (thread, read cursor,
    hidden flag) only when their `IssueID`s agree. If either lacks an `IssueID`, the
    `RepoID` and number must both agree. `worklist.MergeRunItems` applies this rule.
    - **Same IDs, new name** (a renamed repository): the stored item is re-keyed to
      the new name, and its thread follows.
    - **Same name and number, different IDs** (a reused name): the stored item is
      re-keyed to an orphan key, `github:<old name>#<n>@r<old RepoID>`, and keeps its
      thread. The incoming item is stored fresh, with an empty thread.
- **Visibility is kept per repository ID, apart from the items.**
  - New table `repository_visibility`, keyed by owner and `RepoID`. Columns: the
    repository's current full name (informational only), the verdict (`public`,
    `private` or `inaccessible`), and `checked_at`, when the check ran. A repository
    with no row is unknown. This is migration `0002`, an additive table.
  - A verdict reaches an item only through the item's `RepoID`, never by name.
  - Work items carry no visibility. A run's write-back therefore has nothing to
    overwrite, and a ranking run that read an item as public cannot write back a stale
    classification after ingest has found it private.
  - A new item counts as classified as soon as its repository has a verdict, with no
    per-item label to set. The first ingest's new rows are therefore visible to the
    enrich and rank steps later in the same run, once their repository's verdict has
    been reported.
  - Only a new sink call, `ReportVisibility(repoID, fullName, verdict, checkedAt)`,
    changes a row, in one transaction. It applies only when `checkedAt` is newer than
    the stored one, so when ingests overlap, an older check finishing late cannot
    overwrite a newer one.
  - Ingest reports a repository's verdict before writing back that repository's
    items.
- **Legacy items have no `RepoID` or `IssueID`, so their identity can be ambiguous.**
  Ingest binds a legacy item to a `RepoID` only on evidence that it is the same
  repository:
  - **A search hit:** this ingest's search returns the same name and number, now with
    IDs (the user is involved in it now). The item binds to those IDs.
  - **A rename redirect:** looking up the stored name redirects to a renamed
    repository. GitHub redirects only to the original, so that repository's ID is
    the item's.

  A legacy item with neither is ambiguous. It is held back from every model input,
  its thread is never merged onto a new item, and it stays visible to the user,
  marked as unverified. Every item stored before rollout came through a token that
  reads public repositories only, so this affects only items whose name changed hands
  in the meantime.
- **Ingest checks repositories, not just search hits.** A repository that turns
  private drops out of public search, and an item that closed before rollout never
  shows up in an open-item search again, so checking only search hits would miss both.
  - Each ingest checks the repositories of its search hits, plus an inventory of the
    repositories gofer has stored for the user. The inventory comes from a new
    endpoint, `GET /agent/worklist/repositories`: repository IDs and names, with no
    item bodies or threads.
  - A repository with a known ID is checked by ID (`GET /repositories/{id}`), which
    follows renames and can never land on a different repository that took the old
    name. Only legacy names go through the binding rules above.
  - Large inventories are checked in a rotating window per run, oldest check first.
- **What each check outcome does:**
  - **Public:** reported as `public`; the repository's search hits are written as
    usual.
  - **Private (a definite answer), or `inaccessible`** (404, or 403 that is not a rate
    limit): reported as such. Search hits from that repository are not written, and
    stored items are not retired as completed. They are held back from models
    (below).
  - **Unknown** (a timeout, rate limit or other error on the lookup): nothing is
    reported. The repository's search hits are skipped, and the fetch counts as
    incomplete. A complete fetch is what lets `runIngest` call `retireMissing`, so
    open work stays on the radar until a later ingest can check it.
  - **Retirement needs a check in the same ingest.** Even after a complete search, an
    absent item is retired only if this ingest confirmed that its search could have
    seen the item.
    - A repository that has just turned private drops out of public search, so its
      open items would otherwise look finished. If that repository fell outside this
      run's rotating window, it would be retired before its next check.
    - **Public-only run:** the repository must be confirmed `public` in this pass.
    - **Private-enabled run** (credential carries `github:private`, searches carry no
      `is:public`): the repository must be confirmed *accessible*, public or private,
      in this pass. A private PR gofer legitimately ingested is then retired once it
      closes, instead of staying on the radar for ever.
    - Before retiring, ingest checks the repositories of the absent items first,
      ahead of the rotating window.
    - An absent item whose repository's check in this pass was missing, unknown or
      `inaccessible` is kept as it is, and its retirement waits for a later ingest.
      So is an ambiguous legacy item.
    - `retireMissing` takes the set of repository IDs that qualify in this pass.
  - Ingests after rollout work through every stored repository, the first of them
    taking as many as its window allows. Repositories stored before this release
    have no row, so their items are held back from models until they are checked.
    Today's token reads public repositories only, so in practice they come out
    `public`.
- **Stored model inputs: the web tier hands models only data it can show was public
  and produced without private access.**
  - Discuss, ranking and research read what gofer has kept: `sink.List` in `runRank`
    and `runResearch`, and the item and its thread in `runConverse`. A live check
    cannot catch those, so the web tier enforces the policy where it serves them, on
    its own side of the plane.
  - **Unfiltered data needs both the setting and the run's scope.** The web tier
    serves a request unfiltered only when `GITHUB_PRIVATE_REPOS` is on now *and* the
    request's verified credential carries `github:private`. Otherwise it filters.
    - The setting alone would not be enough. A Discuss run dispatched in public-only
      mode might still be running when the setting is turned on. It would then read
      retained private history and save a reply with no `private_access` stamp, since
      its credential has no `github:private`.
    - A run is filtered for its whole life by the mode it was dispatched in.
  - **Items.** Filtered, `GET /agent/worklist` serves a run only items whose
    repository's verdict is `public`. The web tier refuses to dispatch Discuss,
    review or research on any other item, and says why in the thread.
  - **Text a private-access run produced.** While private access is on, a Discuss
    turn on a *public* issue can read a private PR, and its reply is saved in the
    public issue's thread. A research or ranking note can likewise draw on several
    items at once.
    - So the sink stamps every agent turn and every run-written text field (ranking
      rationale, research notes) with `private_access: true` when the run's
      credential carries `github:private`. The web tier does the stamping from the
      verified credential; the runtime's word is not involved.
    - This is conservative: it marks everything such a run wrote, whether or not it
      actually touched a private repository.
    - Filtered, stamped turns and fields are left out of everything served to a run
      (thread history, research inputs). The user still sees them in the UI,
      since they had access.
  - The filter guards what is kept. The labels themselves come from checks run by the
    workload, so the filter does not protect against a compromised runtime; such a
    runtime already holds the user's token.
- **On:** for runs dispatched with `github:private`, the gate, the redirect check and
  the stored filters are skipped. The operator accepts that private titles, bodies,
  code and logs reach the configured models and gofer's database.
- **Switching it off takes effect for new runs at once, and for all runs within one run
  credential lifetime.**
  - A run dispatched before the switch keeps its `github:private` scope, and may
    already hold a token and private context in memory. Filtering what it reads next
    cannot stop its next model call.
  - From the restart that applies the setting, the web tier refuses every
    agent-plane request whose credential carries `github:private`: no vend, no
    worklist read, no write-back. Such a run can get nothing new and save nothing.
  - It can still finish a model call with context it already holds. Its credential
    expires within its run deadline (at most 15 minutes, converse), so the opt-out
    applies to all runs once that much time has passed since the restart. The
    operator should treat the switch as effective only then.
- **On, then off again:** retained private items and stamped text stay in the database,
  and the stored filters keep them out of every workload from the moment above.
  Deleting them is a separate clean-up step, not designed here.
- Org installations stay the org owners' choice. Neither setting needs one.

### Sign-in (`internal/auth`)

GitHub Apps use the same web flow and endpoints as OAuth apps
(`golang.org/x/oauth2/github.Endpoint`), and gofer already sends PKCE. The changes:

- **The client's kind is explicit.** New setting `GITHUB_CLIENT_KIND`: `oauth-app`
  (the default) or `github-app`. `GITHUB_CLIENT_ID` and `GITHUB_CLIENT_SECRET` hold
  whichever client it names.
  - `oauth-app` keeps today's scopes, `read:user user:email`, so a sign-in through
    the legacy client during rollout or after a rollback still reads hidden primary
    emails.
  - `github-app` sends no scopes. GitHub Apps ignore them; the only one GitHub
    recognises is `offline_access`, and expiring tokens make it unnecessary. The
    app's Email addresses permission takes `user:email`'s place.
  - gofer does not guess the kind from the client ID's prefix.
- The token response now carries `expires_in` (8 h) and a refresh token. The callback
  already stores both in `vault.Credential` (`Expiry`, `RefreshToken`).
- Identity does not change. `GET /user` gives the same numeric ID, so the session user
  stays `github:<id>`, and the worklist, threads and read cursors keyed by it carry
  over.
- **Revocation.** `DELETE /applications/{client_id}/token` accepts `ghu_` tokens (per
  the OAuth-applications REST page). For a refused sign-in and for logout, revoke the
  *grant* instead (`DELETE /applications/{client_id}/grant`), so the refresh token
  dies too. Whether deleting the access token alone also kills the refresh token is
  unverified.
- **Logout revokes before it discards.** Revoking the grant needs a *valid* access
  token, and a gofer session lasts 24 hours (`session.defaultMaxAge`), three times an
  access token's 8. Today logout deletes the vault row first, then revokes. Under the
  app, logout does this while holding the credential lock (below):
  1. Read the credential.
  2. If the access token has expired or is about to, refresh it.
  3. Revoke the grant with the current access token.
  4. Delete the row.

  Logout is never blocked: any failure is logged without the token, the row is deleted
  anyway, and the session is destroyed. A failure means remote revocation is
  *unconfirmed*:
  - a refresh that fails with `bad_refresh_token`;
  - a revoke call that fails.

  `bad_refresh_token` does not prove the grant is gone. After a restore from backup,
  the vault can hold a refresh token that was already spent, while the newer pair and
  the grant are still valid at GitHub. In that case logout says the remote side was
  not confirmed. Its response links the user to GitHub's "Authorized GitHub Apps"
  settings, to revoke gofer there. The restore runbook
  (`docs/backup-restore.md`) should say that a restore can do this, and that affected
  users sign in again.
- **Record the issuing client in the stored credential.** A new `client_id` field
  goes in the sealed JSON; old rows decode without it. The broker treats a credential
  from another client as absent, so a cutover or rollback never vends a token from the
  wrong app.
- **Legacy rows get a version before their first vend.** A row stored before this
  release has neither `client_id` nor the credential version (below). Its token never
  expires, so no refresh would ever trigger the `Put` that assigns them, and a run
  could never name it in a rejection. So, under the credential lock, before vending
  an untagged row:
  - **Kind `oauth-app`:** the broker tags it with the configured client ID and a new
    version, writes it back, and vends that. Every legacy row in use is versioned
    before step 3 of the rollout, and again after a rollback.
  - **Kind `github-app`:** an untagged row cannot be from the app, so the broker
    answers "sign-in required".

### Expiry and refresh (`internal/auth`, `internal/api`, `internal/storage`)

GitHub's rules, per its docs:

- An access token lasts 8 hours (`expires_in=28800`).
- A refresh token lasts about 6 months. The docs say 15897600 s; the probe got
  15638400.
- Refreshing returns a new pair, and "that refresh token and the old user access
  token will no longer work".

Today the broker (`api.CredentialHandler`) vends the vault's token as is.
`auth.Handler.Credential` can refresh, but nothing calls it. It would also refresh
only once the token has expired, and with no lock.

1. **The broker vends through a credential source**, `auth.Handler.Credential`, instead
   of reading the vault. Refresh happens only in the web tier. The refresh token never
   leaves it: the broker returns only `access_token`, `token_type` and `expires_in`, as
   today.
2. **Refresh early.** Refresh when less than 30 minutes is left, more than the longest
   run deadline (converse, 15 minutes) plus a margin, so a vended token outlives the
   run that holds it. Use `oauth2.ReuseTokenSourceWithExpiry` with that early expiry;
   the package's default is 10 s.
3. **One writer at a time per credential.** A refresh token works once, so two
   concurrent vends (an ingest and a Discuss turn) must not both spend it; the loser
   would get `bad_refresh_token` and the user would have to sign in again. Sign-in and
   logout write the same row, so they race refreshes too:
   - A refresh rotates the pair, logout deletes the row, then the refresh writes a
     working credential back after the user signed out.
   - A failed refresh deletes the row a newer sign-in has just written.

   So one per-(user, provider) mutex in `auth.Handler` covers all three: the callback's
   `Put`, logout, and the broker's refresh. The web tier is a single replica by
   construction, so an in-process mutex is enough. Whoever takes it re-reads the row
   and acts only on what it finds.

   The callback exchanges and validates its new token *before* it can know whose lock
   to take, so a logout that revokes the whole grant can land in between. The
   callback would then store an already-revoked token and open a session. So, holding
   the lock, the callback checks the token again:
   - `POST /applications/{client_id}/token`, GitHub's check-token endpoint, which does
     not count against failed-login limits.
   - Only a valid answer is stored and gets a session.
   - An invalid one restarts sign-in. The grant was revoked mid-flow, and the user
     consents again.

   As a second line of defence, each stored credential gets a version: a random ID
   minted at every `Put`, kept in the sealed JSON. A refresh's write and a failed
   refresh's delete apply only if the row still holds the version they read.
4. **Persist before vending.** Store the rotated pair before returning the access
   token. If the write fails, the old refresh token is already spent: retry the write,
   and if it still fails, log it (without the token) and require sign-in.
5. **A run re-vends once on 401, naming the token GitHub rejected.** Rotation kills
   the old access token, which a run vended just before the threshold may still hold.
   Each job vends once today (`runIngest`, `runEnrich`, `runConverse`).
   - The vend response gains the credential's version (`credential_version`). It is
     not a secret.
   - The workload's GitHub calls get their token from a small holder. On "401 Bad
     credentials" it re-vends once with `rejected=<version>`, then fails as today.
   - A plain re-vend is not enough. If the user revokes gofer in their GitHub settings
     five minutes after signing in, the stored token still has hours left before the
     refresh threshold, so the broker would hand back the same dead token, run after
     run, and the user would never be asked to sign in.

   The broker, under the credential lock:
   - **The stored version differs from the rejected one:** rotation already happened,
     so it returns the stored token.
   - **It is the same:** the broker confirms the rejection itself with GitHub's
     check-token endpoint (`POST /applications/{client_id}/token`, client
     credentials). A run's say-so alone never rotates or deletes a credential.
     - **GitHub says the token is valid:** the 401 was something else, and the broker
       returns the same token.
     - **Invalid, and the credential has a refresh token:** it refreshes now,
       whatever the expiry. Success returns the new pair. `bad_refresh_token` deletes
       the row and answers "sign-in required" (below).
     - **Invalid, and no refresh token:** for example a legacy `gho_` row revoked at
       rollout step 4 and matching again after a rollback. The version is dead, so the
       broker deletes the row (version-checked) and answers "sign-in required".
   - **At most one forced refresh per run.** Each refresh mints a new version, so a
     version check alone would let one run rotate again and again, killing the tokens
     concurrent runs hold. The broker keys the allowance on the run credential's run
     ID (`Principal.JobID`, from the verified claims). A run's second `rejected=` gets
     the current token, never another refresh. The allowance is kept in memory for the
     run credential's lifetime.
6. **A dead refresh token** (`bad_refresh_token`: expired, revoked by the user, the
   app deleted, or a stale copy restored from backup) deletes the vault row
   (version-checked, as above).
   - The broker then answers "sign-in required", for this case, a missing credential
     and one from another client alike: a distinct status and error code, not today's
     generic 404.
   - The landing page should ask the user to sign in again (a small UI change: today
     it shows no such prompt).
   - Discuss treats it specially (next section).
7. **One additive migration.** The credential changes need none: `sealedCredential`
   already seals the refresh token and the expiry, and `client_id`, the version (and
   optionally the refresh token's expiry, so the UI can warn before 6 months) are new
   `omitempty` JSON fields inside the sealed blob.
   - The `private_access` stamps need none either: work items are stored as JSON
     documents (`work_items.doc`). They are store-owned, like the thread:
     `worklist.MergeRunItems` preserves them, and only the sink's stamping sets them.
   - The only new table is `repository_visibility`, migration `0002`. Older code
     ignores it.

### Discuss without GitHub access

`runConverse` treats a failed vend as best-effort. It calls the model anyway, with no
live context and no tools, and saves the reply as a normal answer. Under the app that
happens to every user between cutover and their next sign-in, and after any
revocation. Someone asking why a job failed would get a confident answer built from
stored metadata and the thread alone, with no live GitHub evidence.

- **"Sign-in required" from the broker:** the run does not call the model. It posts an
  agent turn answering the user's turn: "gofer can't reach GitHub for you: sign in
  again to reconnect, then ask again". It is marked as a system notice, so the thread
  shows it as such and review state does not count it as an answer. The same marker
  lets the UI show the sign-in prompt.
- **"Sign-in required" found mid-turn.** The first vend can succeed with a token that
  is unexpired by its clock but revoked at GitHub. Then the first GitHub call gets a
  401, and the holder's re-vend comes back "sign-in required". Today two paths would
  swallow that:
  - `runConverse` ignores errors from the snapshot fetch (`Discussion`, `Login`);
  - the model's tool loop turns tool errors into ordinary model input.

  So the token holder returns a typed reconnect error, and both paths pass it
  through. The snapshot fetch returns it to `runConverse`, which posts the notice
  without calling the model. A tool call that hits it ends the tool loop: the
  converser treats it as fatal, not as a result for the model. The turn then saves
  the reconnect notice and discards any partial model output, never a normal reply
  or review. Other tool errors are still model input, as today.
- **Any other vend failure** (the broker unreachable, a 5xx): the model still runs, but
  the prompt says GitHub is unavailable for this turn. The saved reply carries a
  visible "answered from stored context only" note, so the user knows to ask again.

### Search under user tokens

GitHub's search docs: "For requests made by GitHub Apps with a user access token, you
can't retrieve a combination of issues and pull requests in a single query. Requests
that don't include the is:issue or is:pull-request qualifier will receive an HTTP 422."

- **Ingest.** `signals` in `internal/github/github.go` has
  `is:open involves:@me archived:false`, which fails outright. Split it into
  `is:issue …` and `is:pr …`, both `ReasonInvolved`. Measured with a broader token,
  limited to public repositories: 119 issues + 122 PRs = 241, exactly the unsplit
  count, so nothing is lost. `review-requested:` already has `is:pr`; under the app
  token it returned 56, the same as the public-only count with a broad token.
- **Search budget.** Ingest becomes three paged queries instead of two, against the
  search limit of 30 requests a minute (the probe's responses counted down from 29).
  Each query keeps its own 1,000-result ceiling, so the split raises the
  per-relationship ceiling. `searchBudget` already paces pages.
- **`github_search`.** The model writes the query. When it has none of
  `is:issue`/`is:pr`/`is:pull-request`/`type:issue`/`type:pr`, the tool should run it
  twice (issues, then PRs) and merge, rather than pass back a 422.
- **Reach.** With the app installed nowhere, search returns public repositories only.
  The probe's counts equal the public-only counts, while the broad token also sees
  private items. Whether today's `read:user` token sees any private items is
  unverified. If it doesn't, users lose nothing. Either way, with
  `GITHUB_PRIVATE_REPOS` off, every query also carries `is:public`, so an installation
  made later changes nothing.

### Organisations

- No organisation has to approve or install anything for public repositories. The
  probe read `kubernetes-sigs`, where the app is not installed.
- OAuth-app access restrictions apply to OAuth apps. GitHub Apps reach an
  organisation's private resources only by installation, which its owners control.
  Whether `kubernetes-sigs` restricts OAuth apps no longer matters for public reads.
- SAML SSO organisations: per GitHub, the user needs an active SAML session to see
  that organisation's resources through the app. This only matters for private
  resources, which gofer reads only with `GITHUB_PRIVATE_REPOS` on.

### Existing users

Their vault holds a non-expiring `gho_` token from the OAuth app, with no refresh
token. After the switch, each user signs in once more. Until they do, the broker
answers "sign-in required" (the `client_id` check). Ingest fails as it does for a
signed-out user, Discuss posts the reconnect notice instead of a model answer, and the
page should ask them to sign in. Their worklist and threads stay, keyed by the same
`github:<id>`.

The old tokens must be revoked explicitly. Logout revokes with the *configured*
client ID, so after the switch it can no longer revoke an OAuth-app token. Use "Revoke
all user tokens" on the OAuth app's settings page once the switch is made.

### Rollout

1. The owner registers the app (permissions and settings above) and stores its client
   ID and secret wherever the OAuth app's are stored today.
2. Ship one release that works under either app:
   - the broker vends through the refreshing source, with the lock, early refresh and
     persist-first write;
   - one credential lock and version shared by sign-in, logout and refresh;
   - logout revoking before it deletes, and reporting unconfirmed revocation;
   - the callback re-checking its token under the lock;
   - the runtime re-vends once on 401, naming the rejected version; the broker
     confirms with check-token and allows one forced refresh per run;
   - "sign-in required" from the broker, and Discuss's reconnect notice, including one
     found mid-turn;
   - `GITHUB_CLIENT_KIND`, with each kind's scopes;
   - `GITHUB_PRIVATE_REPOS` (off), with the `github:private` run scope:
     - the visibility gate and the redirect check;
     - `RepoID` and `IssueID` on items, ID-based matching in `MergeRunItems`, and the
       legacy binding rules;
     - the `repository_visibility` table with `ReportVisibility`, and the repository
       inventory;
     - ingest's unknown-visibility rule, and retirement only after a check in the
       same pass;
     - the web tier's filters on stored items and on text stamped `private_access`,
       lifted only for a run that carries the scope while the setting is on.

     Before anyone turns the setting on, `github:private` must be added to the
     AgentApp's `maxScopes` and applied to the cluster.
   - ingest's involves query is split, and `github_search` adds the type qualifier;
   - the `client_id` check, with untagged legacy rows tagged and versioned at first
     vend under `oauth-app`, and refused under `github-app`.

   Under the OAuth app this release changes almost nothing visible. No refresh token
   means no refresh. Split and `is:public` queries work for `gho_` tokens, and today's
   token reads no private repository anyway (if the unverified reach question below
   says otherwise, this release is where those items drop out). The web and runtime
   images ship together, as AGENTS.md already requires.
3. Set `GITHUB_CLIENT_KIND=github-app` and switch `GITHUB_CLIENT_ID` and
   `GITHUB_CLIENT_SECRET` to the app's, then restart the web pod. Users sign in
   again.
4. Revoke all of the OAuth app's user tokens from its settings page.
5. Verify in prod:
   - a Discuss turn on a failing Actions job reads the log (`gha_job_summary`);
   - an ingest completes with the same item count as before;
   - a token is refreshed after 8 hours without the user signing in again.
6. Delete the OAuth app once the rollback window has passed.

### Rollback

Set `GITHUB_CLIENT_KIND=oauth-app` and switch the two secrets back to the OAuth
app's (keep it until step 6), then restart. Sign-in asks for `read:user user:email`
again. The
`client_id` check treats the app's `ghu_` credentials as absent, so users sign in
again and get `gho_` tokens. A user who never signed in through the app still has a
legacy row that matches again, but its token was revoked at step 4. Its first 401 is
confirmed with check-token, and since it has no refresh token, the row is deleted and
the user is asked to sign in. Revoke the app's tokens, or delete the app. The step-2
release works under both apps, so no image rollback is needed. The only schema
change is the additive `repository_visibility` table, so there is nothing to
restore.

### The security invariants

- **Source scopes READ:** every permission is read-only, and the user token is the
  intersection with the user's rights. The probe saw `scope: ''` and only `*=read`
  permission headers. With `GITHUB_PRIVATE_REPOS` off, reads are also limited to
  public repositories, whatever anyone installs.
- **Credentials vended on demand:** unchanged, and stronger. The vended token now
  expires within 8 hours where today's never does, and the refresh token never leaves
  the web tier.
- **The verify-only web tier and the run-credential plane:** untouched. The app's
  client secret is the same class of secret as today's OAuth client secret. There is
  no app private key.

## Alternatives

- **Add `public_repo` to the OAuth app.** It works (verified), and it is a one-line
  change, but it grants write access to public repositories, which the invariant rules
  out.
- **A fine-grained personal token per user.** It works (verified) and is read-only,
  but each user pastes a standing secret into gofer and renews it by hand, outside the
  sign-in that delegates everything else.
- **Leave it.** The `gha_*` tools keep reporting GitHub's refusal and its reason, and
  the conversation says it cannot read the log.

## Verification

Verified on 2026-11-18, by the probe above and GitHub's documentation:

- A read-only app user token, with the app installed nowhere, gets the 302 for the
  job log and the step log in `kubernetes-sigs`.
- Every other endpoint gofer's tools call answers that token, with the permissions in
  the table.
- Search needs a type qualifier under that token, and the split loses no item
  (public-only counts).
- Tokens expire after 8 hours and come with a refresh token. Refreshing rotates both
  (docs only).

Left unverified:

- **Other places:** other organisations and repositories, including ones with stricter
  policies (IP allow lists, restrictions on GitHub Apps). Only one public job was
  probed.
- **Whether Actions: read is needed** for a public repository's log, or whether the
  token's implicit public read access would do, as it seems to for the fine-grained
  personal token. It is kept either way: read-only, and named by the endpoint.
- **The blob download** behind the 302: the probe did not follow it. gofer's blob
  client is unchanged and already handles it with other tokens.
- **`GET /user/emails`** with the Email addresses permission: not probed.
- **Refresh against real GitHub:** rotation, invalidation of the old access token, and
  `bad_refresh_token` after revocation. The documented and observed refresh lifetimes
  differ slightly.
- **Revocation of `ghu_` tokens:** whether `DELETE /applications/{client_id}/token`
  also invalidates the refresh token (the reason for preferring the grant endpoint),
  and the "Revoke all user tokens" control on a GitHub App.
- **The public-only guard against a real installation:** `is:public` together with
  the `@me` qualifiers under an app token, and the gate's `GET /repos/{repo}`
  `private` field for a private repository the app is installed on. The probe had no
  installation, so it could not exercise either.
- **Redirects:** the exact form of the redirect for a transferred issue or a renamed
  repository (`/repositories/{id}/…` or `/repos/{owner}/{repo}/…`, 301 or 307). The
  check is designed for both.
- **The check-token endpoint** (`POST /applications/{client_id}/token`) for `ghu_`
  tokens, and its answer for a revoked grant.
- **ID lookups under an app token:** `GET /repositories/{id}` for a public repository
  the app is not installed on, and whether an issue keeps its global `id` when it is
  transferred. If it does not, a transferred issue arrives as a new item, and its old
  copy waits for its own repository's check before it is retired.
- **A dispatched run's credential carrying `github:private`** once the AgentApp
  ceiling allows it. AEI's code copies requested scopes within the ceiling into the
  minted credential, but no run with the new scope has been dispatched.
- **Who can authorise a private app** owned by a personal account: the docs speak of
  organisation-owned apps.
- **Today's private reach:** whether the `read:user` token sees any private items in
  search, which decides whether the switch drops anything from a user's worklist.
- **An end-to-end gofer run** with an app token: sign-in, ingest, a Discuss turn
  reading a log, and a refresh after 8 hours.
