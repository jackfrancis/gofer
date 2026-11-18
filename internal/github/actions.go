package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackfrancis/gofer/internal/httpretry"
)

// GitHub Actions: a workflow run's jobs and their steps, and one job's log. All of
// it is read-only and uses the run's vended token against the API host only.

// Bounds on Actions lookups.
const (
	actionsJobsPerPage  = 100
	actionsJobsMaxPages = 3 // 300 jobs: a very wide matrix
	// logBlobTimeout bounds one log download from the blob host; the caller's context
	// bounds it too.
	logBlobTimeout = 2 * time.Minute
	maxLogRedirect = 3
)

// ErrLogNotFound reports a job log that does not exist or cannot be seen with the
// token (GitHub answers 404 to both).
var ErrLogNotFound = errors.New("job log not found")

// ErrLogTooLarge reports a job log over the caller's byte bound.
var ErrLogTooLarge = errors.New("job log too large")

// ErrLogForbidden reports a log the API refused to this token (401/403, other than a
// rate limit). The error carries GitHub's own reason and the token's and endpoint's
// OAuth scopes, which is what tells a missing scope from an organisation's OAuth App
// access restriction.
var ErrLogForbidden = errors.New("access denied")

// LogAccessError is a log the API refused to this token. It matches
// ErrLogForbidden, and its message carries GitHub's reason and the scope headers.
type LogAccessError struct {
	What   string // "job log" or "step log"
	Status int    // 401 or 403
	reason reason
}

func (e *LogAccessError) Error() string {
	return fmt.Sprintf("%s: %s (status %d): GitHub refused this token access to the %s (gofer's access to GitHub, not anything the job itself printed)%s",
		e.What, ErrLogForbidden.Error(), e.Status, e.What, e.reason.String())
}

// Is makes a LogAccessError match ErrLogForbidden.
func (e *LogAccessError) Is(target error) bool { return target == ErrLogForbidden }

// adminRightsMessage is GitHub's message when it refuses an Actions log to a token
// without repository scope (or to no token).
const adminRightsMessage = "Must have admin rights to Repository."

// ScopeRefusal reports the refusal gofer's tests pinned down (ADR 0006): a 403 with
// GitHub's "Must have admin rights to Repository." message, to a classic OAuth token
// whose granted scopes, as GitHub reported them, include neither repo nor
// public_repo, with no SAML SSO requirement. GitHub refuses such a token exactly as
// it refuses no token, and serves the log to a public_repo or read-only fine-grained
// token. The scopes header alone is not enough: a 403 to the same token can have
// another cause, such as an organisation's OAuth App restriction, which no broader
// token fixes. A refusal without the scopes header (a fine-grained or app token) is
// not this case either.
func (e *LogAccessError) ScopeRefusal() bool {
	if e.Status != http.StatusForbidden || e.reason.message != adminRightsMessage {
		return false
	}
	if e.reason.sso || e.reason.tokenScopes == nil {
		return false
	}
	for _, sc := range strings.Split(*e.reason.tokenScopes, ",") {
		switch strings.TrimSpace(sc) {
		case "repo", "public_repo":
			return false
		}
	}
	return true
}

// ErrLogRateLimited reports a log request refused by a GitHub rate limit.
var ErrLogRateLimited = errors.New("GitHub rate limit")

// ActionsRef is what an Actions URL names: a run, optionally one job in it, and
// optionally a step of that job (a "#step:N:L" link).
type ActionsRef struct {
	Repo    string
	RunID   int64
	Attempt int   // a run attempt the URL names; 0 for the latest
	JobID   int64 // 0 when the URL names only a run
	Step    int   // 0 when the URL names no step
}

// ParseActionsURL parses a GitHub Actions link as a check run or a page shows it:
//
//	https://github.com/<owner>/<repo>/actions/runs/<run>
//	https://github.com/<owner>/<repo>/actions/runs/<run>/attempts/<n>
//	https://github.com/<owner>/<repo>/actions/runs/<run>/job/<job>[?pr=N][#step:S:L]
//	https://github.com/<owner>/<repo>/runs/<job>                (a check run's html_url)
//	https://github.com/<owner>/<repo>/commit/<sha>/checks/<job>
//
// A check run's ID is its Actions job's ID, so the last two name a job without its run.
func ParseActionsURL(raw string) (ActionsRef, error) {
	var ref ActionsRef
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return ref, fmt.Errorf("not a GitHub Actions URL (want https://github.com/<owner>/<repo>/actions/runs/<run>/job/<job>)")
	}
	if h := strings.ToLower(u.Hostname()); h != "github.com" && h != "www.github.com" {
		return ref, fmt.Errorf("not a GitHub Actions URL: host %q is not github.com", u.Hostname())
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) < 4 {
		return ref, fmt.Errorf("not a GitHub Actions run or job URL")
	}
	ref.Repo = segs[0] + "/" + segs[1]
	if !repoName.MatchString(ref.Repo) || strings.Contains(ref.Repo, "..") {
		return ref, fmt.Errorf("not a GitHub Actions URL: bad repository %q", ref.Repo)
	}
	id := func(s string) (int64, bool) {
		n, err := strconv.ParseInt(s, 10, 64)
		return n, err == nil && n > 0
	}
	rest := segs[2:]
	var ok bool
	switch {
	case rest[0] == "actions" && rest[1] == "runs" && len(rest) >= 3:
		if ref.RunID, ok = id(rest[2]); !ok {
			return ref, fmt.Errorf("not a GitHub Actions URL: bad run id %q", rest[2])
		}
		tail := rest[3:]
		if len(tail) >= 2 && tail[0] == "attempts" {
			n, ok := id(tail[1])
			if !ok {
				return ref, fmt.Errorf("not a GitHub Actions URL: bad attempt %q", tail[1])
			}
			ref.Attempt = int(n)
			tail = tail[2:]
		}
		if len(tail) >= 2 && tail[0] == "job" {
			if ref.JobID, ok = id(tail[1]); !ok {
				return ref, fmt.Errorf("not a GitHub Actions URL: bad job id %q", tail[1])
			}
		}
	case rest[0] == "runs" && len(rest) == 2:
		if ref.JobID, ok = id(rest[1]); !ok {
			return ref, fmt.Errorf("not a GitHub Actions URL: bad check run id %q", rest[1])
		}
	case rest[0] == "commit" && len(rest) == 4 && rest[2] == "checks":
		if ref.JobID, ok = id(rest[3]); !ok {
			return ref, fmt.Errorf("not a GitHub Actions URL: bad check run id %q", rest[3])
		}
	default:
		return ref, fmt.Errorf("not a GitHub Actions run or job URL")
	}
	// "#step:5:12" links a step (and a line of the web view, which is not a line of
	// the downloaded log).
	if f, found := strings.CutPrefix(u.Fragment, "step:"); found {
		s, _, _ := strings.Cut(f, ":")
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			ref.Step = n
		}
	}
	return ref, nil
}

// ActionsStep is one step of a job.
type ActionsStep struct {
	Number      int       `json:"number"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`     // queued, in_progress, completed
	Conclusion  string    `json:"conclusion"` // success, failure, skipped, cancelled, …
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
}

// ActionsJob is one job of a workflow run. Every string comes from the workflow
// (which a pull request can change) and is untrusted.
type ActionsJob struct {
	ID           int64         `json:"id"`
	RunID        int64         `json:"run_id"`
	RunAttempt   int           `json:"run_attempt"`
	Name         string        `json:"name"`
	WorkflowName string        `json:"workflow_name"`
	HeadBranch   string        `json:"head_branch"`
	HeadSHA      string        `json:"head_sha"`
	Status       string        `json:"status"`
	Conclusion   string        `json:"conclusion"`
	HTMLURL      string        `json:"html_url"`
	StartedAt    time.Time     `json:"started_at"`
	CompletedAt  time.Time     `json:"completed_at"`
	RunnerName   string        `json:"runner_name"`
	Labels       []string      `json:"labels"`
	Steps        []ActionsStep `json:"steps"`
}

// Failing reports a completed job that did not pass.
func (j ActionsJob) Failing() bool {
	if j.Status != "completed" {
		return false
	}
	switch j.Conclusion {
	case "success", "neutral", "skipped", "":
		return false
	}
	return true
}

func checkActionsRepo(repo string) error {
	if !repoName.MatchString(repo) || strings.Contains(repo, "..") {
		return fmt.Errorf("repo must be owner/name")
	}
	return nil
}

// ActionsJob returns one job and its steps.
func (c *Client) ActionsJob(ctx context.Context, token, repo string, jobID int64) (ActionsJob, error) {
	var j ActionsJob
	if err := checkActionsRepo(repo); err != nil {
		return j, err
	}
	body, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/actions/jobs/%d", repo, jobID))
	if err != nil {
		if strings.Contains(err.Error(), "status 404") {
			return j, fmt.Errorf("Actions job %d not found in %s, or not visible with this token (status 404)", jobID, repo)
		}
		return j, fmt.Errorf("Actions job %d: %w", jobID, err)
	}
	if err := json.Unmarshal(body, &j); err != nil {
		return j, fmt.Errorf("Actions job %d: %w", jobID, err)
	}
	return j, nil
}

// ActionsRunJobs returns a workflow run's jobs and their steps: the latest attempt's
// jobs, or attempt's when it is above 0. complete is false when the run holds more
// jobs than were read.
func (c *Client) ActionsRunJobs(ctx context.Context, token, repo string, runID int64, attempt int) (jobs []ActionsJob, complete bool, err error) {
	if err := checkActionsRepo(repo); err != nil {
		return nil, false, err
	}
	base := fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?filter=latest&", repo, runID)
	if attempt > 0 {
		base = fmt.Sprintf("/repos/%s/actions/runs/%d/attempts/%d/jobs?", repo, runID, attempt)
	}
	for page := 1; page <= actionsJobsMaxPages; page++ {
		body, err := c.get(ctx, token, fmt.Sprintf("%sper_page=%d&page=%d", base, actionsJobsPerPage, page))
		if err != nil {
			if strings.Contains(err.Error(), "status 404") {
				return nil, false, fmt.Errorf("Actions run %d not found in %s, or not visible with this token (status 404)", runID, repo)
			}
			return nil, false, fmt.Errorf("Actions run %d jobs: %w", runID, err)
		}
		var r struct {
			TotalCount int          `json:"total_count"`
			Jobs       []ActionsJob `json:"jobs"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, false, fmt.Errorf("Actions run %d jobs: %w", runID, err)
		}
		jobs = append(jobs, r.Jobs...)
		if len(r.Jobs) < actionsJobsPerPage || len(jobs) >= r.TotalCount {
			return jobs, true, nil
		}
	}
	return jobs, false, nil
}

// ActionsJobLog downloads one job's plain-text log into w, writing at most max bytes,
// and returns the bytes written. A log over max is ErrLogTooLarge; a missing or
// invisible one ErrLogNotFound.
//
// The API answers with a redirect to a short-lived, pre-signed URL on a blob host.
// The token is sent to the API host only: the API request is a single round trip,
// whose redirect is read rather than followed, and the log is fetched by a separate client that carries no
// credential, strips any Authorization, Cookie, Proxy-Authorization or Referer
// header, and requires https (unless the API itself is plain http, a test stub).
// The pre-signed URL is itself a credential, so no error ever includes it, and a
// link refused by the blob host (expired before it was used) is replaced by a
// fresh one once.
func (c *Client) ActionsJobLog(ctx context.Context, token, repo string, jobID int64, w io.Writer, max int64) (int64, error) {
	if err := checkActionsRepo(repo); err != nil {
		return 0, err
	}
	return c.downloadLog(ctx, token, fmt.Sprintf("/repos/%s/actions/jobs/%d/logs", repo, jobID), "job log", w, max)
}

// ActionsStepLog downloads one step's log (its lines alone, numbered from 1) the
// same way ActionsJobLog downloads a job's.
func (c *Client) ActionsStepLog(ctx context.Context, token, repo string, jobID int64, step int, w io.Writer, max int64) (int64, error) {
	if err := checkActionsRepo(repo); err != nil {
		return 0, err
	}
	if step <= 0 {
		return 0, fmt.Errorf("step must be a positive step number")
	}
	return c.downloadLog(ctx, token, fmt.Sprintf("/repos/%s/actions/jobs/%d/steps/%d/logs", repo, jobID, step), "step log", w, max)
}

// downloadLog asks the API at path for a log's download link and fetches it. A link
// the blob host refuses (403/404: it expired between the redirect and the fetch) is
// asked for again once; nothing has been written to w by then.
func (c *Client) downloadLog(ctx context.Context, token, path, what string, w io.Writer, max int64) (int64, error) {
	for attempt := 1; ; attempt++ {
		loc, n, err := c.logLocation(ctx, token, path, what, w, max)
		if loc == nil {
			return n, err // served directly, or refused by the API
		}
		n, status, err := c.fetchBlob(ctx, loc, what, w, max)
		if status == http.StatusForbidden || status == http.StatusNotFound {
			if attempt < 2 {
				continue
			}
			return 0, fmt.Errorf("%s download: status %d, again with a fresh download link", what, status)
		}
		return n, err
	}
}

// logLocation requests a log from the API without following its redirect. It returns
// the redirect's location, or nil when the API served the log itself (written to w)
// or refused.
func (c *Client) logLocation(ctx context.Context, token, path, what string, w io.Writer, max int64) (*url.URL, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("build %s request", what)
	}
	setHeaders(req, token)
	// One round trip through the client's transport (its retries included), not
	// Client.Do: the client parses a redirect's Location before CheckRedirect can stop
	// it, and quotes a malformed one, pre-signed URL and all, in its error. Here only
	// resp.Location() below parses it, and its error is not passed on.
	if t := c.http.Timeout; t > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t)
		defer cancel()
		req = req.WithContext(ctx)
	}
	rt := c.http.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", what, stripURL(err))
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		// A server that serves the log itself (no redirect). The URL is the API's own,
		// not a credential.
		n, err := copyBounded(w, resp, max, func(err error) error { return fmt.Errorf("%s: %w", what, stripURL(err)) })
		return nil, n, err
	case resp.StatusCode == http.StatusNotFound:
		return nil, 0, fmt.Errorf("%w (status 404): the job or step does not exist, or its repository is not visible with this token%s", ErrLogNotFound, apiReason(resp).String())
	case resp.StatusCode == http.StatusGone:
		return nil, 0, fmt.Errorf("%w (status 410): the log has expired or was deleted%s", ErrLogNotFound, apiReason(resp).String())
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests:
		r := apiReason(resp)
		if r.rateLimited || resp.StatusCode == http.StatusTooManyRequests {
			return nil, 0, fmt.Errorf("%s: %w (status %d)%s", what, ErrLogRateLimited, resp.StatusCode, r.String())
		}
		return nil, 0, &LogAccessError{What: what, Status: resp.StatusCode, reason: r}
	case resp.StatusCode < 300 || resp.StatusCode > 399:
		return nil, 0, fmt.Errorf("%s: status %d%s", what, resp.StatusCode, apiReason(resp).String())
	}
	loc, err := resp.Location()
	if err != nil {
		return nil, 0, fmt.Errorf("%s: redirect without a usable location", what)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return loc, 0, nil
}

// reason is what an API error response says about itself: GitHub's message, the
// token's granted OAuth scopes and the scopes the endpoint accepts. None of it is a
// credential: the body comes from the API host (never the blob host), and scope
// headers hold scope names only.
type reason struct {
	message     string
	tokenScopes *string // X-OAuth-Scopes, when sent
	accepted    *string // X-Accepted-OAuth-Scopes, when sent
	sso         bool    // X-GitHub-SSO: the organisation requires SAML SSO for this token
	rateLimited bool
}

const maxReasonBody = 8 << 10

// apiReason reads an API error response's reason (a bounded read of its body).
func apiReason(resp *http.Response) reason {
	var r reason
	var body struct {
		Message string `json:"message"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxReasonBody))
	if json.Unmarshal(raw, &body) == nil {
		r.message = clipReason(body.Message)
	}
	if v, ok := resp.Header["X-Oauth-Scopes"]; ok && len(v) > 0 {
		r.tokenScopes = &v[0]
	}
	if v, ok := resp.Header["X-Accepted-Oauth-Scopes"]; ok && len(v) > 0 {
		r.accepted = &v[0]
	}
	r.sso = resp.Header.Get("X-GitHub-SSO") != ""
	r.rateLimited = resp.Header.Get("X-RateLimit-Remaining") == "0" ||
		strings.Contains(strings.ToLower(r.message), "rate limit")
	return r
}

// String renders the reason as a suffix for an error ("" when there is none). The
// scopes come first: they are short and decide the diagnosis, and GitHub's message
// can run to hundreds of bytes, so a caller that clips the error keeps them.
func (r reason) String() string {
	var parts []string
	scopes := func(s string) string {
		if strings.TrimSpace(s) == "" {
			return "(none)"
		}
		return clipReason(s)
	}
	if r.tokenScopes != nil {
		parts = append(parts, "the token's scopes: "+scopes(*r.tokenScopes))
	}
	if r.accepted != nil {
		parts = append(parts, "scopes this endpoint accepts: "+scopes(*r.accepted))
	}
	if r.sso {
		parts = append(parts, "the organisation requires SAML SSO authorisation for this token")
	}
	if r.message != "" {
		parts = append(parts, fmt.Sprintf("GitHub says %q", r.message))
	}
	if len(parts) == 0 {
		return ""
	}
	return "; " + strings.Join(parts, "; ")
}

// clipReason flattens s to one line of at most 300 bytes of valid UTF-8.
func clipReason(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		cut := 300
		for cut > 0 && (s[cut]&0xC0) == 0x80 {
			cut--
		}
		s = s[:cut] + "…"
	}
	return strings.ToValidUTF8(s, "\uFFFD")
}

// fetchBlob downloads a pre-signed location with the credential-free client. status
// is the blob host's answer (0 when there was none).
func (c *Client) fetchBlob(ctx context.Context, loc *url.URL, what string, w io.Writer, max int64) (n int64, status int, err error) {
	blob, err := http.NewRequestWithContext(ctx, http.MethodGet, loc.String(), nil)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: bad redirect location", what)
	}
	blob.Header.Set("User-Agent", "gofer-agent")
	resp, err := c.blobClient().Do(blob)
	if err != nil {
		return 0, 0, blobError(ctx, what, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, resp.StatusCode, fmt.Errorf("%s download: status %d", what, resp.StatusCode)
	}
	n, err = copyBounded(w, resp, max, func(err error) error { return blobError(ctx, what, err) })
	return n, resp.StatusCode, err
}

// copyBounded copies resp's body to w, failing with ErrLogTooLarge past max bytes.
// A read error is reported through wrap.
func copyBounded(w io.Writer, resp *http.Response, max int64, wrap func(error) error) (int64, error) {
	if resp.ContentLength > max {
		return 0, fmt.Errorf("%w: %d bytes, over the %d-byte limit", ErrLogTooLarge, resp.ContentLength, max)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, max+1))
	if err != nil {
		return n, wrap(err)
	}
	if n > max {
		return n, fmt.Errorf("%w: over the %d-byte limit", ErrLogTooLarge, max)
	}
	return n, nil
}

// stripURL drops the URL net/http embeds in a transport error to the API host.
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// blobError reports a failed blob request without its text. Below the outer
// *url.Error, net/http can still quote the pre-signed URL (a redirect Location it
// could not parse, say), so nothing of it is kept but what a caller acts on:
// cancellation and timeouts keep their identity.
func blobError(ctx context.Context, what string, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("%s download: %w", what, cerr)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s download timed out: %w", what, context.DeadlineExceeded)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("%s download timed out: %w", what, context.DeadlineExceeded)
	}
	return fmt.Errorf("%s download failed (details withheld: they can contain the pre-signed download URL)", what)
}

// blobClient is the credential-free client for log downloads, built on first use.
func (c *Client) blobClient() *http.Client {
	c.blobOnce.Do(func() {
		if c.blob != nil {
			return
		}
		var rt http.RoundTripper = http.DefaultTransport
		if t, ok := http.DefaultTransport.(*http.Transport); ok {
			rt = t.Clone() // never a caller's transport, which might add a credential
		}
		allowHTTP := strings.HasPrefix(c.baseURL, "http://")
		c.blob = httpretry.Wrap(&http.Client{
			Timeout:   logBlobTimeout,
			Transport: &blobGuard{base: rt, allowHTTP: allowHTTP},
			CheckRedirect: func(_ *http.Request, via []*http.Request) error {
				if len(via) >= maxLogRedirect {
					return errors.New("too many redirects")
				}
				return nil
			},
		})
	})
	return c.blob
}

// blobGuard is the last RoundTripper before the network for log downloads: it
// removes every credential header (and Referer, which can carry a pre-signed URL)
// and refuses anything but https, so no caller mistake can send the GitHub token or
// a download link to a blob host.
type blobGuard struct {
	base      http.RoundTripper
	allowHTTP bool
}

func (g *blobGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" && !(g.allowHTTP && req.URL.Scheme == "http") {
		return nil, fmt.Errorf("log download: refusing a %s:// location", req.URL.Scheme)
	}
	if req.URL.User != nil {
		return nil, fmt.Errorf("log download: refusing a location with user info")
	}
	// Referer too: on a redirect from the blob host, net/http sets it to the previous
	// URL, pre-signed query and all.
	drop := []string{"Authorization", "Cookie", "Proxy-Authorization", "Referer"}
	for _, h := range drop {
		if req.Header.Get(h) != "" {
			req = req.Clone(req.Context())
			for _, h := range drop {
				req.Header.Del(h)
			}
			break
		}
	}
	return g.base.RoundTrip(req)
}
