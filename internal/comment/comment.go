// Package comment posts the user's own comment to the GitHub items they selected,
// and suggests whom to @-mention (ADR 0009). It runs in the web tier: the write grant
// it posts with never leaves this process, and no model is involved.
package comment

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackfrancis/gofer/internal/auth"
	"github.com/jackfrancis/gofer/internal/ghcomment"
	"github.com/jackfrancis/gofer/internal/vault"
)

// Bounds.
const (
	// MaxTargets is the most items one bulk comment may go to.
	MaxTargets = 50
	// MaxMentionRepos is the most repositories one suggestion request may name.
	MaxMentionRepos = 20
	// assigneeTTL is how long a repository's assignee list is reused.
	assigneeTTL = 6 * time.Hour
	// maxCachedRepos bounds the assignee cache.
	maxCachedRepos = 500
	// mentionFetchers is how many repositories' assignees are read at once.
	mentionFetchers = 4
	// postSpacing is the pause between two posts: GitHub asks for at least a second
	// between content-creating requests, and serial posting alone does not give it.
	postSpacing = time.Second
)

// ErrNotConnected means the user has no usable write grant: they must connect (or
// reconnect) commenting first.
var ErrNotConnected = errors.New("commenting is not connected")

// Credentials is the auth handler's side: the write grant for posting, the read
// credential for suggestions, and the write grant's state.
type Credentials interface {
	CommentingState(ctx context.Context, userID string) auth.CommentingState
	WriteCredential(ctx context.Context, userID string) (vault.Credential, error)
	WriteRejected(ctx context.Context, userID, version string)
	Credential(ctx context.Context, userID, provider string, req auth.VendRequest) (vault.Credential, error)
}

// GitHub is the GitHub client (ghcomment.Client).
type GitHub interface {
	Comment(ctx context.Context, token, repo string, number int, body string) (string, error)
	Assignees(ctx context.Context, token, repo string) ([]string, bool, error)
}

// Target is one item a comment goes to.
type Target struct {
	ID     string // the worklist item id
	Repo   string
	Number int
	Title  string
	URL    string // the item on GitHub
}

// Result is what happened to one target: Posted, Unknown (the request reached
// GitHub but its answer was lost, so it may have been posted), or neither (Error
// says why it was not posted, or why it was not attempted).
type Result struct {
	Target
	Posted     bool
	Unknown    bool
	CommentURL string // the new comment, when GitHub gave a link
	Error      string // for the user, when not Posted
}

// Mention is one suggested login, against the selected repositories: Repos can
// assign it, and in Unknown of them gofer could not tell (a list that could not be
// read, was cut at its bound without naming it, or was past the lookup cap). The
// rest cannot assign it.
type Mention struct {
	Login   string `json:"login"`
	Repos   int    `json:"repos"`
	Unknown int    `json:"unknown,omitempty"`
}

// Mentions is the answer to a suggestion request. Selected is how many distinct
// repositories were asked about: the denominator of every Mention.
type Mentions struct {
	Users    []Mention `json:"users"`
	Selected int       `json:"selected"`
	// Checked is how many repositories answered with their whole list. Failed names
	// those whose list could not be read, Truncated those whose list was cut, and
	// Unchecked counts those past MaxMentionRepos, which were not asked.
	Checked   int      `json:"checked"`
	Failed    []string `json:"failed,omitempty"`
	Truncated []string `json:"truncated,omitempty"`
	Unchecked int      `json:"unchecked,omitempty"`
}

// Service posts comments and serves suggestions.
type Service struct {
	creds   Credentials
	gh      GitHub
	now     func() time.Time
	spacing time.Duration // between posts; postSpacing

	mu    sync.Mutex
	cache map[string]cached // lower-cased repo -> its assignees
}

type cached struct {
	logins   []string
	complete bool
	at       time.Time
}

// New builds the service.
func New(creds Credentials, gh GitHub) *Service {
	return &Service{creds: creds, gh: gh, now: time.Now, spacing: postSpacing, cache: map[string]cached{}}
}

// State reports whether commenting is configured and whether the user connected it.
func (s *Service) State(ctx context.Context, userID string) (enabled, connected bool) {
	st := s.creds.CommentingState(ctx, userID)
	return st.Enabled, st.Connected
}

// Details is the rest of the commenting state the radar shows: whether the user may
// connect with a personal access token, and whether the connected grant is one, with
// its expiry (ADR 0011).
type Details struct {
	TokensAllowed bool
	Token         bool
	Expires       time.Time
}

// Details reports the user's commenting details.
func (s *Service) Details(ctx context.Context, userID string) Details {
	st := s.creds.CommentingState(ctx, userID)
	return Details{TokensAllowed: st.TokensAllowed, Token: st.Token, Expires: st.Expires}
}

// Post posts body to every target, in order, one at a time and at least spacing
// apart (GitHub asks for serial content creation, a second apart). ErrNotConnected
// means nothing was attempted. The batch stops, leaving the remaining targets
// unattempted, on a 401 (which GitHub's check-token confirms before the grant is
// deleted: WriteRejected), on a rate limit, when ctx ends, or when the grant is no
// longer usable locally: it is read again before every post after the first, so a
// disconnect (or logout) mid-batch stops the batch even if GitHub has not yet
// confirmed the revocation, and so does a grant of another version (disconnected and
// connected again): a batch never continues under a grant it did not start with. A post whose answer was lost is Unknown, never reported
// as not posted.
func (s *Service) Post(ctx context.Context, userID string, targets []Target, body string) ([]Result, error) {
	cred, err := s.creds.WriteCredential(ctx, userID)
	if errors.Is(err, vault.ErrSignInRequired) || errors.Is(err, vault.ErrNotFound) {
		return nil, ErrNotConnected
	}
	if err != nil {
		return nil, err
	}
	out := make([]Result, len(targets))
	stopped := ""
	for i, t := range targets {
		out[i].Target = t
		if stopped == "" && i > 0 && s.spacing > 0 {
			select {
			case <-time.After(s.spacing):
			case <-ctx.Done():
			}
		}
		if stopped == "" && ctx.Err() != nil {
			stopped = "Not attempted: gofer ran out of time."
		}
		if stopped == "" && i > 0 {
			fresh, err := s.creds.WriteCredential(ctx, userID)
			switch {
			case errors.Is(err, vault.ErrSignInRequired) || errors.Is(err, vault.ErrNotFound):
				stopped = "Not attempted: commenting was disconnected."
			case err != nil:
				stopped = "Not attempted: gofer could not confirm commenting is still connected."
			case fresh.Version != cred.Version:
				// Disconnected and connected again (or refreshed) since the batch
				// began: the batch was approved under the grant it started with, so it
				// never carries on under another.
				stopped = "Not attempted: commenting was reconnected while this batch ran."
			}
		}
		if stopped != "" {
			out[i].Error = stopped
			continue
		}
		link, err := s.gh.Comment(ctx, cred.AccessToken, t.Repo, t.Number, body)
		var rl *ghcomment.RateLimitError
		switch {
		case err == nil:
			out[i].Posted, out[i].CommentURL = true, link
		case errors.Is(err, ghcomment.ErrOutcomeUnknown):
			out[i].Unknown = true
			out[i].Error = "Unknown: GitHub's answer was lost, so this may have been posted. Check the item before posting again."
		case errors.Is(err, ghcomment.ErrUnauthorized):
			s.creds.WriteRejected(context.WithoutCancel(ctx), userID, cred.Version)
			out[i].Error = "Not posted: GitHub rejected gofer's access. Connect commenting again."
			stopped = "Not attempted: GitHub rejected gofer's access. Connect commenting again."
		case errors.As(err, &rl):
			wait := "a while"
			if rl.RetryAfter > 0 {
				wait = rl.RetryAfter.Round(time.Minute).String()
				if rl.RetryAfter < time.Minute {
					wait = "a minute"
				}
			}
			out[i].Error = "Not posted: GitHub is rate-limiting gofer. Try again in " + wait + "."
			stopped = "Not attempted: GitHub is rate-limiting gofer. Try again in " + wait + "."
		default:
			out[i].Error = describe(err)
		}
	}
	return out, nil
}

// describe turns a posting error into words for the user.
func describe(err error) string { return describeAs("Not posted", err) }

// describeAs is describe with the outcome's own word ("Not posted", "Not created").
func describeAs(outcome string, err error) string {
	if msg := describeKnown(err); msg != "" {
		return outcome + ": " + msg
	}
	return outcome + ": " + strings.TrimPrefix(describeStatus(err), "Not posted: ")
}

// describeKnown words the refusals the user can act on, or "".
func describeKnown(err error) string {
	var ar *ghcomment.AppRestrictedError
	if errors.As(err, &ar) {
		org := "This repository's organization"
		if ar.Org != "" {
			org = "The " + ar.Org + " organization"
		}
		return org + " blocks OAuth apps it has not approved, including gofer's commenting app. Disconnect commenting and connect it with a personal access token instead (if this gofer allows tokens), or ask the organization to approve the app."
	}
	return ""
}

// describeStatus words any other posting error.
func describeStatus(err error) string {
	var se *ghcomment.StatusError
	switch {
	case errors.Is(err, ghcomment.ErrNotPublic):
		return "Not posted: the repository is not public, or GitHub would not show it."
	case errors.Is(err, ghcomment.ErrInvalidRepo):
		return "Not posted: not a repository gofer recognises."
	case errors.As(err, &se) && se.Status == http.StatusForbidden && se.Message != "":
		return "Not posted: GitHub refused: " + se.Message
	case errors.As(err, &se) && se.Status == http.StatusNotFound:
		return "Not posted: GitHub could not find the item, or your account cannot comment on it."
	case errors.As(err, &se) && se.Message != "":
		return "Not posted: " + se.Message
	}
	return "Not posted: " + err.Error()
}

// Mentions suggests logins assignable in repos, ranked by how many of them can
// assign each, then by login. Every count is against all the distinct repositories
// asked about (Selected), not just those that answered: a login gofer could not
// check somewhere carries that as Unknown. It reads with the user's read credential,
// at most MaxMentionRepos repositories, and caches each list for assigneeTTL.
func (s *Service) Mentions(ctx context.Context, userID string, repos []string) (Mentions, error) {
	repos = dedupe(repos)
	out := Mentions{Users: []Mention{}, Selected: len(repos)}
	if len(repos) > MaxMentionRepos {
		out.Unchecked = len(repos) - MaxMentionRepos
		repos = repos[:MaxMentionRepos]
	}
	if len(repos) == 0 {
		return out, nil
	}
	var token string // fetched only if a repository is not cached
	var tokenErr error
	var tokenOnce sync.Once
	getToken := func() (string, error) {
		tokenOnce.Do(func() {
			cred, err := s.creds.Credential(ctx, userID, "github", auth.VendRequest{})
			token, tokenErr = cred.AccessToken, err
		})
		return token, tokenErr
	}

	type answer struct {
		repo string
		c    cached
		err  error
	}
	answers := make([]answer, len(repos))
	sem := make(chan struct{}, mentionFetchers)
	var wg sync.WaitGroup
	for i, repo := range repos {
		if c, ok := s.cached(repo); ok {
			answers[i] = answer{repo: repo, c: c}
			continue
		}
		wg.Add(1)
		go func(i int, repo string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			tok, err := getToken()
			if err != nil {
				answers[i] = answer{repo: repo, err: err}
				return
			}
			logins, complete, err := s.gh.Assignees(ctx, tok, repo)
			if err != nil {
				answers[i] = answer{repo: repo, err: err}
				return
			}
			c := cached{logins: logins, complete: complete, at: s.now()}
			s.store(repo, c)
			answers[i] = answer{repo: repo, c: c}
		}(i, repo)
	}
	wg.Wait()

	type tally struct {
		login     string
		found     int
		truncated int // truncated lists that named it
	}
	counts := map[string]*tally{}
	answered := 0 // lists read, whole or cut
	for _, a := range answers {
		if a.err != nil {
			out.Failed = append(out.Failed, a.repo)
			continue
		}
		answered++
		if a.c.complete {
			out.Checked++
		} else {
			out.Truncated = append(out.Truncated, a.repo)
		}
		seen := map[string]bool{}
		for _, l := range a.c.logins {
			k := strings.ToLower(l)
			if seen[k] {
				continue
			}
			seen[k] = true
			t := counts[k]
			if t == nil {
				t = &tally{login: l}
				counts[k] = t
			}
			t.found++
			if !a.c.complete {
				t.truncated++
			}
		}
	}
	if answered == 0 && len(out.Failed) > 0 && errors.Is(tokenErr, vault.ErrSignInRequired) {
		return out, vault.ErrSignInRequired
	}
	// Not checked for everyone: lists that failed and repositories past the cap; a
	// cut list that did not name a login is unknown for that login too.
	notChecked := len(out.Failed) + out.Unchecked
	for _, t := range counts {
		out.Users = append(out.Users, Mention{
			Login:   t.login,
			Repos:   t.found,
			Unknown: notChecked + len(out.Truncated) - t.truncated,
		})
	}
	sort.Slice(out.Users, func(i, j int) bool {
		a, b := out.Users[i], out.Users[j]
		if a.Repos != b.Repos {
			return a.Repos > b.Repos
		}
		return strings.ToLower(a.Login) < strings.ToLower(b.Login)
	})
	return out, nil
}

func (s *Service) cached(repo string) (cached, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cache[strings.ToLower(repo)]
	if !ok || s.now().Sub(c.at) > assigneeTTL {
		return cached{}, false
	}
	return c, true
}

func (s *Service) store(repo string, c cached) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cache) >= maxCachedRepos {
		// Evict the stale first, then the oldest.
		var oldest string
		for k, v := range s.cache {
			if s.now().Sub(v.at) > assigneeTTL {
				delete(s.cache, k)
				continue
			}
			if oldest == "" || v.at.Before(s.cache[oldest].at) {
				oldest = k
			}
		}
		if len(s.cache) >= maxCachedRepos && oldest != "" {
			delete(s.cache, oldest)
		}
	}
	s.cache[strings.ToLower(repo)] = c
}

// dedupe drops empty and repeated (case-insensitive) names, keeping order.
func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, r := range in {
		r = strings.TrimSpace(r)
		k := strings.ToLower(r)
		if r == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, r)
	}
	return out
}
