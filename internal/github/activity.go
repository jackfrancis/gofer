package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// get performs an authenticated GET and returns the response body.
func (c *Client) get(ctx context.Context, token, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	setHeaders(req, token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return body, nil
}

type timelineEvent struct {
	Event             string    `json:"event"`
	State             string    `json:"state"` // for "reviewed": approved | changes_requested | commented | dismissed
	CreatedAt         time.Time `json:"created_at"`
	SubmittedAt       time.Time `json:"submitted_at"`
	RequestedReviewer *struct {
		Login string `json:"login"`
	} `json:"requested_reviewer"`
	User *struct {
		Login string `json:"login"`
	} `json:"user"`
	Actor *struct {
		Login string `json:"login"`
	} `json:"actor"`
}

// Activity holds the signals derived from a single read of an item's timeline.
type Activity struct {
	Participants        int       // distinct people who commented or reviewed
	InboundRefs         int       // cross-references from other issues/PRs (hub centrality)
	OtherReviewers      int       // distinct reviewers other than login (someone else is engaged)
	AwaitingMeSince     time.Time // when login was asked to review with no engagement since; zero if none
	AwaitingOthersSince time.Time // when the ball is in others' court; zero if none
	RequestedByLogin    string    // who requested login's pending review (the actor); empty when none pending
}

// ItemActivity reads one page of the issue/PR timeline and derives the engagement
// and centrality signals from it in a single call, plus how long the item has
// been blocked on login's review.
func (c *Client) ItemActivity(ctx context.Context, token, repo string, number int, login string) (Activity, error) {
	events, err := c.fetchTimeline(ctx, token, repo, number)
	if err != nil {
		return Activity{}, err
	}
	participants := make(map[string]struct{})
	otherReviewers := make(map[string]struct{})
	var inbound int
	var requestedAt, myLastActivityAt, lastActivityAt time.Time
	var lastActor, requestedByLogin string
	var lastActivityDecisive bool
	for _, e := range events {
		switch e.Event {
		case "commented":
			l := eventLogin(e)
			if l == "" {
				break
			}
			participants[l] = struct{}{}
			if e.CreatedAt.After(lastActivityAt) {
				lastActivityAt, lastActor, lastActivityDecisive = e.CreatedAt, l, false
			}
			if l == login && e.CreatedAt.After(myLastActivityAt) {
				myLastActivityAt = e.CreatedAt
			}
		case "reviewed":
			if e.User == nil || e.User.Login == "" {
				break
			}
			l := e.User.Login
			participants[l] = struct{}{}
			at := e.SubmittedAt
			if at.IsZero() {
				at = e.CreatedAt
			}
			if at.After(lastActivityAt) {
				lastActivityAt, lastActor = at, l
				lastActivityDecisive = e.State == "changes_requested" || e.State == "approved"
			}
			if l == login {
				if at.After(myLastActivityAt) {
					myLastActivityAt = at
				}
			} else {
				otherReviewers[l] = struct{}{}
			}
		case "cross-referenced":
			inbound++
		case "review_requested":
			if e.RequestedReviewer != nil && e.RequestedReviewer.Login == login && e.CreatedAt.After(requestedAt) {
				requestedAt = e.CreatedAt
				requestedByLogin = eventLogin(e)
			}
		}
	}
	a := Activity{Participants: len(participants), InboundRefs: inbound, OtherReviewers: len(otherReviewers)}

	// Decide whose court the ball is in from the most recent court-changing event.
	// Ball on login: a review was requested of them and they have not engaged
	// since. Ball on others: login had the last word, or someone's decisive review
	// is the last word so progress is on the author.
	var meSince, othersSince time.Time
	if !requestedAt.IsZero() && requestedAt.After(myLastActivityAt) {
		meSince = requestedAt
	}
	switch {
	case lastActor == login && !myLastActivityAt.IsZero():
		othersSince = myLastActivityAt
	case lastActivityDecisive:
		othersSince = lastActivityAt
	}
	switch {
	case meSince.After(othersSince):
		a.AwaitingMeSince = meSince
		a.RequestedByLogin = requestedByLogin
	case !othersSince.IsZero():
		a.AwaitingOthersSince = othersSince
	}
	return a, nil
}

func (c *Client) fetchTimeline(ctx context.Context, token, repo string, number int) ([]timelineEvent, error) {
	path := fmt.Sprintf("/repos/%s/issues/%d/timeline?per_page=%d", repo, number, perPage)
	body, err := c.get(ctx, token, path)
	if err != nil {
		return nil, err
	}
	var events []timelineEvent
	if err := json.Unmarshal(body, &events); err != nil {
		return nil, err
	}
	return events, nil
}

func eventLogin(e timelineEvent) string {
	if e.Actor != nil && e.Actor.Login != "" {
		return e.Actor.Login
	}
	if e.User != nil && e.User.Login != "" {
		return e.User.Login
	}
	return ""
}
