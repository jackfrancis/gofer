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
	"strconv"
	"strings"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

const (
	defaultBaseURL = "https://api.github.com"
	perPage        = 50
	maxBody        = 8 << 20 // 8 MiB
)

// Client retrieves work signals from GitHub.
type Client struct {
	http    *http.Client
	baseURL string
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
// The reason records why an item surfaced and feeds the Relevance/Urgency axes;
// archived:false keeps stale archived-repo items out of the worklist.
var signals = []struct {
	reason worklist.Reason
	query  string
}{
	{worklist.ReasonAuthor, "is:pr is:open author:@me archived:false"},
	{worklist.ReasonAssignee, "is:pr is:open assignee:@me archived:false"},
	{worklist.ReasonReviewRequested, "is:pr is:open review-requested:@me archived:false"},
	{worklist.ReasonCommented, "is:open commenter:@me archived:false"},
}

// FetchWorklist retrieves the issues and pull requests on the user's radar —
// authored, assigned, review-requested, and commented-on — deduplicated by item
// ID. When an item surfaces under more than one query, the reasons are merged
// onto a single work item.
func (c *Client) FetchWorklist(ctx context.Context, token string) ([]worklist.WorkItem, error) {
	now := time.Now().UTC()
	seen := make(map[string]worklist.WorkItem)
	for _, s := range signals {
		items, err := c.searchIssues(ctx, token, s.query)
		if err != nil {
			return nil, fmt.Errorf("github %s search: %w", s.reason, err)
		}
		for _, it := range items {
			wi, ok := it.toWorkItem(s.reason, now)
			if !ok {
				continue
			}
			if existing, dup := seen[wi.ID]; dup {
				existing.Signals.Reasons = appendReason(existing.Signals.Reasons, s.reason)
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
	Labels        []struct {
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

func (it searchItem) toWorkItem(reason worklist.Reason, now time.Time) (worklist.WorkItem, bool) {
	// The search/issues endpoint returns both issues and PRs; a pull_request
	// object marks a PR. Both belong on the radar, so map the type instead of
	// dropping issues.
	itemType := worklist.TypeIssue
	if it.PullRequest != nil {
		itemType = worklist.TypePullRequest
	}
	repo := strings.TrimPrefix(it.RepositoryURL, defaultBaseURL+"/repos/")

	sig := worklist.Signals{
		Reasons:        []worklist.Reason{reason},
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
	u := c.baseURL + "/search/issues?per_page=" + strconv.Itoa(perPage) + "&q=" + url.QueryEscape(q)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	setHeaders(req, token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var sr searchResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, err
	}
	return sr.Items, nil
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
