# ADR 0011 — Commenting with a classic personal access token

Status: proposed — implemented; checked against real GitHub by hand (below), not yet
through gofer.
Date: 2026-11-24
Amends: ADR 0009 (the write grant can also be a personal access token).

## Context

ADR 0009 lets a user connect commenting through a separate classic OAuth app with
`public_repo`, and ADR 0010 uses the same grant to create pending reviews. The first
draft review against `kubernetes/autoscaler` failed: the `kubernetes` organization has
turned on **OAuth App access restrictions**, so tokens from an OAuth app it has not
approved cannot write there. Approval by the organization's owners is unlikely for a
personal tool, and other organizations gofer's users work in may do the same.

Organizations restrict personal access tokens separately. A smoke test (2026-11-24) on
`kubernetes/autoscaler#10402` created a pending review and deleted it:

- a **fine-grained** token for the user's account was refused (403, "Resource not
  accessible by personal access token"): fine-grained tokens write only where the
  token's owner is the organization, and only if it opts in;
- a **classic** token with only `public_repo` created the review (200, `PENDING`).

## Decision

### An alternative, not a replacement

A deployment may let users connect commenting with a classic personal access token
(`GITHUB_WRITE_TOKENS=true`). It is off by default and requires the write client: the
OAuth grant stays the default way to connect, because gofer can revoke it (at logout
and disconnect, and its owner can revoke every user's grant at once from the app's
settings) and the user never handles its token. A token is for organizations that
block the app. No configured secret is replaced or added.

Each user holds **one** commenting credential, of either kind, in the same vault slot
(provider `github-write`), under the same credential lock, versioning and pending-logout
ledger. The bulk comment and draft reviews use whichever is stored. A token is marked
by its client ID (`PersonalTokenClient`), so nothing about storage changes.

### What gofer accepts

`POST /auth/github-write/token` (same-origin, signed in with GitHub) checks the token
with `GET /user`, authenticated with it, and stores it only if all hold:

- it is a **classic** token: it starts `ghp_`, and GitHub's answer carries
  `X-OAuth-Scopes`, which GitHub sends for classic tokens only (a fine-grained token is
  refused with an explanation);
- its scopes are **exactly** `public_repo`: the narrowest scope that can comment and
  review on a public repository, and the same reach as the OAuth grant, so a leaked
  token can do no more than a leaked grant;
- it **expires**, within 90 days (`maxTokenLifetime`, with a day of slack), read from
  `GitHub-Authentication-Token-Expiration`: a token with no expiry is refused, so a
  copy that leaks stops working on its own;
- it belongs to the **signed-in** GitHub account, which `ALLOWED_GITHUB_LOGINS` still
  admits;
- commenting is **not already connected**, and no disconnect or logout of it is pending:
  storing over an OAuth grant would leave that grant unrevoked at GitHub, so the user
  disconnects first.

The token is never logged, echoed in a page or put in an error. A refused token is not
kept, and the page says to delete it on GitHub if it will not be used.

### Using it

The token is used exactly as the OAuth grant is: only by the web tier, only for what
the user confirmed, never vended to a run (`Credential` and the broker refuse the
provider, as before). Further:

- **Expiry.** An expired token reads as not connected and is not handed out
  (`WriteCredential`), so the radar asks the user to connect again.
- **Turned off.** A token stored while tokens were allowed is not used once they are not
  (`fromConfiguredClient`); disconnect and logout still delete it.
- **Rejected.** A 401 on a token is confirmed with `GET /user` (not the OAuth app's
  check-token, which knows nothing of tokens) before gofer drops its copy.
- **Disconnect and logout.** gofer cannot revoke a personal access token. Both delete
  gofer's copy through the usual revoke-then-delete machinery, skipping the revocation,
  and tell the user to delete the token in GitHub's token settings. The token is never
  sent to an OAuth app's revoke or check endpoint.

### Telling the user when an organization blocks the app

`ghcomment` recognizes GitHub's OAuth App access restriction refusal (a 403 naming the
organization) as `AppRestrictedError`. The bulk comment and draft reviews then say which
organization blocks gofer's commenting app and that connecting with a token (where this
gofer allows it) or asking the organization to approve the app are the ways forward,
instead of GitHub's message cut at 200 characters. A refused review now says "Not
created" rather than "Not posted".

## Consequences

- ADR 0009's invariant becomes: **the only write-capable credential is the user's
  commenting credential, an OAuth grant from the write client or, where allowed, a
  classic `public_repo` token that expires within 90 days; either never leaves the web
  tier.**
- With a token, a compromise of the vault has the same reach as with an OAuth grant,
  but gofer cannot shut it off: the user must delete the token, and the operator cannot
  revoke every user's token at once. The 90-day expiry bounds how long a stolen copy
  works. This is the trade the deployment accepts by setting `GITHUB_WRITE_TOKENS`.
- A token passes through the user's browser and clipboard before gofer stores it; the
  OAuth token never does.
- Using a token in an organization that blocks unapproved OAuth apps is within its
  rules (it allows classic tokens) but reaches around its vetting of third-party apps.
  gofer only ever acts with it on what the user confirmed.
- Users renew the token at least every 90 days.
