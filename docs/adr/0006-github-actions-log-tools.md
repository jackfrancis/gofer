# ADR 0006 — The conversation can read GitHub Actions job logs

Status: accepted — implemented and unit-tested; in prod, job logs in at least one organisation are refused to gofer's token (see Scope)
Date: 2026-11-18

## Context

ADR 0005 gave the conversation a PR's CI results (`github_get_ci_status`) and Prow job
logs (`ci_*`). GitHub Actions was left out. `github_get_ci_status` lists Actions check
runs and their details URLs, but nothing could open those URLs. In a live Discuss
turn the assistant said it could not open the job log because its tools cannot read
GitHub Actions logs.

The REST API covers this:

- `GET /repos/{repo}/actions/jobs/{job_id}` returns a job and its steps (number, name,
  conclusion, start and end to the second).
- `GET /repos/{repo}/actions/runs/{run_id}/jobs` (or `…/attempts/{n}/jobs`) lists a
  run's jobs.
- `GET /repos/{repo}/actions/jobs/{job_id}/logs` answers with a 302 to a short-lived,
  pre-signed URL on a blob host. The job's plain-text log is there, with an RFC 3339
  timestamp at the start of every line.
- `GET /repos/{repo}/actions/jobs/{job_id}/steps/{step_number}/logs` does the same
  for one step's lines.
- A check run's ID is its Actions job's ID, so a check's `html_url`
  (`/<owner>/<repo>/runs/<id>`) names the job.

## Decision

Add three read-only tools in `internal/agent/ghatools.go`, backed by
`internal/github/actions.go`. Both packages are workload-only. They ship with the
runtime image (`make kind-load-runtime`), and the web tier does not change.

| Tool | Does |
| --- | --- |
| `gha_job_summary` | For a job URL (or `job_id`): its result, the workflow's head commit, every step with its result and approximate log line range, and the failure markers in the log. The runner's `##[error]` lines come with the 15 lines before them; go test FAIL lines, panics and make errors are found by the Prow patterns. When nothing matches, the summary shows the last 40 lines of the failing step. A run URL (or `run_id`) summarises the run's first failing job, then lists its jobs, failing first. A pull request `number` does the same for the failing Actions checks on the PR's head commit. |
| `gha_search` | RE2 or literal search over one job's log, with context lines. Results are `log:line: text`. |
| `gha_read` | A line window of one job's log. `step=N` reads that step, a `#step:N` link reads the step it names, and a negative `start_line` counts from the end. A step the job log cannot place is read from its own log (`step-N.log`). |

`github_get_ci_status` now tags Actions URLs `[actions]` and points the model to
`gha_job_summary`. A `/runs/<id>` or `/commit/<sha>/checks/<id>` link is the generic
check-run page GitHub gives every app's check, so it counts as an Actions job (for the
tag, and when picking a PR's failing Actions jobs) only when the check's app is
`github-actions`. The `/actions/runs/…` form is Actions' own and needs no such check.

The "workflow head" is the job's `head_branch` and `head_sha`: what the run was
triggered on. It is not necessarily the commit tested. A `pull_request` run checks out
a merge commit by default, and a workflow can check out anything, so the summary
labels it that way and points to the checkout step's log.

An overview (a run, or a PR) puts the first failing job's diagnosis ahead of its job
list and lists at most 40 jobs, so a wide failing matrix cannot push the log evidence
out of the 48 KB result. The converser's tool-budget note routes failed Prow jobs to
`ci_job_summary` and failed Actions jobs to `gha_job_summary`. The common case costs
one round (`gha_job_summary` with the PR number) or two (`github_get_ci_status`, then
`gha_job_summary` on each failing job).

### Reuse of the Prow machinery

The log is stored in the run's existing `prow.Session` through a new
`FetchWith(key, rel, fetch, opts)`, which `Fetch` itself now uses. Prow and Actions
logs therefore share everything:

- the per-run limits on bytes, files and time;
- the scratch directory under an `os.Root`, with clean-up on `Close`;
- normalisation (ANSI sequences stripped, `\r` handled) and caching, including the
  remembered failure of a fetch;
- the search, read and marker scan code, exported for one fetched file as
  `SearchFile`, `ReadFile` and `ScanLog`, with the Prow patterns as
  `DefaultLogPatterns`;
- the output formatters and the 48 KB clamp in `citools.go`.

`FetchOptions.LineHead` sees the start of every stored line, numbered the way search
and read number them. The Actions tools use it to drop the runner's timestamp, which
saves about 29 bytes a line and lets `^`-anchored patterns match. Its writes are
buffered on their way to disk, and normalisation checks the fetch's context every
MiB, so a large log stays inside the fetch's time budget.

The tools also use `LineHead` to assign lines to steps: a line belongs to the latest
step start at or before its second. Two cases defeat that, and both are reported as
"mixed with step(s) N", never as a clean range:

- **Same-second starts.** The API gives step starts only to the second, and quick
  steps often start in the same second as each other. Such steps form one group whose
  lines the timestamps cannot split, so the summary shows the group's range on each.
- **Overlap in time.** A `background` step runs while the steps after it run, so its
  output lands, by time, inside theirs. A step that starts before an earlier one has
  ended (in whole seconds, so starting in the second the previous one ended does not
  count), or after one that has not ended, overlaps it. A step left with no lines of
  its own is still listed as running alongside the others.

For such a step (or one with no lines at all), `gha_read step=N` and the failing-step
tail fetch the step's own log from the step-log endpoint instead. Its lines are
numbered within the step, and results name it `step-N.log` to keep the two numberings
apart. Ranges are still approximate where one step ends and the next starts within a
second, and the summary marks them with "≈".

A step read from the job log stays inside the step on every page. With `step` set
(or, when it is absent, a `#step:N` link in the URL), `start_line` is a job-log line inside the step (as a continuation hint gives it; the
default is the step's first line), a negative one counts back from the step's end,
and one outside the step is an error. The window never runs past the step's last
line, whatever `max_lines` asks, and offers no continuation once the step's end is
reached.

When the job log is over the per-file or per-run byte budget, an explicit
`gha_read step=N` reads the step's own log instead, and the summary takes its
failing-step tail from there. Both count under the job's slot and the same budgets.

The scanner gained a `Before` window, and `ScanLog` orders windows by where they
start before it merges them. A merge that would grow past the 25 lines a block shows
continues in an adjacent block, so a hit's own line is never cut off. Prow summaries
are unaffected except when merged blocks would have exceeded 25 lines, which used to
truncate them.

### Credential handling

- The token goes to the API host only. The log request is a single round trip
  through the shared client's transport (its retries included), not `Client.Do`,
  and its 302 is read rather than followed. `Client.Do` would parse a redirect's
  `Location` before `CheckRedirect` could stop it, and quote a malformed one,
  pre-signed URL and all, in its error. The blob is
  then fetched by a separate client, built on a fresh clone of the default
  transport, never on the caller's client, because that client's transport might
  add a credential. A guard RoundTripper on that client deletes `Authorization`,
  `Cookie`, `Proxy-Authorization` and `Referer`, refuses user-info URLs, and requires
  https. Plain http is allowed only when the API base is itself plain http, which
  happens only with a test stub. `Referer` matters on a second hop: when the blob
  host redirects, net/http sets it to the previous URL, pre-signed query included.
- The pre-signed URL is a credential, so no error includes it. Below the outer
  `*url.Error`, net/http can still quote it (a redirect `Location` it cannot parse),
  so a failed blob request reports only a fixed message, keeping just cancellation
  and timeouts as such.
- A link the blob host refuses with 403 or 404 has expired between the redirect and
  the fetch. The client asks the API for a fresh link once, within the same fetch,
  so the session never caches a failure that a retry would have fixed.
- Bounds: the session's 64 MiB per file and 512 MiB per run; the session's fetch
  timeout, which is 60 s capped by the run's deadline less a 2-minute reserve; a
  2-minute timeout on the blob client; at most 3 redirects on the blob hop; and at
  most 6 distinct Actions jobs per answer. A job takes its slot when its fetch is
  attempted, so a failed, oversized or binary download counts too, and a retry
  reuses the slot.

### Scope

gofer's GitHub OAuth app requests `read:user user:email`, and this change does not
widen that. GitHub's documentation says that for a private repository, a classic
OAuth token needs the `repo` scope. gofer does not request `repo`, so
private-repository logs, like private-repository files, stay out of reach.
Requesting `repo` (or `public_repo`) would also grant write access, so it needs its
own decision.

**Public repositories are not enough either, at least in one organisation.** On
2026-11-18, the first prod conversation to use these tools read a `kubernetes-sigs`
job's metadata and steps, but both the job log and the failing step's log came back
403 for gofer's token. For the same job, a token with `repo`, `read:org`,
`workflow` and `gist` scopes got the 302 to the log. Two causes fit, with different
fixes: the log endpoints may need more scope than `read:user`, or the organisation
may restrict OAuth apps it has not approved, which keeps public metadata readable
but refuses anything that needs an authenticated identity.

The first release dropped GitHub's response body on a refusal, so the tool said only
"status 403". A refused log request now carries GitHub's own message, the token's
granted scopes (`X-OAuth-Scopes`), the scopes the endpoint accepts
(`X-Accepted-OAuth-Scopes`), and whether the organisation requires SAML SSO, and it
says the refusal concerns gofer's access, not anything the job printed. A rate-limit
403 is reported as a rate limit instead. Those headers name scopes only, and the
body comes from the API host, never the pre-signed blob host, so none of it is a
credential.

## Verification

Unit tests use `httptest` servers: the API on one server, the blob host on another.

- `internal/github`:
  - URL parsing for each accepted form, and rejection of other or look-alike hosts,
    bad IDs and `..`;
  - pagination of a run's jobs and the attempt endpoint;
  - log download through the redirect, with the token on the API request and none
    on the blob request, even when the shared client's transport adds one;
  - no `Referer` on a second blob hop;
  - a malformed redirect `Location` kept out of the error, and cancellation kept as
    such;
  - one fresh link after a refused one, and a clear error after two;
  - a malformed `Location` on the API's own redirect kept out of the error, with the
    API hop still going through the client's transport;
  - the step-log endpoint;
  - the size bound, with and without a Content-Length;
  - 404 and 410;
  - an error from an unreachable blob host that does not include the pre-signed URL;
  - the guard stripping headers (`Referer` included) without changing the caller's
    request.
- `internal/prow`:
  - `headWriter` numbering and dropping across awkward write boundaries and long
    lines;
  - normalisation stopping on a cancelled context;
  - `FetchWith` caching, remembered failures and limits;
  - `SearchFile`, `ReadFile` and `ScanLog` with a `Before` window;
  - the long-merge split.
- `internal/agent`:
  - the job, run and PR summaries, with step ranges, the `##[error]` block,
    timestamps and colour stripped, and the failing-step tail when there is no
    marker;
  - search and read, including `step`, `#step:N` and a negative start;
  - one download per job across calls;
  - errors for ambiguous runs, Prow URLs, other hosts and missing jobs;
  - a background step overlapping the step after it: both reported as mixed, and the
    failing background step's tail read from its own log;
  - an oversized job log: an explicit step read, and the summary's failing-step tail,
    from the step's own log;
  - a step read capped at the step's last line, with no continuation past it, also
    on a second page that follows the continuation hint, for both `step` and a
    `#step:N` link;
  - steps starting in the same second: shared ranges, and the failing step's tail and
    `step=N` read from its own log;
  - the diagnosis ahead of a capped job list, for a run and for a PR;
  - the job limit, including a failed fetch taking its slot once;
  - a third-party check on a generic `/runs/<id>` page sorting first, yet neither
    tagged `[actions]` nor picked as the PR's first Actions job;
  - `[actions]` tagging;
  - neither the token nor the pre-signed URL appearing in any output.

Not yet covered:

- why GitHub refuses gofer's token the logs of a `kubernetes-sigs` job (see Scope);
- the real blob host and its redirect behaviour;
- the step-log endpoint against real GitHub, including which scopes it needs and
  whether its lines carry the same timestamp prefix;
- real log formats beyond the documented timestamp prefix and `##[error]`
  annotation;
- a run inside the agent sandbox;
- an end-to-end conversation turn in which the model picks these tools.
