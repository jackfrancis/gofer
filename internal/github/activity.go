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
	ID                int64     `json:"id"`        // the comment's or review's id ("commented", "reviewed")
	Body              string    `json:"body"`      // the comment's or review's text
	State             string    `json:"state"`     // for "reviewed": approved | changes_requested | commented | dismissed
	CommitID          string    `json:"commit_id"` // for "reviewed": the head commit the review was made on
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"` // for "commented": the last edit, or its creation
	SubmittedAt       time.Time `json:"submitted_at"`
	RequestedReviewer *struct {
		Login string `json:"login"`
	} `json:"requested_reviewer"`
	RequestedTeam *struct {
		Slug string `json:"slug"`
	} `json:"requested_team"`
	User  *timelineUser `json:"user"`
	Actor *timelineUser `json:"actor"`
}

// teamRequest is a team review request outstanding as of some event: when it was
// made, and by whom.
type teamRequest struct {
	at    time.Time
	actor string
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
	// ReviewRequested says this refresh's review-requested:@me search returned the
	// item (a request carried over from an earlier search is no evidence of who made a
	// later one). The hit is evidence that an outstanding team request names login when
	// nothing else explains it: no personal request of login's is outstanding. Which
	// team is unknown when several are outstanding, so the item awaits login only if
	// every one of them was made since login's last comment or review (since the
	// latest, which never overstates the wait), and RequestedByLogin names their actor
	// only if they share one. Otherwise team membership is unknown and a team request
	// awaits no one in particular.
	ReviewRequested bool
	// Confirmed is the team requests an earlier refresh's search confirmed
	// (worklist.Signals.TeamConfirmed), passed when this refresh's search was cut short
	// and kept the item's request without returning it. It is evidence as
	// ReviewRequested is, but only while exactly those requests are outstanding: a
	// partial search neither confirms a new request nor cancels an unchanged one.
	Confirmed *worklist.TeamConfirmation
	// PullRequest says the item is a pull request, whose head commit says whether the
	// author pushed since login's last word.
	PullRequest bool
	// Prior is the last word stored for the item, if any. When it is the same version
	// of the last word (LastWord.Key: same comment, same text), what it recorded
	// stands: the head first seen with a comment (LastWord.Head), so a later head means
	// the author pushed since, and that a push answered it (LastWord.Answered), which no
	// later read undoes, not even one that cannot read the head.
	Prior *worklist.LastWord
}

// Activity holds the signals derived from a single read of an item's timeline.
type Activity struct {
	Participants        int       // distinct people who commented or reviewed
	InboundRefs         int       // cross-references from other issues/PRs (hub centrality)
	OtherReviewers      int       // distinct reviewers other than login (someone else is engaged)
	Commented           bool      // login themselves commented or reviewed — proves participation
	AwaitingMeSince     time.Time // when login was asked to review with no engagement since; zero if none
	AwaitingOthersSince time.Time // when the ball is in others' court; zero if none
	RequestedByLogin    string    // who requested login's pending review (the actor), personal or attributed team; empty when none pending or unknown
	// LastWord is login's own comment or review when it is the last human word,
	// whoever's turn it is: why AwaitingOthersSince is set, or, with it unset, a last
	// word a push answered (LastWord.Answered) or one a review request now outranks. It
	// carries the head it was made on (LastWord.Head) for the next read; nil when
	// someone else had the last word.
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
	// TeamConfirmed records the outstanding team requests when the evidence
	// (ActivityOptions.ReviewRequested or Confirmed) attributes one of them to login,
	// for the next refresh's Confirmed; nil otherwise. Unset, like RequestedByLogin,
	// when the timeline is incomplete.
	TeamConfirmed *worklist.TeamConfirmation
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
// a bot (opts.Bots). On a pull request login did not author, a push or marking it
// ready for review is the author answering, and so is the last word too. The ball is in others' court when login had the last
// word (LastWord records it), or when someone's approving or changes-requested review
// is the last word on an item login did not author, so progress is on its author.
// A review request still awaiting login outranks any later word: login's personal
// request with no comment or review by login since, or a team request attributed to
// login (see ActivityOptions.ReviewRequested) made since login's last one.
//
// A commit's date is not when it was pushed (a commit made earlier can be pushed
// later, and committer clocks drift), so pushes are judged by the head, read once
// (opts.PullRequest, on an item login did not author): a review's last word stands
// while the head is the commit it was made on, and a head that cannot be read leaves
// it off the waiting list rather than guess; a comment's last word stands while the
// head is the one first seen with it (opts.Prior), and a force-push or ready-for-review
// since it, which GitHub dates itself, answers it too. A review with no summary (only
// inline comments) is read through its comments, so its text is what the user wrote
// inline; if they cannot all be read the last word has no text, and an undecisive
// last word with no text never makes an item wait (worklist.WorkItem.Waiting).
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
	teamRequests := make(map[string]teamRequest)
	// answeredAt is the latest force-push or ready-for-review, which GitHub dates, on
	// an item login did not author: the author's answer, which takes the last word from
	// any word it follows. Words are ordered among themselves by when they were made,
	// so editing a comment never jumps it past someone's reply to it; against an
	// answer, a comment counts from its last edit, which may ask something new.
	var answeredAt time.Time
	answered := func(at time.Time) {
		if !isAuthor && at.After(answeredAt) {
			answeredAt = at
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
					if !requested.at.After(at) {
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
		case "head_ref_force_pushed", "ready_for_review":
			answered(e.CreatedAt)
		case "review_requested", "review_request_removed":
			if e.RequestedTeam != nil {
				sawRequest = true
				if e.Event == "review_requested" {
					teamRequests[e.RequestedTeam.Slug] = teamRequest{at: e.CreatedAt, actor: eventLogin(e)}
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
	if lastEvent != nil && lastEvent.Event == "commented" && lastEvent.UpdatedAt.After(lastActivityAt) {
		lastActivityAt = lastEvent.UpdatedAt // an edit: the comment as it reads now
	}
	if answeredAt.After(lastActivityAt) {
		lastActivityAt, lastActor, lastActivityDecisive, lastEvent = answeredAt, "", false, nil
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
	var earliest, latest time.Time
	actors := map[string]bool{}
	for _, tr := range teamRequests {
		if earliest.IsZero() || tr.at.Before(earliest) {
			earliest = tr.at
		}
		if tr.at.After(latest) {
			latest = tr.at
		}
		actors[tr.actor] = true
	}
	// An earlier search's confirmation stands only for exactly the requests it saw:
	// none withdrawn (the count) and none made since (the latest).
	confirmed := opts.Confirmed != nil && opts.Confirmed.Count == len(teamRequests) && opts.Confirmed.Latest.Equal(latest)
	if (opts.ReviewRequested || confirmed) && !personalPending && len(teamRequests) > 0 {
		// The search hit is otherwise unexplained: one of the outstanding team requests
		// names login, though which is unknown when there are several.
		a.TeamConfirmed = &worklist.TeamConfirmation{Latest: latest, Count: len(teamRequests)}
		if len(actors) == 1 {
			for actor := range actors {
				a.RequestedByLogin = actor
			}
		}
		if earliest.After(myLastActivityAt) && latest.After(meSince) {
			meSince = latest
		}
	}
	switch {
	case lastActor == login && lastEvent != nil:
		othersSince = lastActivityAt
		e := *lastEvent
		if e.Event == "reviewed" && strings.TrimSpace(e.Body) == "" && !decisive(e.State) && e.ID != 0 {
			e.Body = c.reviewCommentText(ctx, token, repo, number, e.ID)
		}
		lastWord = newLastWord(e, lastActivityAt, login, bots)
	case lastActivityDecisive && !isAuthor:
		othersSince = lastActivityAt
	}
	// What the stored last word recorded stands only for the same version of it: an
	// edit is a new question, anchored afresh.
	var prior *worklist.LastWord
	if p := opts.Prior; p != nil && lastWord != nil && p.Key() == lastWord.Key() {
		prior = p
	}
	if !othersSince.IsZero() && opts.PullRequest && !isAuthor {
		if prior != nil && prior.Answered {
			// A push already answered this version of the last word: nothing undoes that.
			lastWord.Head, lastWord.Answered = prior.Head, true
			othersSince = time.Time{}
		} else {
			head, headErr := c.pullHead(ctx, token, repo, number)
			switch {
			case lastEvent != nil && lastEvent.Event == "reviewed" && lastEvent.CommitID != "":
				// A review names the commit it was made on; it stands while that is the
				// head, and an unreadable head leaves it off the waiting list.
				if lastWord != nil {
					lastWord.Head = lastEvent.CommitID
				}
				if headErr != nil {
					othersSince = time.Time{}
				} else if !strings.EqualFold(head, lastEvent.CommitID) {
					othersSince = time.Time{}
					if lastWord != nil {
						lastWord.Answered = true
					}
				}
			case lastWord != nil:
				// A comment names no commit: it stands while the head is the one first
				// seen with it. Unread, the head anchors nothing new, and the timeline
				// decides.
				switch {
				case prior != nil && prior.Head != "":
					lastWord.Head = prior.Head
					if headErr == nil && !strings.EqualFold(head, prior.Head) {
						othersSince, lastWord.Answered = time.Time{}, true
					}
				case headErr == nil:
					lastWord.Head = head
				}
			}
		}
	}
	switch {
	case !meSince.IsZero():
		a.AwaitingMeSince = meSince
	case !othersSince.IsZero():
		a.AwaitingOthersSince = othersSince
	}
	// Kept whoever's turn it is, so its head and whether a push answered it survive a
	// review request that outranks it for a while.
	a.LastWord = lastWord
	return a, nil
}

// pullHead returns the SHA of the pull request's head commit.
func (c *Client) pullHead(ctx context.Context, token, repo string, number int) (string, error) {
	body, err := c.getPull(ctx, token, repo, number)
	if err != nil {
		return "", err
	}
	var pr struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return "", err
	}
	if pr.Head.SHA == "" {
		return "", fmt.Errorf("pull request %s#%d has no head commit", repo, number)
	}
	return pr.Head.SHA, nil
}

// reviewCommentPages bounds the pages of inline comments read for one review.
const reviewCommentPages = 3

// reviewCommentText returns the inline comments of review id, joined, following the
// Link header up to reviewCommentPages pages; "" when they cannot all be read (an
// error, or more pages than that), since a turn read from part of a review could be
// wrong. worklist.NewLastWord cuts the result.
func (c *Client) reviewCommentText(ctx context.Context, token, repo string, number int, id int64) string {
	var parts []string
	for page := 1; page <= reviewCommentPages; page++ {
		path := fmt.Sprintf("/repos/%s/pulls/%d/reviews/%d/comments?per_page=%d&page=%d", repo, number, id, timelinePerPage, page)
		body, header, err := c.getHeader(ctx, token, path)
		if err != nil {
			return ""
		}
		var comments []struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(body, &comments); err != nil {
			return ""
		}
		for _, cm := range comments {
			if t := strings.TrimSpace(cm.Body); t != "" {
				parts = append(parts, t)
			}
		}
		if len(comments) == 0 || !hasNextPage(header) {
			return strings.Join(parts, "\n\n")
		}
	}
	return "" // more pages than read: the review cannot be read whole
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
