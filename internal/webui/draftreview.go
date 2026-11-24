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
	Index     int    // the position of the draft the thread offers to create; -1 for none
	Digest    string // that draft's Digest, posted back with the selection
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
	if i := it.LatestDraft(); i >= 0 && connected {
		v.Index = i
		v.Digest = it.Thread[i].Draft.Digest()
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
	URL      string // the pending review on GitHub, when GitHub gave a link
	Comments int    // inline comments sent
	Summary  bool   // the summary was sent
	Error    string
}

// CreateDraftReview handles POST /items/draft-review/create (same-origin only). It
// creates a PENDING review on GitHub from the thread's newest draft, with the
// comments the owner kept checked (form "c", by position) and the summary if checked
// (form "summary"). The draft is read from the store, never from the form: the form
// names the turn and the draft's digest, and a turn that is gone, hidden, no longer
// the newest draft, or whose draft does not match the digest is refused. Selection
// may only leave comments out. GitHub's one-pending-review rule, checked before
// creating, keeps a resubmit from creating a second review.
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
	id := r.PostForm.Get("id")
	back := "/items/thread?id=" + url.QueryEscape(id)
	if !connected {
		http.Redirect(w, r, "/?commenting=connect", http.StatusSeeOther)
		return
	}
	fail := func(status int, msg string) {
		h.render(w, status, pageData{View: "error", User: user, Failure: msg, RetryURL: back})
	}
	it, ok, err := h.store.Get(r.Context(), user.ID, id)
	switch {
	case err != nil:
		fail(http.StatusBadGateway, "gofer could not read this item right now. Nothing was created; try again in a moment.")
		return
	case !ok || it.Type != worklist.TypePullRequest || !it.OnRadar():
		fail(http.StatusConflict, "This pull request is no longer on your radar, so gofer created nothing.")
		return
	}
	i, found := it.TurnIndex(r.PostForm.Get("msg"))
	if !found || i != it.LatestDraft() || it.Thread[i].Draft.Digest() != r.PostForm.Get("digest") {
		fail(http.StatusConflict, "This draft is no longer the thread's newest one (it was cleared, hidden or replaced). Nothing was created; look at the thread again.")
		return
	}
	draft := it.Thread[i].Draft
	rv := comment.Review{Repo: it.GitHub.Repo, Number: it.GitHub.Number, Commit: draft.Commit}
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
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), draftReviewTimeout)
	defer cancel()
	res, err := rc.PendingReview(ctx, user.ID, rv)
	switch {
	case errors.Is(err, comment.ErrNotConnected):
		http.Redirect(w, r, "/?commenting=connect", http.StatusSeeOther)
		return
	case err != nil:
		slog.WarnContext(ctx, "pending review failed", "user", user.ID, "err", err)
		fail(http.StatusBadGateway, "gofer could not create the pending review right now; nothing was created. Try again in a moment.")
		return
	}
	slog.InfoContext(ctx, "pending review", "user", user.ID, "created", res.Created, "unknown", res.Unknown, "comments", len(rv.Comments))
	view := draftResult{Item: it, Created: res.Created, Unknown: res.Unknown, URL: res.URL, Comments: len(rv.Comments), Summary: rv.Body != "", Error: res.Error}
	status := http.StatusOK
	if !res.Created && !res.Unknown {
		status = http.StatusConflict
	}
	h.render(w, status, pageData{View: "draft-review-result", User: user, Item: it, DraftResult: &view})
}

// isDraftReviewRequest reports whether a thread turn is the draft-review request the
// button posts; the thread shows it as a notice, since the user never typed it.
func isDraftReviewRequest(m worklist.Message) bool {
	return m.Role == worklist.RoleUser && m.Kind == worklist.KindDraftReviewRequest
}
