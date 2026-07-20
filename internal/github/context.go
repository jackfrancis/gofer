// Read-only context fetches for the assistive conversation: a discussion
// snapshot for the item under discussion, plus the lookups the conversation
// tools call (a file at a ref, a PR/issue's state, a search). Every call only
// ever GETs, with the user's vended credential; reach is bounded by that token's
// own scopes, and any repository the token can see is fair game. gofer core
// never imports this client — only the agent runtime does.
package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Bounds on a fetched discussion snapshot, so a large thread cannot blow up the
// prompt. The client trims here; the converser frames the result as untrusted.
const (
	maxDiscussionBody     = 8 << 10 // description text kept
	maxCommentBody        = 2 << 10 // per-comment / per-review text kept
	maxDiscussionComments = 40      // most-recent comments/reviews kept
	maxChangedFiles       = 100     // changed file paths kept for a PR
)

// Bounds on the conversation tools' read-only lookups.
const (
	maxFileBytes     = 32 << 10 // decoded file text per read; a window, paged via offset
	maxSearchResults = 20       // search matches returned to the model
)

// Discussion is a bounded, read-only snapshot of an item's GitHub discussion:
// the description, recent comments, and — for a PR — reviews, inline review
// comments, changed file paths, and the head commit SHA.
type Discussion struct {
	Body           string
	Comments       []Comment
	Reviews        []Review
	ReviewComments []Comment
	ChangedFiles   []string
	HeadSHA        string
}

// Comment is one discussion or inline review comment.
type Comment struct {
	Author string
	Body   string
	At     time.Time
}

// Review is one PR review submission (state + optional summary body).
type Review struct {
	Author string
	State  string
	Body   string
	At     time.Time
}

// Discussion fetches a bounded snapshot of an item's discussion. The description
// comes from the issues endpoint (which serves both issues and PRs); comments,
// reviews, inline comments, changed files, and the head SHA are best-effort — a
// missing piece is not fatal, so a degraded snapshot is better than none.
func (c *Client) Discussion(ctx context.Context, token, repo string, number int, isPR bool) (Discussion, error) {
	var d Discussion

	// Description from the issues endpoint (works for both issues and PRs).
	issueBody, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/issues/%d", repo, number))
	if err != nil {
		return Discussion{}, err
	}
	var issue struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(issueBody, &issue); err != nil {
		return Discussion{}, err
	}
	d.Body = truncateRunes(issue.Body, maxDiscussionBody)

	// Comments are best-effort: a missing discussion is not fatal.
	if body, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", repo, number)); err == nil {
		var raw []struct {
			Body string `json:"body"`
			User *struct {
				Login string `json:"login"`
			} `json:"user"`
			CreatedAt time.Time `json:"created_at"`
		}
		if err := json.Unmarshal(body, &raw); err == nil {
			if len(raw) > maxDiscussionComments {
				raw = raw[len(raw)-maxDiscussionComments:] // keep the most recent
			}
			for _, cm := range raw {
				cc := Comment{Body: truncateRunes(cm.Body, maxCommentBody), At: cm.CreatedAt}
				if cm.User != nil {
					cc.Author = cm.User.Login
				}
				d.Comments = append(d.Comments, cc)
			}
		}
	}

	if !isPR {
		return d, nil
	}

	// The PR head commit, so the assistant reads changed files at the exact ref
	// the PR proposes rather than guessing a branch name. Best-effort but
	// important for fork PRs: the head *branch* lives on the contributor's fork
	// and does not resolve in this repo, whereas GitHub exposes the head *commit*
	// in the base repo — so the SHA is the reliable ref.
	if body, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/pulls/%d", repo, number)); err == nil {
		var pr struct {
			Head struct {
				SHA string `json:"sha"`
			} `json:"head"`
		}
		if err := json.Unmarshal(body, &pr); err == nil {
			d.HeadSHA = pr.Head.SHA
		}
	}

	// PR review submissions: state + summary body. Best-effort.
	if body, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=100", repo, number)); err == nil {
		var raw []struct {
			Body  string `json:"body"`
			State string `json:"state"`
			User  *struct {
				Login string `json:"login"`
			} `json:"user"`
			SubmittedAt time.Time `json:"submitted_at"`
		}
		if err := json.Unmarshal(body, &raw); err == nil {
			if len(raw) > maxDiscussionComments {
				raw = raw[len(raw)-maxDiscussionComments:] // keep the most recent
			}
			for _, rv := range raw {
				// Skip empty "commented" reviews: they are just containers for the
				// inline comments fetched below and carry no signal of their own.
				if strings.TrimSpace(rv.Body) == "" && strings.EqualFold(rv.State, "commented") {
					continue
				}
				r := Review{State: strings.ToLower(rv.State), Body: truncateRunes(rv.Body, maxCommentBody), At: rv.SubmittedAt}
				if rv.User != nil {
					r.Author = rv.User.Login
				}
				d.Reviews = append(d.Reviews, r)
			}
		}
	}

	// Inline review comments on the diff (path-prefixed). Best-effort.
	if body, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/pulls/%d/comments?per_page=100", repo, number)); err == nil {
		var raw []struct {
			Body string `json:"body"`
			Path string `json:"path"`
			User *struct {
				Login string `json:"login"`
			} `json:"user"`
			CreatedAt time.Time `json:"created_at"`
		}
		if err := json.Unmarshal(body, &raw); err == nil {
			if len(raw) > maxDiscussionComments {
				raw = raw[len(raw)-maxDiscussionComments:] // keep the most recent
			}
			for _, cm := range raw {
				text := cm.Body
				if cm.Path != "" {
					text = "[" + cm.Path + "] " + cm.Body
				}
				cc := Comment{Body: truncateRunes(text, maxCommentBody), At: cm.CreatedAt}
				if cm.User != nil {
					cc.Author = cm.User.Login
				}
				d.ReviewComments = append(d.ReviewComments, cc)
			}
		}
	}

	// Changed file paths are best-effort and low-risk (paths, not patch).
	if body, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=100", repo, number)); err == nil {
		var files []struct {
			Filename string `json:"filename"`
		}
		if err := json.Unmarshal(body, &files); err == nil {
			for i, f := range files {
				if i >= maxChangedFiles {
					break
				}
				d.ChangedFiles = append(d.ChangedFiles, f.Filename)
			}
		}
	}

	return d, nil
}

// truncateRunes caps s to at most max bytes without splitting a UTF-8 rune,
// appending a marker when it trims.
func truncateRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	t := s[:max]
	for len(t) > 0 && !utf8.ValidString(t) {
		t = t[:len(t)-1]
	}
	return t + "… (truncated)"
}

// FileContents reads a file from a repo at an optional ref (branch/tag/SHA;
// empty uses the default branch) and returns its decoded text, bounded to a
// maxFileBytes window starting at offset so the model can page through a file
// larger than one window. A path that resolves to a directory, a binary blob, or
// an unreadable object returns a short explanatory note rather than an error, so
// the assistant can react.
func (c *Client) FileContents(ctx context.Context, token, repo, path, ref string, offset int) (string, error) {
	p := fmt.Sprintf("/repos/%s/contents/%s", repo, path)
	if ref != "" {
		p += "?ref=" + url.QueryEscape(ref)
	}
	body, err := c.get(ctx, token, p)
	if err != nil {
		return "", err
	}
	// A file responds with an object; a directory responds with a JSON array.
	var file struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(body, &file); err != nil {
		return "(path is a directory, not a file)", nil
	}
	if file.Type != "file" || file.Encoding != "base64" {
		return "(not a readable text file)", nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil {
		return "", fmt.Errorf("decode file contents: %w", err)
	}
	if !utf8.Valid(raw) {
		return "(binary file omitted)", nil
	}
	return fileWindow(string(raw), offset), nil
}

// fileWindow returns the maxFileBytes-sized slice of text beginning at offset,
// aligned to UTF-8 boundaries and framed with markers that report the file's
// total size and the exact offset to request next, so the model can page through
// a file larger than one window. offset is clamped into range, so an
// out-of-range value from the model is harmless; a file that fits in one window
// from the start is returned verbatim with no markers.
func fileWindow(text string, offset int) string {
	total := len(text)
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	for offset < total && !utf8.RuneStart(text[offset]) {
		offset++
	}
	end := offset + maxFileBytes
	if end > total {
		end = total
	}
	for end > offset && !utf8.ValidString(text[offset:end]) {
		end--
	}
	window := text[offset:end]
	if offset == 0 && end == total {
		return window
	}
	var b strings.Builder
	fmt.Fprintf(&b, "(file is %d bytes; showing %d-%d", total, offset, end)
	if end < total {
		fmt.Fprintf(&b, "; request offset %d to continue", end)
	}
	b.WriteString(")\n")
	if offset > 0 {
		b.WriteString("…\n")
	}
	b.WriteString(window)
	if end < total {
		b.WriteString("\n… (truncated; more below)")
	}
	return b.String()
}

// PullRequestStatus returns a pull request's current state (open/closed, merged
// or not, base/head, title) as a compact text summary.
func (c *Client) PullRequestStatus(ctx context.Context, token, repo string, number int) (string, error) {
	body, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/pulls/%d", repo, number))
	if err != nil {
		return "", err
	}
	var pr struct {
		Number   int        `json:"number"`
		State    string     `json:"state"`
		Merged   bool       `json:"merged"`
		MergedAt *time.Time `json:"merged_at"`
		Title    string     `json:"title"`
		Base     struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Head struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return "", err
	}
	merged := "not merged"
	if pr.Merged {
		merged = "merged"
		if pr.MergedAt != nil {
			merged += " at " + pr.MergedAt.UTC().Format(time.RFC3339)
		}
	}
	summary := fmt.Sprintf("PR %s#%d %q: state=%s, %s, base=%s, head=%s",
		repo, pr.Number, pr.Title, pr.State, merged, pr.Base.Ref, pr.Head.Ref)
	if pr.Head.SHA != "" {
		summary += ", head commit=" + pr.Head.SHA
	}
	return summary + "\n" + pr.HTMLURL, nil
}

// IssueStatus returns an issue's (or PR's) current state as a compact summary.
func (c *Client) IssueStatus(ctx context.Context, token, repo string, number int) (string, error) {
	body, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/issues/%d", repo, number))
	if err != nil {
		return "", err
	}
	var is struct {
		Number      int        `json:"number"`
		State       string     `json:"state"`
		Title       string     `json:"title"`
		ClosedAt    *time.Time `json:"closed_at"`
		HTMLURL     string     `json:"html_url"`
		PullRequest *struct{}  `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &is); err != nil {
		return "", err
	}
	kind := "issue"
	if is.PullRequest != nil {
		kind = "pull request"
	}
	closed := ""
	if is.ClosedAt != nil {
		closed = ", closed at " + is.ClosedAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("%s %s#%d %q: state=%s%s\n%s", kind, repo, is.Number, is.Title, is.State, closed, is.HTMLURL), nil
}

// Search runs a GitHub issues/PRs search and returns the top matches as compact
// text. It accepts the full GitHub search syntax (e.g. "repo:owner/name otel").
func (c *Client) Search(ctx context.Context, token, query string) (string, error) {
	items, err := c.searchIssues(ctx, token, query)
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "no matching issues or pull requests", nil
	}
	var b strings.Builder
	shown := len(items)
	if shown > maxSearchResults {
		shown = maxSearchResults
	}
	fmt.Fprintf(&b, "%d result(s) (showing %d):\n", len(items), shown)
	for _, it := range items[:shown] {
		repo := strings.TrimPrefix(it.RepositoryURL, defaultBaseURL+"/repos/")
		kind := "issue"
		if it.PullRequest != nil {
			kind = "PR"
		}
		fmt.Fprintf(&b, "- %s %s#%d %q [%s] %s\n", kind, repo, it.Number, it.Title, it.State, it.HTMLURL)
	}
	return b.String(), nil
}
