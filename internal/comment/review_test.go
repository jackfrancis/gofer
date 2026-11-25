package comment

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackfrancis/gofer/internal/ghcomment"
	"github.com/jackfrancis/gofer/internal/vault"
)

type fakeReviewGH struct {
	fakeGH
	err   error
	token string
	got   []ghcomment.ReviewComment
}

func (g *fakeReviewGH) PendingReview(_ context.Context, token, _ string, _ int, rv ghcomment.Review) (string, error) {
	g.token, g.got = token, rv.Comments
	if g.err != nil {
		return "", g.err
	}
	return "https://github.com/o/r/pull/1#pullrequestreview-9", nil
}

var testReview = Review{Repo: "o/r", Number: 1, Review: ghcomment.Review{Commit: "abc", BaseRef: "main", Comments: []ghcomment.ReviewComment{{Path: "a.go", Line: 1, Side: "RIGHT", Body: "x"}}}}

// The review is created with the write grant, and each GitHub outcome becomes words
// for the user; a 401 marks the grant rejected.
func TestPendingReviewOutcomes(t *testing.T) {
	gh := &fakeReviewGH{}
	creds := &fakeCreds{}
	s := New(creds, gh)
	res, err := s.PendingReview(context.Background(), "u", testReview)
	if err != nil || !res.Created || res.URL == "" || gh.token != "write-token" || len(gh.got) != 1 {
		t.Fatalf("res %+v, err %v, token %q", res, err, gh.token)
	}
	for name, tc := range map[string]struct {
		err     error
		unknown bool
		want    string
	}{
		"unknown":  {ghcomment.ErrOutcomeUnknown, true, "may have been created"},
		"pending":  {ghcomment.ErrPendingExists, false, "already have a pending review"},
		"moved":    {ghcomment.ErrHeadMoved, false, "new commits"},
		"closed":   {ghcomment.ErrNotOpen, false, "no longer open"},
		"rate":     {&ghcomment.RateLimitError{}, false, "rate-limiting"},
		"refused":  {&ghcomment.StatusError{Status: 422, Message: "Line could not be resolved"}, false, "Line could not be resolved"},
		"rejected": {ghcomment.ErrUnauthorized, false, "Connect commenting again"},
	} {
		gh.err = tc.err
		res, err := s.PendingReview(context.Background(), "u", testReview)
		if err != nil || res.Created || res.Unknown != tc.unknown || !strings.Contains(res.Error, tc.want) {
			t.Errorf("%s: res %+v, err %v", name, res, err)
		}
	}
	if len(creds.rejected) != 1 || creds.rejected[0] != "v1" {
		t.Errorf("rejected = %v", creds.rejected)
	}
}

// Without a write grant nothing is attempted.
func TestPendingReviewNotConnected(t *testing.T) {
	gh := &fakeReviewGH{}
	s := New(&fakeCreds{writeErr: vault.ErrNotFound}, gh)
	if _, err := s.PendingReview(context.Background(), "u", testReview); !errors.Is(err, ErrNotConnected) || gh.got != nil {
		t.Fatalf("err %v, attempted %v", err, gh.got)
	}
}

// An organization blocking the commenting app is explained, with what to do, for a
// review ("Not created") and a comment ("Not posted") alike.
func TestAppRestrictionIsExplained(t *testing.T) {
	gh := &fakeReviewGH{err: &ghcomment.AppRestrictedError{Org: "kubernetes"}}
	res, err := New(&fakeCreds{}, gh).PendingReview(context.Background(), "u", testReview)
	if err != nil || res.Created || !strings.HasPrefix(res.Error, "Not created: The kubernetes organization blocks") || !strings.Contains(res.Error, "personal access token") {
		t.Errorf("review: %+v %v", res, err)
	}
	if got := describe(&ghcomment.AppRestrictedError{}); !strings.HasPrefix(got, "Not posted: This repository's organization blocks") {
		t.Errorf("comment: %q", got)
	}
	if got := describeAs("Not created", &ghcomment.StatusError{Status: 404}); got != "Not created: GitHub could not find the item, or your account cannot comment on it." {
		t.Errorf("other errors keep their wording under the new outcome: %q", got)
	}
}

// A write refused because the token's scopes were widened stops gofer using it, for a
// bulk comment (the rest of the batch is not attempted) and for a review. The words
// claim gofer's copy was deleted only when WriteScopesChanged confirmed it; otherwise
// they send the user to Disconnect.
func TestWidenedScopesStopTheToken(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		creds := &fakeCreds{widenDeletes: deleted}
		gh := &fakeReviewGH{err: ghcomment.ErrTokenScopes}
		res, err := New(creds, gh).PendingReview(context.Background(), "u", testReview)
		if err != nil || res.Created || !strings.Contains(res.Error, "scopes beyond public_repo") {
			t.Errorf("deleted=%v review: %+v %v", deleted, res, err)
		}
		g := &fakeGH{fail: map[string]error{"a/one": ghcomment.ErrTokenScopes}}
		s := New(creds, g)
		s.spacing = 0
		out, err := s.Post(context.Background(), "u", []Target{{Repo: "a/one", Number: 1}, {Repo: "b/two", Number: 2}}, "hi")
		if err != nil || out[0].Posted || !strings.Contains(out[1].Error, "Not attempted") || len(g.posts) != 0 {
			t.Errorf("deleted=%v batch: %+v %v posts %v", deleted, out, err, g.posts)
		}
		if len(creds.widened) != 2 || creds.widened[0] != "v1" {
			t.Errorf("deleted=%v WriteScopesChanged calls = %v", deleted, creds.widened)
		}
		for _, msg := range []string{res.Error, out[0].Error, out[1].Error} {
			if claims := strings.Contains(msg, "deleted its copy"); claims != deleted {
				t.Errorf("deleted=%v but the message claims deletion=%v: %q", deleted, claims, msg)
			}
			if !deleted && !strings.Contains(msg, "Disconnect commenting") {
				t.Errorf("a kept credential's message does not send the user to Disconnect: %q", msg)
			}
		}
	}
}

// A write refused because GitHub did not report the credential's scopes writes
// nothing, stops the batch, and keeps the credential (nothing says it is wrong).
func TestUnknownScopesStopTheWrite(t *testing.T) {
	creds := &fakeCreds{}
	gh := &fakeReviewGH{err: ghcomment.ErrScopesUnknown}
	res, err := New(creds, gh).PendingReview(context.Background(), "u", testReview)
	if err != nil || res.Created || !strings.Contains(res.Error, "could not confirm") {
		t.Errorf("review: %+v %v", res, err)
	}
	g := &fakeGH{fail: map[string]error{"a/one": ghcomment.ErrScopesUnknown}}
	s := New(creds, g)
	s.spacing = 0
	out, err := s.Post(context.Background(), "u", []Target{{Repo: "a/one", Number: 1}, {Repo: "b/two", Number: 2}}, "hi")
	if err != nil || out[0].Posted || !strings.Contains(out[1].Error, "Not attempted") || len(g.posts) != 0 {
		t.Errorf("batch: %+v %v posts %v", out, err, g.posts)
	}
	if len(creds.widened) != 0 || len(creds.rejected) != 0 {
		t.Errorf("an unknown answer touched the credential: widened %v rejected %v", creds.widened, creds.rejected)
	}
}
