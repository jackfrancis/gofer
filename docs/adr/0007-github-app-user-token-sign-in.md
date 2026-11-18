# ADR 0007 — Sign in through a read-only GitHub App instead of an OAuth app

Status: proposed — not implemented. The question it rests on is verified: a GitHub
App user access token with only read permissions gets the job log's 302 in an
organisation where the app is not installed (see "The question").
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
| Metadata | required by GitHub with any repository permission | — |
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
account's private repositories, still read-only.

### Sign-in (`internal/auth`)

GitHub Apps use the same web flow and endpoints as OAuth apps
(`golang.org/x/oauth2/github.Endpoint`), and gofer already sends PKCE. The changes:

- The provider's `ClientID` and `ClientSecret` become the app's (same env names,
  `GITHUB_CLIENT_ID` and `GITHUB_CLIENT_SECRET`).
- `Scopes` becomes empty. GitHub Apps ignore scopes; the only one GitHub recognises
  is `offline_access`, and expiring tokens make it unnecessary.
- The token response now carries `expires_in` (8 h) and a refresh token. The callback
  already stores both in `vault.Credential` (`Expiry`, `RefreshToken`).
- Identity does not change. `GET /user` gives the same numeric ID, so the session user
  stays `github:<id>`, and the worklist, threads and read cursors keyed by it carry
  over.
- Revocation: `DELETE /applications/{client_id}/token` accepts `ghu_` tokens (per the
  OAuth-applications REST page). For a refused sign-in and for logout, revoke the
  *grant* instead (`DELETE /applications/{client_id}/grant`), so the refresh token
  dies too. Whether deleting the access token alone also kills the refresh token is
  unverified.
- Record the issuing client ID in the stored credential: a new `client_id` field in
  the sealed JSON, which old rows decode without. The broker treats a credential from
  another client (or with none) as absent, so a cutover or rollback never vends a
  token from the wrong app.

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
3. **One refresh at a time per user.** A refresh token works once, so two concurrent
   vends (an ingest and a Discuss turn) must not both spend it; the loser would get
   `bad_refresh_token` and the user would have to sign in again. The web tier is a
   single replica by construction, so a per-(user, provider) mutex is enough. The
   holder re-reads the vault after taking it and refreshes only if the stored token is
   still due.
4. **Persist before vending.** Store the rotated pair before returning the access
   token. If the write fails, the old refresh token is already spent: retry the write,
   and if it still fails, log it (without the token) and require sign-in.
5. **A run re-vends once on 401.** Rotation kills the old access token, which a run
   vended just before the threshold may still hold. Each job vends once today
   (`runIngest`, `runEnrich`, `runConverse`). The workload's GitHub calls should get
   their token from a small holder that re-vends once on "401 Bad credentials", then
   fails as today. This also covers a user revoking gofer in their GitHub settings
   mid-run: the re-vend then gets "sign in again".
6. **A dead refresh token** (`bad_refresh_token`: expired, revoked by the user, or the
   app deleted) deletes the vault row. The broker then answers as it does for a missing
   credential, and the landing page should ask the user to sign in again (a small UI
   change: today it shows no such prompt).
7. **No schema migration.** `sealedCredential` already seals the refresh token and the
   expiry. `client_id` (and optionally the refresh token's expiry, so the UI can warn
   before 6 months) are new `omitempty` JSON fields inside the sealed blob.

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
  unverified. If it doesn't, users lose nothing.

### Organisations

- No organisation has to approve or install anything for public repositories. The
  probe read `kubernetes-sigs`, where the app is not installed.
- OAuth-app access restrictions apply to OAuth apps. GitHub Apps reach an
  organisation's private resources only by installation, which its owners control.
  Whether `kubernetes-sigs` restricts OAuth apps no longer matters for public reads.
- SAML SSO organisations: per GitHub, the user needs an active SAML session to see
  that organisation's resources through the app. This only matters for private
  resources, which are out of scope here, as they are today.

### Existing users

Their vault holds a non-expiring `gho_` token from the OAuth app, with no refresh
token. After the switch, each user signs in once more. Until they do, the broker finds
no credential for the new client (the `client_id` check), ingest and Discuss runs fail
as they do for a signed-out user, and the page should ask them to sign in. Their
worklist and threads stay, keyed by the same `github:<id>`.

The old tokens must be revoked explicitly. Logout revokes with the *configured*
client ID, so after the switch it can no longer revoke an OAuth-app token. Use "Revoke
all user tokens" on the OAuth app's settings page once the switch is made.

### Rollout

1. The owner registers the app (permissions and settings above) and stores its client
   ID and secret wherever the OAuth app's are stored today.
2. Ship one release that works under either app:
   - the broker vends through the refreshing source, with the lock, early refresh and
     persist-first write;
   - the runtime re-vends once on 401;
   - ingest's involves query is split, and `github_search` adds the type qualifier;
   - the `client_id` check, with old rows matched to the OAuth app until the switch.

   Under the OAuth app this release changes nothing visible: no refresh token means no
   refresh, and split queries work for `gho_` tokens. The web and runtime images ship
   together, as AGENTS.md already requires.
3. Switch `GITHUB_CLIENT_ID`/`GITHUB_CLIENT_SECRET` to the app's and restart the web
   pod. Users sign in again.
4. Revoke all of the OAuth app's user tokens from its settings page.
5. Verify in prod:
   - a Discuss turn on a failing Actions job reads the log (`gha_job_summary`);
   - an ingest completes with the same item count as before;
   - a token is refreshed after 8 hours without the user signing in again.
6. Delete the OAuth app once the rollback window has passed.

### Rollback

Switch the two secrets back to the OAuth app's (keep it until step 6) and restart. The
`client_id` check treats the app's `ghu_` credentials as absent, so users sign in
again and get `gho_` tokens. Revoke the app's tokens, or delete the app. The step-2
release works under both apps, so no image rollback is needed. Nothing in the schema
changes, so there is nothing to restore.

### The security invariants

- **Source scopes READ:** every permission is read-only, and the user token is the
  intersection with the user's rights. The probe saw `scope: ''` and only `*=read`
  permission headers.
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
- **Who can authorise a private app** owned by a personal account: the docs speak of
  organisation-owned apps.
- **Today's private reach:** whether the `read:user` token sees any private items in
  search, which decides whether the switch drops anything from a user's worklist.
- **An end-to-end gofer run** with an app token: sign-in, ingest, a Discuss turn
  reading a log, and a refresh after 8 hours.
