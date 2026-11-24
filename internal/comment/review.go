package comment

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackfrancis/gofer/internal/ghcomment"
	"github.com/jackfrancis/gofer/internal/vault"
)

// Reviewer is the GitHub client's pending-review call (ghcomment.Client).
type Reviewer interface {
	PendingReview(ctx context.Context, token, repo string, number int, rv ghcomment.Review) (string, error)
}

// Review is a pending review the owner confirmed from a draft (ADR 0010): the pull
// request, and the review (its anchors, summary, comments and the commented files'
// patch digests).
type Review struct {
	Repo   string
	Number int
	ghcomment.Review
}

// ReviewResult is what happened: Created, Unknown (the request reached GitHub but
// its answer was lost, so it may have been created), or neither, with Error saying
// why, for the user.
type ReviewResult struct {
	Created bool
	Unknown bool
	URL     string
	Error   string
}

// PendingReview creates rv as a PENDING review with the user's write grant. It never
// submits the review: on GitHub it stays a draft only the user sees until they
// submit it there. ErrNotConnected means nothing was attempted. The review is one
// request, so unlike a bulk comment there is nothing to space out or stop midway;
// GitHub's one-pending-review rule, checked first, keeps a resubmit from creating a
// second.
func (s *Service) PendingReview(ctx context.Context, userID string, rv Review) (ReviewResult, error) {
	gh, ok := s.gh.(Reviewer)
	if !ok {
		return ReviewResult{}, errors.New("comment: the GitHub client cannot create reviews")
	}
	cred, err := s.creds.WriteCredential(ctx, userID)
	if errors.Is(err, vault.ErrSignInRequired) || errors.Is(err, vault.ErrNotFound) {
		return ReviewResult{}, ErrNotConnected
	}
	if err != nil {
		return ReviewResult{}, err
	}
	link, err := gh.PendingReview(ctx, cred.AccessToken, rv.Repo, rv.Number, rv.Review)
	var rl *ghcomment.RateLimitError
	var se *ghcomment.StatusError
	switch {
	case err == nil:
		return ReviewResult{Created: true, URL: link}, nil
	case errors.Is(err, ghcomment.ErrOutcomeUnknown):
		return ReviewResult{Unknown: true, Error: "GitHub's answer was lost, so the pending review may have been created. Check the pull request on GitHub before trying again."}, nil
	case errors.Is(err, ghcomment.ErrUnauthorized):
		s.creds.WriteRejected(context.WithoutCancel(ctx), userID, cred.Version)
		return ReviewResult{Error: "GitHub rejected gofer's access. Connect commenting again, then try again."}, nil
	case errors.Is(err, ghcomment.ErrPendingExists):
		return ReviewResult{Error: "You already have a pending review on this pull request, and GitHub allows one. Submit or discard it on GitHub, then try again."}, nil
	case errors.Is(err, ghcomment.ErrHeadMoved):
		return ReviewResult{Error: "The pull request changed since these comments were drafted (new commits, a new base branch, or a change merged into its base that touched these files), so their lines may mean something else now. Nothing was created; draft the review comments again."}, nil
	case errors.Is(err, ghcomment.ErrNotOpen):
		return ReviewResult{Error: "The pull request is no longer open."}, nil
	case errors.As(err, &rl):
		wait := "a while"
		if rl.RetryAfter > 0 {
			wait = rl.RetryAfter.Round(time.Minute).String()
			if rl.RetryAfter < time.Minute {
				wait = "a minute"
			}
		}
		return ReviewResult{Error: "GitHub is rate-limiting gofer. Try again in " + wait + "."}, nil
	case errors.As(err, &se) && se.Status == http.StatusUnprocessableEntity:
		msg := "GitHub refused the review"
		if se.Message != "" {
			msg += ": " + se.Message
		}
		return ReviewResult{Error: msg + ". Nothing was created."}, nil
	}
	return ReviewResult{Error: describeAs("Not created", err)}, nil
}
