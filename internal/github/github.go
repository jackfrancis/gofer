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
	perPage        = 50      // timeline page size
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
// archived:false keeps stale archived-repo items out of the worklist.
//
// MEMBERSHIP IS ONE UNION QUERY. involves:@me is author ∪ assignee ∪ mentions ∪
// commenter, so it replaces four separate per-relationship queries with one. That is
// not just tidier: the individual qualifiers are not reliable — a PR the user was
// tagged in and replied to has been observed matching involves: while matching
// neither commenter: nor any other single qualifier. Chasing those quirks one at a
// time is a losing game; asking the broad question once is not.
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
	{worklist.ReasonInvolved, "is:open involves:@me archived:false"},
	{worklist.ReasonReviewRequested, "is:pr is:open review-requested:@me archived:false"},
}

// FetchWorklist retrieves the issues and pull requests on the user's radar —
// everything they are involved in, plus everything awaiting their review —
// deduplicated by item ID. When an item surfaces under more than one query, the
// reasons are merged onto a single work item.
//
// Each query is paged to exhaustion rather than sampled: a long-running
// contributor's involvement runs to hundreds of open items, and taking one arbitrary
// page silently drops work off the radar. See searchIssues for the two bounds GitHub
// imposes on that walk (a per-query result ceiling and a small per-minute request
// budget).
func (c *Client) FetchWorklist(ctx context.Context, token string) ([]worklist.WorkItem, error) {
	now := time.Now().UTC()
	// The relationship a union query cannot report is derived from each item's payload,
	// which needs the viewer's own login to compare against.
	viewer, err := c.Login(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("github login: %w", err)
	}
	seen := make(map[string]worklist.WorkItem)
	for _, s := range signals {
		items, err := c.searchIssues(ctx, token, s.query)
		if err != nil {
			return nil, fmt.Errorf("github %s search: %w", s.reason, err)
		}
		for _, it := range items {
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
	return out, nil
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
// A targeted query (review-requested:) already knows its own answer.
func reasonsFor(from worklist.Reason, it searchItem, viewer string) []worklist.Reason {
	if from != worklist.ReasonInvolved {
		return []worklist.Reason{from}
	}
	var out []worklist.Reason
	if it.User != nil && strings.EqualFold(it.User.Login, viewer) {
		out = append(out, worklist.ReasonAuthor)
	}
	for _, a := range it.Assignees {
		if strings.EqualFold(a.Login, viewer) {
			out = append(out, worklist.ReasonAssignee)
			break
		}
	}
	if len(out) == 0 {
		out = append(out, worklist.ReasonInvolved)
	}
	return out
}

type searchResponse struct {
	Items []searchItem `json:"items"`
}

type searchItem struct {
	Number        int       `json:"number"`
	Title         string    `json:"title"`
	HTMLURL       string    `json:"html_url"`
	State         string    `json:"state"`
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
		Name string `json:"name"`
	} `json:"labels"`
	Milestone *struct {
		DueOn time.Time `json:"due_on"`
	} `json:"milestone"`
	Reactions *struct {
		TotalCount int `json:"total_count"`
	} `json:"reactions"`
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request"`
}

func (it searchItem) toWorkItem(reason worklist.Reason, viewer string, now time.Time) (worklist.WorkItem, bool) {
	// The search/issues endpoint returns both issues and PRs; a pull_request
	// object marks a PR. Both belong on the radar, so map the type instead of
	// dropping issues.
	itemType := worklist.TypeIssue
	if it.PullRequest != nil {
		itemType = worklist.TypePullRequest
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
	for _, l := range it.Labels {
		sig.Labels = append(sig.Labels, l.Name)
	}

	return worklist.WorkItem{
		ID:     "github:" + repo + "#" + strconv.Itoa(it.Number),
		Source: "github",
		Type:   itemType,
		GitHub: worklist.GitHubRef{
			Number:    it.Number,
			Repo:      repo,
			Title:     it.Title,
			URL:       it.HTMLURL,
			State:     it.State,
			UpdatedAt: it.UpdatedAt,
		},
		Signals: sig,
		Meta:    worklist.Metadata{Origin: worklist.OriginAgent},
	}, true
}

func (c *Client) searchIssues(ctx context.Context, token, q string) ([]searchItem, error) {
	var out []searchItem
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
		u := fmt.Sprintf("%s/search/issues?per_page=%d&page=%d&sort=updated&order=desc&q=%s",
			c.baseURL, searchPerPage, page, url.QueryEscape(q))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		setHeaders(req, token)

		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		// Record the budget and the next-page link before the body is discarded; the
		// response is closed here rather than deferred because this runs in a loop.
		c.search.note(resp.Header)
		more := hasNextPage(resp.Header)
		status := resp.StatusCode
		resp.Body.Close()
		if rerr != nil {
			return nil, rerr
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("status %d", status)
		}
		var sr searchResponse
		if err := json.Unmarshal(body, &sr); err != nil {
			return nil, err
		}
		out = append(out, sr.Items...)
		if !more || len(sr.Items) == 0 {
			break
		}
	}
	return out, nil
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/user", nil)
	if err != nil {
		return "", err
	}
	setHeaders(req, token)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
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
