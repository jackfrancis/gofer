package github

// The public-only guard (ADR 0007, phase 1). gofer processes public repositories
// only, whatever the token can reach: a GitHub App can be installed on private
// repositories by anyone, and its user token then reads them. So the client itself
// enforces the boundary, for every tool and job alike, with no setting:
//
//   - every search carries is:public (searchIssues);
//   - every per-repository call is preceded by a visibility gate,
//     GET /repos/{owner}/{name}, which must answer "private": false. The gate's
//     canonical full_name is what the call then uses, and its answer is cached for the
//     life of the Client, which is one run;
//   - the client follows no API-host redirect on its own. A redirect is followed only
//     once its destination repository (/repos/{o}/{r}/… or /repositories/{id}/…) has
//     passed the gate;
//   - an issue or PR body naming another repository than the gated one is dropped
//     unless that repository passes the gate too.
//
// Redirects off the API host (a tarball to codeload, a log to the blob host) carry
// the gated repository's own content and are handled as before (ADR 0006).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// ErrNotPublic reports a repository the public-only guard refused: GitHub said it is
// private, or it could not be seen at all.
var ErrNotPublic = errors.New("gofer is configured for public repositories only")

// maxAPIRedirects bounds the API-host redirects one call follows (a rename, a
// transfer).
const maxAPIRedirects = 3

// verdict is what the gate knows about a repository.
type verdict int

const (
	// verdictUnknown: no answer (a timeout, a rate limit, another error). The call is
	// refused, and ingest counts its fetch as incomplete.
	verdictUnknown verdict = iota
	// verdictPublic: GitHub answered "private": false.
	verdictPublic
	// verdictPrivate: GitHub answered "private": true.
	verdictPrivate
	// verdictInaccessible: GitHub answered 404, or a 403 that is not a rate limit.
	verdictInaccessible
)

// gateResult is one visibility check's outcome. It holds no error text: a refusal is
// worded by the caller, from the name the caller asked for (errFor) or with no name
// at all (hiddenErr), so a private repository's name that came from GitHub (a
// redirect's destination, a rename, another repository named in a body) never
// reaches the model.
type gateResult struct {
	verdict  verdict
	fullName string // canonical owner/name, when GitHub answered; never put in an error
	invalid  bool   // the name was not owner/name; nothing was asked
	status   int    // GitHub's status, for an inaccessible or unknown answer
	detail   string // why an answer was inaccessible or unknown, naming no repository
}

// errFor words a refusal of the repository the caller asked for by name. nil for a
// public one.
func (r gateResult) errFor(asked string) error {
	switch r.verdict {
	case verdictPublic:
		return nil
	case verdictPrivate:
		return fmt.Errorf("%s is private: %w", asked, ErrNotPublic)
	case verdictInaccessible:
		if r.invalid {
			return fmt.Errorf("repo must be owner/name")
		}
		if r.status == http.StatusForbidden {
			return fmt.Errorf("GitHub refused access to %s (status 403)%s", asked, r.detail)
		}
		return fmt.Errorf("%s was not found, or is not visible with this token (status %d)", asked, r.status)
	}
	return fmt.Errorf("could not confirm that %s is public (%s); try again", asked, r.detail)
}

// hiddenErr words a refusal of a repository the caller did not name: the
// destination of a redirect, or another repository a response named. It says nothing
// about which repository that was. nil for a public one.
func (r gateResult) hiddenErr() error {
	switch r.verdict {
	case verdictPublic:
		return nil
	case verdictUnknown:
		return errors.New("GitHub answered with another repository, and gofer could not confirm that it is public; try again")
	}
	return fmt.Errorf("GitHub answered with another repository, which is not public: %w", ErrNotPublic)
}

// repoGate caches definite answers for the life of the Client (one run). Unknown
// answers are not cached, so a later call in the run asks again.
type repoGate struct {
	mu     sync.Mutex
	byName map[string]gateResult // lower-cased owner/name, as asked and as canonical
	byID   map[int64]gateResult
}

func (g *repoGate) lookupName(name string) (gateResult, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, ok := g.byName[strings.ToLower(name)]
	return r, ok
}

func (g *repoGate) lookupID(id int64) (gateResult, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, ok := g.byID[id]
	return r, ok
}

func (g *repoGate) store(r gateResult, id int64, names ...string) {
	if r.verdict == verdictUnknown {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.byName == nil {
		g.byName = make(map[string]gateResult)
		g.byID = make(map[int64]gateResult)
	}
	for _, n := range names {
		if n != "" {
			g.byName[strings.ToLower(n)] = r
		}
	}
	if id > 0 {
		g.byID[id] = r
	}
}

// validRepo reports whether repo is a plain owner/name, the only shape the client
// puts in a /repos/ path. It is the one rule for a repository name across the client.
// Only a component that IS "." or ".." is refused, as unsafePath refuses such a path
// segment: GitHub allows dots in names, and a repository can be called "...".
func validRepo(repo string) bool {
	if !repoName.MatchString(repo) {
		return false
	}
	owner, name, _ := strings.Cut(repo, "/")
	return owner != "." && owner != ".." && name != "." && name != ".."
}

// gate checks that repo is public and returns its canonical owner/name. Any other
// answer is an error: ErrNotPublic for a private repository, a not-found error for
// one GitHub will not show, and an "could not confirm" error when there was no answer.
func (c *Client) gate(ctx context.Context, token, repo string) (string, error) {
	r := c.checkRepo(ctx, token, repo)
	if r.verdict != verdictPublic {
		return "", r.errFor(repo)
	}
	return r.fullName, nil
}

// checkRepo runs the visibility check for repo (owner/name), cached per run.
func (c *Client) checkRepo(ctx context.Context, token, repo string) gateResult {
	if !validRepo(repo) {
		return gateResult{verdict: verdictInaccessible, invalid: true}
	}
	if r, ok := c.repos.lookupName(repo); ok {
		return r
	}
	r, id := c.lookupRepo(ctx, token, "/repos/"+repo)
	c.repos.store(r, id, repo, r.fullName)
	return r
}

// checkRepoID runs the visibility check for a repository named by its numeric ID (the
// form a rename or transfer redirect takes), cached per run.
func (c *Client) checkRepoID(ctx context.Context, token string, id int64) gateResult {
	if r, ok := c.repos.lookupID(id); ok {
		return r
	}
	r, gotID := c.lookupRepo(ctx, token, "/repositories/"+strconv.FormatInt(id, 10))
	if gotID == 0 {
		gotID = id
	}
	c.repos.store(r, gotID, r.fullName)
	return r
}

// lookupRepo asks the API for a repository's metadata at path and judges the final
// answer. A rename redirect is followed within the API host, to another repository
// metadata path only: this request reads nothing but the verdict, which is taken
// from the response it ends on.
func (c *Client) lookupRepo(ctx context.Context, token, path string) (gateResult, int64) {
	unknown := func(format string, a ...any) gateResult {
		return gateResult{verdict: verdictUnknown, detail: fmt.Sprintf(format, a...)}
	}
	u := c.baseURL + path
	for hop := 0; ; hop++ {
		resp, err := c.apiDo(ctx, token, u)
		if err != nil {
			return unknown("%v", stripURL(err)), 0
		}
		if isRedirect(resp.StatusCode) {
			loc, lerr := resp.Location()
			drain(resp)
			if lerr != nil || hop >= maxAPIRedirects || unsafePath(loc.EscapedPath()) {
				return unknown("unusable redirect"), 0
			}
			kind, _, _ := c.repoFromLocation(loc)
			rel, _ := c.apiRelPath(loc)
			if kind == "" || strings.Count(strings.Trim(rel, "/"), "/") > 2 {
				return unknown("redirect to something other than a repository"), 0
			}
			u = loc.String()
			continue
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		status := resp.StatusCode
		var why reason
		if status != http.StatusOK {
			why = reasonFrom(resp.Header, body)
		}
		resp.Body.Close()
		if rerr != nil {
			return unknown("%v", rerr), 0
		}
		switch {
		case status == http.StatusOK:
		case status == http.StatusNotFound || status == http.StatusGone:
			return gateResult{verdict: verdictInaccessible, status: status}, 0
		case status == http.StatusForbidden && !why.rateLimited:
			r := gateResult{verdict: verdictInaccessible, status: status}
			// GitHub's free text can name the repository. After a rename redirect that
			// is not the name the caller asked for, so only the status is kept.
			if hop == 0 {
				r.detail = why.String()
			}
			return r, 0
		default: // 401 (a token to refresh), 429 and rate-limited 403s, 5xx
			return unknown("status %d", status), 0
		}
		var meta struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
			Private  *bool  `json:"private"`
		}
		if err := json.Unmarshal(body, &meta); err != nil || meta.Private == nil || !validRepo(meta.FullName) {
			return unknown("the answer did not say"), 0
		}
		if *meta.Private {
			return gateResult{verdict: verdictPrivate, fullName: meta.FullName}, meta.ID
		}
		return gateResult{verdict: verdictPublic, fullName: meta.FullName}, meta.ID
	}
}

// unsafePath reports whether an escaped URL path (its query, if any, is ignored)
// could address something other than what it spells: a "." or ".." segment, also
// percent-encoded or inside an encoded slash, or a backslash. A server or proxy that
// normalised such a path could serve another repository than the one gated, so the
// client sends none.
func unsafePath(escaped string) bool {
	p, _, _ := strings.Cut(escaped, "?")
	for _, seg := range strings.Split(p, "/") {
		dec, err := url.PathUnescape(seg)
		if err != nil || strings.Contains(dec, "\\") {
			return true
		}
		for _, piece := range strings.Split(dec, "/") {
			if piece == "." || piece == ".." {
				return true
			}
		}
	}
	return false
}

// gatePath gates the repository a /repos/{owner}/{name}[/…] path names and returns
// the path with the canonical name. Any other path is returned unchanged. A path
// with a dot segment is refused outright (unsafePath).
func (c *Client) gatePath(ctx context.Context, token, path string) (string, error) {
	if unsafePath(path) {
		return "", fmt.Errorf("refusing a request path with a \".\" or \"..\" segment")
	}
	rest, ok := strings.CutPrefix(path, "/repos/")
	if !ok {
		return path, nil
	}
	owner, rest, _ := strings.Cut(rest, "/")
	name, tail, hasTail := strings.Cut(rest, "/")
	canon, err := c.gate(ctx, token, owner+"/"+name)
	if err != nil {
		return "", err
	}
	out := "/repos/" + canon
	if hasTail {
		out += "/" + tail
	}
	return out, nil
}

// apiRelPath returns loc's path relative to the API base (which has a path prefix on
// GitHub Enterprise), and whether loc is on the API host at all.
func (c *Client) apiRelPath(loc *url.URL) (string, bool) {
	base, err := url.Parse(c.baseURL)
	if err != nil || loc.User != nil ||
		!strings.EqualFold(loc.Scheme, base.Scheme) || !strings.EqualFold(loc.Host, base.Host) {
		return "", false
	}
	rel, ok := strings.CutPrefix(loc.Path, strings.TrimRight(base.Path, "/"))
	if !ok || (rel != "" && !strings.HasPrefix(rel, "/")) {
		return "", false
	}
	return rel, true
}

// repoFromLocation says which repository an API-host location names: kind "name"
// with owner/name, kind "id" with the numeric ID, or kind "" for neither (or off the
// API host).
func (c *Client) repoFromLocation(loc *url.URL) (kind, repo string, id int64) {
	rel, ok := c.apiRelPath(loc)
	if !ok {
		return "", "", 0
	}
	segs := strings.Split(strings.TrimPrefix(rel, "/"), "/")
	switch {
	case len(segs) >= 3 && segs[0] == "repos":
		if r := segs[1] + "/" + segs[2]; validRepo(r) {
			return "name", r, 0
		}
	case len(segs) >= 2 && segs[0] == "repositories":
		if n, err := strconv.ParseInt(segs[1], 10, 64); err == nil && n > 0 {
			return "id", "", n
		}
	}
	return "", "", 0
}

// gateLocation decides whether an API-host redirect may be followed. A destination
// naming a repository is followed only if that repository passes the gate. One naming
// none is followed only when the original request was not about a repository either.
func (c *Client) gateLocation(ctx context.Context, token string, loc *url.URL, repoScoped bool) error {
	if unsafePath(loc.EscapedPath()) {
		return fmt.Errorf("GitHub redirected to a path with a dot segment: %w", ErrNotPublic)
	}
	switch kind, repo, id := c.repoFromLocation(loc); kind {
	case "name":
		return c.checkRepo(ctx, token, repo).hiddenErr()
	case "id":
		return c.checkRepoID(ctx, token, id).hiddenErr()
	}
	if repoScoped {
		return fmt.Errorf("GitHub redirected a repository request to a location that names no repository: %w", ErrNotPublic)
	}
	return nil
}

// repoFromAPIURL returns the owner/name an API repository URL names (an issue's
// repository_url), or "" when it names none.
func (c *Client) repoFromAPIURL(raw string) string {
	if raw == "" {
		return ""
	}
	for _, base := range []string{c.baseURL, defaultBaseURL} {
		if r, ok := strings.CutPrefix(raw, base+"/repos/"); ok {
			return r
		}
	}
	return raw // an unexpected form: never equal to a gated name, so it is gated itself
}

// confirmSameOrPublic is the body backstop: an issue or PR body that names another
// repository than the gated one (a transfer the API answered without a redirect) is
// dropped unless that repository passes the gate too. other "" names nothing.
func (c *Client) confirmSameOrPublic(ctx context.Context, token, gated, other string) error {
	if other == "" || strings.EqualFold(other, gated) {
		return nil
	}
	if !validRepo(other) {
		return fmt.Errorf("GitHub answered for an unrecognised repository: %w", ErrNotPublic)
	}
	return c.checkRepo(ctx, token, other).hiddenErr()
}

// backstopError is a response refused because it names another repository than the
// gated one, which did not pass the gate. The whole response is dropped.
type backstopError struct{ err error }

func (e *backstopError) Error() string { return e.err.Error() }
func (e *backstopError) Unwrap() error { return e.err }

// getIssue reads an issue (or a PR through the issues endpoint) of a gated
// repository, refusing an answer whose repository_url names another repository that
// is not public.
func (c *Client) getIssue(ctx context.Context, token, repo string, number int) ([]byte, error) {
	body, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/issues/%d", repo, number))
	if err != nil {
		return nil, err
	}
	var is struct {
		RepositoryURL string `json:"repository_url"`
	}
	if err := json.Unmarshal(body, &is); err != nil {
		return nil, err
	}
	if err := c.confirmSameOrPublic(ctx, token, repo, c.repoFromAPIURL(is.RepositoryURL)); err != nil {
		return nil, &backstopError{err}
	}
	return body, nil
}

// getPull reads a pull request of a gated repository, refusing an answer whose
// base.repo names another repository that is not public. Every reader of a PR answer
// goes through it, before using any of the answer (its head SHA included).
func (c *Client) getPull(ctx context.Context, token, repo string, number int) ([]byte, error) {
	body, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/pulls/%d", repo, number))
	if err != nil {
		return nil, err
	}
	var pr struct {
		Base struct {
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, err
	}
	if err := c.confirmSameOrPublic(ctx, token, repo, pr.Base.Repo.FullName); err != nil {
		return nil, &backstopError{err}
	}
	return body, nil
}

// apiDo performs one authenticated GET against the API without following a redirect:
// the caller decides, after gating the destination.
func (c *Client) apiDo(ctx context.Context, token, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	setHeaders(req, token)
	return c.apiClient().Do(req)
}

// apiClient is the shared client with redirects left to the caller.
func (c *Client) apiClient() *http.Client {
	c.apiOnce.Do(func() {
		hc := *c.http
		hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		c.api = &hc
	})
	return c.api
}

// get performs an authenticated GET of an API path and returns the response body. A
// per-repository path is gated first and uses the canonical name; an API-host
// redirect is followed only to a repository that passes the gate; a redirect off the
// API host is refused (no JSON endpoint has one).
func (c *Client) get(ctx context.Context, token, path string) ([]byte, error) {
	gated, err := c.gatePath(ctx, token, path)
	if err != nil {
		return nil, err
	}
	repoScoped := strings.HasPrefix(path, "/repos/")
	u := c.baseURL + gated
	for hop := 0; ; hop++ {
		resp, err := c.apiDo(ctx, token, u)
		if err != nil {
			return nil, err
		}
		if isRedirect(resp.StatusCode) {
			loc, lerr := resp.Location()
			status := resp.StatusCode
			drain(resp)
			if lerr != nil {
				return nil, fmt.Errorf("status %d: redirect without a usable location", status)
			}
			if hop >= maxAPIRedirects {
				return nil, fmt.Errorf("status %d: too many redirects", status)
			}
			if _, on := c.apiRelPath(loc); !on {
				return nil, fmt.Errorf("status %d: refused a redirect off the API host", status)
			}
			if err := c.gateLocation(ctx, token, loc, repoScoped); err != nil {
				return nil, err
			}
			u = loc.String()
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("status %d", resp.StatusCode)
		}
		return body, nil
	}
}

// followGated is a CheckRedirect for a client that must follow off-host redirects (a
// tarball to codeload): an API-host hop is followed only to a repository that passes
// the gate; an off-host hop is followed as net/http does, which drops the token.
func (c *Client) followGated(token string) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if _, on := c.apiRelPath(req.URL); !on {
			// Off the API origin (codeload): no credential goes along. net/http drops
			// Authorization only when the hostname changes, so it would keep it for
			// another port on the same host, or for a subdomain.
			for _, h := range []string{"Authorization", "Cookie", "Proxy-Authorization"} {
				req.Header.Del(h)
			}
			return nil
		}
		return c.gateLocation(req.Context(), token, req.URL, true)
	}
}

func isRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// drain discards (a bounded part of) a response body and closes it.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

// publicQuery is a search query restricted to public repositories.
func publicQuery(q string) string {
	return strings.TrimSpace(q) + " is:public"
}

// hasTypeQualifier reports whether a search query already says whether it wants
// issues or pull requests. A GitHub App user token answers 422 to a query that does
// not. Text inside double quotes is a phrase to match, not a qualifier, so it is
// ignored ("adding is:pr support" asks for neither).
func hasTypeQualifier(q string) bool {
	var unquoted strings.Builder
	inQuote := false
	for _, r := range q {
		switch {
		case r == '"':
			inQuote = !inQuote
			unquoted.WriteRune(' ')
		case inQuote:
			unquoted.WriteRune(' ')
		default:
			unquoted.WriteRune(r)
		}
	}
	for _, f := range strings.Fields(strings.ToLower(unquoted.String())) {
		switch f {
		case "is:issue", "is:pr", "is:pull-request", "type:issue", "type:pr", "type:pull-request":
			return true
		}
	}
	return false
}
