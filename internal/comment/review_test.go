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

func (g *fakeReviewGH) PendingReview(_ context.Context, token, _ string, _ int, _, _ string, comments []ghcomment.ReviewComment) (string, error) {
	g.token, g.got = token, comments
	if g.err != nil {
		return "", g.err
	}
	return "https://github.com/o/r/pull/1#pullrequestreview-9", nil
}

var testReview = Review{Repo: "o/r", Number: 1, Commit: "abc", Comments: []ghcomment.ReviewComment{{Path: "a.go", Line: 1, Side: "RIGHT", Body: "x"}}}

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
