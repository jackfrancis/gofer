package webui

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackfrancis/gofer/internal/comment"
	"github.com/jackfrancis/gofer/internal/ghcomment"
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

// draftReviewTimeout bounds creating the pending review: a few GitHub calls. It runs
// detached from the browser request, so a closed tab does not cut it short midway.
const draftReviewTimeout = 60 * time.Second

// ReviewCreator creates a pending review (comment.Service). A Commenter that does
// not implement it offers no draft review.
type ReviewCreator interface {
	PendingReview(ctx context.Context, userID string, rv comment.Review) (comment.ReviewResult, error)
}

// draftThread is the thread view's draft-review state. A nil one shows nothing: the
// item is not a pull request on the radar, or commenting or the assistant is off.
type draftThread struct {
	Connected bool   // the write grant is connected; otherwise the thread links to connecting it
	Ready     bool   // the button is enabled: a completed review, and no reply in flight
	Why       string // why the button is disabled
	Index     int    // the position of the draft the thread offers; -1 for none
	Digest    string // that draft's Digest, posted back with the selection
	// Receipt is the offered draft's confirmation, if it has one: the thread then
	// shows what happened instead of the selection. Interrupted marks a claim older
	// than draftReviewTimeout, whose request gofer never settled.
	Receipt     *worklist.DraftReceipt
	Interrupted bool
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
	case !it.HasReview():
		v.Why = "Review this pull request first: there are no review comments to draft yet."
	case it.ReplyPending(now, reviewStaleAfter):
		v.Why = "Wait for the reply in progress, so the draft includes it."
	default:
		v.Ready = true
	}
	// Only a draft pinned to the item's pull request as it stands is offered: a
	// write-back that changed the item's destination retires it.
	if i := it.LatestDraft(); i >= 0 && connected && it.Thread[i].Draft.For(it) {
		v.Index = i
		v.Digest = it.Thread[i].Draft.Digest()
		v.Receipt = it.Thread[i].DraftReceipt
		v.Interrupted = v.Receipt != nil && v.Receipt.State == worklist.DraftCreating && now.Sub(v.Receipt.At) > draftReviewTimeout
	}
	return v
}

// draftable reports whether a draft-review request may be added to it now: a pull
// request on the radar (so not hidden or closed) with a completed review and no reply
// in flight. It is the button's condition, checked again inside the store update.
func draftable(it worklist.WorkItem, now time.Time) bool {
	return it.Type == worklist.TypePullRequest && it.OnRadar() && it.HasReview() && !it.ReplyPending(now, reviewStaleAfter)
}

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
			if !draftable(current[i], now) {
				return nil, nil // a stale page: no review yet, a reply in flight, or off the radar
			}
			current[i].Thread = append(current[i].Thread, msg)
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

// draftResult is the result view's data.
type draftResult struct {
	Item     worklist.WorkItem
	Created  bool
	Unknown  bool
	Repeat   bool   // an earlier confirmation of this draft: nothing was sent now
	URL      string // the pending review on GitHub, when GitHub gave a link
	Comments int    // inline comments sent
	Summary  bool   // the summary was sent
	Error    string
}

// CreateDraftReview handles POST /items/draft-review/create (same-origin only). It
// creates a PENDING review on GitHub from the thread's offered draft, with the
// comments the owner kept checked (form "c", by position) and the summary if checked
// (form "summary").
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
	i, offered := it.OfferedDraft(key, digest)
	if !offered {
		fail(http.StatusConflict, notOffered)
		return
	}
	if prior := it.Thread[i].DraftReceipt; prior != nil {
		h.renderReceipt(w, user, it, prior)
		return
	}
	if !connected {
		http.Redirect(w, r, "/?commenting=connect", http.StatusSeeOther)
		return
	}
	draft := it.Thread[i].Draft
	rv := comment.Review{Repo: draft.Repo, Number: draft.Number, Commit: draft.Commit, BaseRef: draft.BaseRef}
	seen := map[int]bool{}
	for _, v := range r.PostForm["c"] {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n >= len(draft.Comments) || seen[n] {
			continue
		}
		seen[n] = true
		c := draft.Comments[n]
		rv.Comments = append(rv.Comments, ghcomment.ReviewComment{Path: c.Path, Line: c.Line, Side: c.Side, Body: c.Body})
	}
	if r.PostForm.Get("summary") == "1" {
		rv.Body = draft.Body
	}
	if len(rv.Comments) == 0 && rv.Body == "" {
		fail(http.StatusBadRequest, "Select at least one comment, or the summary, to create a pending review.")
		return
	}
	// Claim the confirmation. The claim re-checks, atomically, that the draft is the
	// one just read (same digest, still offered), so the review built above is still
	// the stored draft's.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), draftReviewTimeout)
	defer cancel()
	claimedAt := h.now().UTC()
	it, prior, err := worklist.ClaimDraft(ctx, h.store, user.ID, id, key, digest, claimedAt)
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
		if err := worklist.SettleDraft(ctx, h.store, user.ID, id, key, claimedAt, receipt); err != nil {
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
		settle(&worklist.DraftReceipt{State: worklist.DraftCreated, URL: res.URL, At: claimedAt})
	case res.Unknown:
		settle(&worklist.DraftReceipt{State: worklist.DraftUnknown, At: claimedAt})
	default:
		settle(nil) // GitHub certainly created nothing: the owner may try again
	}
	slog.InfoContext(ctx, "pending review", "user", user.ID, "created", res.Created, "unknown", res.Unknown, "comments", len(rv.Comments))
	view := draftResult{Item: it, Created: res.Created, Unknown: res.Unknown, URL: res.URL, Comments: len(rv.Comments), Summary: rv.Body != "", Error: res.Error}
	status := http.StatusOK
	if !res.Created && !res.Unknown {
		status = http.StatusConflict
	}
	h.render(w, status, pageData{View: "draft-review-result", User: user, Item: it, DraftResult: &view})
}

// renderReceipt answers a confirmation of a draft already confirmed: what the
// earlier one did, with nothing sent to GitHub now.
func (h *Handler) renderReceipt(w http.ResponseWriter, user *session.User, it worklist.WorkItem, rec *worklist.DraftReceipt) {
	view := draftResult{Item: it, Repeat: true, Created: rec.State == worklist.DraftCreated, URL: rec.URL}
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
