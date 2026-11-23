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
	postTimeout = 3 * time.Minute
	// nonceTTL is how long a confirmed batch's results are kept against a resubmit.
	nonceTTL = time.Hour
	// mentionsTimeout bounds a suggestion request.
	mentionsTimeout = 15 * time.Second
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
	Repeat  bool             // results view: a resubmit of a batch already posted
	Error   string           // why nothing was posted
}

type skippedTarget struct {
	Label  string
	Reason string
}

// batch is a confirmed submission, kept by its nonce against a resubmit.
type batch struct {
	owner   string
	done    chan struct{}
	view    commentView // valid once done is closed
	created time.Time
}

type batches struct {
	mu sync.Mutex
	m  map[string]*batch
}

// claim returns the batch already registered for nonce (mine=false), or registers a
// new one for owner (mine=true). A nonce another owner used is refused (nil).
func (b *batches) claim(nonce, owner string, now time.Time) (bt *batch, mine bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.m == nil {
		b.m = map[string]*batch{}
	}
	for k, v := range b.m {
		if now.Sub(v.created) > nonceTTL {
			delete(b.m, k)
		}
	}
	if bt := b.m[nonce]; bt != nil {
		if bt.owner != owner {
			return nil, false
		}
		return bt, false
	}
	bt = &batch{owner: owner, done: make(chan struct{}), created: now}
	b.m[nonce] = bt
	return bt, true
}

// forget drops a batch that posted nothing, so the user can try again.
func (b *batches) forget(nonce string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.m, nonce)
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

// Comment handles POST /items/comment. Without confirm=1 it renders the preview:
// the comment and every item it will go to, with a one-time nonce, and posts
// nothing. With confirm=1 (same-origin only) it posts the comment to each item, in
// order, and renders the results. A confirm whose nonce was already used shows that
// batch's results instead of posting again.
func (h *Handler) Comment(w http.ResponseWriter, r *http.Request) {
	user := h.sessions.CurrentUser(r)
	enabled, connected := h.commentState(r.Context(), user)
	if !enabled {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !connected {
		http.Redirect(w, r, "/?commenting=connect", http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxCommentForm)
	if !parseURLEncoded(r) {
		h.commentFailure(w, user, http.StatusBadRequest, "gofer could not read that form. Go back and try again.")
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

	if r.PostForm.Get("confirm") != "1" {
		view := commentView{Body: body, Targets: targets, Skipped: skipped, Nonce: newNonce()}
		if len(targets) == 0 {
			view.Error = "None of the selected items can take a comment now."
		}
		h.render(w, http.StatusOK, pageData{View: "comment-preview", User: user, Comment: &view})
		return
	}

	if !sameOrigin(r) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	nonce := r.PostForm.Get("nonce")
	if !validNonce(nonce) {
		h.commentFailure(w, user, http.StatusBadRequest, "That confirmation has expired. Go back and preview the comment again.")
		return
	}
	bt, mine := h.batches.claim(nonce, user.ID, h.now())
	if bt == nil {
		h.commentFailure(w, user, http.StatusBadRequest, "That confirmation has expired. Go back and preview the comment again.")
		return
	}
	if mine {
		go h.runBatch(context.WithoutCancel(r.Context()), nonce, bt, user.ID, body, targets, skipped)
	}
	select {
	case <-bt.done:
	case <-r.Context().Done():
		return // the batch carries on; a resubmit shows its results
	}
	view := bt.view
	view.Repeat = !mine
	status := http.StatusOK
	if view.Error != "" && view.Posted == 0 {
		status = http.StatusBadGateway
	}
	h.render(w, status, pageData{View: "comment-results", User: user, Comment: &view})
}

// runBatch posts one confirmed batch and records its outcome on bt.
func (h *Handler) runBatch(ctx context.Context, nonce string, bt *batch, userID, body string, targets []comment.Target, skipped []skippedTarget) {
	ctx, cancel := context.WithTimeout(ctx, postTimeout)
	defer cancel()
	view := commentView{Body: body, Targets: targets, Skipped: skipped}
	defer close(bt.done)
	if len(targets) == 0 {
		view.Error = "None of the selected items can take a comment now; nothing was posted."
		bt.view = view
		h.batches.forget(nonce)
		return
	}
	results, err := h.commenter.Post(ctx, userID, targets, body)
	switch {
	case errors.Is(err, comment.ErrNotConnected):
		view.Error = "gofer has no GitHub access to post with. Connect commenting, then try again; nothing was posted."
		h.batches.forget(nonce)
	case err != nil:
		slog.WarnContext(ctx, "bulk comment failed", "user", userID, "err", err)
		view.Error = "gofer could not post right now; nothing was posted. Try again in a moment."
		h.batches.forget(nonce)
	}
	view.Results = results
	for _, res := range results {
		if res.Posted {
			view.Posted++
		}
	}
	slog.InfoContext(ctx, "bulk comment", "user", userID, "targets", len(targets), "posted", view.Posted)
	bt.view = view
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
	if len(repos) > comment.MaxMentionRepos {
		repos = repos[:comment.MaxMentionRepos]
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

// sameOrigin reports whether a state-changing request came from gofer's own page:
// the browser's Sec-Fetch-Site, when sent, must say so, and an Origin, when sent,
// must be this host. SameSite=Lax cookies already keep a cross-site POST
// unauthenticated; this is the second check a write to GitHub deserves.
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || u.Host != r.Host {
			return false
		}
	}
	return true
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
