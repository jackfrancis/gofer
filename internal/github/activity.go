package github

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

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
	Commented           bool      // login themselves commented or reviewed — proves participation
	AwaitingMeSince     time.Time // when login was asked to review with no engagement since; zero if none
	AwaitingOthersSince time.Time // when the ball is in others' court; zero if none
	RequestedByLogin    string    // who requested login's pending review (the actor); empty when none pending or unknown
	// TimelineComplete reports that every timeline event was read, so
	// RequestedByLogin is authoritative; when false it is unknown, not "none".
	TimelineComplete bool
	// RequestEnded reports that the whole timeline was read and the latest change to
	// login's own review request ended it: a review_request_removed, or login
	// submitting a review. It is the timeline's positive evidence that no personal
	// request is pending. A request made through a team appears as no event for
	// login, so the absence of a request event is not evidence.
	RequestEnded bool
}

// The timeline is read oldest first, a page at a time, so the latest events, which
// decide whose court the ball is in and who made the pending review request, come
// last. timelineMaxPages bounds the calls per item; the caller bounds their time.
const (
	timelinePerPage  = 100
	timelineMaxPages = 5
)

// ItemActivity reads the issue/PR timeline (up to timelineMaxPages pages) and
// derives the engagement and centrality signals from it, plus how long the item has
// been blocked on login's review and who asked.
//
// login's review request is pending from the latest review_requested for them until
// a review_request_removed withdraws it or they submit a review, as on GitHub; a
// comment does not end it. RequestedByLogin names the pending request's actor
// whether or not the ball is in login's court.
//
// When the timeline is longer than the pages read, the latest events are unseen, so
// who is waiting on whom is unknown: RequestedByLogin and both waiting timestamps
// are left unset rather than taken from older events.
func (c *Client) ItemActivity(ctx context.Context, token, repo string, number int, login string) (Activity, error) {
	repo, err := c.gate(ctx, token, repo)
	if err != nil {
		return Activity{}, err
	}
	events, complete, err := c.fetchTimeline(ctx, token, repo, number)
	if err != nil {
		return Activity{}, err
	}
	participants := make(map[string]struct{})
	otherReviewers := make(map[string]struct{})
	var inbound int
	var requestedAt, requestChangedAt, myLastActivityAt, lastActivityAt time.Time
	var lastActor, requestedByLogin string
	var lastActivityDecisive, requestEnded bool
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
			if l == login && !at.Before(requestChangedAt) {
				// Submitting the review fulfils the pending request.
				requestChangedAt, requestedAt, requestedByLogin, requestEnded = at, time.Time{}, "", true
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
		case "review_requested", "review_request_removed":
			if e.RequestedReviewer == nil || e.RequestedReviewer.Login != login || e.CreatedAt.Before(requestChangedAt) {
				break
			}
			requestChangedAt = e.CreatedAt
			if e.Event == "review_requested" {
				requestedAt, requestedByLogin, requestEnded = e.CreatedAt, eventLogin(e), false
			} else {
				requestedAt, requestedByLogin, requestEnded = time.Time{}, "", true
			}
		}
	}
	a := Activity{Participants: len(participants), InboundRefs: inbound, OtherReviewers: len(otherReviewers)}
	a.Commented = !myLastActivityAt.IsZero()
	if !complete {
		return a, nil // the latest events are unread: who is waiting is unknown
	}
	a.TimelineComplete = true
	a.RequestedByLogin = requestedByLogin
	a.RequestEnded = requestEnded

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
	case !othersSince.IsZero():
		a.AwaitingOthersSince = othersSince
	}
	return a, nil
}

// fetchTimeline reads the item's timeline, oldest first, up to timelineMaxPages
// pages, following the Link header. complete is false when more events remain
// unread.
func (c *Client) fetchTimeline(ctx context.Context, token, repo string, number int) (events []timelineEvent, complete bool, err error) {
	for page := 1; page <= timelineMaxPages; page++ {
		path := fmt.Sprintf("/repos/%s/issues/%d/timeline?per_page=%d&page=%d", repo, number, timelinePerPage, page)
		body, header, err := c.getHeader(ctx, token, path)
		if err != nil {
			return nil, false, err
		}
		var batch []timelineEvent
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, false, err
		}
		events = append(events, batch...)
		if len(batch) == 0 || !hasNextPage(header) {
			return events, true, nil
		}
	}
	return events, false, nil
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
