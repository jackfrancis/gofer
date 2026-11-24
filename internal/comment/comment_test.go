package comment

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/auth"
	"github.com/jackfrancis/gofer/internal/ghcomment"
	"github.com/jackfrancis/gofer/internal/vault"
)

type fakeCreds struct {
	writeErr error
	// disconnectAfter, when > 0, makes WriteCredential answer "sign-in required"
	// from that call on (a disconnect mid-batch).
	disconnectAfter int
	writeCalls      int
	readErr         error
	rejected        []string
	readCalls       int
	mu              sync.Mutex
}

func (f *fakeCreds) CommentingState(context.Context, string) auth.CommentingState {
	return auth.CommentingState{Enabled: true, Connected: f.writeErr == nil}
}

func (f *fakeCreds) WriteCredential(context.Context, string) (vault.Credential, error) {
	f.mu.Lock()
	f.writeCalls++
	disconnected := f.disconnectAfter > 0 && f.writeCalls > f.disconnectAfter
	f.mu.Unlock()
	if disconnected {
		return vault.Credential{}, vault.ErrSignInRequired
	}
	if f.writeErr != nil {
		return vault.Credential{}, f.writeErr
	}
	return vault.Credential{Provider: auth.WriteProvider, AccessToken: "write-token", Version: "v1"}, nil
}

func (f *fakeCreds) WriteRejected(_ context.Context, _, version string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejected = append(f.rejected, version)
}

func (f *fakeCreds) Credential(_ context.Context, _, provider string, _ auth.VendRequest) (vault.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readCalls++
	if provider != "github" {
		return vault.Credential{}, vault.ErrNotFound
	}
	if f.readErr != nil {
		return vault.Credential{}, f.readErr
	}
	return vault.Credential{Provider: "github", AccessToken: "read-token"}, nil
}

type fakeGH struct {
	mu        sync.Mutex
	posts     []string // repo#number, in order
	tokens    map[string]string
	fail      map[string]error // repo -> error
	assignees map[string][]string
	fetches   map[string]int
}

func (g *fakeGH) Comment(_ context.Context, token, repo string, number int, _ string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.tokens == nil {
		g.tokens = map[string]string{}
	}
	g.tokens["comment"] = token
	if err := g.fail[repo]; err != nil {
		return "", err
	}
	g.posts = append(g.posts, repo)
	return "https://github.com/" + repo + "/issues/1#c", nil
}

func (g *fakeGH) Assignees(_ context.Context, token, repo string) ([]string, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.tokens == nil {
		g.tokens = map[string]string{}
	}
	g.tokens["assignees"] = token
	if g.fetches == nil {
		g.fetches = map[string]int{}
	}
	g.fetches[repo]++
	if err := g.fail[repo]; err != nil {
		return nil, false, err
	}
	return g.assignees[repo], true, nil
}

func targets(repos ...string) []Target {
	out := make([]Target, len(repos))
	for i, r := range repos {
		out[i] = Target{ID: r, Repo: r, Number: 1}
	}
	return out
}

// Every target is posted, in order, with the write grant; a failure on one does not
// stop the others.
func TestPostPostsEachTarget(t *testing.T) {
	gh := &fakeGH{fail: map[string]error{"b/locked": &ghcomment.StatusError{Status: http.StatusForbidden, Message: "issue is locked"}}}
	s := newTestService(&fakeCreds{}, gh)
	res, err := s.Post(context.Background(), "u", targets("a/one", "b/locked", "c/three"), "/lgtm")
	if err != nil {
		t.Fatal(err)
	}
	if len(gh.posts) != 2 || gh.posts[0] != "a/one" || gh.posts[1] != "c/three" {
		t.Errorf("posts = %v", gh.posts)
	}
	if gh.tokens["comment"] != "write-token" {
		t.Errorf("posted with %q, not the write grant", gh.tokens["comment"])
	}
	if !res[0].Posted || res[1].Posted || !res[2].Posted || res[1].Error != "Not posted: GitHub refused: issue is locked" {
		t.Errorf("results = %+v", res)
	}
}

// A 401 reports the grant as rejected and attempts nothing more.
func TestPostStopsOnUnauthorized(t *testing.T) {
	creds := &fakeCreds{}
	gh := &fakeGH{fail: map[string]error{"b/two": ghcomment.ErrUnauthorized}}
	s := newTestService(creds, gh)
	res, err := s.Post(context.Background(), "u", targets("a/one", "b/two", "c/three"), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if len(gh.posts) != 1 {
		t.Errorf("posts after a 401: %v", gh.posts)
	}
	if res[2].Posted || res[2].Error == "" {
		t.Errorf("the target after a 401 = %+v", res[2])
	}
	if len(creds.rejected) != 1 || creds.rejected[0] != "v1" {
		t.Errorf("rejected = %v", creds.rejected)
	}
}

// Without a usable write grant nothing is attempted.
func TestPostNotConnected(t *testing.T) {
	gh := &fakeGH{}
	s := newTestService(&fakeCreds{writeErr: vault.ErrSignInRequired}, gh)
	if _, err := s.Post(context.Background(), "u", targets("a/one"), "hi"); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v", err)
	}
	if len(gh.posts) != 0 {
		t.Error("posted without a grant")
	}
}

// Suggestions are ranked by how many repositories can assign each login, read with
// the read credential, and a failed repository is reported, not fatal.
func TestMentionsRankAcrossRepos(t *testing.T) {
	gh := &fakeGH{
		assignees: map[string][]string{
			"a/one": {"alice", "Bob", "carol"},
			"b/two": {"bob", "carol"},
		},
		fail: map[string]error{"c/down": errors.New("boom")},
	}
	s := newTestService(&fakeCreds{}, gh)
	m, err := s.Mentions(context.Background(), "u", []string{"a/one", "b/two", "c/down", "A/One"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Selected != 3 || m.Checked != 2 || len(m.Failed) != 1 || m.Failed[0] != "c/down" {
		t.Errorf("selected=%d checked=%d failed=%v", m.Selected, m.Checked, m.Failed)
	}
	// Every count is against the three selected repositories; the one that failed
	// is unknown for everyone, never counted as assignable.
	want := []Mention{{"Bob", 2, 1}, {"carol", 2, 1}, {"alice", 1, 1}}
	if len(m.Users) != len(want) {
		t.Fatalf("users = %+v", m.Users)
	}
	for i := range want {
		if m.Users[i] != want[i] {
			t.Errorf("users[%d] = %+v, want %+v", i, m.Users[i], want[i])
		}
	}
	if gh.tokens["assignees"] != "read-token" {
		t.Errorf("read assignees with %q, not the read credential", gh.tokens["assignees"])
	}
	if gh.fetches["a/one"] != 1 {
		t.Errorf("a duplicate repository was fetched %d times", gh.fetches["a/one"])
	}
}

// A repository's list is reused within the TTL and read again after it; a failure
// is not cached.
func TestMentionsCache(t *testing.T) {
	gh := &fakeGH{assignees: map[string][]string{"a/one": {"alice"}}, fail: map[string]error{}}
	creds := &fakeCreds{}
	s := newTestService(creds, gh)
	now := time.Now()
	s.now = func() time.Time { return now }
	ctx := context.Background()
	_, _ = s.Mentions(ctx, "u", []string{"a/one"})
	_, _ = s.Mentions(ctx, "u", []string{"a/one"})
	if gh.fetches["a/one"] != 1 {
		t.Errorf("fetches within the TTL = %d, want 1", gh.fetches["a/one"])
	}
	if creds.readCalls != 1 {
		t.Errorf("read credential vended %d times, want 1 (none for a cached list)", creds.readCalls)
	}
	now = now.Add(assigneeTTL + time.Minute)
	_, _ = s.Mentions(ctx, "u", []string{"a/one"})
	if gh.fetches["a/one"] != 2 {
		t.Errorf("fetches after the TTL = %d, want 2", gh.fetches["a/one"])
	}
	gh.fail["b/two"] = errors.New("boom")
	_, _ = s.Mentions(ctx, "u", []string{"b/two"})
	_, _ = s.Mentions(ctx, "u", []string{"b/two"})
	if gh.fetches["b/two"] != 2 {
		t.Errorf("a failure was cached: fetches = %d", gh.fetches["b/two"])
	}
}

// With no usable read credential, suggestions say sign-in is required.
func TestMentionsSignInRequired(t *testing.T) {
	s := newTestService(&fakeCreds{readErr: vault.ErrSignInRequired}, &fakeGH{})
	if _, err := s.Mentions(context.Background(), "u", []string{"a/one"}); !errors.Is(err, vault.ErrSignInRequired) {
		t.Errorf("err = %v", err)
	}
}

func newTestService(creds Credentials, gh GitHub) *Service {
	s := New(creds, gh)
	s.spacing = 0
	return s
}

// Review P2: a rate limit stops the batch; the rest are left unattempted.
func TestPostStopsOnRateLimit(t *testing.T) {
	gh := &fakeGH{fail: map[string]error{"a/one": &ghcomment.RateLimitError{RetryAfter: 10 * time.Minute}}}
	s := newTestService(&fakeCreds{}, gh)
	res, err := s.Post(context.Background(), "u", targets("a/one", "b/two", "c/three"), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if len(gh.posts) != 0 {
		t.Errorf("posted after a rate limit: %v", gh.posts)
	}
	for i, r := range res {
		if r.Posted || r.Error == "" {
			t.Errorf("result %d = %+v", i, r)
		}
	}
	if res[1].Error[:len("Not attempted")] != "Not attempted" || !strings.Contains(res[0].Error, "10m") {
		t.Errorf("errors = %q / %q", res[0].Error, res[1].Error)
	}
}

// Review P2: a post whose answer was lost is unknown, not "not posted", and the
// batch carries on.
func TestPostUnknownOutcome(t *testing.T) {
	gh := &fakeGH{fail: map[string]error{"a/one": ghcomment.ErrOutcomeUnknown}}
	s := newTestService(&fakeCreds{}, gh)
	res, err := s.Post(context.Background(), "u", targets("a/one", "b/two"), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].Unknown || res[0].Posted || !strings.HasPrefix(res[0].Error, "Unknown") {
		t.Errorf("lost answer = %+v", res[0])
	}
	if !res[1].Posted {
		t.Errorf("the next target = %+v", res[1])
	}
}

// Posts are spaced apart, and a cancelled batch stops waiting.
func TestPostSpacing(t *testing.T) {
	gh := &fakeGH{}
	s := New(&fakeCreds{}, gh)
	s.spacing = 30 * time.Millisecond
	start := time.Now()
	if _, err := s.Post(context.Background(), "u", targets("a/one", "b/two", "c/three"), "hi"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 60*time.Millisecond {
		t.Errorf("three posts took %s; want at least two spacings", d)
	}
	s.spacing = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	res, _ := s.Post(ctx, "u", targets("a/one", "b/two"), "hi")
	if !res[0].Posted || res[1].Posted || res[1].Error == "" {
		t.Errorf("results after the deadline = %+v", res)
	}
}

// Review P2: a cut list or a repository past the cap is unknown, not "cannot assign".
func TestMentionsCoverage(t *testing.T) {
	gh := &fakeGH{assignees: map[string][]string{}}
	var repos []string
	for i := 0; i < MaxMentionRepos+2; i++ {
		r := fmt.Sprintf("o/r%d", i)
		repos = append(repos, r)
		gh.assignees[r] = []string{"alice"}
	}
	s := newTestService(&fakeCreds{}, &truncating{fakeGH: gh, cut: "o/r0"})
	m, err := s.Mentions(context.Background(), "u", append(repos, "o/r1"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Selected != MaxMentionRepos+2 || m.Unchecked != 2 || len(m.Truncated) != 1 || m.Checked != MaxMentionRepos-1 {
		t.Fatalf("coverage = %+v", m)
	}
	if len(m.Users) != 1 || m.Users[0].Repos != MaxMentionRepos || m.Users[0].Unknown != 2 {
		t.Errorf("alice = %+v; want found in every checked repo, unknown in the two unchecked", m.Users)
	}
}

// truncating reports one repository's list as cut.
type truncating struct {
	*fakeGH
	cut string
}

func (g *truncating) Assignees(ctx context.Context, token, repo string) ([]string, bool, error) {
	l, _, err := g.fakeGH.Assignees(ctx, token, repo)
	return l, repo != g.cut, err
}

// Review P2 (second round): a disconnect mid-batch stops the remaining posts, even
// though the batch holds a copy of the token GitHub may not yet have revoked.
func TestPostStopsWhenDisconnected(t *testing.T) {
	gh := &fakeGH{}
	s := newTestService(&fakeCreds{disconnectAfter: 2}, gh)
	res, err := s.Post(context.Background(), "u", targets("a/one", "b/two", "c/three"), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if len(gh.posts) != 2 {
		t.Errorf("posts = %v; want two, then stop", gh.posts)
	}
	if res[2].Posted || !strings.Contains(res[2].Error, "disconnected") {
		t.Errorf("the post after the disconnect = %+v", res[2])
	}
}
