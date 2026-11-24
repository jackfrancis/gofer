// Package ghcomment is the web tier's only GitHub client (ADR 0009). It does exactly
// three things, each on a public repository only:
//
//   - check a repository is public (GET /repos/{repo}), using the canonical name
//     GitHub answers with for the call that follows;
//   - post a comment on an issue or pull request (POST /repos/{repo}/issues/{n}/comments);
//   - list a repository's assignable users (GET /repos/{repo}/assignees), for the
//     bulk comment box's @-mention suggestions.
//
// It follows no redirect at all (it calls the transport directly, so no Location
// reaches an error) and never puts a token in an error or a log.
package ghcomment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Bounds.
const (
	// MaxBodyBytes is the most a posted comment may hold; GitHub's own limit is
	// 65536 characters.
	MaxBodyBytes = 65536
	// MaxAssigneePages bounds how many pages (of 100) of assignees are read per
	// repository: a very large organisation's list is cut, and the result says so.
	MaxAssigneePages = 5
	maxResponseBytes = 1 << 20
	requestTimeout   = 15 * time.Second
)

// Errors a caller distinguishes.
var (
	// ErrUnauthorized is GitHub's 401: the token is dead or revoked.
	ErrUnauthorized = errors.New("github rejected the credential")
	// ErrNotPublic means the repository is private, or GitHub would not show it.
	ErrNotPublic = errors.New("repository is not public")
	// ErrInvalidRepo means the name is not a plain owner/name.
	ErrInvalidRepo = errors.New("not a repository name")
	// ErrOutcomeUnknown means a POST was sent but gofer cannot tell whether GitHub
	// acted on it: the answer was lost (timeout, dropped connection) or was a gateway
	// error (502/503/504), which GitHub can give after the comment was created. A
	// retry could post a duplicate.
	ErrOutcomeUnknown = errors.New("github may or may not have acted on the request")
)

// RateLimitError is GitHub refusing because of a primary or secondary rate limit
// (429, or 403 with rate-limit signals). RetryAfter is how long GitHub asked gofer to
// wait, when it said (Retry-After, or X-RateLimit-Reset with none remaining).
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("github: rate limited; retry after %s", e.RetryAfter.Round(time.Second))
	}
	return "github: rate limited"
}

// StatusError is any other non-success answer, with GitHub's message when it gave
// one (never a token, never a URL).
type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("github: status %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("github: status %d", e.Status)
}

var repoName = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)

// ValidRepo reports whether repo is a plain owner/name.
func ValidRepo(repo string) bool {
	if !repoName.MatchString(repo) {
		return false
	}
	owner, name, _ := strings.Cut(repo, "/")
	return owner != "." && owner != ".." && name != "." && name != ".."
}

// Client calls GitHub's REST API.
type Client struct {
	base      string
	transport http.RoundTripper
}

// New returns a client for base (e.g. https://api.github.com). A nil transport uses
// http.DefaultTransport.
func New(base string, transport http.RoundTripper) *Client {
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &Client{base: strings.TrimRight(base, "/"), transport: transport}
}

// PublicRepo checks that repo is public and returns its canonical owner/name.
func (c *Client) PublicRepo(ctx context.Context, token, repo string) (string, error) {
	if !ValidRepo(repo) {
		return "", ErrInvalidRepo
	}
	body, err := c.do(ctx, token, http.MethodGet, "/repos/"+repo, nil, http.StatusOK)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) && (se.Status == http.StatusNotFound || se.Status == http.StatusGone) {
			return "", ErrNotPublic
		}
		return "", err
	}
	var meta struct {
		FullName string `json:"full_name"`
		Private  *bool  `json:"private"`
	}
	if err := json.Unmarshal(body, &meta); err != nil || meta.Private == nil || !ValidRepo(meta.FullName) {
		return "", errors.New("github: could not confirm the repository is public")
	}
	if *meta.Private {
		return "", ErrNotPublic
	}
	return meta.FullName, nil
}

// Comment posts body as a new comment on issue or pull request number in repo, after
// checking repo is public, and returns the new comment's web URL.
func (c *Client) Comment(ctx context.Context, token, repo string, number int, body string) (string, error) {
	if number <= 0 {
		return "", errors.New("github: no issue number")
	}
	if body == "" || len(body) > MaxBodyBytes {
		return "", fmt.Errorf("github: a comment must be 1 to %d bytes", MaxBodyBytes)
	}
	canon, err := c.PublicRepo(ctx, token, repo)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return "", err
	}
	resp, err := c.do(ctx, token, http.MethodPost, "/repos/"+canon+"/issues/"+strconv.Itoa(number)+"/comments", payload, http.StatusCreated)
	if err != nil && !errors.Is(err, errBodyLost) {
		return "", err
	}
	// A 201 is posted, even if its body (the link) was lost on the way.
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	_ = json.Unmarshal(resp, &out)
	if u, perr := url.Parse(out.HTMLURL); perr != nil || u.Scheme != "https" {
		out.HTMLURL = "" // posted, but no link worth showing
	}
	return out.HTMLURL, nil
}

// Assignees lists the logins that can be assigned in repo (public only), up to
// MaxAssigneePages pages. complete is false when the list was cut.
func (c *Client) Assignees(ctx context.Context, token, repo string) (logins []string, complete bool, err error) {
	canon, err := c.PublicRepo(ctx, token, repo)
	if err != nil {
		return nil, false, err
	}
	for page := 1; page <= MaxAssigneePages; page++ {
		body, err := c.do(ctx, token, http.MethodGet, "/repos/"+canon+"/assignees?per_page=100&page="+strconv.Itoa(page), nil, http.StatusOK)
		if err != nil {
			return nil, false, err
		}
		var users []struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		}
		if err := json.Unmarshal(body, &users); err != nil {
			return nil, false, errors.New("github: unreadable assignee list")
		}
		for _, u := range users {
			if u.Login != "" {
				logins = append(logins, u.Login)
			}
		}
		if len(users) < 100 {
			return logins, true, nil
		}
	}
	return logins, false, nil
}

// errBodyLost is a response with the wanted status whose body could not be read.
// do returns what it read with it.
var errBodyLost = errors.New("github: response body lost")

// do sends one request and returns the body of a response with status want. Any
// redirect is an error: the client follows none. For a POST, an error after the
// request was written, or a gateway error, is ErrOutcomeUnknown; an error before it
// was written is a definite failure.
func (c *Client) do(ctx context.Context, token, method, path string, payload []byte, want int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var wrote atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { wrote.Store(true) },
	})
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, errors.New("github: could not build the request")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "gofer")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	post := method == http.MethodPost
	resp, err := c.transport.RoundTrip(req)
	if err != nil {
		// A transport error can quote the URL; the path is not secret, but keep the
		// error to its kind.
		if post && wrote.Load() {
			return nil, ErrOutcomeUnknown
		}
		if ctx.Err() != nil {
			return nil, errors.New("github: request timed out")
		}
		return nil, errors.New("github: request failed")
	}
	defer resp.Body.Close()
	data, rerr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	switch {
	case resp.StatusCode == want:
		if rerr != nil {
			return data, errBodyLost
		}
		return data, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, ErrUnauthorized
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return nil, &StatusError{Status: resp.StatusCode, Message: "redirected; gofer follows no redirect"}
	case post && (resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout):
		return nil, ErrOutcomeUnknown
	}
	var msg struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(data, &msg)
	if rl := rateLimit(resp, msg.Message, time.Now()); rl != nil {
		return nil, rl
	}
	m := msg.Message
	if len(m) > 200 {
		m = m[:200]
	}
	return nil, &StatusError{Status: resp.StatusCode, Message: m}
}

// rateLimit reads GitHub's rate-limit signals from a refusal: 429; or 403 with no
// requests remaining, a Retry-After, or a message naming a rate limit (the secondary
// limits on content creation). It returns nil for any other answer.
func rateLimit(resp *http.Response, message string, now time.Time) *RateLimitError {
	limited := resp.StatusCode == http.StatusTooManyRequests
	if resp.StatusCode == http.StatusForbidden {
		limited = resp.Header.Get("X-RateLimit-Remaining") == "0" ||
			resp.Header.Get("Retry-After") != "" ||
			strings.Contains(strings.ToLower(message), "rate limit")
	}
	if !limited {
		return nil
	}
	rl := &RateLimitError{}
	if secs, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && secs > 0 {
		rl.RetryAfter = time.Duration(secs) * time.Second
	} else if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			if d := time.Unix(reset, 0).Sub(now); d > 0 {
				rl.RetryAfter = d
			}
		}
	}
	return rl
}
