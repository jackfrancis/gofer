# AGENTS.md — gofer project context

Canonical, tool-neutral context for humans and AI agents working in this repo.
Keep it current.

## What this is

gofer is a lean, secure Go backend for **GitHub worklist management**. It retrieves
a user's GitHub work (issues, PRs, comments, reviews), persists it, and decorates
each item with agent-derived metadata (relevance, impact, engagement, urgency →
rank). A landing page renders the user's work ordered by that metadata.

**Agent runs execute on AEI.** gofer is an opinionated application stack for its
mission with the agent-runtime layer factored out behind a single seam
(`ingest.Dispatcher`). The selected backend is the Agent Execution Interface (AEI,
`github.com/jackfrancis/agent-execution-interface`), in `internal/runtime/aei`: the web
tier dispatches each run to a pre-installed AEI control plane, which launches gofer's
runtime image (`cmd/runtime`) per run on the substrate the app's provider class names.
gofer needs AEI on the cluster and does not run standalone. A backend is still purely
additive — a package under `internal/runtime/<name>` plus one wiring line in
`cmd/server`, with no change to gofer's interfaces (see `internal/runtime` for the
contract).

The `gofer-clean` branch keeps the backend-less prototype (no-op dispatcher wired), as
a reference for the seam with nothing plugged in.

## Tech & layout

- Go 1.26.0. gofer imports its own packages, a few libraries (goldmark, bluemonday,
  prometheus, oauth2, modernc.org/sqlite — pure Go, so `CGO_ENABLED=0` holds), and AEI's two stdlib-only SDK modules (the app SDK
  `sdks/go/aeiapp` to dispatch, the runtime SDK `aeiruntime` for the agent binary).
  Those are consumed through `replace` directives in `go.mod` pointing at a SIBLING
  checkout (`../agent-execution-interface`), so the build needs that checkout present
  until AEI is published. gofer imports no provider and no client-go.
- Module: `github.com/jackfrancis/gofer`.

```
cmd/server/          web tier entrypoint (HTTP, UI); selects the backend
cmd/runtime/         agent runtime entrypoint (one-line shim over internal/runtime/aei)
cmd/snapshot/        takes and prunes data-volume snapshots (the backup CronJob runs it from the web image)
hack/restore/       guarded restore script + fake-kubectl regression tests (make restore)
hack/aeicheck/      the deploy-time AEI dispatch-listener check + fake-kubectl tests (make aei-check)
internal/runtime/    the backend contract (doc.go)
internal/runtime/aei/  the AEI backend: dispatcher (aei.go) + workload entrypoint (workload.go)
internal/config/     env configuration
internal/principal/  Principal{Kind, Subject, ActingUserID, Scopes}
internal/identity/   run-credential authority (Ed25519); web verifies, AEI mints
internal/vault/      delegated provider tokens; vended via internal/api credential broker
internal/ingest/     Dispatcher seam (THE runtime socket) + the reference NoopDispatcher
internal/agent/      agent-runtime WORKLOAD (the runtime runs it per run)
internal/worklist/   WorkItem + Metadata, Store + Ingestor seams, sort, scoring
internal/auth/       OAuth provider wiring + login/callback
internal/authn/      RequireAuth / RequireScope (cookie OR bearer)
internal/session/    HMAC-signed cookie sessions; Store seam for authenticated sessions
internal/storage/    SQLite persistence: worklist, encrypted vault, sessions (web tier only)
internal/snapshot/   CSI VolumeSnapshot take/wait/prune over raw HTTP (no client-go)
internal/api/        JSON handlers: worklist, agent sink, credential broker
internal/server/     web-tier router + middleware
internal/webui/      landing page
internal/github/     GitHub client (workload-only)
internal/checkout/   unpack a repo tarball safely; search/list/read it (workload-only)
internal/prow/       read a Prow job's public logs/artifacts from GCS, allowlisted (workload-only)
internal/llm/        LLM ranking / converse (workload-only)
internal/markdown/   render + sanitize assistant Markdown
internal/comment/    bulk comment: post the user's comment with the write grant; @ suggestions; create a pending review from a confirmed draft (web tier, ADR 0009, ADR 0010)
internal/ghcomment/  the web tier's only GitHub client: public check, post a comment, list assignees, create a pending review
internal/metrics/    Prometheus metrics
Dockerfile           web-tier image
Dockerfile.runtime   agent-runtime image (AEI launches it per run)
deploy/k8s/base/     app manifests: namespace, SA, config, statefulset (+ data PVC), service
deploy/k8s/overlays/dev/  base + agentapp.yaml (gofer's AEI dispatch registration)
deploy/k8s/backup/   optional snapshot CronJob + RBAC (`make backup-apply`)
deploy/k8s/model-gateway/  optional model gateway: agentgateway holds the model key + a shared budget (`make model-gateway-apply`)
internal/modelgateway/  end-to-end check of that gateway against the real binary (`make model-gateway-test`)
deploy/k8s/tls/      HTTPS: ACME Issuer, Traefik allow-list, Ingress (make deploy TLS_HOST=...)
```

## The runtime socket (what the agent runtime plugs into)

gofer builds the full *intent* of every agent action and hands it to a seam. The AEI
backend fills each one:

| Seam | gofer's side | The AEI backend |
| --- | --- | --- |
| `ingest.Dispatcher` (`Submit`/`Status`) | constructs each `RunSpec` | `aei.Dispatcher` POSTs it to the aei-controller (`AEI_DISPATCH_ENDPOINT`) as the `AgentApp` named by `AEI_APP`, on its dispatch listener (`:8081`), authenticating with a ServiceAccount token projected for the `aei-dispatch` audience (`AEI_DISPATCH_TOKEN_PATH`), from a web pod labelled `agents.x-k8s.io/dispatch-client: "true"` (AEI's NetworkPolicy admits only those to `:8081`) |
| `agent.Vendor` / `agent.Sink` | `agent.NewPlaneClient`: one HTTP client to the `/agent/*` plane | the runtime uses it with its run credential (vend a credential, read/write the worklist) |
| `agent.Run` (`internal/agent`) | the workload | `aei.RunWorkload` (`cmd/runtime`) executes it per run, out-of-process |
| `identity.Authority` | verify-only (`internal/identity`, `MINT_PUBLIC_KEY`) | AEI's control plane owns the keypair and is the sole minter |

`ingest.RunSpec` is the provider-neutral run intent (task ref, parameters, scoped
identity, deadline). gofer constructs one for `github-ingest` (backfill/refresh) and
`github-converse` (Discuss / review) and submits it to the `Dispatcher`. The backend
contract is in `internal/runtime`; the selection point is in `cmd/server`.

AEI itself (the aei-controller in `aei-system`, the `agents.x-k8s.io` CRDs, the signing
authority, and an `AgentProviderClass`) is installed from the AEI repository, not by
gofer. gofer's `AgentApp` (`deploy/k8s/overlays/dev/agentapp.yaml`) names a provider
class by capability; the default is `isolated` (agent-sandbox pods under gVisor, run in
the `gofer-runtime` namespace), and `k8s-job` is the lighter alternative.

## The domain (100% gofer's own, unaffected by the runtime)

`WorkItem{GitHubRef + Metadata}`. Axes relevance/impact/engagement/urgency → rank →
priority band. Server-side sort. Scoring is a pure function of an item's Signals, so
the worklist is re-scored at read time. Job types the workload implements:
github-ingest, github-enrich, llm-rank, github-converse, github-research.

Ingest leaves out another contributor's draft pull request, whatever the user's
relationship to it (`github.searchItem.othersDraft`); the user's own drafts stay.
Hits are reconciled by ID first, and the copy with the newest `updated_at` decides
the state, since the searches are walked at different moments; GitHub's timestamps
are to the second, so on a tie the draft wins (`github.newerState`). The fetch still
counts as complete, and it names the drafts it saw with their `updated_at`
(`github.Fetch.OthersDrafts`), so a stored PR converted back to draft is retired
(thread kept) even by a partial refresh (`agent.retireDrafts`), and resurfaces once
marked ready for review. Retiring one records the confirmed state (`Draft`, and
`GitHub.UpdatedAt` advanced to the draft result's, `agent.retireDrafts`); a draft
with no stored row is stored already retired, never on the radar, to remember it.
The stored GitHub state never moves back in time: a hit older than it
(`agent.newerState`, the draft winning a tie) does not write its GitHub fields
(`agent.keepNewerState`), though it still counts as open for retirement, so an older
ready copy can neither reopen a draft nor roll back the time a later draft result is
judged against. Which searches matched is current even when a payload is not, so a
stale hit of a live item still carries its review-request membership onto the stored
item (`agent.withReviewMembership`), reconciled as any hit's is. A stale hit that
changes nothing is not written, but the run still goes on to enrich (CI can move on
the same commit, and a request kept by a partial search needs the timeline read): an
empty write-back never means nothing to enrich. A stale hit of a retired item writes
nothing. A draft result never retires a stored ready PR GitHub updated strictly later
(`agent.readyAfterDraft`): the search index lagged it. Draft observations pass the
public-only gate like hits, so one from a repository not confirmed public is
evidence of nothing.

## Security invariants (the contract a runtime must honor)

Wired on the web-tier side; they define what a runtime must respect:

- **Two disjoint auth planes.** A browser session authenticates `/api/*`; a run
  credential authenticates `/agent/*`. They never cross (audience-bound tokens).
- **Verify-only web tier.** It holds only the run-credential public key and can
  never mint. A runtime's control plane would be the sole minter — so a web-tier
  compromise cannot forge a job token.
- **Credentials vended on demand.** Downstream provider tokens come from
  `GET /agent/credential` (behind RequireScope) and are held by the workload for the
  run only — never a standing secret, never logged, never persisted. gofer core
  packages import no provider client; only the workload (`internal/agent`) does,
  apart from the web tier's narrow `internal/ghcomment` (ADR 0009).
  The broker vends through `auth.Handler.Credential`, which refreshes expiring
  (GitHub App) tokens; the refresh token never leaves the web tier.
- **The chat-model token is the same class.** The web tier holds only the non-secret
  coordinates (`AI_CONNECTIONS`) and brokers the token; the workload runs the model.
  In the optional model-gateway profile (`AI_GATEWAY_URL`, ADR 0008) neither holds
  it: agentgateway does, runs present their run credential (`ai_auth=gateway`), and
  the gateway asks gofer (`/agent/model-access`) before attaching the key.
- **Least privilege.** Tokens vended to runs are READ; only gofer metadata is WRITE.
  The one write-capable credential is the opt-in write grant (ADR 0009, vault
  provider `github-write`, a separate classic OAuth app with `public_repo`), and it
  never leaves the web tier: `auth.Handler.Credential` and the broker each refuse it,
  independently. It posts only what the user confirmed: their own comment, with no
  model involved, or the comments they kept from a draft review, created as a
  PENDING review that only they see until they submit it on GitHub (ADR 0010).
  Where the deployment allows it (`GITHUB_WRITE_TOKENS`, ADR 0011), the user may
  connect with a classic personal access token instead, in the same vault slot:
  exactly `public_repo` (checked at connection and again before every write), an
  expiry within 90 days, the signed-in account, an https `BASE_URL`, stored only when
  no usable grant is connected. gofer cannot revoke a token, so disconnect and logout
  delete its copy and send the user to GitHub's token settings; a token is never sent
  to an OAuth app endpoint.

## Conventions & gotchas

- Each `.go` file has exactly one `package` clause.
- gofer imports no client-go and no provider. The runtime socket is the
  `ingest.Dispatcher` interface; `cmd/server` wires `aei.Dispatcher`, and
  `NoopDispatcher` remains as the reference no-op (used by tests).
- Interfaces are the extension seams: `identity.Authority`, `worklist.Store` /
  `worklist.Ingestor`, `vault.Vault`, `session.Store`, `ingest.Dispatcher`, and the workload's `agent.Vendor` /
  `agent.Sink`.
- The web tier fails fast at startup without `AEI_DISPATCH_ENDPOINT` and `AEI_APP`,
  or when `AEI_DISPATCH_TOKEN_PATH` is set but holds no readable token.
  With them set but no reachable aei-controller (e.g. `make run`), sign-in works but
  dispatch fails, so the worklist stays empty.
- Web-tier state (sessions, delegated tokens in the vault, the worklist and its
  threads) is durable when `DATA_DIR` is set: one SQLite database
  (`internal/storage`, pure-Go `modernc.org/sqlite`) on the StatefulSet's
  PersistentVolume, with tokens sealed by `VAULT_ENCRYPTION_KEY` (required with
  `DATA_DIR`; startup fails without it). With `DATA_DIR` unset (`make run`, tests) the
  in-memory stores are used and a restart loses the state. The web tier is a single
  replica by construction. Pre-auth OAuth flow state and the ingest backfill tracker
  are always in memory. See `docs/adr/0004-sqlite-persistent-storage.md` and
  `docs/backup-restore.md`.
- Recovery: `make deploy` sets the data volume's reclaim policy to `Retain`;
  `make backup-apply SNAPSHOT_CLASS=<class>` schedules snapshots and `make restore`
  swaps the claim for one restored from a snapshot. The restore script validates the
  claim before downtime and pauses/checks backup activity. Replacement/startup
  failures after attempted claim deletion leave backups paused for manual recovery.
  Once the restored rollout is confirmed, finalization errors are reported separately
  and the prior backup schedule is restored without restarting or rolling back the app.
  Never strip storage finalizers to force a restore. Never write an environment's
  names (cloud resource groups, registries, hostnames, snapshot classes) into this
  repository: they are make parameters or live with the cluster.
- Change an existing work item with `worklist.Store.Update` (one atomic
  read-modify-write), never `List` then `Upsert`, which overwrites anything written
  in between. A run's write-back is merged into the stored item
  (`worklist.MergeRunItems`): runs own the GitHub fields, signals and scoring; the
  thread, read cursor, hidden flag and creation time always come from the store.
  Hidden and closed items are off the radar
  (`WorkItem.OnRadar`), and no bulk action (Review all, the second-opinion batch)
  draws from them. A
  run adds a thread turn only through `POST /agent/worklist/reply`
  (`worklist.AppendReply`). A write-back that carries no model proposal or research
  adjustment (`Signals.Proposed` / `Signals.Research`) keeps the stored one
  (`worklist.KeepModelSignals`), and every workload pass sends only the model signal
  it derived (ingest and enrich none, rank only the proposal), never the copy it
  read, which a newer write may have replaced. A converse or research run writes its
  research adjustment only through `POST /agent/worklist/research`
  (`worklist.ApplyResearch`), never the whole item it read before the model call, so
  a refresh that landed meanwhile stands. An adjustment is dropped if a turn it read
  (`ResearchAdjustment.Turns`) is no longer in the stored thread (the conversation
  was cleared); otherwise adjustments are ordered by `AppliedAt` (required), and an
  older one is dropped, so a resend cannot undo a later result. A refresh keeps the
  last known `ReviewRequestedBy` of a review that may still be pending (search does
  not name the requester; enrich's timeline read replaces it), so a proposal the bot
  policy set aside stays set aside. Only a complete review-request search
  (`github.Fetch.ReviewRequestsComplete`) confirms a request was removed; after a
  partial one the stored request is kept, and the chained enrich drops that kept
  request only if the whole timeline shows no request that may include the user still
  outstanding (`Activity.RequestEnded`: the personal request withdrawn or fulfilled by
  the user's own review, and every team request withdrawn or made before that review,
  since team membership is unknown and the search counts team requests). A request
  the search showed is never dropped by the timeline, since a team request names the
  team, not the user. A retired item
  carries no request into its resurfacing.
  Before the merge, every write-back path applies
  `worklist.RunPolicy` (the agent plane's ingest handler, and the in-process
  `StoreSink`): workload metadata is stamped `OriginAgent`, so a run cannot claim the
  `OriginUser` override that `Resolve` leaves unrescored, and
  `Signals.ReviewRequestedByBot` is derived from `ReviewRequestedBy` against
  `BOT_REVIEWERS` (case-insensitive), replacing whatever the run sent. The agent
  plane's reads (`GET /agent/worklist`, its `select` and `item` reads, and
  `StoreSink.Item`/`Select`, all through `RunPolicy.ClassifyReads`, which classifies
  `List`, `Get` and `Briefs`) re-derive the flag too (`RunPolicy.Classify`), so a run
  ranks by the current bot list, and so do the user's
  (the JSON worklist and the radar get `RunPolicy.ClassifyReads(store)`), so the rank
  shown and the rank Discuss explains agree. The rank job picks candidates by
  `worklist.BaselineScore`, which clears the previous proposal and research (the
  top-N selection, `worklist.Select`). A proposal records the bot
  classification it was made under (`AxisProposal.ReviewRequestedByBot`); read it
  through `Signals.CurrentProposal`, which drops it once the classification changes. The requester is
  the actor of the user's pending review request on the timeline: the latest
  `review_requested` for them, until a `review_request_removed` or their own submitted
  review ends it (a comment does not). The timeline is paged by its `Link` header up
  to a cap, inside enrich's reserve-adjusted deadline; a timeline longer than the cap
  leaves both waiting timestamps unset and the stored requester unchanged
  (`Activity.TimelineComplete`). The deterministic scorer discounts a bot's request (no awaiting-me
  pressure, low floors) unless the user is also an assignee. The web and runtime images must ship together: a runtime
  from before these rules posts its reply inside the item, where it is now ignored,
  and a web tier from before them has no research route.
- An item is WAITING (`WorkItem.Waiting`, `internal/worklist/waiting.go`) when the
  ball is in someone else's court; the radar lists waiting items in their own section
  after the rest and above the hidden section, ranked the same way
  (`Resolution.Waiting`; the JSON worklist's `waiting`). It is not a score: it says
  whose turn it is, not how much the item matters. Enrich decides it from the
  timeline (`github.ItemActivityFor`): the last human word is the user's own comment
  or review (recorded as `Signals.LastWord`, text cut to `MaxLastWordBytes`, mentions
  parsed outside quotes and code, never a team or an `@name[bot]` account), or an
  approving or changes-requested review on an item the user did not author. A bot's
  comment never takes the last word (GitHub type `Bot`, a `[bot]` login, or
  `BOT_REVIEWERS`, sent to every run as run parameter `bots`). A review request still
  awaiting the user outranks any later word: their personal request with no comment
  or review by them since, or a team request attributed to them. A team request is
  the user's only on evidence: THIS refresh's `review-requested:@me` search returned
  the item (`ActivityOptions.ReviewRequested`, from `freshRequests`; a request carried
  over from an earlier search is no evidence of who made a later one, and a
  standalone enrich attributes none), and no personal request of theirs explains the
  hit. The confirmation is recorded (`Signals.TeamConfirmed`: each team request it
  covered, by team and request time), and unless this refresh's review search was
  walked whole without returning the item (cut short, or none ran, as in a standalone
  enrich) it stands while every one of those requests is still outstanding
  (`ActivityOptions.Confirmed`, `TeamConfirmation.StillOutstanding`), covering only
  them: a partial search neither confirms a new request nor cancels an unchanged one,
  and another team asked since neither ends it nor joins it. While it stands it wins
  over a fresh hit too, which it already explains more specifically, so asking (and
  later withdrawing) another team never resets the requester or the date. Which team names them is unknown when several are outstanding, so the item
  awaits them only if every outstanding team request was made since their last
  comment or review (since the latest), and the requester (`ReviewRequestedBy`, so a
  bot's team request scores as a bot's) is named only if those requests share an
  actor. On a pull request the user did not author, a push or
  ready-for-review is the author's answer. A commit's date is never its push time
  (commits are pushed late, clocks drift), so pushes are judged by the head
  (`ActivityOptions.PullRequest`): a review's last word stands while the head is its
  `commit_id` (an unreadable head ends the wait); a comment's last word stands while
  the head is the one first seen with it (`LastWord.Head`, passed back as
  `ActivityOptions.Prior`). Once a push answers a last word, `LastWord.Answered`
  records it, and no later read undoes it, not even one that cannot read the head.
  Both apply only to the same version of the last word (`LastWord.Key`: the ID and a
  digest of the whole body taken before the text is cut, so an edited comment is a
  new question, anchored afresh). The last word is kept whoever's turn
  it is (a review request may outrank it for a while) and across every refresh. Force-push and
  ready-for-review events, which GitHub dates, answer a comment too. Words are
  ordered among themselves by when they were made, so an edit never jumps a reply;
  against those events, for the wait's date, and, when it is the last word, as the
  user's engagement (a review request made before the edit no longer outranks it), a
  comment counts from its last edit (`updated_at`). A review with no
  summary is read through its inline comments, up to `reviewCommentPages` pages; one
  longer than that has no text. On the user's own item a reviewer's decisive review is the
  user's turn, and a wait with no recorded last word (stored before `LastWord`
  existed) is not trusted; an undecisive last word with no text never waits. With a
  chat model, a reading pass between enrich and rank (`runReadTurns`, at most
  `turnReadLimit` calls a pass) asks the model to read a non-decisive last word
  (`Signals.NeedsTurnReading`) and writes the pass's readings alone, in one store
  transaction, through `POST /agent/worklist/turns` (`worklist.ApplyTurns`), never the
  items it read: a reading is dropped unless the stored last word is still the one it read (keyed by
  the last word's ID and text) and the item is not hidden. It is a model signal kept
  like the proposal (`KeepModelSignals`) and applied only while current
  (`CurrentTurnReading`): `my_turn` keeps the item with the user, `nothing_pending`
  and `waiting_on` keep it waiting, and a `waiting_on` login counts only if the last
  word mentions it or it is the author. New activity always wins over a reading. A
  refresh keeps the waiting timestamps only for an item whose GitHub `updated_at` is
  unchanged, and the last word always (`carryTurnSignals`); `AwaitingMeSince` dates
  the review request, so it goes whenever the reconciliation leaves no request (a
  complete search confirmed it removed; a partial one keeps both). The rank selection leaves waiting
  items out before its top-N cutoff (`Selection.Top`), and Review all PRs and Get 2nd
  Opinion skip them (`WorkItem.Actionable`); Reset Conversations still covers them.
  `RunPolicy.Apply` bounds the last word and reading on every write-back.
- A run never reads the whole worklist: its threads grow without bound and the agent
  plane caps a response at 8 MiB. `agent.Sink` has only bounded reads, served by the
  same `internal/worklist/select.go` functions in process and on the plane:
  `Item` (`GET /agent/worklist/item`, `worklist.ReadItem`: one owner-scoped item, at
  most `MaxItemBytes` encoded, whose `ThreadWindow` is the dispatched turn and the
  visible history before it, or the turn and its reply if the store shows it
  answered; a Discuss run without a turn asks for `worklist.LatestUserTurn`, which
  the read resolves against the stored thread; research, naming none, gets the newest
  turns, and so does Discuss's research pass, which reads again after its reply
  lands and skips research if the reply was dropped; position keys are written into legacy turns) and `Select` (`GET /agent/worklist/select`, `worklist.SelectFrom`: no
  threads, at most `MaxSelectLimit` items and `MaxSelectBytes`, with a cursor).
  Stored data past the bounds still reads: an oversized message is cut with a note,
  and an item over `MaxBriefBytes` is projected (`worklist.Project`), which marks
  each cut field (`WorkItem.Projected`) so `MergeRunItems` restores it on write-back
  unless the run rewrote it. Never store a projection. A cut keeps what the score
  reads (the strongest labels, every distinct reason), so a projected item scores as
  the whole item does for the ranker and Discuss. Projection drops a research
  adjustment's source-turn list rather than shorten it, so the visibility check
  withholds research it cannot check whole. Ranking reads the top N by `worklist.BaselineScore`
  of the whole item, projected only after it is chosen (ties keep insertion order); ingest and enrichment page through
  every item with `readAll`, and ingest fails if it cannot read the stored items.
  Enrichment must keep reaching every open item; never cap it. The stores serve these
  reads directly (`Store.Get`, `Store.Briefs`), so a page never decodes every thread.
  Selections have their own route so a runtime facing an older web tier fails instead
  of getting the whole list. A write-back need not carry a thread (`MergeRunItems`
  keeps the stored one); research is written back alone (`Sink.Research`). A user message is capped at 16 KiB (`webui.maxMessageBytes`).
- Replies can land out of order, so a turn's position does not say what it answers.
  Every new turn gets an ID; an agent turn's `ReplyTo` names the user turn it
  answers; dispatch carries the turn a run must answer (`worklist.TurnRef`, run
  parameter `turn`). Review and pending state (`HasReview`, `ReplyPending`, …) follow
  those links. Turns stored before IDs are keyed by position (`WorkItem.TurnKey`),
  and a legacy reply answers the user turn directly before it.
- The read cursor (`WorkItem.ThreadReadThrough`) is the key of the newest agent
  turn the owner has seen, never a time: reply times are stamped by the run and
  replies land out of order. Delivering the thread page is the read receipt (the UI
  has no script): after a GET renders and flushes to the client without error,
  `webui.Thread` acknowledges the newest reply in the snapshot it rendered
  (`worklist.MarkThreadRead`), so a reply landing mid-request stays unread, and a
  HEAD or failed write marks nothing. A middleware that wraps the
  `http.ResponseWriter` must implement `Unwrap` (as `statusRecorder` does), or the
  flush fails and no thread is ever marked read. The
  cursor only moves forward; a re-render of an already-read thread skips the store.
- Hiding an item (`internal/worklist/hide.go`) is permanent until the owner unhides
  it: no run clears `Meta.HiddenAt`, whatever changes on GitHub. A hidden item
  carries no gofer judgment: `worklist.HideItem` deletes its signals (all but the
  labels) and its scoring metadata (`worklist.StripScoring`), `MergeRunItems` strips
  every write-back while it stays hidden (inside the store's update, so the run sends
  its fresh signals: the owner may unhide the item before they land), `SetResearch`
  drops research for it, `Rescore` leaves it unscored, and the selection a run reads
  (`Selection.Open`, which means on the radar) leaves it out of enrichment and
  ranking. The radar lists open hidden items in a greyscale section below the ranked
  list (`Resolution.Hidden`, most recently hidden first) with an Unhide button and no
  Discuss: a hidden item takes no new turn, retry, 2nd opinion or turn hide/unhide
  (`appendTurn`, `ThreadRetry`, `HideMessage`), and its thread page is read-only.
  Unhide (`worklist.UnhideItem`) brings it back empty. Its pre-hide CI summary is
  kept but marked `CISummary.Stale` (labeled as last known) until enrich reads the
  head; `Equal` compares the mark, so that read is written. An open item is flagged
  `Meta.AwaitingRefresh`: unscored by `Rescore` and by Discuss's ranking context,
  left out of the ranker's top-N selection, shown as waiting for the next refresh. A
  retired one is not flagged, and a retirement's merge clears the flag, since no
  refresh revisits a retired item. Unhide then calls `Ingestor.RefreshFollowUp`,
  which owes the owner one ingest run reserved after the call until the backend
  accepts one (`settle` pays it, whoever reserved the run). It tries at once; while
  the debt stands a server-side worker retries through the refresh gate (which asks
  the backend whether the run in flight ended), backing off after a refusal, so
  nothing waits on a page polling. Never consume the debt on a run's end: only an
  accepted run reserved after it pays, and every attempt (the unhide's own and the
  worker's) reserves only while the debt still stands, checked under the reserving
  lock after any probe. Each unhide joins a standing debt and gives it a full
  `followUpGiveUp`; in memory only. The refresh's write-back clears the flag, since runs own
  `Metadata`. A hidden item that closes leaves the section, still hidden.
- The radar's project (repository) filter (`internal/webui/projects.go`) is a view
  preference kept in the `gofer_projects` cookie, bound to the account that saved it
  (none = all projects). It only drops items from the already-ranked list and the
  hidden section (its checklist names both lists' repositories), never re-sorts. The cookie never decides what a bulk action touches: each bulk form posts
  the selection its page was rendered with (`scope`), narrowed to the projects the
  checklist names, so neither a filter changed in another tab nor a saved repository
  that left the radar can widen it; an unreadable form changes nothing, and only a
  URL-encoded body is read (`ParseForm` leaves multipart unread, which would drop the
  scope). Only an absent `scope` means every project; a present one that is not exactly
  one strict encoding (`parseSelection`), empty included, acts on nothing. Apply clears
  the filter only for "Select all" or when every project of the checklist the user saw
  is checked (form `offered`), never by comparing with the store at POST time, where an
  unchecked project that retired meanwhile would turn the selection into "all" and
  admit the next new project. Review all
  PRs and Get 2nd Opinion act only on items the radar shows (`WorkItem.OnRadar`, the
  rule `worklist.Resolve` applies); Reset Conversations also clears hidden and
  completed items in scope, and its count includes them (`Resolution.OffRadar`, from
  the same read).
- The radar's label filter (`internal/webui/labelfilter.go`) applies after the project
  filter, under the same rules: a view preference in its own account-bound cookie
  (`gofer_labels`), dropping items without re-sorting, the Waiting on others and hidden
  sections included. Its checklist offers every label on the radar (ranked, waiting or
  hidden) in every project,
  whatever the project filter selects, merged by name with Unicode case folding
  (`labelKey`, as `strings.EqualFold` compares), plus "No label"; counts follow the
  project filter. An item shows when it carries any selected label. Apply judges
  "Select all" from the rendered checklist (form `offered`), as the project filter does.
  Each bulk form posts the page's label selection (`labels`, narrowed to the labels the
  checklist names) beside `scope`, and the actions cover only items both select
  (`viewScope`, read by `formScope`). Only an absent `labels` means every label; a
  present one that is not exactly one strict encoding (`parseLabels`), empty included,
  acts on nothing.
- A user turn keeps one answer: `worklist.AppendReply` drops any reply to a turn that
  already has a substantive one (a notice such as `NoticeReconnect` is not an answer),
  so the first answer stored wins. That store rule is what makes retrying a turn safe,
  across a restart or a first run that finishes late. The ingest package's in-flight
  gate (`internal/ingest/gate.go`) holds one run per (owner, task, item, turn) and
  only saves the cost of a duplicate run: it is in memory and guarantees nothing. A
  converse dispatch it turns away returns a `*ingest.RunSharedError`
  (`ErrRunShared`), not nil, so a retry knows no run of its own started.
  A second opinion posts its own user turn, so it is never a duplicate.
- Get 2nd Opinion (`POST /items/second-opinion` on a thread, and in bulk; "the review
  panel" in the code, though no reviewers confer: a blind review, then an agreement
  check) posts
  `worklist.IndependentReviewPrompt` as a review request (a panel review is
  `KindReviewRequest` with that text, `Message.IsPanelReview`; the same text typed into
  Discuss is not one) for a blind review by a model other than the item's first
  VISIBLE reviewer (`FirstReviewModel` skips a hidden review or answer;
  `webui.panelChoices`, resolved against the item as stored, inside the update that
  appends the turn). When any review's answer lands on the agent plane, the same store
  update may chain the consensus synthesis turn (`worklist.ApplyReplyAndChain` →
  `ChainSynthesis`): only when a visible answered panel review is not yet covered by a
  synthesis and at least two visible answered reviews, not all by one known model, are
  there to compare. Either review can land last. The synthesis records the reviews it
  weighs (`Message.Reviews`; a legacy request's are rebuilt from the visible reviews
  asked and answered before it, and `ThreadWindow` writes them onto its copy before trimming, so
  a review that does not fit makes the run decline); the plane hands it to `Ingestor.Synthesize` (default model), off
  the reply's request. Its run's read (`ThreadWindow`) keeps those reviews first, the
  model is sent ONLY those request/answer pairs (`WorkItem.SynthesisInputs`, each
  request followed by its own answer, since overlapping reviews answer out of
  position), never the rest of the thread, and the run declines with a `NoticePanelInputs` notice,
  never a verdict, if any is hidden, gone or did not fit, or fewer than two remain.
  The radar's verdict is the newest answered synthesis request's, through its reply
  link (`ReviewVerdict`), never the last reply to arrive. Nothing tracks run IDs in
  memory: a failed or declined synthesis stays retryable wherever it sits in the
  thread (`RetryableSynthesis`, by the default model), so a Discuss turn since never
  strands it, but only while its reviews fit the read its run would get
  (`SynthesisFits`); otherwise the page says a smaller comparison is needed. A retry
  starts a new attempt on the turn (`WorkItem.Restamp`, `Message.Attempt`), carried to
  the run as its `attempt` parameter (`TurnRef.Attempt`), and a notice names the
  attempt its run was dispatched as (never the stored turn's, which a later retry may
  have changed), so only that attempt's notice ends it (`noticed`). A retry whose
  dispatch shares a run already in flight (`ingest.ErrRunShared`) puts the earlier
  attempt back (`RevertAttempt`, `webui.revertShared`), since that run's notice is
  the one that will come. The thread page shows a working
  cue while a reply is pending and offers no panel then; the handler refuses one too.
- The scheduled refresh (`ingest.Refresher`, `REFRESH_INTERVAL`, off when unset or
  0, at least 1m otherwise) dispatches a github-ingest run per GitHub owner with
  stored work (`worklist.OwnerLister`), and only for owners
  `auth.Handler.ScheduledRunAllowed` confirms: GitHub configured, no logout pending,
  a vendable credential stored and readable, and the login recorded on it
  (`vault.Credential.Login`, set at sign-in and kept across refreshes) still in
  `ALLOWED_GITHUB_LOGINS`. A read error skips the owner. With an allow-list set, a
  credential stored before gofer recorded the login gets no scheduled runs until
  that user signs in again. (`SignInRequired` is for the page and answers false on
  a read error, so never use it to gate a run.) A tick and a Refresh click both go
  through the gate under `refreshGate`, which asks the backend about a run that
  still looks live: neither starts anything while any ingest run for the owner (a
  backfill, a Refresh, an earlier tick) is running, so overlapping ticks give one
  run per owner and two ingest runs never write one owner's worklist at once.
  Neither waits out a finished run's deadline; the empty-worklist poll
  (`ingestGate`) still does. Nothing forces a run past the gate, so a reservation is
  never replaced before its Submit returns. Turn the schedule on only against an AEI
  that contains provider failures (archive-plan item 11, AEI PR #21): scheduled
  runs dispatch with nobody watching.
- Each pass of the scheduled refresh goes least recently served first, so an owner
  whose submission failed (AEI's queue full) goes first on the next pass and owners
  share a capacity limit evenly.
- With a default chat model configured, each scheduled tick also runs the research
  pass (`Refresher.ResearchOnce`, on the same `REFRESH_INTERVAL`, for the same
  confirmed owners): a github-research run (`Ingestor.ScheduledResearch`) for every
  item `worklist.ResearchStale` reports: on the radar, no human override, at least
  one visible turn, and research missing or no longer matching what it read. Judge
  research by what it READ, never by when it was applied (a refresh or a new turn
  can land while the model works): the GitHub version it read
  (`ResearchAdjustment.GitHubUpdatedAt`, stamped by `agent.stampResearch`) against
  the stored one, and its `Turns` against the thread (a read turn gone or hidden, or
  a turn in the window a research run would read now, `ThreadWindow` with no turn as
  `ReadItem` serves it, left unread: posted or unhidden since; a window holding fewer
  turns than were read does not count). The refresh and research passes alternate
  going first each tick (`Refresher.Tick`); within the research pass owners take
  turns starting after the last owner actually served (`lastResearched`), and
  eligibility is re-checked right before every research
  dispatch, since research vends no GitHub credential that a logout would revoke. A
  research run writes back only the adjustment and never adds a turn, so it is how a
  failed post-reply adjustment is retried. The gate holds one research run per
  (owner, item), holds it through a dispatch of unknown acceptance
  (`uncertainHold`), and skips an item while a Discuss run for it is in flight.
  Research, like Discuss,
  never sends a hidden turn to the model: the research read window
  (`worklist.ThreadWindow` with no turn) and the prompt (`internal/llm/research.go`)
  both leave hidden turns out, and the adjustment's `Turns` list only what it read.
- The radar watches a refresh version: the highest generation of an ingest run
  known to have ended (each accepted ingest run takes the owner's next generation).
  `signedInView` reads it BEFORE the items and from local state only
  (`Ingestor.LocalRefreshState`: no backend call, so a slow AEI never delays the
  radar; a run gofer has not seen end counts as in flight). Since a run writes
  before it ends, the items hold everything up to that version. `static/reload.js`
  polls `GET /items/refresh-status?done=` (every 3s while a run is in flight, 30s
  otherwise), which asks AEI within 5s (`Ingestor.RefreshState`), and reloads once
  the version moves: a run that ended since the render, even one that started and
  ended between polls, or one that ended while the next was already running. A
  failed status read counts as still in flight until the run's deadline. The script
  never reloads while a `<details>` menu is open or a form control has focus.
- With persistent storage, each owner's ingest gate state (generation counter,
  refresh version, and the current run's generation, id, time and whether its
  acceptance is known) is kept in `ingest_runs` (`ingest.RunLedger`, implemented by
  `storage.WorklistStore`, loaded by `Ingestor.UseLedger`; `server.New` fails, and
  the pod exits, if it cannot be read). A reservation is recorded BEFORE Submit, and
  not dispatched if it cannot be. Runs outlive a web-pod restart, so the new process
  restores a run within its deadline into the gate and keeps counting the version an
  open tab compares. A run past its deadline counts as ended when anything observes
  it (a poll, a render, a reservation), which raises the version.
- `Dispatcher.Submit` errors say whether a run might exist: one wrapping
  `ingest.ErrNotAccepted` is a definite refusal, and releases the reservation. The
  AEI backend judges the final attempt (after redirects) by watching its own
  transport, since the app SDK types only 429: refused if it was never sent (ctx
  already done, or no connection) or answered with a 3xx or 4xx. A 5xx (a gateway
  can answer 502/503/504 after AEI accepted the POST), an answer lost after the
  request went out, or an unreadable 202 leaves acceptance unknown, and the ingest
  reservation is held, unprobeable, until the backend's own deadline could have
  passed: AEI starts that clock at acceptance, so the hold runs from when Submit
  returned plus a grace (`holdUncertain`), or, for a record written before Submit
  returned, from that write (after any wait for earlier writes) through the whole
  bounded dispatch window (`holdPending`). Each hold is recorded (`RunUntil`). In
  process, a reservation whose Submit has not returned never expires. An ingest
  Submit is bounded at 30s; a browser's is detached from its request, while a
  scheduled one honours the Refresher's context, so `Stop` cancels it. A converse
  turn is rolled back either way.
- Without script, the radar's `<noscript>` refresh goes to `/?refresh=1`, whose
  render asks AEI (within 5s) instead of reading local state, so it stops once the
  run has ended.
- The page's CSP admits one third-party origin, GitHub's avatar host, for images
  only, for the header's avatar and each item's author's (`webui.AvatarOrigin`).
  Rendered Markdown never emits an `<img>` (`internal/markdown` renders images as
  links), so no reply can reach that host or make the browser fetch anything. The
  avatars are added by `static/avatar.js` after the load event, never as an `<img>`
  in the markup, which would delay a meta refresh until it loaded. An author's
  avatar is stored as a path on that host (`GitHubRef.AuthorAvatar`, set at ingest
  from the search hit's `avatar_url`: `u/<id>` for a user, `in/<id>` for an app's
  bot); read it only through `GitHubRef.AuthorAvatarPath`, which admits nothing
  else, since it reaches the page. `RunPolicy.Apply` drops any other value a run
  writes back (projection does not bound the field). The worklist and Discuss pages
  load `avatar.js` whatever the viewer's provider; other views only for a header
  avatar.
- GitHub labels render in their own colors on the radar. The CSP allows no inline
  style, so a label carries a `ghl-<hex>` class and the page links `/labels.css?c=…`
  (`internal/webui/labels.go`), a stylesheet generated from the query alone. Colors
  live in `GitHubRef.LabelColors`, set at ingest; read one only through
  `GitHubRef.LabelColor` (six hex digits or nothing), since it reaches CSS.
- A pull request's CI shows under its labels on the radar and at the top of its
  Discuss page (one `ci` template): jobs succeeded, jobs in progress, and each failed
  job's name, linked where GitHub's pull request links it (a status's target URL,
  e.g. the Prow job page, or a check run's details URL) (`GitHubRef.CI`, a
  `worklist.CISummary`). The links are `FailedURLs`, index-aligned with `Failed`, so
  `failed` keeps its stored shape (a list of names) for an older web tier; read the
  pairs only through `CISummary.FailedJobs`, which admits only http(s) links
  (`worklist.CIJobURL`), since they reach an href. Counts follow the
  outcome GitHub reported (`github.ciOutcome`: skipped, neutral and stale are in no
  count), not the sorting class. Enrich sets it from `github.CIStatusForPR`
  (`CIStatus.Summary`, bounded by `CISummary.AddFailed`) in a second pass, after
  every timeline read, so CI never costs an item its ranking signals. Statuses and
  check runs are read independently (one unreadable = `Partial`; both = error, and
  the stored summary stands). A write-back that carries no CI keeps the stored one
  (`worklist.KeepCI`, in the merge), so every pass but enrich's CI pass (refresh,
  retire, rank, and an enrich write that changed only the timeline) sends none: never write back CI from a run's read, which may be a projection or
  older than an enrich that landed meanwhile. CI pages after the first are pinned to
  the commit the first resolved, since a named ref can advance between pages. After
  a projected read, enrich writes the fresh summary through `WorkItem.SetCI`, which
  drops the projection marker, since a cut summary compares equal to fresh data it
  does not match. A retired or hidden item's CI (enrich reads neither) is labeled as
  the last known result; hiding keeps it, since it is a GitHub field, and unhiding
  marks it `Stale` (labeled as from before the hide) until enrich reads the head. It is display-only: nothing scores on it.
- The bulk comment (ADR 0009) is on only with `GITHUB_WRITE_CLIENT_ID`/`_SECRET`,
  which config requires to name a different OAuth app from sign-in (a classic grant
  accumulates scopes per user and app, which would put `public_repo` on the token runs
  hold). The grant is connected under its own cookie (`session.StartGrant`, path
  `/auth/github-write`), so the session survives, and only by the GitHub account the
  session belongs to. Logout records the write grant's pending revocation in the same
  commit as the session's end (`beginSessionLogout`), whether or not commenting is
  still configured. The flow is bound to the session that began it
  (`OAuthFlow.SessionKey`), and the callback re-checks that session under the write
  grant's marker lock before storing, so an in-flight connection cannot outlive a
  logout. Storing a write grant is strict, and every failed connection revokes the
  token it was issued, that token only (`discardToken`), never the authorization. The web tier posts with it
  (`internal/comment` over `internal/ghcomment`, which checks `"private": false`
  first and follows no redirect). The preview registers the comment and the destinations
  it showed as eligible (item, repository and number) under a one-time nonce;
  confirming drops one whose item left the radar or now names another destination,
  and never adds or redirects one. A same-origin confirm consumes it, and a nonce
  gofer does not hold is refused, so nothing posts twice from one preview, across a
  restart included. A confirmed batch's results are its owner's to read without a
  live grant; only starting one needs it.
  Same-origin trusts `Sec-Fetch-Site: same-origin`, since `Referrer-Policy:
  no-referrer` makes a form's `Origin` "null". Posts are serial, a second apart; a
  rate limit stops the batch, the grant is re-read before each post (a disconnect,
  or a grant of another version, stops it), and a POST whose answer was lost is "unknown", never "not posted". Targets are items on the radar only (`WorkItem.OnRadar`), waiting items included (a nudge to whoever has the turn), and the Waiting section's rows carry the same checkbox (the shared `item` template). @
  suggestions are each repository's assignees, read with the user's read credential
  and cached in memory, counted against every selected repository (unchecked ones are
  unknown). `reload.js` does not reload while items are checked.
- Draft review comments (ADR 0010): at the bottom of a reviewed pull request's
  thread, "Draft Review Comments" (offered only with commenting configured; a connect
  link without the grant) posts a `KindDraftReviewRequest` turn and dispatches a
  converse run by the default model. The button is not shown, and the handler refuses
  the request, while a redraft would only repeat the offered draft
  (`WorkItem.RedraftRedundant`: unconfirmed, no visible turn after its request but the
  draft itself, and no earlier turn hidden or unhidden since the request was saved).
  That order is by stored revision, never time: a toggle's `Message.HiddenRev` (only
  `WorkItem.SetHidden` writes it, inside the saving update) against the request's
  `Message.ReadRev` (read inside its update, marked `ReadRevKnown`; a retry keeps
  it). A request with no such mark (saved before revisions) never counts as redundant.
  `AppendReply` clears all three on a reply. The draft's head is not compared with the item's (no ordered
  observation); a confirmation refused as out of date (`comment.ReviewResult.Stale`)
  offers drafting again on the result page, bound to that draft's turn and digest
  (`redraft=1`), admitted only while that draft is still offered and otherwise
  refused, never judged by the button's check. The run (`internal/agent/draft.go`) reads the
  head commit and patches (`github.Client.PullDiff`), annotates the lines a comment
  may anchor to, asks `worklist.ReviewDrafter` (`llm.ReviewDrafter`) for only the
  points the visible conversation agrees on (the user's own messages decide; an
  unsettled disagreement is left out and listed), keeps inline only comments on a line
  the diff showed (others move into the summary), and replies with a
  `worklist.DraftReview` on `Message.Draft` (marked `Approve` when the conversation
  agrees the pull request is ready to approve: the review body then ends with
  `worklist.ApprovalCommands`, `/lgtm` and `/approve` on lines of their own, a flag
  apart from the summary so its bound never cuts them, offered as its own checkbox
  and joined by `worklist.ReviewBody`; an approval stands only on a draft pinned to
  the revision the latest visible review read, head, base and whole-diff digest,
  `WorkItem.Reviewed` over the `Message.Head`/`Base`/`Diff` a review records from
  one settled snapshot that is also what its model is shown (`github.PullDiff`,
  settled on head, base branch, base commit and file count, re-read while the base
  moves; a reconnect at the final read discards the answer; no
  digest if the pull request moved while the model worked, or for a diff
  `PullDiff.Unverifiable` names: cut, unsettled, or a binary change with no patch;
  `github.DiffDigest`, matched by `ghcomment.DiffDigest`, which `PendingReview`
  checks against every file, read 100 a page to `changed_files` as the workload
  reads them, with every commented path and the base commit unmoved since (re-read
  if it moved; past the cap is out of date), before creating an approval,
  `ghcomment.Review.Diff`),
  checked by `AppendReply` whatever the run sent; the run flags a stale review
  whatever the model answered, and while the latest review is older than the draft
  the thread offers "Review the current revision", `WorkItem.ReviewOutdated`,
  `POST /items/review-again`; model-written draft text is LF-normalized and has
  every bot-command line made inert, `worklist.NeutralizeCommands` in `BoundDraft`,
  a suggestion block holding one (found by goldmark, lists included) turned into
  plain code rather than altered, so the checkbox is the only source of a command; a stored draft `BoundDraft` would change
  today is neither offered nor confirmable, `DraftReview.Current`; GitHub's submit
  dialog can replace the body, so the body is saved on the receipt in the claim,
  before GitHub is contacted, `DraftReceipt.Body`, and shown to copy), pinned to the repository, number, head
  commit and base branch it was anchored to, with each commented file's patch digest
  (`DraftReview.Patches`, `worklist.PatchDigest` = `ghcomment.PatchDigest`). Context lines anchor RIGHT only, as
  GitHub defines them, and the annotated diff stays within its byte budget, notices
  included. `AppendReply` bounds the draft (`BoundDraft`) and drops it from a reply to
  any other turn or when pinned to another pull request; no agent read sends a draft
  or receipt back to a run (`ThreadWindow`, and `RedactDrafts` on the whole-list
  read). The thread offers only the newest draft request's reply
  (`WorkItem.LatestDraft`; an empty redraft retires the previous draft), only while
  its pin matches the item, and shows the exact stored text beside each checkbox.
  `webui.CreateDraftReview` (same-origin only) reads the draft from the store, never
  the form (which names the turn, the draft's `Digest`, covering the pin, and the
  positions to keep), claims a durable `DraftReceipt` on the turn
  (`worklist.ClaimDraft`) before calling GitHub, and settles or releases it
  (`SettleDraft`, under its own deadline, never the request's): a draft creates at
  most one review, and a replayed confirmation shows the receipt and sends nothing.
  `comment.Service.PendingReview` creates it with the write grant only while the user
  has no pending review there, every commented file's patch still has its digest (a
  related merge into the base can move the merge base without moving head or base
  branch), and, checked last, the pull request is open with the draft's head and base
  branch; the POST has no `event`, so GitHub keeps it pending. Any 5xx on a ghcomment
  POST is an unknown outcome, never "not posted". gofer never submits a review.
- Commenting with a personal access token (ADR 0011, `internal/auth/token.go`) is
  opt-in (`GITHUB_WRITE_TOKENS=true`, which config refuses without the write
  client or without an https `BASE_URL`, loopback excepted; a bad value is never
  echoed). The token is stored as the write grant (provider `github-write`,
  `ClientID` = `auth.PersonalTokenClient`), so the lock, versioning, logout ledger and
  the broker's refusal all apply unchanged. `ConnectWriteToken` checks it with
  `GET /user` (classic: `X-OAuth-Scopes` present; scopes exactly `public_repo`; an
  expiry header within `maxTokenLifetime`; the session's account; the allow-list) and
  stores it only when nothing usable is connected or pending (`storeWriteToken`; an
  expired token is replaced, a pending disconnect nobody here is running is finished
  first through `claimLogout`/`runLogout`, and the OAuth callback refuses to replace
  a live token, revoking its new OAuth token). Read
  through `fromConfiguredClient` (a token counts only while tokens are allowed) and
  `tokenExpired` (an expired token is not connected and not handed out). Every
  stranded state keeps a way out on the radar (`CommentingState`): an expired or
  disabled token (stored while tokens were allowed) or a grant from a previous
  commenting app (`OtherClient`) gets Disconnect, and a pending cleanup gets Retry
  disconnect. The OAuth callback enforces it too (the thread page has its own
  connect link): it refuses to replace a live token or another app's grant. A token
  connection that ran a pending disconnect first reports what that left
  (`tokenConnectedNotice`, `priorWarning` on failure): an OAuth grant GitHub did not
  confirm revoked sends the user to revoke it, since gofer no longer holds it; a
  credential of unknown kind names both settings pages; the copy is called deleted
  only once the vault confirmed it; and the token just connected is never one to
  delete. A rejection
  is confirmed with `GET /user` (`tokenAlive`), never check-token, and logout's
  `revokeLocked` skips revocation for a token (`tokensSettings`). Report a deletion
  only from `logoutResult.tokenDeleted`/`cleanupPending`, never from which settings
  page is listed, and report `cleanupPending` even when revocation was `confirmed`
  (for a non-GitHub provider "confirmed" only means nothing to revoke: claim no
  revocation), and nothing about cleanup from a logout that did not own it
  (`cleanupUnknown`). A revocation GitHub confirmed is remembered for that
  credential version (`credLocks.revoked`, in memory), so a retry only deletes.
  Every ghcomment write checks the token's `X-OAuth-Scopes` on the
  request before it (`writeRepo`): more than `public_repo` writes nothing
  (`ErrTokenScopes`), and the service drops a widened token (`WriteScopesChanged`,
  which reports whether the vault confirmed the deletion; only then do the words say
  so); a missing header writes nothing either (`ErrScopesUnknown`) and keeps the
  credential. `ghcomment` reads an
  organization's OAuth App access restriction as `AppRestrictedError`, which the
  comment service words for the user.
- Schema changes are a new numbered file in `internal/storage/migrations/`; never
  edit an applied one.
- To ship a workload-only change (`internal/agent`, `internal/llm`,
  `internal/github`) to the kind cluster without restarting the web pod, use
  `make kind-load-runtime`.
- The conversation's tool loop is capped at `maxToolIterations` rounds
  (`internal/llm/converse.go`); the model's GitHub tools are in `internal/agent/tools.go`,
  and the whole-repository tools (`repo_checkout`/`repo_search`/`repo_list`/`repo_read`)
  in `internal/agent/repotools.go`. A checkout is the ref's tarball unpacked into the
  sandbox's temp directory for that run only, under gofer-side size caps (AEI's pod
  has no scratch volume or storage limit) — see `docs/adr/0003-repository-checkout-tools.md`.
- The runtime reports a run's model time and model/tool call counts on completion
  (`internal/runtime/aei/workload.go`): `measured` collects them and hands them to
  `rt.SetModelTiming`, which the AEI SDK merges into the one terminal report it sends
  from `aeiruntime.Main`. gofer never calls `rt.Complete`: the SDK owns delivery, its
  retry budget, and freeing a durable actor's slot once the report is acknowledged.
  The counts come from an `llm.Collector` on the run's context: model time is elapsed
  time with any call in flight (concurrent ranking calls count once), recorded in
  `llm.chat`, so any new model transport must go through `chat`; tool calls are
  recorded in the converse loop.
- The CI tools (`github_get_ci_status`, `ci_job_summary`/`ci_list`/`ci_search`/`ci_read`)
  are in `internal/agent/citools.go`. The `ci_*` tools open only Prow job URLs in
  allowlisted GCS buckets (`internal/prow/url.go`) and talk only to
  `storage.googleapis.com`, anonymously, through a client that strips credentials and
  refuses other hosts: the runtime's egress is open, so never widen this into a
  general URL fetch. See `docs/adr/0005-prow-ci-log-tools.md`.
- The GitHub Actions log tools (`gha_job_summary`/`gha_search`/`gha_read`) are in
  `internal/agent/ghatools.go`, over `internal/github/actions.go`. They store each job
  log in the same `prow.Session` (`FetchWith`), so the CI byte/file/time budgets, search,
  read and marker scan are shared. The log endpoint redirects to a pre-signed blob URL:
  the API client reads the redirect instead of following it, and a separate client
  fetches the blob, stripping credential headers and `Referer` (net/http sets it to the
  previous, pre-signed URL on a redirect), so the token never leaves the API host.
  Never put the blob URL in an error, not even net/http's inner one. A generic
  `/runs/<id>` check link is an Actions job only when the check's app is
  `github-actions`. See `docs/adr/0006-github-actions-log-tools.md`.
- The workload's GitHub client is public-only, always (ADR 0007 phase 1,
  `internal/github/visibility.go`): every search carries `is:public`, every
  per-repository call first checks `GET /repos/{owner}/{name}` for `"private": false`
  (cached per `Client`, i.e. per run) and uses the canonical name it returns, and an
  API-host redirect is followed only to a repository that passes the same check. A new
  per-repository call must go through `Client.get` or call `c.gate` itself, and no
  GitHub API request uses `http.Client.Do`, which quotes a malformed redirect `Location`
  in its error: use `apiDo`/`roundTrip`. Test stubs
  must answer that lookup: wrap the handler in `githubtest.PublicRepos`. Ingest drops
  hits not confirmed public, and an unknown answer makes the fetch incomplete.
- GitHub sign-in is an OAuth app or a read-only GitHub App (`GITHUB_CLIENT_KIND`,
  ADR 0007). Every write of a stored credential — the callback's `Put`, logout, the
  broker's refresh and legacy tagging — goes through `internal/auth/credential.go`
  under one per-(user, provider) lock, and every write mints a new credential version
  (sealed with the token; no migration). Logout sets a tombstone (`credLocks`) that
  makes it the only writer left: other holders refuse to vend and delete nothing,
  and logout revokes, then deletes, under the lock (or in the background once the
  lock frees). The pending logout is also recorded durably (`vault.LogoutLedger`: a
  marker row under a reserved provider name in `credentials`, no migration), so the
  refusal survives a restart; the next lock holder, or startup, finishes it. Pairs
  that must agree after a crash commit in one SQLite transaction: the logout marker
  with the session delete (`MarkLogoutEndingSession`, which records nothing unless
  it removed the session), and a sign-in's credential with clearing the marker
  (`PutClearingLogout`). Marker transitions and in-memory ownership (a
  `localLogout` record per logout, in `internal/auth/logout.go`) change together
  under `credLocks.markerLock`.
  Never write the vault from anywhere else. A
  credential from another client is never vended. The broker answers "sign-in
  required" with 410 and code `sign_in_required`; the workload (`internal/agent`,
  which does not import the web tier) repeats both, and a test in `internal/api`
  holds them together. The workload's GitHub calls go through a `tokenHolder`, which
  re-vends once on 401 with `rejected=<version>`; a Discuss turn that finds sign-in
  required saves a reconnect notice (`worklist.NoticeReconnect`), never an answer.
- The model gateway (ADR 0008) is optional; per-run vending is the default. With
  `AI_GATEWAY_URL` set the web tier refuses `AI_TOKEN` and any `AI_CONNECTIONS`
  endpoint off the gateway's origin, because a run sends its run credential to a
  gateway connection. The gateway routes by model (no `params.model`; both
  `completions` and `responses`), and its config is the plain file
  `deploy/k8s/model-gateway/config.yaml`, which `internal/modelgateway`'s test runs
  unchanged: edit that file, not a copy, and re-run `make model-gateway-test` when you
  change it or bump the pinned release. The cluster steps live in
  `hack/model-gateway/model-gateway.sh` (fake-kubectl tests beside it): check and read
  back every write, remove gofer's `AI_TOKEN` only after the gateway holds the key and
  gofer's config points at it, restore the saved `AI_CONNECTIONS_BEFORE_GATEWAY`
  verbatim on rollback (and keep it until the rollback finishes, so a partial down can
  be re-run), and never write a key from `make deploy`. `make deploy` sets
  gofer-config's `AI_CONNECTIONS` in the rendered base (`model-gateway.sh render`)
  rather than patching after the apply; behind the gateway that value comes from the
  saved provider connections, in the script's own temp directory (`apply-base`). A
  failed read is never "absent" (dev-secrets included), the key never appears in a
  process's arguments, gofer-secrets' key is removed by a test-and-remove JSON Patch
  (from the data and from the `last-applied-configuration` annotation),
  and no AI_CONNECTIONS is written that gofer's `LoadConnections` would refuse with
  the model on (the script's jq validator mirrors it; `TestValidatorMatchesLoadConnections`
  holds them together, so extend its corpus when either changes) or whose endpoint is
  not exactly the upstream plus `/chat/completions` or `/responses`. Read a
  connection's endpoint only through `jq_lib`'s `endpoint` (gofer's effective field),
  never `.endpoint`. `/agent/*` gets no CORS handling: a 204 from
  `/agent/model-access` authorizes a model call.
- `go test` binds httptest sockets; run it outside a restrictive sandbox.

## Build / test / run

```
make build   # go build ./...
make test    # go test ./...
make run     # run the web tier alone (local UI/OAuth; dispatch needs an aei-controller)
make genkey  # an Ed25519 keypair for local runs (in-cluster, AEI's public key is used)
make dev-up  # deploy web + runtime images and the AgentApp to kind (AEI must be installed first)
make kind-load-runtime  # rebuild + load only the runtime image
make push IMAGE=... RUNTIME_IMAGE=... PLATFORM=linux/amd64  # build both images for a platform and push them
make deploy KUBECONFIG=<file> IMAGE=... RUNTIME_IMAGE=...  # apply to any cluster (no kind, no secret creation); dev-up shares its apply step
make backup-apply KUBECONFIG=<file> SNAPSHOT_CLASS=<class>  # scheduled data-volume snapshots (snapshot, snapshots, restore, backup-down)
make model-gateway-apply KUBECONFIG=<file>  # optional: agentgateway holds the model key + a shared budget (model-gateway-check, model-gateway-down)
make model-gateway-test  # the gateway's end-to-end check on this machine (fetches the pinned agentgateway release)
make install-ingress KUBECONFIG=<file>  # once per cluster: Traefik + cert-manager (pinned charts) for HTTPS
make deploy ... TLS_HOST=<dns name> ALLOWED_GITHUB_LOGINS=<logins> [ALLOWED_SOURCE_RANGES=...]  # HTTPS with a Let's Encrypt certificate, sign-in limited to those GitHub logins; make tls-status to watch it
```

See README.md for the kind walkthrough, including the AEI-side install.
