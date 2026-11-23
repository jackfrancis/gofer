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

// Result is what happened to one target.
type Result struct {
	Target
	Posted     bool
	CommentURL string // the new comment, when GitHub gave a link
	Error      string // why it was not posted, for the user
}

// Mention is one suggested login.
type Mention struct {
	Login string `json:"login"`
	Repos int    `json:"repos"` // how many of the asked repositories can assign it
}

// Mentions is the answer to a suggestion request.
type Mentions struct {
	Users []Mention `json:"users"`
	// Repos is how many repositories answered; a Mention with fewer cannot be
	// assigned in all of them.
	Repos int `json:"repos"`
	// Failed names repositories whose list could not be read; Partial says some
	// list was cut at its bound.
	Failed  []string `json:"failed,omitempty"`
	Partial bool     `json:"partial,omitempty"`
}

// Service posts comments and serves suggestions.
type Service struct {
	creds Credentials
	gh    GitHub
	now   func() time.Time

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
	return &Service{creds: creds, gh: gh, now: time.Now, cache: map[string]cached{}}
}

// State reports whether commenting is configured and whether the user connected it.
func (s *Service) State(ctx context.Context, userID string) (enabled, connected bool) {
	st := s.creds.CommentingState(ctx, userID)
	return st.Enabled, st.Connected
}

// Post posts body to every target, in order, one at a time (GitHub asks for serial
// content creation). ErrNotConnected means nothing was attempted. A 401 that GitHub
// confirms deletes the grant (WriteRejected) and the remaining targets are not
// attempted.
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
		if stopped != "" {
			out[i].Error = stopped
			continue
		}
		if ctx.Err() != nil {
			stopped = "Not posted: gofer ran out of time."
			out[i].Error = stopped
			continue
		}
		link, err := s.gh.Comment(ctx, cred.AccessToken, t.Repo, t.Number, body)
		if err == nil {
			out[i].Posted, out[i].CommentURL = true, link
			continue
		}
		if errors.Is(err, ghcomment.ErrUnauthorized) {
			s.creds.WriteRejected(context.WithoutCancel(ctx), userID, cred.Version)
			stopped = "Not posted: GitHub rejected gofer's access. Connect commenting again."
			out[i].Error = stopped
			continue
		}
		out[i].Error = describe(err)
	}
	return out, nil
}

// describe turns a posting error into words for the user.
func describe(err error) string {
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
// assign each, then by login. It reads with the user's read credential and caches
// each repository's list for assigneeTTL.
func (s *Service) Mentions(ctx context.Context, userID string, repos []string) (Mentions, error) {
	repos = dedupe(repos)
	if len(repos) > MaxMentionRepos {
		repos = repos[:MaxMentionRepos]
	}
	if len(repos) == 0 {
		return Mentions{Users: []Mention{}}, nil
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

	out := Mentions{Users: []Mention{}}
	type tally struct {
		login string
		n     int
	}
	counts := map[string]*tally{}
	for _, a := range answers {
		if a.err != nil {
			out.Failed = append(out.Failed, a.repo)
			continue
		}
		out.Repos++
		if !a.c.complete {
			out.Partial = true
		}
		seen := map[string]bool{}
		for _, l := range a.c.logins {
			k := strings.ToLower(l)
			if seen[k] {
				continue
			}
			seen[k] = true
			if t := counts[k]; t != nil {
				t.n++
			} else {
				counts[k] = &tally{login: l, n: 1}
			}
		}
	}
	if out.Repos == 0 && len(out.Failed) > 0 {
		if errors.Is(tokenErr, vault.ErrSignInRequired) {
			return out, vault.ErrSignInRequired
		}
	}
	for _, t := range counts {
		out.Users = append(out.Users, Mention{Login: t.login, Repos: t.n})
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
