package webui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackfrancis/gofer/internal/comment"
	"github.com/jackfrancis/gofer/internal/ghcomment"
	"github.com/jackfrancis/gofer/internal/ingest"
	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// This file is the draft review (ADR 0010). At the bottom of a pull request's thread,
// "Draft Review Comments" posts a draft-review request turn and dispatches a converse
// run, which answers with the comments the conversation agrees on (a DraftReview on
// its reply). Under the newest draft the thread offers a selection and "Create
// pending review": the web tier then creates a PENDING review on GitHub with the
// write grant, from the stored draft, never from the form. Nothing is submitted: the
// review stays a draft only the user sees until they submit it on GitHub.

// draftReviewPrompt is the request turn the button posts.
const draftReviewPrompt = "Draft review comments for the points this conversation agrees on."

// reviewAgainPrompt is the review request "Review the current revision" posts.
const reviewAgainPrompt = "Review this PR again as it stands now: it changed since the earlier review."

// outdatedDraft refuses a draft stored under older rules (DraftReview.Current).
const outdatedDraft = "This draft was saved before gofer's current checks on drafted text, so gofer will not send it as it is. Nothing was created; draft the review comments again."

// draftReviewTimeout bounds creating the pending review: a few GitHub calls. It runs
// detached from the browser request, so a closed tab does not cut it short midway.
var draftReviewTimeout = 60 * time.Second // a variable so tests can shorten it

// settleTimeout bounds recording what happened to a confirmation (or releasing it),
// apart from draftReviewTimeout, which the GitHub calls may have used up.
const settleTimeout = 10 * time.Second

// ReviewCreator creates a pending review (comment.Service). A Commenter that does
// not implement it offers no draft review.
type ReviewCreator interface {
	PendingReview(ctx context.Context, userID string, rv comment.Review) (comment.ReviewResult, error)
}

// draftThread is the thread view's draft-review state. A nil one shows nothing: the
// item is not a pull request on the radar, or commenting or the assistant is off.
type draftThread struct {
	Connected bool   // the write grant is connected; otherwise the thread links to connecting it
	Redundant bool   // connected, and a redraft would repeat the draft the thread offers (RedraftRedundant): no button
	Ready     bool   // the button is enabled: a completed review, and no reply in flight
	Why       string // why the button is disabled
	Index     int    // the position of the draft the thread offers; -1 for none
	Digest    string // that draft's Digest, posted back with the selection
	// Receipt is the offered draft's confirmation, if it has one: the thread then
	// shows what happened instead of the selection. Interrupted marks a claim older
	// than draftReviewTimeout, whose request gofer never settled.
	Receipt     *worklist.DraftReceipt
	Interrupted bool
	// Outdated marks a newest draft stored under older rules (DraftReview.Current):
	// it is not offered, and the thread says to draft again.
	Outdated bool
	// ReviewOutdated marks a newest draft that read a revision other than the one
	// the latest review read (WorkItem.ReviewOutdated): the thread offers a review
	// of the current revision, enabled while no reply is in flight and nothing blocks
	// it (ReviewReady, else AgainWhy). AgainKey names a review of the current revision
	// already asked for and unanswered (one the backend refused, say), which the
	// button then retries instead of asking again.
	ReviewOutdated bool
	ReviewReady    bool
	AgainKey       string
	AgainWhy       string
	// NeedsReview marks a pull request with no completed review, no reply in flight
	// (firstReviewable) and no review request at all: the thread offers one right
	// there ("Review this pull request", webui.ReviewAgain). Drafting does not need
	// it when the conversation already has an answered discussion
	// (WorkItem.HasDiscussion); an approval does, which ReviewNote then says.
	NeedsReview bool
	// ReviewNote, with drafting ready but no review in the conversation, says what a
	// review is still for (or why one cannot be asked for now).
	ReviewNote string
	// RetryKey names, in that state but with a review request that has no answer,
	// the request "Retry the earlier review" retries (retryCandidate): never a second
	// request, which the first's run could also answer.
	RetryKey string
	// ReviewRefused reports that the backend certainly refused the last review this
	// thread's buttons dispatched, so nothing is running (query review_refused).
	ReviewRefused bool
}

// Review actions a ReviewAgain form names (form "review"), each checked against its
// own condition inside the store update, so a page rendered for one never does the
// other once the thread changed (a conversation reset in another tab, say).
const (
	reviewFirst = "first" // "Review this pull request": no review, and no review request
	reviewRetry = "retry" // "Retry the earlier review": no review, and a request (form "turn") unanswered
	reviewAgain = "again" // "Review the current revision": the latest review is outdated
)

// While a review request has no answer, no other one is added: its run may still
// answer. It is retried instead (same turn, a new attempt), so the dispatcher's gate
// shares a run still in flight and the store keeps one answer per turn, whatever the
// timing. The requests that count are those after `after`: the whole thread (-1) for
// a first review, and for a review of the current revision those asked since the
// latest review landed (WorkItem.LatestReviewAt), since that review superseded any
// asked before it. Requests record no revision, only answers do, so a redraft of the
// same revision, which moves no review, leaves an outstanding re-review the one to
// retry.

// retryCandidate returns the newest review request after `after` with no answer
// that the thread can retry: visible (a hidden one waits for the user to unhide it,
// never reversed for them), and by a model it may use (retryChoices: a 2nd opinion
// only blind, never as an ordinary review that would still count as independent).
// It is -1 when there is none. An older ordinary request is a candidate even when a
// newer 2nd opinion cannot be retried.
func (h *Handler) retryCandidate(it worklist.WorkItem, after int) int {
	for i := len(it.Thread) - 1; i > after; i-- {
		m := it.Thread[i]
		if m.Role != worklist.RoleUser || m.Kind != worklist.KindReviewRequest || m.Hidden || it.Answered(it.TurnKey(i)) {
			continue
		}
		if _, _, ok := h.retryChoices(it, i); ok {
			return i
		}
	}
	return -1
}

// reviewBlocker says why no review may be asked for, or "": a review request after
// `after` has no answer and may still get one, yet the thread cannot retry it
// (hidden, or a 2nd opinion no model may run). A request whose current attempt can no longer answer
// (WorkItem.Ended: a notice ended its run, or the backend certainly refused it)
// blocks nothing.
func (h *Handler) reviewBlocker(it worklist.WorkItem, after int) string {
	for i := len(it.Thread) - 1; i > after; i-- {
		m := it.Thread[i]
		key := it.TurnKey(i)
		if m.Role != worklist.RoleUser || m.Kind != worklist.KindReviewRequest || it.Answered(key) || it.Ended(key) {
			continue
		}
		if m.Hidden {
			return "A review asked for in this conversation is hidden and has no answer yet. Unhide it to retry it."
		}
		if _, _, ok := h.retryChoices(it, i); !ok {
			return "A 2nd opinion asked for in this conversation has no answer yet, and no model it may use is configured to retry it."
		}
	}
	return ""
}

// firstReviewable reports whether the thread may act on a first review now: a pull
// request on the radar with no completed review and no reply plausibly in flight
// (the page polls while one is).
func firstReviewable(it worklist.WorkItem, now time.Time) bool {
	return it.Type == worklist.TypePullRequest && it.OnRadar() && !it.HasReview() && !it.ReplyPending(now, reviewStaleAfter)
}

// againReviewable reports whether the thread may act on a review of the current
// revision now: the latest review is outdated and no reply is plausibly in flight.
func againReviewable(it worklist.WorkItem, now time.Time) bool {
	return it.Type == worklist.TypePullRequest && it.OnRadar() && it.ReviewOutdated() && !it.ReplyPending(now, reviewStaleAfter)
}

// reviewCreator returns the commenter's pending-review side, if it has one.
func (h *Handler) reviewCreator() (ReviewCreator, bool) {
	rc, ok := h.commenter.(ReviewCreator)
	return rc, ok
}

// draftThreadView builds the thread's draft-review state for item it.
func (h *Handler) draftThreadView(ctx context.Context, user *session.User, it worklist.WorkItem, now time.Time) *draftThread {
	if _, ok := h.reviewCreator(); !ok || !h.convEnabled || it.Type != worklist.TypePullRequest || !it.OnRadar() {
		return nil
	}
	enabled, connected := h.commentState(ctx, user)
	if !enabled {
		return nil
	}
	v := &draftThread{Connected: connected, Index: -1}
	switch {
	case connected && it.RedraftRedundant():
		v.Redundant = true
	case it.ReplyPending(now, reviewStaleAfter):
		// Before the discussion check: a first answer still running is a wait.
		v.Why = "Wait for the reply in progress, so the draft includes it."
	case !it.HasDiscussion():
		v.Why = "There is nothing in this conversation to draft review comments from yet: ask about this pull request, or have it reviewed."
	default:
		v.Ready = true
	}
	// With no review yet, offer one (or the retry of one already asked for) whether
	// or not drafting is ready: comments can come from any discussion, but an
	// approval is pinned to the revision a review read.
	if !v.Redundant && firstReviewable(it, now) {
		switch j := h.retryCandidate(it, -1); {
		case j >= 0:
			v.RetryKey = it.TurnKey(j)
			v.ReviewNote = "The review asked for earlier has no answer. Retry it to have one: it runs as it was asked, without the messages that came after it."
		default:
			if why := h.reviewBlocker(it, -1); why != "" {
				v.ReviewNote = why
			} else {
				v.NeedsReview = true
				if v.Ready {
					v.ReviewNote = "Drafting works from this conversation. A review is needed only to draft an approval (/lgtm, /approve)."
				}
			}
		}
	}
	if it.ReviewOutdated() {
		v.ReviewOutdated = true
		switch {
		case it.ReplyPending(now, reviewStaleAfter):
			v.AgainWhy = "Wait for the reply in progress."
		case h.retryCandidate(it, it.LatestReviewAt()) >= 0:
			v.AgainKey = it.TurnKey(h.retryCandidate(it, it.LatestReviewAt()))
		default:
			v.AgainWhy = h.reviewBlocker(it, it.LatestReviewAt())
		}
		v.ReviewReady = v.AgainWhy == ""
	}
	// Only a draft pinned to the item's pull request as it stands is offered: a
	// write-back that changed the item's destination retires it. A draft stored under
	// older rules is not offered either, though its receipt, if it was confirmed,
	// still shows.
	if i := it.LatestDraft(); i >= 0 && connected && it.Thread[i].Draft.For(it) {
		if !it.Thread[i].Draft.Current() && it.Thread[i].DraftReceipt == nil {
			v.Outdated = true
			return v
		}
		v.Index = i
		v.Digest = it.Thread[i].Draft.Digest()
		v.Receipt = it.Thread[i].DraftReceipt
		v.Interrupted = v.Receipt != nil && v.Receipt.State == worklist.DraftCreating && now.Sub(v.Receipt.At) > draftReviewTimeout
	}
	return v
}

// draftable reports whether a draft-review request may be added to it now: a pull
// request on the radar (so not hidden or closed) with a completed review, no reply
// in flight, and no draft a new one would only repeat (RedraftRedundant). It is the
// button's condition, checked again inside the store update.
//
// redraft, when set, is the out-of-date result page's "Draft Review Comments again",
// naming the draft (turn key and digest) GitHub refused: the item may have moved in
// ways gofer cannot see (a new head or base), so that request is judged by the draft
// it names alone. It is admitted only while that very draft is still the one
// offered, whatever its redundancy, and refused otherwise, never falling back to the
// ordinary check: once anything replaced the draft (a redraft from another tab, say,
// and a discussion since), a stale result page must not retire the newer one.
func draftable(it worklist.WorkItem, now time.Time, redraft *draftRef) bool {
	if it.Type != worklist.TypePullRequest || !it.OnRadar() || !it.HasDiscussion() || it.ReplyPending(now, reviewStaleAfter) {
		return false
	}
	if redraft != nil {
		_, ok := it.OfferedDraft(redraft.Key, redraft.Digest)
		return ok
	}
	return !it.RedraftRedundant()
}

// draftRef names one draft: the key of the turn carrying it and its Digest.
type draftRef struct{ Key, Digest string }

// DraftReview handles POST /items/draft-review, the thread's "Draft Review
// Comments". It appends the draft-review request to the item's thread and
// dispatches a converse run by the default model to answer it, then redirects back
// to the thread (Post/Redirect/Get). Nothing reaches GitHub here.
func (h *Handler) DraftReview(w http.ResponseWriter, r *http.Request) {
	user := h.sessions.CurrentUser(r)
	_, creates := h.reviewCreator()
	enabled, connected := h.commentState(r.Context(), user)
	if user == nil || !h.convEnabled || !creates || !enabled {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	id := r.FormValue("id")
	var redraft *draftRef // set only by the out-of-date result page's form
	if r.FormValue("redraft") == "1" {
		redraft = &draftRef{Key: r.FormValue("msg"), Digest: r.FormValue("digest")}
	}
	back := "/items/thread?id=" + url.QueryEscape(id)
	if id == "" || !connected {
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	now := h.now().UTC()
	msg := worklist.Message{ID: worklist.NewMessageID(), Role: worklist.RoleUser, Content: draftReviewPrompt, Kind: worklist.KindDraftReviewRequest, At: now}
	appended := false
	err := h.store.Update(r.Context(), user.ID, func(current []worklist.WorkItem) ([]worklist.WorkItem, error) {
		appended = false
		for i := range current {
			if current[i].ID != id {
				continue
			}
			if !draftable(current[i], now, redraft) {
				return nil, nil // a stale page: no review yet, a reply in flight, a draft already last, or off the radar
			}
			req := msg
			req.ReadRev, req.ReadRevKnown = current[i].VisibilityRev(), true // inside the update, ordered with every hide
			current[i].Thread = append(current[i].Thread, req)
			appended = true
			return current[i : i+1], nil
		}
		return nil, nil
	})
	if err == nil && appended {
		opt, _ := h.defaultOption()
		_ = h.pipeline.Converse(r.Context(), user.ID, worklist.TurnRef{Item: id, Turn: msg.ID}, opt.Endpoint, opt.Model)
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// ReviewAgain handles POST /items/review-again, the thread's "Review the current
// revision" and "Review this pull request": offered when the newest draft read a
// pull request revision other than the one the latest review read
// (WorkItem.ReviewOutdated), so no approval could be drafted from it, or when the
// pull request has no completed review yet, so nothing can be drafted at all. It
// appends a review request, keeping the conversation, and dispatches it to the
// default model; its answer records the revision it read, so a draft afterwards can
// use it. A stale page (the review already current, a reply in flight, the item off
// the radar) appends nothing.
func (h *Handler) ReviewAgain(w http.ResponseWriter, r *http.Request) {
	user := h.sessions.CurrentUser(r)
	if user == nil || !h.convEnabled {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	id := r.FormValue("id")
	back := "/items/thread?id=" + url.QueryEscape(id)
	action := r.FormValue("review")
	if action == "" {
		action = reviewAgain // a page rendered before forms named their action had only this one
	}
	turn := r.FormValue("turn")
	if id == "" || (action != reviewFirst && action != reviewRetry && action != reviewAgain) || (action == reviewRetry && turn == "") {
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	now := h.now().UTC()
	msg := worklist.Message{ID: worklist.NewMessageID(), Role: worklist.RoleUser, Kind: worklist.KindReviewRequest, Content: reviewAgainPrompt, At: now}
	if action != reviewAgain {
		msg.Content = reviewPrompt
	}
	var (
		ref         worklist.TurnRef     // the turn to dispatch; empty when nothing changed
		retry       *worklist.RetryStamp // set when an unanswered review request is retried
		independent bool
		opt         ModelOption
	)
	err := h.store.Update(r.Context(), user.ID, func(current []worklist.WorkItem) ([]worklist.WorkItem, error) {
		ref, retry, independent = worklist.TurnRef{}, nil, false
		for i := range current {
			it := current[i]
			if it.ID != id {
				continue
			}
			again := againReviewable(it, now)
			first := firstReviewable(it, now)
			switch action {
			case reviewFirst:
				if !first || h.retryCandidate(it, -1) >= 0 || h.reviewBlocker(it, -1) != "" {
					return nil, nil // reviewed since, a reply in flight, a request to retry instead, or off the radar
				}
			case reviewRetry:
				// The turn must be the one the thread offers to retry, as a first review
				// or as a review of the current revision; otherwise the page is out of date.
				j, ok := it.TurnIndex(turn)
				if !ok || !((first && h.retryCandidate(it, -1) == j) || (again && h.retryCandidate(it, it.LatestReviewAt()) == j)) {
					return nil, nil
				}
				choices, ind, _ := h.retryChoices(it, j)
				opt, independent = pickOption(choices, ""), ind
				s := current[i].Restamp(j, now)
				retry = &s
				ref = worklist.TurnRef{Item: id, Turn: s.Key, Attempt: s.Attempt}
				return current[i : i+1], nil
			case reviewAgain:
				if !again || h.retryCandidate(it, it.LatestReviewAt()) >= 0 || h.reviewBlocker(it, it.LatestReviewAt()) != "" {
					return nil, nil // the review is current (or the conversation was reset), a request to retry instead, a reply in flight, or off the radar
				}
			}
			current[i].Thread = append(current[i].Thread, msg)
			opt, _ = h.defaultOption()
			ref = worklist.TurnRef{Item: id, Turn: msg.ID}
			return current[i : i+1], nil
		}
		return nil, nil
	})
	if err != nil || ref.Turn == "" {
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	if independent {
		err = h.pipeline.SecondOpinion(r.Context(), user.ID, ref, opt.Endpoint, opt.Model)
	} else {
		err = h.pipeline.Converse(r.Context(), user.ID, ref, opt.Endpoint, opt.Model)
	}
	if retry != nil {
		// A run for the turn was still in flight, and the retry shares it: the turn
		// keeps that run's attempt.
		h.revertShared(r.Context(), user.ID, err, []worklist.RetryStamp{*retry})
	}
	if errors.Is(err, ingest.ErrNotAccepted) {
		// The backend certainly started nothing: say so on the thread, whichever turn
		// is the latest, and leave the turn retryable at once. A new turn records the
		// refusal of its only attempt, so it is neither pending nor able to answer
		// (WorkItem.MarkRefused). A retried turn goes back to its earlier attempt, and
		// with it whatever was known of that attempt: recording the retry's refusal
		// would only replace the earlier attempt's record. Any other error leaves the
		// turn pending, since a run may exist.
		if retry != nil {
			h.undoRefusedRetry(r.Context(), user.ID, *retry)
		} else {
			h.markRefused(r.Context(), user.ID, []worklist.TurnRef{ref})
		}
		back += "&review_refused=1"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// draftResult is the result view's data.
type draftResult struct {
	Item     worklist.WorkItem
	Created  bool
	Unknown  bool
	Repeat   bool   // an earlier confirmation of this draft: nothing was sent now
	URL      string // the pending review on GitHub, when GitHub gave a link
	Comments int    // inline comments sent
	Summary  bool   // the summary was sent
	Approve  bool   // the approval commands were sent
	Redraft  bool   // GitHub refused the draft as out of date: the page offers drafting again
	Key      string // the refused draft's turn key and Digest, binding that offer to it
	Digest   string
	// Body is the review body sent (the summary and approval kept), shown to copy:
	// GitHub's dialog for submitting a pending review can open with an empty summary
	// that replaces it.
	Body  string
	Error string
}

// CreateDraftReview handles POST /items/draft-review/create (same-origin only). It
// creates a PENDING review on GitHub from the thread's offered draft, with the
// comments the owner kept checked (form "c", by position), the summary if checked
// (form "summary"), and, for an approving draft, its approval commands if checked
// (form "approve"; worklist.ReviewBody puts them after the summary).
//
// The draft is read from the store, never from the form: the form names the turn and
// the draft's digest, and the draft must still be the one the thread offers
// (worklist.OfferedDraft: the newest request's reply, unchanged, and pinned to the
// item's pull request as it stands, so a write-back that changed the item's
// destination is refused). Selection may only leave comments out. Before asking
// GitHub for anything, the confirmation is claimed in the store
// (worklist.ClaimDraft), so a replay of this POST, even after the pending review was
// submitted or discarded on GitHub, or after a restart, gets the recorded receipt
// back and writes nothing; the claim is released only when GitHub certainly created
// nothing.
func (h *Handler) CreateDraftReview(w http.ResponseWriter, r *http.Request) {
	user := h.sessions.CurrentUser(r)
	rc, creates := h.reviewCreator()
	enabled, connected := h.commentState(r.Context(), user)
	if user == nil || !creates || !enabled {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if !parseURLEncoded(r) {
		h.commentFailure(w, user, http.StatusBadRequest, "gofer could not read that form. Go back and try again.")
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	id, key, digest := r.PostForm.Get("id"), r.PostForm.Get("msg"), r.PostForm.Get("digest")
	back := "/items/thread?id=" + url.QueryEscape(id)
	fail := func(status int, msg string) {
		h.render(w, status, pageData{View: "error", User: user, Failure: msg, RetryURL: back})
	}
	const notOffered = "This draft is no longer the one the thread offers: it was cleared, hidden or replaced, or the item now points at another pull request. Nothing was created; look at the thread again."
	it, ok, err := h.store.Get(r.Context(), user.ID, id)
	switch {
	case err != nil:
		fail(http.StatusBadGateway, "gofer could not read this item right now. Nothing was created; try again in a moment.")
		return
	case !ok || !it.OnRadar():
		fail(http.StatusConflict, "This pull request is no longer on your radar, so gofer created nothing.")
		return
	}
	if j := it.LatestDraft(); j >= 0 && it.TurnKey(j) == key && it.Thread[j].Draft.Digest() == digest && it.Thread[j].Draft.For(it) {
		if prior := it.Thread[j].DraftReceipt; prior != nil {
			h.renderReceipt(w, user, it, prior) // confirmed before, under whatever rules: nothing is sent
			return
		}
		if !it.Thread[j].Draft.Current() {
			fail(http.StatusConflict, outdatedDraft)
			return
		}
	}
	i, offered := it.OfferedDraft(key, digest)
	if !offered {
		fail(http.StatusConflict, notOffered)
		return
	}
	if !connected {
		http.Redirect(w, r, "/?commenting=connect", http.StatusSeeOther)
		return
	}
	draft := it.Thread[i].Draft
	rv := comment.Review{Repo: draft.Repo, Number: draft.Number, Review: ghcomment.Review{Commit: draft.Commit, BaseRef: draft.BaseRef, Patches: map[string]string{}}}
	seen := map[int]bool{}
	for _, v := range r.PostForm["c"] {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n >= len(draft.Comments) || seen[n] {
			continue
		}
		seen[n] = true
		c := draft.Comments[n]
		rv.Comments = append(rv.Comments, ghcomment.ReviewComment{Path: c.Path, Line: c.Line, Side: c.Side, Body: c.Body})
		rv.Patches[c.Path] = draft.Patches[c.Path]
	}
	summary := ""
	if r.PostForm.Get("summary") == "1" {
		summary = draft.Body
	}
	approve := draft.Approve && r.PostForm.Get("approve") == "1"
	rv.Body = worklist.ReviewBody(summary, approve)
	if approve {
		rv.Diff = draft.Diff // GitHub must still show exactly the diff the approval is of
	}
	if len(rv.Comments) == 0 && rv.Body == "" {
		fail(http.StatusBadRequest, "Select at least one comment, the summary or the approval to create a pending review.")
		return
	}
	// Claim the confirmation. The claim re-checks, atomically, that the draft is the
	// one just read (same digest, still offered), so the review built above is still
	// the stored draft's.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), draftReviewTimeout)
	defer cancel()
	claimedAt := h.now().UTC()
	it, prior, err := worklist.ClaimDraft(ctx, h.store, user.ID, id, key, digest, rv.Body, claimedAt)
	switch {
	case errors.Is(err, worklist.ErrDraftNotOffered):
		fail(http.StatusConflict, notOffered)
		return
	case err != nil:
		fail(http.StatusBadGateway, "gofer could not record this confirmation, so it created nothing. Try again in a moment.")
		return
	case prior != nil:
		h.renderReceipt(w, user, it, prior)
		return
	}
	settle := func(receipt *worklist.DraftReceipt) {
		// Its own deadline: the request's may have run out reading GitHub, and a
		// release that never lands would leave a draft nothing was sent for
		// unconfirmable.
		sctx, scancel := context.WithTimeout(context.WithoutCancel(r.Context()), settleTimeout)
		defer scancel()
		if err := worklist.SettleDraft(sctx, h.store, user.ID, id, key, claimedAt, receipt); err != nil {
			// The claim stands: the thread shows the draft as possibly created, and no
			// replay writes again. Safe, if less precise.
			slog.WarnContext(ctx, "pending review: could not record the outcome", "user", user.ID, "err", err)
		}
	}
	res, err := rc.PendingReview(ctx, user.ID, rv)
	switch {
	case errors.Is(err, comment.ErrNotConnected):
		settle(nil)
		http.Redirect(w, r, "/?commenting=connect", http.StatusSeeOther)
		return
	case err != nil:
		settle(nil)
		slog.WarnContext(ctx, "pending review failed", "user", user.ID, "err", err)
		fail(http.StatusBadGateway, "gofer could not create the pending review right now; nothing was created. Try again in a moment.")
		return
	case res.Created:
		settle(&worklist.DraftReceipt{State: worklist.DraftCreated, URL: res.URL, Body: rv.Body, At: claimedAt})
	case res.Unknown:
		settle(&worklist.DraftReceipt{State: worklist.DraftUnknown, Body: rv.Body, At: claimedAt})
	default:
		settle(nil) // GitHub certainly created nothing: the owner may try again
	}
	slog.InfoContext(ctx, "pending review", "user", user.ID, "created", res.Created, "unknown", res.Unknown, "comments", len(rv.Comments), "approve", approve)
	view := draftResult{Item: it, Created: res.Created, Unknown: res.Unknown, URL: res.URL, Comments: len(rv.Comments), Summary: summary != "", Approve: approve, Body: rv.Body, Redraft: res.Stale, Key: key, Digest: digest, Error: res.Error}
	status := http.StatusOK
	if !res.Created && !res.Unknown {
		status = http.StatusConflict
	}
	h.render(w, status, pageData{View: "draft-review-result", User: user, Item: it, DraftResult: &view})
}

// Parts names what the review was created with, for the result page: "2 inline
// comments, a summary and the approval commands", "the approval commands", ...
func (v draftResult) Parts() string {
	var parts []string
	if v.Comments > 0 {
		noun := "inline comments"
		if v.Comments == 1 {
			noun = "inline comment"
		}
		parts = append(parts, fmt.Sprintf("%d %s", v.Comments, noun))
	}
	if v.Summary {
		parts = append(parts, "a summary")
	}
	if v.Approve {
		parts = append(parts, "the approval commands")
	}
	switch len(parts) {
	case 0:
		return "nothing selected"
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// renderReceipt answers a confirmation of a draft already confirmed: what the
// earlier one did, with nothing sent to GitHub now.
func (h *Handler) renderReceipt(w http.ResponseWriter, user *session.User, it worklist.WorkItem, rec *worklist.DraftReceipt) {
	view := draftResult{Item: it, Repeat: true, Created: rec.State == worklist.DraftCreated, URL: rec.URL, Body: rec.Body}
	if !view.Created {
		view.Unknown = true
		view.Error = "An earlier confirmation of this draft may have created a pending review: its answer was lost, or it was interrupted. Check the pull request on GitHub. gofer will not create another review from this draft; draft the comments again to start over."
	}
	h.render(w, http.StatusOK, pageData{View: "draft-review-result", User: user, Item: it, DraftResult: &view})
}

// isDraftReviewRequest reports whether a thread turn is the draft-review request the
// button posts; the thread shows it as a notice, since the user never typed it.
func isDraftReviewRequest(m worklist.Message) bool {
	return m.Role == worklist.RoleUser && m.Kind == worklist.KindDraftReviewRequest
}
