// Package github is the GitHub provider client. It is imported only by agent
// runtimes — never by gofer core packages — because gofer is a credential broker,
// not a data broker: the agent connects to GitHub directly with a vended token.
//
// It retrieves a user's work via the search API using the user's own vended
// token, so `@me` resolves to that user. Results map to worklist.WorkItem with
// default gofer metadata; scoring decorates the axes later.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

const (
	defaultBaseURL = "https://api.github.com"
	searchPerPage  = 100     // the search API's maximum, so a full walk costs the fewest calls
	maxBody        = 8 << 20 // 8 MiB
	// searchResultCap is GitHub's hard ceiling on a single search query: it refuses
	// to page past 1000 results. Pagination stops there rather than erroring.
	searchResultCap = 1000
	// maxSearchWait bounds one pause for the search rate limit, so a wrong or stale
	// reset time can never park a run until its deadline.
	maxSearchWait = 90 * time.Second
	// searchTimeReserve is the slice of the run's remaining time pagination refuses to
	// spend, leaving room for the rest of the pipeline: persisting the fetch, then the
	// chained enrich and rank. Paging the whole result set is best-effort against the
	// clock — an expired run persists NOTHING, so a walk that stops early and returns
	// what it has always beats one that runs out of budget mid-flight. Because results
	// are ordered by recency, stopping early keeps the freshest ones.
	searchTimeReserve = 90 * time.Second
)

// Client retrieves work signals from GitHub.
type Client struct {
	http    *http.Client
	baseURL string
	search  searchBudget

	// blob downloads Actions job logs from the pre-signed URL the API redirects to.
	// It never carries the token (actions.go); built on first use.
	blobOnce sync.Once
	blob     *http.Client

	// repos caches the public-only gate's answers. A Client serves one run, so this is
	// the per-run cache ADR 0007 calls for.
	repos repoGate
}

// NewClient returns a GitHub client. A nil httpClient uses http.DefaultClient; an
// empty baseURL uses the public API (tests point it at a stub).
func NewClient(httpClient *http.Client, baseURL string) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{http: httpClient, baseURL: strings.TrimRight(baseURL, "/")}
}

// signals are the (reason, query) pairs retrieved for the authenticated user.
// archived:false keeps stale archived-repo items out of the worklist, and
// searchIssues adds is:public to each (ADR 0007's public-only guard).
//
// MEMBERSHIP IS ONE UNION QUALIFIER. involves:@me is author ∪ assignee ∪ mentions ∪
// commenter, so it replaces four separate per-relationship queries. That is not just
// tidier: the individual qualifiers are not reliable — a PR the user was tagged in and
// replied to has been observed matching involves: while matching neither commenter:
// nor any other single qualifier. Chasing those quirks one at a time is a losing game;
// asking the broad question is not.
//
// It runs as two queries, issues and PRs, because a GitHub App user token answers 422
// to a search without is:issue or is:pr (ADR 0007). Together they match exactly what
// the unsplit query did.
//
// review-requested: is NOT part of involves:, and it is gofer's most important
// relationship, so it stays a query of its own.
//
// The reason attached here is only a starting point: because a union query cannot say
// WHICH member matched, reasonsFor derives what the payload proves (authored,
// assigned) and enrich later upgrades the rest from the item's timeline.
var signals = []struct {
	reason worklist.Reason
	query  string
}{
	{worklist.ReasonInvolved, "is:issue is:open involves:@me archived:false"},
	{worklist.ReasonInvolved, "is:pr is:open involves:@me archived:false"},
	{worklist.ReasonReviewRequested, "is:pr is:open review-requested:@me archived:false"},
}

// FetchWorklist retrieves the issues and pull requests on the user's radar —
// everything they are involved in, plus everything awaiting their review —
// deduplicated by item ID. When an item surfaces under more than one query, the
// reasons are merged onto a single work item. Another contributor's draft pull
// request is left out (searchItem.othersDraft) without making the fetch partial.
//
// Each query is paged to exhaustion rather than sampled: a long-running
// contributor's involvement runs to hundreds of open items, and taking one arbitrary
// page silently drops work off the radar. See searchIssues for the two bounds GitHub
// imposes on that walk (a per-query result ceiling and a small per-minute request
// budget).
//
// complete reports that every query was walked to its last page with no early stop
// and GitHub did not flag the results as incomplete — so the result is the WHOLE open
// set, and an item missing from it is genuinely no longer open (merged, closed, or no
// longer involving the user). The ingest relies on that to retire stale items; on a
// partial walk it must not, since a missing item may simply not have been reached.
//
// Every hit's repository must pass the public-only gate (publicHits). A hit from a
// repository that is not confirmed public is left out; one whose check had no answer
// also makes the result incomplete, since its item may still be open.
func (c *Client) FetchWorklist(ctx context.Context, token string) (items []worklist.WorkItem, complete bool, err error) {
	f, err := c.FetchWorklistDetail(ctx, token)
	return f.Items, f.Complete, err
}

// Fetch is FetchWorklist's result with the completeness of the review-request
// search reported on its own.
type Fetch struct {
	Items    []worklist.WorkItem
	Complete bool // see FetchWorklist
	// ReviewRequestsComplete reports that the review-requested search was walked
	// whole. Only then does an item without ReasonReviewRequested prove that no
	// review is requested: when that search is partial, an item may arrive through
	// involves: alone while its request is still pending.
	ReviewRequestsComplete bool
}

// FetchWorklistDetail is FetchWorklist, also reporting whether the review-request
// search was complete (Fetch.ReviewRequestsComplete).
func (c *Client) FetchWorklistDetail(ctx context.Context, token string) (Fetch, error) {
	now := time.Now().UTC()
	// The relationship a union query cannot report is derived from each item's payload,
	// which needs the viewer's own login to compare against.
	viewer, err := c.Login(ctx, token)
	if err != nil {
		return Fetch{}, fmt.Errorf("github login: %w", err)
	}
	seen := make(map[string]worklist.WorkItem)
	complete, reviewsComplete := true, true
	for _, s := range signals {
		found, whole, err := c.searchIssues(ctx, token, s.query)
		if err != nil {
			return Fetch{}, fmt.Errorf("github %s search: %w", s.reason, err)
		}
		complete = complete && whole
		if s.reason == worklist.ReasonReviewRequested {
			reviewsComplete = reviewsComplete && whole
		}
		for _, it := range found {
			wi, ok := it.toWorkItem(s.reason, viewer, now)
			if !ok {
				continue
			}
			if existing, dup := seen[wi.ID]; dup {
				for _, r := range wi.Signals.Reasons {
					existing.Signals.Reasons = appendReason(existing.Signals.Reasons, r)
				}
				seen[wi.ID] = existing
				continue
			}
			seen[wi.ID] = wi
		}
	}
	out := make([]worklist.WorkItem, 0, len(seen))
	for _, wi := range seen {
		out = append(out, wi)
	}
	out, confirmed := c.publicHits(ctx, token, out)
	complete = complete && confirmed
	// Deduplication runs through a map, so without this the result order is Go's
	// randomized map iteration — and every downstream "first N" (the rank cap, any
	// paging) would pick a different arbitrary subset on each run, reshuffling the
	// radar for no reason. Order by most recently active, with the ID as a stable
	// tiebreak, so a truncation keeps the freshest work and repeated ingests agree.
	slices.SortFunc(out, func(a, b worklist.WorkItem) int {
		if c := b.GitHub.UpdatedAt.Compare(a.GitHub.UpdatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return Fetch{Items: out, Complete: complete, ReviewRequestsComplete: reviewsComplete}, nil
}

// appendReason adds r to rs if not already present, keeping reasons unique and in
// first-seen order.
func appendReason(rs []worklist.Reason, r worklist.Reason) []worklist.Reason {
	for _, x := range rs {
		if x == r {
			return rs
		}
	}
	return append(rs, r)
}

// reasonsFor derives the user's relationship to an item. A union membership query
// (involves:) reports only THAT the user is involved, so the relationships the item
// payload can prove — they authored it, they are assigned — are read from the item
// itself and the generic reason is kept only when neither applies. enrich upgrades
// that residue to ReasonCommented once the timeline shows the user actually spoke.
// A targeted query (review-requested:) already knows its own answer, and still
// records what the payload proves: when the union search is partial, an item may
// arrive only from the targeted one, and an assignment it carries must not be lost.
func reasonsFor(from worklist.Reason, it searchItem, viewer string) []worklist.Reason {
	var out []worklist.Reason
	if from != worklist.ReasonInvolved {
		out = append(out, from)
	}
	if it.User != nil && strings.EqualFold(it.User.Login, viewer) {
		out = appendReason(out, worklist.ReasonAuthor)
	}
	for _, a := range it.Assignees {
		if strings.EqualFold(a.Login, viewer) {
			out = appendReason(out, worklist.ReasonAssignee)
			break
		}
	}
	if len(out) == 0 {
		out = append(out, worklist.ReasonInvolved)
	}
	return out
}

type searchResponse struct {
	Items      []searchItem `json:"items"`
	TotalCount int          `json:"total_count"`
	// IncompleteResults is GitHub's own flag that the search timed out server-side
	// and the items are not the full match set.
	IncompleteResults bool `json:"incomplete_results"`
}

type searchItem struct {
	Number        int       `json:"number"`
	Title         string    `json:"title"`
	HTMLURL       string    `json:"html_url"`
	State         string    `json:"state"`
	StateReason   string    `json:"state_reason"`
	Draft         bool      `json:"draft"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	Comments      int       `json:"comments"`
	RepositoryURL string    `json:"repository_url"`
	// User and Assignees let gofer prove the specific relationship a union membership
	// query cannot report.
	User *struct {
		Login string `json:"login"`
	} `json:"user"`
	Assignees []struct {
		Login string `json:"login"`
	} `json:"assignees"`
	Labels []struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	} `json:"labels"`
	Milestone *struct {
		DueOn time.Time `json:"due_on"`
	} `json:"milestone"`
	Reactions *struct {
		TotalCount int `json:"total_count"`
	} `json:"reactions"`
	PullRequest *struct {
		URL      string     `json:"url"`
		MergedAt *time.Time `json:"merged_at"`
	} `json:"pull_request"`
}

// othersDraft reports a draft pull request someone other than the viewer opened.
// Its author is still working on it, so it is not the viewer's work yet, whatever
// their relationship to it (mentioned, assigned, even asked to review): it stays off
// the worklist until it is marked ready for review. A hit with no author is not
// proven to be the viewer's, so it is treated as someone else's.
func (it searchItem) othersDraft(viewer string) bool {
	if it.PullRequest == nil || !it.Draft {
		return false
	}
	return it.User == nil || !strings.EqualFold(it.User.Login, viewer)
}

// toWorkItem maps a search hit to a work item. ok is false for a hit that does not
// belong on the worklist: another contributor's draft pull request (othersDraft).
// Leaving it out of the fetch means a complete refresh retires a stored copy (a pull
// request converted back to draft), keeping its thread, and it resurfaces once it is
// marked ready for review.
func (it searchItem) toWorkItem(reason worklist.Reason, viewer string, now time.Time) (worklist.WorkItem, bool) {
	if it.othersDraft(viewer) {
		return worklist.WorkItem{}, false
	}
	// The search/issues endpoint returns both issues and PRs; a pull_request
	// object marks a PR. Both belong on the radar, so map the type instead of
	// dropping issues.
	itemType := worklist.TypeIssue
	merged := false
	if it.PullRequest != nil {
		itemType = worklist.TypePullRequest
		merged = it.PullRequest.MergedAt != nil
	}
	repo := strings.TrimPrefix(it.RepositoryURL, defaultBaseURL+"/repos/")

	sig := worklist.Signals{
		Reasons:        reasonsFor(reason, it, viewer),
		Comments:       it.Comments,
		OpenedAt:       it.CreatedAt,
		LastActivityAt: it.UpdatedAt,
		ObservedAt:     now,
	}
	if it.Reactions != nil {
		sig.Reactions = it.Reactions.TotalCount
	}
	if it.Milestone != nil {
		sig.DeadlineAt = it.Milestone.DueOn
	}
	var colors map[string]string
	for _, l := range it.Labels {
		sig.Labels = append(sig.Labels, l.Name)
		if c := worklist.NormalizeLabelColor(l.Color); c != "" {
			if colors == nil {
				colors = map[string]string{}
			}
			colors[l.Name] = c
		}
	}

	return worklist.WorkItem{
		ID:     "github:" + repo + "#" + strconv.Itoa(it.Number),
		Source: "github",
		Type:   itemType,
		GitHub: worklist.GitHubRef{
			Number:      it.Number,
			Repo:        repo,
			Title:       it.Title,
			URL:         it.HTMLURL,
			State:       it.State,
			UpdatedAt:   it.UpdatedAt,
			LabelColors: colors,

			Draft:       it.Draft && itemType == worklist.TypePullRequest,
			Merged:      merged,
			StateReason: it.StateReason,
		},
		Signals: sig,
		Meta:    worklist.Metadata{Origin: worklist.OriginAgent},
	}, true
}

// searchIssues walks a search query page by page. whole reports that the walk saw
// the query's entire match set, so an item missing from it is no longer a match.
//
// That takes more than reaching the last page. GitHub stops serving pages at the
// 1,000-result ceiling, and the match set can change while the walk runs: an item
// updated mid-walk moves to the front (sort=updated) and is served twice while
// another slips behind a page boundary; one that closes shifts the rest up a row. So
// a walk is whole only if it reached the last page with no early stop (time budget,
// rate-limit wait), GitHub never flagged a page incomplete, no page counted more
// matches than the ceiling, total_count was the same on every page, no item came
// back twice, and the distinct items collected reach that count. Anything else is
// partial: everything found is still returned, and the caller skips retirement.
func (c *Client) searchIssues(ctx context.Context, token, q string) (out []searchItem, whole bool, err error) {
	unsure := false // something says the walk may have missed a match
	total := -1
	seen := make(map[string]bool)
	for page := 1; page*searchPerPage <= searchResultCap; page++ {
		// Never spend the time the rest of the run still needs. The first page is always
		// attempted (an empty fetch is worse than a late one); every page after it is a
		// nice-to-have that yields to the budget.
		if page > 1 && !fitsBudget(ctx, searchTimeReserve) {
			break
		}
		if !c.search.wait(ctx, searchTimeReserve) {
			break
		}
		sr, more, err := c.searchPage(ctx, token, q, page, searchPerPage)
		if err != nil {
			return nil, false, err
		}
		out = append(out, sr.Items...)
		if sr.IncompleteResults || sr.TotalCount > searchResultCap || (total >= 0 && sr.TotalCount != total) {
			unsure = true
		}
		total = sr.TotalCount
		for _, it := range sr.Items {
			key := it.RepositoryURL + "#" + strconv.Itoa(it.Number)
			if seen[key] {
				unsure = true // the set shifted between pages
			}
			seen[key] = true
		}
		if !more || len(sr.Items) == 0 {
			// The last page GitHub will serve.
			return out, !unsure && len(seen) >= total, nil
		}
	}
	// Left the loop early: the budget broke it, or the result ceiling ended it with
	// pages still to come. Either way the set is partial.
	return out, false, nil
}

// searchPage fetches one page of a search, restricted to public repositories
// (publicQuery). more reports that the API advertises a further page.
func (c *Client) searchPage(ctx context.Context, token, q string, page, per int) (sr searchResponse, more bool, err error) {
	u := fmt.Sprintf("%s/search/issues?per_page=%d&page=%d&sort=updated&order=desc&q=%s",
		c.baseURL, per, page, url.QueryEscape(publicQuery(q)))
	resp, err := c.apiDo(ctx, token, u)
	if err != nil {
		return sr, false, err
	}
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	// Record the budget and the next-page link before the body is discarded.
	c.search.note(resp.Header)
	more = hasNextPage(resp.Header)
	status := resp.StatusCode
	resp.Body.Close()
	if rerr != nil {
		return sr, false, rerr
	}
	if status != http.StatusOK {
		return sr, false, fmt.Errorf("status %d", status)
	}
	if err := json.Unmarshal(body, &sr); err != nil {
		return sr, false, err
	}
	return sr, more, nil
}

const (
	// gateConcurrency bounds the visibility checks ingest runs at once.
	gateConcurrency = 8
	// gateTimeReserve is the slice of the run's remaining time ingest's visibility
	// checks leave for persisting the fetch. It is smaller than searchTimeReserve on
	// purpose: pagination stops at that reserve, and the hits it has collected by
	// then are worth nothing unless their repositories can still be checked. What
	// follows the write-back (enrich, rank) is best-effort and budgets itself.
	gateTimeReserve = 30 * time.Second
)

// publicHits keeps the items whose repository passes the public-only gate. confirmed
// is false when any repository's check had no answer (a timeout, a rate limit, the
// run's time budget): its items are left out, and the caller must not treat the
// fetch as the whole open set. A definite answer (private, not found, refused) drops
// the items without that.
func (c *Client) publicHits(ctx context.Context, token string, items []worklist.WorkItem) (kept []worklist.WorkItem, confirmed bool) {
	var repos []string
	verdicts := make(map[string]verdict)
	for _, it := range items {
		if _, ok := verdicts[it.GitHub.Repo]; !ok {
			verdicts[it.GitHub.Repo] = verdictUnknown
			repos = append(repos, it.GitHub.Repo)
		}
	}
	// The checks stop gateTimeReserve before the run's deadline, in flight or not:
	// a repository left unchecked stays unknown rather than spend what the write-back
	// needs.
	gctx := ctx
	if deadline, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		gctx, cancel = context.WithDeadline(ctx, deadline.Add(-gateTimeReserve))
		defer cancel()
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, gateConcurrency)
	for _, repo := range repos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if gctx.Err() != nil {
				return
			}
			r := c.checkRepo(gctx, token, repo)
			v := r.verdict
			if r.invalid {
				// A name gofer cannot put in a request was never checked: that says
				// nothing about the item, so it must not let the fetch count as complete.
				v = verdictUnknown
			}
			mu.Lock()
			verdicts[repo] = v
			mu.Unlock()
		}()
	}
	wg.Wait()
	confirmed = true
	for _, v := range verdicts {
		if v == verdictUnknown {
			confirmed = false
		}
	}
	kept = items[:0]
	for _, it := range items {
		if verdicts[it.GitHub.Repo] == verdictPublic {
			kept = append(kept, it)
		}
	}
	return kept, confirmed
}

// hasNextPage reports whether a response's Link header advertises a further page.
// It is the API's own end-of-results signal, so pagination never guesses from a
// short page or spends a request discovering an empty one.
func hasNextPage(h http.Header) bool {
	for _, link := range h.Values("Link") {
		for _, part := range strings.Split(link, ",") {
			if strings.Contains(part, `rel="next"`) {
				return true
			}
		}
	}
	return false
}

// searchBudget mirrors the search API's own rate limit — 30 requests per minute for
// an authenticated user, a separate and far smaller budget than the 5,000/hour core
// limit — as advertised by the last search response. Paging consults it BEFORE each
// request, so a multi-page walk pauses politely instead of walking into a 429 and
// relying on retries to dig itself out. The reactive half is still there: the
// client is httpretry-wrapped, which honors Retry-After on a 429 that slips through.
//
// It does not model GitHub's secondary (abuse) rate limit, which arrives as a 403;
// staying inside the primary budget is what avoids provoking it.
type searchBudget struct {
	mu        sync.Mutex
	known     bool
	remaining int
	resetAt   time.Time
}

// note records the rate-limit state a search response advertised. Responses without
// the headers (a test stub, a proxy that strips them) leave the budget unknown, which
// simply disables the proactive pause.
func (b *searchBudget) note(h http.Header) {
	remaining, errR := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	reset, errT := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
	if errR != nil || errT != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.known, b.remaining, b.resetAt = true, remaining, time.Unix(reset, 0)
}

// wait pauses until the next search request is within the API's budget. It reports
// whether pagination should continue: false when the run was cancelled, or when the
// pause would not fit in the run's remaining time — in which case the caller keeps the
// pages it already has rather than sleeping into its own deadline.
func (b *searchBudget) wait(ctx context.Context, reserve time.Duration) bool {
	b.mu.Lock()
	known, remaining, resetAt := b.known, b.remaining, b.resetAt
	b.mu.Unlock()
	if !known || remaining > 0 {
		return true
	}
	d := time.Until(resetAt)
	if d <= 0 {
		return true
	}
	if d > maxSearchWait {
		d = maxSearchWait
	}
	if !fitsBudget(ctx, d+reserve) {
		return false
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// resetIn is how long until the search budget resets, as last advertised.
func (b *searchBudget) resetIn() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return max(time.Until(b.resetAt), 0)
}

// fitsBudget reports whether ctx has room for d before its deadline. A context
// without a deadline always fits.
func fitsBudget(ctx context.Context, d time.Duration) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return time.Until(deadline) > d
}

// Login returns the authenticated user's GitHub login.
func (c *Client) Login(ctx context.Context, token string) (string, error) {
	body, err := c.get(ctx, token, "/user")
	if err != nil {
		return "", err
	}
	var u struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(body, &u); err != nil {
		return "", err
	}
	if u.Login == "" {
		return "", fmt.Errorf("github: empty login")
	}
	return u.Login, nil
}

func setHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "gofer-agent")
}
