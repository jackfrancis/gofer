package ghcomment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// This file is the pending review (ADR 0010): gofer creates a review with no event,
// which GitHub keeps PENDING, visible only to its author until they submit it on
// GitHub. It adds three calls to the client, on a public repository only:
//
//   - GET /repos/{repo}/pulls/{n}, for the head commit and state;
//   - GET /repos/{repo}/pulls/{n}/reviews, to find a pending review already there;
//   - POST /repos/{repo}/pulls/{n}/reviews, with no event.

// Bounds on a pending review.
const (
	// MaxReviewComments is the most inline comments one review may carry.
	MaxReviewComments = 50
	// maxReviewPages bounds how many pages (of 100) of reviews are read to look for
	// a pending one.
	maxReviewPages = 10
)

// Errors a caller distinguishes.
var (
	// ErrHeadMoved means the pull request's head is no longer the commit the
	// comments are anchored to: a push moved the lines.
	ErrHeadMoved = errors.New("the pull request's head changed")
	// ErrNotOpen means the pull request is closed or merged.
	ErrNotOpen = errors.New("the pull request is not open")
	// ErrPendingExists means the user already has a pending review on the pull
	// request; GitHub allows one, and its REST API cannot add to it.
	ErrPendingExists = errors.New("a pending review already exists")
)

// ReviewComment is one inline comment of a pending review.
type ReviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

// PendingReview creates a PENDING review on repo's pull request number, anchored to
// commit, with body and comments, after checking the repository is public, the pull
// request is open, its head is still commit, and the user has no pending review on
// it already. It returns the review's web URL when GitHub gave one. A POST whose
// answer was lost is ErrOutcomeUnknown.
func (c *Client) PendingReview(ctx context.Context, token, repo string, number int, commit, body string, comments []ReviewComment) (string, error) {
	if number <= 0 {
		return "", errors.New("github: no pull request number")
	}
	if commit == "" {
		return "", errors.New("github: no commit to anchor the review to")
	}
	if strings.TrimSpace(body) == "" && len(comments) == 0 {
		return "", errors.New("github: an empty review")
	}
	if len(body) > MaxBodyBytes || len(comments) > MaxReviewComments {
		return "", errors.New("github: the review is too large")
	}
	for _, cm := range comments {
		if cm.Path == "" || cm.Line < 1 || (cm.Side != "RIGHT" && cm.Side != "LEFT") || cm.Body == "" || len(cm.Body) > MaxBodyBytes {
			return "", errors.New("github: an invalid review comment")
		}
	}
	canon, err := c.PublicRepo(ctx, token, repo)
	if err != nil {
		return "", err
	}
	pull := "/repos/" + canon + "/pulls/" + strconv.Itoa(number)
	raw, err := c.do(ctx, token, http.MethodGet, pull, nil, http.StatusOK)
	if err != nil {
		return "", err
	}
	var pr struct {
		State string `json:"state"`
		Head  struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(raw, &pr); err != nil {
		return "", errors.New("github: unreadable pull request")
	}
	if !strings.EqualFold(pr.State, "open") {
		return "", ErrNotOpen
	}
	if !strings.EqualFold(pr.Head.SHA, commit) {
		return "", ErrHeadMoved
	}
	pending, err := c.hasPendingReview(ctx, token, pull)
	if err != nil {
		return "", err
	}
	if pending {
		return "", ErrPendingExists
	}
	// No "event": GitHub creates the review PENDING.
	payload, err := json.Marshal(struct {
		CommitID string          `json:"commit_id"`
		Body     string          `json:"body,omitempty"`
		Comments []ReviewComment `json:"comments,omitempty"`
	}{commit, body, comments})
	if err != nil {
		return "", err
	}
	resp, err := c.do(ctx, token, http.MethodPost, pull+"/reviews", payload, http.StatusOK)
	if err != nil && !errors.Is(err, errBodyLost) {
		return "", err
	}
	var out struct {
		HTMLURL string `json:"html_url"`
		State   string `json:"state"`
	}
	_ = json.Unmarshal(resp, &out)
	if u, perr := url.Parse(out.HTMLURL); perr != nil || u.Scheme != "https" {
		out.HTMLURL = ""
	}
	if out.State != "" && !strings.EqualFold(out.State, "PENDING") {
		// GitHub never submits a review without an event; say so if it ever did.
		return out.HTMLURL, fmt.Errorf("github: the review was created %s, not pending", strings.ToLower(out.State))
	}
	return out.HTMLURL, nil
}

// hasPendingReview reports whether the pull request (its API path) has a pending
// review. GitHub lists a pending review only to its author, so any one is the
// user's own.
func (c *Client) hasPendingReview(ctx context.Context, token, pull string) (bool, error) {
	for page := 1; page <= maxReviewPages; page++ {
		raw, err := c.do(ctx, token, http.MethodGet, pull+"/reviews?per_page=100&page="+strconv.Itoa(page), nil, http.StatusOK)
		if err != nil {
			return false, err
		}
		var reviews []struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(raw, &reviews); err != nil {
			return false, errors.New("github: unreadable review list")
		}
		for _, r := range reviews {
			if strings.EqualFold(r.State, "PENDING") {
				return true, nil
			}
		}
		if len(reviews) < 100 {
			return false, nil
		}
	}
	// Too many reviews to tell: refuse rather than risk GitHub's refusal halfway.
	return false, errors.New("github: too many reviews to check for a pending one")
}
