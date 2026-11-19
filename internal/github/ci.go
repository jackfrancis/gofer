package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Bounds on a CI status lookup.
const (
	ciPerPage  = 100
	ciMaxPages = 3 // per endpoint: 300 statuses and 300 check runs is far beyond a real PR
)

// CI check outcome classes, coarse enough to sort and summarise on.
const (
	CIFailing = "failing"
	CIPending = "pending"
	CIPassing = "passing"
)

// CICheck is one commit status or check run on a commit.
type CICheck struct {
	Name        string // status context or check-run name
	Kind        string // "status" (commit status, e.g. Prow) or "check" (check run, e.g. GitHub Actions)
	State       string // the provider's own word: failure, error, pending, success, timed_out, …
	Class       string // CIFailing, CIPending or CIPassing
	Description string // status description or check-run output title
	URL         string // target/details URL (a Prow job, an Actions run)
	App         string // check-run app slug; empty for a status
}

// CIStatus is the CI picture of one commit.
type CIStatus struct {
	Repo   string
	Ref    string // what was asked for (a PR's head branch, or the ref given)
	SHA    string // the commit the checks are on
	Checks []CICheck
	// Partial names a part that could not be read (check runs refused, more pages than
	// fetched); the rest of the result still stands.
	Partial []string
}

// CIStatusForPR returns the CI status of a pull request's head commit.
func (c *Client) CIStatusForPR(ctx context.Context, token, repo string, number int) (CIStatus, error) {
	repo, err := c.gate(ctx, token, repo)
	if err != nil {
		return CIStatus{}, err
	}
	body, err := c.getPull(ctx, token, repo, number)
	if err != nil {
		return CIStatus{}, err
	}
	var pr struct {
		Head struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return CIStatus{}, err
	}
	if pr.Head.SHA == "" {
		return CIStatus{}, fmt.Errorf("pull request %s#%d has no head commit", repo, number)
	}
	st, err := c.CIStatusForRef(ctx, token, repo, pr.Head.SHA)
	st.Ref = pr.Head.Ref
	return st, err
}

// CIStatusForRef returns the commit statuses and check runs on a ref (branch, tag or
// commit SHA). Statuses are the latest per context, as GitHub's combined status
// reports them; check runs are the latest per name, as GitHub lists them by default.
func (c *Client) CIStatusForRef(ctx context.Context, token, repo, ref string) (CIStatus, error) {
	st := CIStatus{Repo: repo, Ref: ref, SHA: ref}
	repo, err := c.gate(ctx, token, repo)
	if err != nil {
		return st, err
	}
	esc := url.PathEscape(ref)

	for page := 1; page <= ciMaxPages; page++ {
		body, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/commits/%s/status?per_page=%d&page=%d", repo, esc, ciPerPage, page))
		if err != nil {
			return st, err
		}
		var cs struct {
			SHA        string `json:"sha"`
			TotalCount int    `json:"total_count"`
			Statuses   []struct {
				Context     string `json:"context"`
				State       string `json:"state"`
				Description string `json:"description"`
				TargetURL   string `json:"target_url"`
			} `json:"statuses"`
		}
		if err := json.Unmarshal(body, &cs); err != nil {
			return st, err
		}
		if cs.SHA != "" {
			st.SHA = cs.SHA
		}
		for _, s := range cs.Statuses {
			st.Checks = append(st.Checks, CICheck{Name: s.Context, Kind: "status", State: s.State,
				Class: statusClass(s.State), Description: s.Description, URL: s.TargetURL})
		}
		if len(cs.Statuses) < ciPerPage || page*ciPerPage >= cs.TotalCount {
			break
		}
		if page == ciMaxPages {
			st.Partial = append(st.Partial, fmt.Sprintf("only the first %d of %d statuses were read", page*ciPerPage, cs.TotalCount))
		}
	}

	for page := 1; page <= ciMaxPages; page++ {
		body, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/commits/%s/check-runs?per_page=%d&page=%d", repo, url.PathEscape(st.SHA), ciPerPage, page))
		if err != nil {
			// Statuses alone still answer the Prow question; say what is missing.
			st.Partial = append(st.Partial, "check runs could not be read ("+err.Error()+")")
			break
		}
		var cr struct {
			TotalCount int `json:"total_count"`
			CheckRuns  []struct {
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
				DetailsURL string `json:"details_url"`
				HTMLURL    string `json:"html_url"`
				Output     struct {
					Title string `json:"title"`
				} `json:"output"`
				App struct {
					Slug string `json:"slug"`
				} `json:"app"`
			} `json:"check_runs"`
		}
		if err := json.Unmarshal(body, &cr); err != nil {
			st.Partial = append(st.Partial, "check runs could not be parsed")
			break
		}
		for _, r := range cr.CheckRuns {
			state := r.Conclusion
			if r.Status != "completed" || state == "" {
				state = r.Status // queued, in_progress, waiting, …
			}
			u := r.DetailsURL
			if u == "" {
				u = r.HTMLURL
			}
			st.Checks = append(st.Checks, CICheck{Name: r.Name, Kind: "check", State: state,
				Class: checkClass(r.Status, r.Conclusion), Description: r.Output.Title, URL: u, App: r.App.Slug})
		}
		if len(cr.CheckRuns) < ciPerPage || page*ciPerPage >= cr.TotalCount {
			break
		}
		if page == ciMaxPages {
			st.Partial = append(st.Partial, fmt.Sprintf("only the first %d of %d check runs were read", page*ciPerPage, cr.TotalCount))
		}
	}

	// Failing first, then pending, then passing; by name within a class, so the
	// result is stable across calls.
	rank := map[string]int{CIFailing: 0, CIPending: 1, CIPassing: 2}
	sort.SliceStable(st.Checks, func(i, j int) bool {
		a, b := st.Checks[i], st.Checks[j]
		if rank[a.Class] != rank[b.Class] {
			return rank[a.Class] < rank[b.Class]
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return st, nil
}

func statusClass(state string) string {
	switch state {
	case "success":
		return CIPassing
	case "pending":
		return CIPending
	default: // failure, error
		return CIFailing
	}
}

func checkClass(status, conclusion string) string {
	if status != "completed" {
		return CIPending
	}
	switch conclusion {
	case "success", "neutral", "skipped":
		return CIPassing
	case "", "stale":
		return CIPending
	default: // failure, timed_out, cancelled, action_required, startup_failure
		return CIFailing
	}
}
