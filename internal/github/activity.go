package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

type timelineEvent struct {
	Event             string    `json:"event"`
	ID                int64     `json:"id"`    // the comment's or review's id ("commented", "reviewed")
	Body              string    `json:"body"`  // the comment's or review's text
	State             string    `json:"state"` // for "reviewed": approved | changes_requested | commented | dismissed
	CreatedAt         time.Time `json:"created_at"`
	SubmittedAt       time.Time `json:"submitted_at"`
	RequestedReviewer *struct {
		Login string `json:"login"`
	} `json:"requested_reviewer"`
	RequestedTeam *struct {
		Slug string `json:"slug"`
	} `json:"requested_team"`
	User  *timelineUser `json:"user"`
	Actor *timelineUser `json:"actor"`
	// Committer dates a "committed" event, which names no GitHub account; a rebase
	// re-dates the commits it rewrites.
	Committer *struct {
		Date time.Time `json:"date"`
	} `json:"committer"`
}

type timelineUser struct {
	Login string `json:"login"`
	Type  string `json:"type"` // "User" | "Bot" | "Organization"
}

// ActivityOptions says what ItemActivityFor needs to know about the item and its
// readers beyond the timeline.
type ActivityOptions struct {
	// Author is the item's author. When it is login, a push or a decisive review by
	// someone else hands the item to login rather than taking it away.
	Author string
	// Bots are logins whose comments are automated (BOT_REVIEWERS), matched without
	// regard to case. An account GitHub types "Bot", or whose login ends in "[bot]",
	// is a bot too. A bot's comment never takes the last word.
	Bots []string
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
	// LastWord is login's own comment or review when it is the last human word and
	// so is why AwaitingOthersSince is set; nil otherwise.
	LastWord *worklist.LastWord
	// TimelineComplete reports that every timeline event was read, so
	// RequestedByLogin is authoritative; when false it is unknown, not "none".
	TimelineComplete bool
	// RequestEnded reports that the whole timeline was read and no review request
	// that may include login is outstanding. login's personal request ends with a
	// review_request_removed or with login submitting a review. Team membership is
	// unknown, so every team request may include login: each stands until it is
	// withdrawn or login reviews after it. A timeline with no request event and no
	// review by login is not evidence, so RequestEnded is false there.
	RequestEnded bool
}

// The timeline is read oldest first, a page at a time, so the latest events, which
// decide whose court the ball is in and who made the pending review request, come
// last. timelineMaxPages bounds the calls per item; the caller bounds their time.
const (
	timelinePerPage  = 100
	timelineMaxPages = 5
)

// ItemActivity is ItemActivityFor with no ActivityOptions: the author is unknown and
// only accounts GitHub marks as bots are bots.
func (c *Client) ItemActivity(ctx context.Context, token, repo string, number int, login string) (Activity, error) {
	return c.ItemActivityFor(ctx, token, repo, number, login, ActivityOptions{})
}

// ItemActivityFor reads the issue/PR timeline (up to timelineMaxPages pages) and
// derives the engagement and centrality signals from it, plus how long the item has
// been blocked on login's review and who asked, or how long it has been waiting on
// someone else.
//
// login's review request is pending from the latest review_requested for them until
// a review_request_removed withdraws it or they submit a review, as on GitHub; a
// comment does not end it. RequestedByLogin names the pending request's actor
// whether or not the ball is in login's court.
//
// Whose turn it is comes from the last human word: comments and reviews by anyone but
// a bot (opts.Bots). On an item login did not author, a push to the pull request
// (a commit, a force-push) or marking it ready for review is the author answering,
// and so is the last word too. The ball is in others' court when login had the last
// word (LastWord records it), or when someone's approving or changes-requested review
// is the last word on an item login did not author, so progress is on its author.
//
// When the timeline is longer than the pages read, the latest events are unseen, so
// who is waiting on whom is unknown: RequestedByLogin, both waiting timestamps and
// LastWord are left unset rather than taken from older events.
func (c *Client) ItemActivityFor(ctx context.Context, token, repo string, number int, login string, opts ActivityOptions) (Activity, error) {
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
	var lastEvent *timelineEvent // the event that is the last word
	isAuthor := opts.Author != "" && strings.EqualFold(opts.Author, login)
	bots := make(map[string]bool, len(opts.Bots))
	for _, b := range opts.Bots {
		bots[strings.ToLower(strings.TrimSpace(b))] = true
	}
	isBot := func(u *timelineUser) bool {
		if u == nil {
			return false
		}
		return strings.EqualFold(u.Type, "Bot") || strings.HasSuffix(strings.ToLower(u.Login), "[bot]") || bots[strings.ToLower(u.Login)]
	}
	// The review requests outstanding as of each event: login's personal one, and
	// each team's (membership is unknown, so any of them may include login), with
	// when it was made. sawRequest records that the timeline holds any request
	// evidence at all — a request event, or login's own review.
	var lastActivityDecisive, personalPending, sawRequest bool
	teamRequests := make(map[string]time.Time)
	// answered records a push or ready-for-review on an item login did not author:
	// the author's answer, which takes the last word.
	answered := func(at time.Time) {
		if !isAuthor && at.After(lastActivityAt) {
			lastActivityAt, lastActor, lastActivityDecisive, lastEvent = at, "", false, nil
		}
	}
	for i := range events {
		e := events[i]
		switch e.Event {
		case "commented":
			l := eventLogin(e)
			if l == "" {
				break
			}
			participants[l] = struct{}{}
			if !isBot(eventUser(e)) && e.CreatedAt.After(lastActivityAt) {
				lastActivityAt, lastActor, lastActivityDecisive, lastEvent = e.CreatedAt, l, false, &events[i]
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
			if l == login {
				sawRequest = true
				if !at.Before(requestChangedAt) {
					// Submitting the review fulfils the pending request.
					requestChangedAt, requestedAt, requestedByLogin, personalPending = at, time.Time{}, "", false
				}
				// It fulfils the team requests made before it too.
				for team, requested := range teamRequests {
					if !requested.After(at) {
						delete(teamRequests, team)
					}
				}
			}
			if !isBot(e.User) && at.After(lastActivityAt) {
				lastActivityAt, lastActor, lastEvent = at, l, &events[i]
				lastActivityDecisive = decisive(e.State)
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
		case "committed":
			if e.Committer != nil {
				answered(e.Committer.Date)
			}
		case "head_ref_force_pushed", "ready_for_review":
			answered(e.CreatedAt)
		case "review_requested", "review_request_removed":
			if e.RequestedTeam != nil {
				sawRequest = true
				if e.Event == "review_requested" {
					teamRequests[e.RequestedTeam.Slug] = e.CreatedAt
				} else {
					delete(teamRequests, e.RequestedTeam.Slug)
				}
				break
			}
			if e.RequestedReviewer == nil || e.RequestedReviewer.Login != login || e.CreatedAt.Before(requestChangedAt) {
				break
			}
			requestChangedAt, sawRequest = e.CreatedAt, true
			if e.Event == "review_requested" {
				requestedAt, requestedByLogin, personalPending = e.CreatedAt, eventLogin(e), true
			} else {
				requestedAt, requestedByLogin, personalPending = time.Time{}, "", false
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
	a.RequestEnded = sawRequest && !personalPending && len(teamRequests) == 0

	// Decide whose court the ball is in from the most recent court-changing event.
	// Ball on login: a review was requested of them and they have not engaged
	// since. Ball on others: login had the last word, or someone's decisive review
	// is the last word on an item login did not author, so progress is on the author.
	var meSince, othersSince time.Time
	var lastWord *worklist.LastWord
	if !requestedAt.IsZero() && requestedAt.After(myLastActivityAt) {
		meSince = requestedAt
	}
	switch {
	case lastActor == login && lastEvent != nil:
		othersSince = lastActivityAt
		lastWord = newLastWord(*lastEvent, lastActivityAt, login, bots)
	case lastActivityDecisive && !isAuthor:
		othersSince = lastActivityAt
	}
	switch {
	case meSince.After(othersSince):
		a.AwaitingMeSince = meSince
	case !othersSince.IsZero():
		a.AwaitingOthersSince = othersSince
		a.LastWord = lastWord
	}
	return a, nil
}

// decisive reports whether a review in state settles the turn by itself: an approval
// or a request for changes.
func decisive(state string) bool {
	state = strings.ToLower(state)
	return state == "changes_requested" || state == "approved"
}

// newLastWord records login's comment or review e, made at, as the last word. Its
// mentions leave out login and the bots.
func newLastWord(e timelineEvent, at time.Time, login string, bots map[string]bool) *worklist.LastWord {
	kind := "comment"
	if e.Event == "reviewed" {
		kind = "review"
	}
	id := kind
	if e.ID != 0 {
		id += ":" + strconv.FormatInt(e.ID, 10)
	} else {
		id += "@" + at.UTC().Format(time.RFC3339)
	}
	var mentions []string
	for _, m := range worklist.Mentions(e.Body) {
		if !strings.EqualFold(m, login) && !bots[strings.ToLower(m)] && !strings.HasSuffix(strings.ToLower(m), "[bot]") {
			mentions = append(mentions, m)
		}
	}
	return worklist.NewLastWord(id, at, e.Event == "reviewed" && decisive(e.State), mentions, e.Body)
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

// eventUser is the account behind e: its actor, else its user.
func eventUser(e timelineEvent) *timelineUser {
	if e.Actor != nil && e.Actor.Login != "" {
		return e.Actor
	}
	if e.User != nil && e.User.Login != "" {
		return e.User
	}
	return nil
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
