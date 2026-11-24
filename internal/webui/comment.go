package webui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackfrancis/gofer/internal/comment"
	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// This file is the bulk comment (ADR 0009): select items on the radar, write one
// comment, preview it, confirm, and gofer posts it to each item as the user, with the
// separate write grant. Posting happens here in the web tier; nothing is dispatched.

// Commenter posts comments and suggests mentions (comment.Service).
type Commenter interface {
	State(ctx context.Context, userID string) (enabled, connected bool)
	Post(ctx context.Context, userID string, targets []comment.Target, body string) ([]comment.Result, error)
	Mentions(ctx context.Context, userID string, repos []string) (comment.Mentions, error)
}

const (
	// maxCommentForm bounds a bulk comment form: the comment, up to MaxTargets ids,
	// and the hidden fields.
	maxCommentForm = 2*maxMessageBytes + comment.MaxTargets*512 + 4<<10
	// postTimeout bounds a whole batch. It runs detached from the browser request,
	// so a closed tab does not leave a batch half-reported.
	postTimeout = 5 * time.Minute
	// previewTTL is how long a preview can be confirmed.
	previewTTL = 30 * time.Minute
	// resultTTL is how long a confirmed batch's results are kept against a resubmit.
	resultTTL = time.Hour
	// maxPreviewsPerOwner and maxRecords bound the previews and results held.
	maxPreviewsPerOwner = 20
	maxRecords          = 2000
	// mentionsTimeout bounds a suggestion request.
	mentionsTimeout = 15 * time.Second
	// maxMentionQuery bounds how many repositories one suggestion request may name.
	maxMentionQuery = comment.MaxTargets
)

// UseCommenter turns the bulk comment on.
func (h *Handler) UseCommenter(c Commenter) { h.commenter = c }

// commentView is the preview and results views' data.
type commentView struct {
	Body    string
	Targets []comment.Target // what the comment goes (or went) to
	Skipped []skippedTarget  // selected items it will not go to, and why
	Nonce   string           // the preview's one-time token, posted back by Confirm
	Results []comment.Result // results view only
	Posted  int              // results view: how many were posted
	Unknown int              // results view: how many may or may not have been posted
	Repeat  bool             // results view: a resubmit of a batch already posted
	Error   string           // why nothing was posted
}

type skippedTarget struct {
	Label  string
	Reason string
}

// commentRecord is one previewed comment, registered under its nonce when the
// preview renders: the comment and the items it previewed, for its owner. Confirm
// posts exactly what the preview showed, once: the record is consumed (batch set)
// atomically, and a nonce gofer does not hold (expired, or from before a restart)
// is refused, never posted.
type commentRecord struct {
	owner   string
	body    string
	targets []comment.Target // the destinations the preview showed: item, repo, number
	created time.Time
	batch   *batch // set once confirmed
}

// batch is a confirmed record's run: its results, once done is closed.
type batch struct {
	done      chan struct{}
	view      commentView // valid once done is closed
	confirmed time.Time
}

type batches struct {
	mu sync.Mutex
	m  map[string]*commentRecord
}

// expired reports whether rec can be dropped at now.
func (rec *commentRecord) expired(now time.Time) bool {
	if rec.batch != nil {
		return now.Sub(rec.batch.confirmed) > resultTTL
	}
	return now.Sub(rec.created) > previewTTL
}

// register records a preview and returns its nonce. It keeps at most
// maxPreviewsPerOwner unconfirmed previews per owner and maxRecords in all, dropping
// the oldest unconfirmed first: a dropped preview can no longer be confirmed, which
// is safe (the user previews again); a kept result guards a resubmit.
func (b *batches) register(owner, body string, targets []comment.Target, now time.Time) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.m == nil {
		b.m = map[string]*commentRecord{}
	}
	ownPending := 0
	for k, rec := range b.m {
		if rec.expired(now) {
			delete(b.m, k)
		} else if rec.owner == owner && rec.batch == nil {
			ownPending++
		}
	}
	for ownPending >= maxPreviewsPerOwner {
		b.dropOldestLocked(func(r *commentRecord) bool { return r.owner == owner && r.batch == nil })
		ownPending--
	}
	for len(b.m) >= maxRecords {
		if !b.dropOldestLocked(func(r *commentRecord) bool { return r.batch == nil }) {
			b.dropOldestLocked(func(*commentRecord) bool { return true })
		}
	}
	nonce := newNonce()
	b.m[nonce] = &commentRecord{owner: owner, body: body, targets: targets, created: now}
	return nonce
}

func (b *batches) dropOldestLocked(match func(*commentRecord) bool) bool {
	var oldest string
	for k, rec := range b.m {
		if match(rec) && (oldest == "" || rec.created.Before(b.m[oldest].created)) {
			oldest = k
		}
	}
	if oldest == "" {
		return false
	}
	delete(b.m, oldest)
	return true
}

// confirm consumes the owner's preview under nonce. ok is false when gofer holds no
// such preview (never issued, expired, dropped, from before a restart, or another
// owner's). A preview confirmed before returns its batch with mine=false; otherwise
// a new batch is started (mine=true) and the caller runs it.
func (b *batches) confirm(nonce, owner string, now time.Time) (rec *commentRecord, bt *batch, mine, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec = b.m[nonce]
	if rec == nil || rec.owner != owner || rec.expired(now) {
		return nil, nil, false, false
	}
	if rec.batch != nil {
		return rec, rec.batch, false, true
	}
	rec.batch = &batch{done: make(chan struct{}), confirmed: now}
	return rec, rec.batch, true, true
}

// receipt returns the batch of the owner's preview under nonce if it was already
// confirmed (its results, or a run still going), else nil. Reading a receipt needs no
// write grant, only the session that owns it.
func (b *batches) receipt(nonce, owner string, now time.Time) *batch {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.m[nonce]
	if rec == nil || rec.owner != owner || rec.batch == nil || rec.expired(now) {
		return nil
	}
	return rec.batch
}

// reopen returns a confirmed preview whose batch attempted nothing (no grant, an
// error before the first post) to its unconfirmed state, so the same preview can be
// confirmed again, until it expires.
func (b *batches) reopen(nonce string, bt *batch) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if rec := b.m[nonce]; rec != nil && rec.batch == bt {
		rec.batch = nil
	}
}

func newNonce() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic("webui: failed to read random bytes: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

// validNonce reports whether s looks like a nonce newNonce made.
func validNonce(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// commentState is the radar's commenting state for user.
func (h *Handler) commentState(ctx context.Context, user *session.User) (enabled, connected bool) {
	if h.commenter == nil || user == nil || user.Provider != "github" {
		return false, false
	}
	return h.commenter.State(ctx, user.ID)
}

// expiredPreview answers a confirmation gofer does not hold. gofer knows only that it
// posted nothing for this request, not what an earlier confirmation of the same
// preview did (its results are gone after resultTTL or a restart).
const expiredPreview = "gofer no longer has this preview: it expired, or gofer restarted since it was shown. Nothing was posted for this request, but gofer cannot tell whether an earlier confirmation of it posted. If you confirmed it before, check the items on GitHub before previewing the comment again."

// Comment handles POST /items/comment. Without confirm=1 it renders the preview:
// the comment and every item it will go to, registered under a one-time nonce, and
// posts nothing. With confirm=1 and that nonce (same-origin only) it consumes the
// preview and posts its comment to each of its items still on the radar, in order,
// and renders the results. A nonce already confirmed shows that batch's results; one
// gofer does not hold is refused, so nothing is ever posted twice from one preview.
func (h *Handler) Comment(w http.ResponseWriter, r *http.Request) {
	user := h.sessions.CurrentUser(r)
	enabled, connected := h.commentState(r.Context(), user)
	if !enabled {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxCommentForm)
	if !parseURLEncoded(r) {
		h.commentFailure(w, user, http.StatusBadRequest, "gofer could not read that form. Go back and try again.")
		return
	}
	if r.PostForm.Get("confirm") == "1" {
		// A live grant is needed only to start posting: the results of a batch
		// already confirmed (one a dead grant cut short, say) are the owner's to read
		// without connecting again.
		h.confirmComment(w, r, user, connected)
		return
	}
	if !connected {
		http.Redirect(w, r, "/?commenting=connect", http.StatusSeeOther)
		return
	}

	body := strings.ReplaceAll(r.PostForm.Get("content"), "\r\n", "\n")
	if strings.TrimSpace(body) == "" {
		h.commentFailure(w, user, http.StatusBadRequest, "Write a comment first.")
		return
	}
	if len(body) > maxMessageBytes {
		h.commentFailure(w, user, http.StatusRequestEntityTooLarge, messageTooLong)
		return
	}
	ids := dedupeIDs(r.PostForm["item"])
	switch {
	case len(ids) == 0:
		h.commentFailure(w, user, http.StatusBadRequest, "Select at least one item to comment on.")
		return
	case len(ids) > comment.MaxTargets:
		h.commentFailure(w, user, http.StatusBadRequest, "Select at most 50 items for one comment.")
		return
	}
	targets, skipped, err := h.commentTargets(r.Context(), user.ID, ids)
	if err != nil {
		h.commentFailure(w, user, http.StatusBadGateway, "gofer could not read your worklist right now. Try again in a moment.")
		return
	}
	view := commentView{Body: body, Targets: targets, Skipped: skipped}
	if len(targets) == 0 {
		view.Error = "None of the selected items can take a comment now."
	} else {
		// Only the destinations shown as eligible are registered, each as the item
		// and the repository and number shown for it: confirming may drop one that
		// left the radar, or whose destination changed, since, but never adds or
		// redirects one.
		view.Nonce = h.batches.register(user.ID, body, targets, h.now())
	}
	// The page's default policy (no-referrer) makes a form POST from it send
	// "Origin: null"; same-origin lets the Confirm button send its real origin, which
	// confirmComment checks on browsers without fetch metadata.
	w.Header().Set("Referrer-Policy", "same-origin")
	h.render(w, http.StatusOK, pageData{View: "comment-preview", User: user, Comment: &view})
}

// confirmComment consumes a preview and posts it (Comment, confirm=1), or shows the
// results of one already confirmed. Starting a batch needs a live write grant
// (connected); reading a receipt does not.
func (h *Handler) confirmComment(w http.ResponseWriter, r *http.Request, user *session.User, connected bool) {
	if !sameOrigin(r) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	nonce := r.PostForm.Get("nonce")
	if !validNonce(nonce) {
		h.commentFailure(w, user, http.StatusBadRequest, expiredPreview)
		return
	}
	var (
		bt   = h.batches.receipt(nonce, user.ID, h.now())
		mine bool
	)
	if bt == nil {
		if !connected {
			http.Redirect(w, r, "/?commenting=connect", http.StatusSeeOther)
			return
		}
		var rec *commentRecord
		var ok bool
		rec, bt, mine, ok = h.batches.confirm(nonce, user.ID, h.now())
		if !ok {
			h.commentFailure(w, user, http.StatusConflict, expiredPreview)
			return
		}
		if mine {
			// The destinations are checked again: one hidden, closed or moved since
			// the preview is skipped now.
			go h.runBatch(context.WithoutCancel(r.Context()), nonce, bt, user.ID, rec.body, rec.targets)
		}
	}
	select {
	case <-bt.done:
	case <-r.Context().Done():
		return // the batch carries on; a resubmit shows its results
	}
	view := bt.view
	view.Repeat = !mine
	status := http.StatusOK
	if view.Error != "" && view.Posted == 0 && view.Unknown == 0 {
		status = http.StatusBadGateway
	}
	h.render(w, status, pageData{View: "comment-results", User: user, Comment: &view})
}

// runBatch posts one confirmed preview and records its outcome on bt. A batch that
// attempted nothing reopens its preview, so it can be confirmed again.
func (h *Handler) runBatch(ctx context.Context, nonce string, bt *batch, userID, body string, pinned []comment.Target) {
	ctx, cancel := context.WithTimeout(ctx, postTimeout)
	defer cancel()
	view := commentView{Body: body}
	defer func() {
		bt.view = view
		close(bt.done)
	}()
	targets, skipped, err := h.pinnedTargets(ctx, userID, pinned)
	view.Targets, view.Skipped = targets, skipped
	switch {
	case err != nil:
		view.Error = "gofer could not read your worklist right now; nothing was posted. Try again in a moment."
		h.batches.reopen(nonce, bt)
		return
	case len(targets) == 0:
		view.Error = "None of the selected items can take a comment now; nothing was posted."
		return
	}
	results, err := h.commenter.Post(ctx, userID, targets, body)
	switch {
	case errors.Is(err, comment.ErrNotConnected):
		view.Error = "gofer has no GitHub access to post with. Connect commenting, then try again; nothing was posted."
		h.batches.reopen(nonce, bt)
	case err != nil:
		slog.WarnContext(ctx, "bulk comment failed", "user", userID, "err", err)
		view.Error = "gofer could not post right now; nothing was posted. Try again in a moment."
		h.batches.reopen(nonce, bt)
	}
	view.Results = results
	for _, res := range results {
		switch {
		case res.Posted:
			view.Posted++
		case res.Unknown:
			view.Unknown++
		}
	}
	slog.InfoContext(ctx, "bulk comment", "user", userID, "targets", len(targets), "posted", view.Posted, "unknown", view.Unknown)
}

// pinnedTargets checks a confirmed preview's destinations against the owner's stored
// items now. A destination is kept only if its item is still on the radar AND still
// names the repository and number the preview showed: a write-back may have changed
// an item's GitHub fields under the same ID, and the user approved the destination,
// not the row. It never adds a destination, and posts to the pinned one.
func (h *Handler) pinnedTargets(ctx context.Context, ownerID string, pinned []comment.Target) ([]comment.Target, []skippedTarget, error) {
	ids := make([]string, len(pinned))
	for i, p := range pinned {
		ids[i] = p.ID
	}
	current, skipped, err := h.commentTargets(ctx, ownerID, ids)
	if err != nil {
		return nil, nil, err
	}
	now := make(map[string]comment.Target, len(current))
	for _, t := range current {
		now[t.ID] = t
	}
	var targets []comment.Target
	for _, p := range pinned {
		t, ok := now[p.ID]
		switch {
		case !ok:
			// skipped by commentTargets already (gone, hidden, closed)
		case !strings.EqualFold(t.Repo, p.Repo) || t.Number != p.Number:
			skipped = append(skipped, skippedTarget{
				Label:  p.Repo + " #" + itoa(p.Number) + " " + p.Title,
				Reason: "now points at " + t.Repo + " #" + itoa(t.Number) + "; not the destination you previewed",
			})
		default:
			targets = append(targets, p)
		}
	}
	return targets, skipped, nil
}

// commentTargets resolves the selected ids against the owner's stored items. Only
// items on the radar take a comment (no bulk action reaches hidden or closed items);
// the rest are reported as skipped.
func (h *Handler) commentTargets(ctx context.Context, ownerID string, ids []string) ([]comment.Target, []skippedTarget, error) {
	items, err := h.store.Briefs(ctx, ownerID, worklist.BriefQuery{})
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[string]worklist.WorkItem, len(items))
	for _, it := range items {
		byID[it.ID] = it
	}
	var targets []comment.Target
	var skipped []skippedTarget
	for _, id := range ids {
		it, ok := byID[id]
		switch {
		case !ok:
			skipped = append(skipped, skippedTarget{Label: "An item no longer in your worklist", Reason: "gone"})
		case !it.OnRadar() && !it.Meta.CompletedAt.IsZero():
			skipped = append(skipped, skippedTarget{Label: itemLabel(it), Reason: "closed"})
		case !it.OnRadar():
			skipped = append(skipped, skippedTarget{Label: itemLabel(it), Reason: "hidden"})
		case it.GitHub.Number <= 0 || it.GitHub.Repo == "":
			skipped = append(skipped, skippedTarget{Label: itemLabel(it), Reason: "not a GitHub issue or pull request"})
		default:
			targets = append(targets, comment.Target{
				ID: it.ID, Repo: it.GitHub.Repo, Number: it.GitHub.Number,
				Title: it.GitHub.Title, URL: it.GitHub.URL,
			})
		}
	}
	return targets, skipped, nil
}

func itemLabel(it worklist.WorkItem) string {
	if it.GitHub.Repo == "" {
		return it.GitHub.Title
	}
	return it.GitHub.Repo + " #" + itoa(it.GitHub.Number) + " " + it.GitHub.Title
}

// commentFailure renders the error view for a bulk comment that changed nothing.
func (h *Handler) commentFailure(w http.ResponseWriter, user *session.User, status int, msg string) {
	h.render(w, status, pageData{View: "error", User: user, Failure: msg})
}

// Mentions handles GET /items/mentions?repo=owner/name (repeated): the logins that
// can be assigned in those repositories, ranked by how many of them can assign each,
// for the comment box's @ suggestions (static/comment.js).
func (h *Handler) Mentions(w http.ResponseWriter, r *http.Request) {
	user := h.sessions.CurrentUser(r)
	if enabled, _ := h.commentState(r.Context(), user); !enabled {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "commenting is not available"})
		return
	}
	var repos []string
	for _, repo := range r.URL.Query()["repo"] {
		if repoName.MatchString(repo) {
			repos = append(repos, repo)
		}
	}
	if len(repos) > maxMentionQuery {
		repos = repos[:maxMentionQuery] // the comment service checks fewer, and says so
	}
	ctx, cancel := context.WithTimeout(r.Context(), mentionsTimeout)
	defer cancel()
	m, err := h.commenter.Mentions(ctx, user.ID, repos)
	if err != nil {
		writeJSONStatus(w, http.StatusBadGateway, map[string]string{"error": "suggestions are unavailable"})
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=300")
	writeJSONStatus(w, http.StatusOK, m)
}

// parseURLEncoded parses a URL-encoded form body, the only kind the page posts.
func parseURLEncoded(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/x-www-form-urlencoded" {
		return false
	}
	return r.ParseForm() == nil
}

// sameOrigin reports whether a state-changing request came from gofer's own page.
// A browser that sends fetch metadata decides: only Sec-Fetch-Site: same-origin
// passes (its Origin may be "null", since the page's referrer policy is no-referrer).
// Without it, an Origin, when sent, must name this host; "null" is refused, since
// any opaque origin (a sandboxed frame, a data: URL) sends it. SameSite=Lax cookies
// already keep a cross-site POST unauthenticated; this is the second check a write to
// GitHub deserves.
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin":
		return true
	case "":
	default:
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return o != "null" && err == nil && u.Host != "" && u.Host == r.Host
}

func dedupeIDs(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, id := range in {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
