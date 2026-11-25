package webui

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
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
// comment, press Post, and gofer posts it to each item as the user, with the separate
// write grant, then shows the results on the radar. Posting happens here in the web
// tier; nothing is dispatched.

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
	// commentKeyTTL is how long a radar page's comment form can post: its key is
	// refused after that, and a used key is remembered at least that long, so a
	// resubmitted form never posts twice.
	commentKeyTTL = 12 * time.Hour
	// commentKeySkew is how far ahead of gofer's clock a key's issue time may be.
	commentKeySkew = time.Minute
	// resultTTL is the least time a batch's results are kept for the radar to show.
	resultTTL = time.Hour
	// maxRecords bounds the batches held. Only one that posted nothing is ever
	// dropped before its key expires; past the bound with none such, posting waits.
	maxRecords = 2000
	// mentionsTimeout bounds a suggestion request.
	mentionsTimeout = 15 * time.Second
	// maxMentionQuery bounds how many repositories one suggestion request may name.
	maxMentionQuery = comment.MaxTargets
)

// commentDetailer is a Commenter that also reports the token side of commenting
// (comment.Service): whether the user may connect with a personal access token, and
// whether the connected grant is one (ADR 0011).
type commentDetailer interface {
	Details(ctx context.Context, userID string) comment.Details
}

// UseCommenter turns the bulk comment on.
func (h *Handler) UseCommenter(c Commenter) { h.commenter = c }

// commentView is a batch's outcome, shown on the radar.
type commentView struct {
	Body     string
	Selected []string         // the item ids the form selected, re-checked when nothing was posted
	Targets  []comment.Target // what the comment went to
	Skipped  []skippedTarget  // selected items it did not go to, and why
	Results  []comment.Result
	Posted   int    // how many were posted
	Unknown  int    // how many may or may not have been posted
	Repeat   bool   // the radar was reached by resubmitting a form already used
	Running  bool   // the batch is still posting
	Error    string // why nothing (more) was posted
}

// Retry reports whether the batch is done and certainly put nothing on GitHub, with
// a comment to offer again: the radar then shows the comment box open with the same
// comment and selection, and the reason beside it.
func (v *commentView) Retry() bool {
	return v != nil && !v.Running && v.Posted == 0 && v.Unknown == 0 && v.Body != ""
}

type skippedTarget struct {
	Label  string
	Reason string
}

// commentRecord is one used comment form, held under the owner and the form's
// one-time key: a resubmit of the same form shows this batch's results and posts
// nothing more. A key gofer could not have issued in this process (from before a
// restart) or that expired is refused, never posted, so a used key never posts
// twice: it is remembered until it expires.
type commentRecord struct {
	created time.Time
	until   time.Time // when the record can be dropped: no sooner than its key expires
	batch   *batch
}

// batch is a used form's run: its results, once done is closed.
type batch struct {
	done chan struct{}
	view commentView // valid once done is closed
}

// finish records view as bt's outcome.
func (bt *batch) finish(view commentView) {
	bt.view = view
	close(bt.done)
}

// postedNothing reports whether bt is done and posted nothing, so dropping it early
// cannot let anything post twice.
func (bt *batch) postedNothing() bool {
	select {
	case <-bt.done:
		return bt.view.Posted == 0 && bt.view.Unknown == 0
	default:
		return false
	}
}

type batches struct {
	mu     sync.Mutex
	m      map[string]*commentRecord // by owner + "\x00" + key
	drafts map[string]draft          // by owner: an unsent comment kept through reconnecting
}

func recordKey(owner, key string) string { return owner + "\x00" + key }

// claim starts the batch for the owner's form key, or returns the batch already
// started under it (mine=false). ok is false when gofer is holding too many batches
// to take another without forgetting a used key.
func (b *batches) claim(owner, key string, until, now time.Time) (bt *batch, mine, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.m == nil {
		b.m = map[string]*commentRecord{}
	}
	k := recordKey(owner, key)
	for rk, rec := range b.m {
		if now.After(rec.until) {
			delete(b.m, rk)
		}
	}
	if rec := b.m[k]; rec != nil {
		return rec.batch, false, true
	}
	for len(b.m) >= maxRecords {
		if !b.dropOldestLocked(func(r *commentRecord) bool { return r.batch.postedNothing() }) {
			return nil, false, false
		}
	}
	bt = &batch{done: make(chan struct{})}
	b.m[k] = &commentRecord{created: now, until: until, batch: bt}
	return bt, true, true
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

// lookup returns the owner's batch under key, running or done, or nil. Reading a
// batch needs no write grant, only the session that owns it.
func (b *batches) lookup(owner, key string, now time.Time) *batch {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.m[recordKey(owner, key)]
	if rec == nil || now.After(rec.until) {
		return nil
	}
	return rec.batch
}

// A comment key is the process's epoch (8 bytes, random at startup), the time it
// was issued (8 bytes, Unix seconds), 16 random bytes, and a MAC (16 bytes) binding
// those to the account the radar was rendered for, in hex. The epoch makes a form
// from before a restart, whose earlier use gofer has forgotten, unusable; the issue
// time bounds how long a used key must be remembered; the MAC keeps a form loaded as
// one account from posting as another (the browser signed in again in another tab).
const commentKeyLen = 96

// keyCheck is what commentKeyIssued found.
type keyCheck int

const (
	keyOK           keyCheck = iota
	keyStale                 // malformed, missing, expired, or from another process
	keyOtherAccount          // issued to another account than the session's
)

func randomBytes(n int) []byte {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic("webui: failed to read random bytes: " + err.Error())
	}
	return buf
}

func newEpoch() (e [8]byte) {
	copy(e[:], randomBytes(len(e)))
	return e
}

func newKeySecret() (k [32]byte) {
	copy(k[:], randomBytes(len(k)))
	return k
}

// commentKeyMAC binds a key's first 32 bytes to owner.
func (h *Handler) commentKeyMAC(head []byte, owner string) []byte {
	m := hmac.New(sha256.New, h.keySecret[:])
	m.Write(head)
	m.Write([]byte(owner))
	return m.Sum(nil)[:16]
}

// newCommentKey mints a radar comment form's one-time key for owner.
func (h *Handler) newCommentKey(owner string, now time.Time) string {
	buf := make([]byte, 32, commentKeyLen/2)
	copy(buf, h.epoch[:])
	binary.BigEndian.PutUint64(buf[8:16], uint64(now.Unix()))
	copy(buf[16:], randomBytes(16))
	buf = append(buf, h.commentKeyMAC(buf, owner)...)
	return hex.EncodeToString(buf)
}

// commentKeyIssued returns when key was issued, if this process issued it to owner
// and it has not expired at now.
func (h *Handler) commentKeyIssued(key, owner string, now time.Time) (time.Time, keyCheck) {
	if len(key) != commentKeyLen {
		return time.Time{}, keyStale
	}
	buf, err := hex.DecodeString(key)
	if err != nil || !bytes.Equal(buf[:8], h.epoch[:]) {
		return time.Time{}, keyStale
	}
	if !hmac.Equal(buf[32:], h.commentKeyMAC(buf[:32], owner)) {
		return time.Time{}, keyOtherAccount
	}
	issued := time.Unix(int64(binary.BigEndian.Uint64(buf[8:16])), 0)
	if issued.After(now.Add(commentKeySkew)) || now.Sub(issued) > commentKeyTTL {
		return time.Time{}, keyStale
	}
	return issued, keyOK
}

// commentState is the radar's commenting state for user.
func (h *Handler) commentState(ctx context.Context, user *session.User) (enabled, connected bool) {
	if h.commenter == nil || user == nil || user.Provider != "github" {
		return false, false
	}
	return h.commenter.State(ctx, user.ID)
}

// staleCommentForm answers a form whose key gofer refuses. gofer knows only that it
// posted nothing for this request, not what an earlier use of the same form did.
const staleCommentForm = "Nothing was posted: this page was loaded too long ago, or gofer restarted since. gofer cannot tell whether an earlier submission of this form posted, so check the items on GitHub before you post again."

// otherAccountForm answers a form rendered for another account than the session's.
const otherAccountForm = "Nothing was posted: this page was loaded while you were signed in to gofer as another GitHub account. Check which account you are signed in as, the comment, and the items, then post again."

// draftRestored introduces a comment kept while commenting was reconnected.
const draftRestored = "Your comment is back from before you connected commenting. Nothing was posted: check it and the items, then press Post."

// badSelection answers a form whose selections gofer cannot read.
const badSelection = "Nothing was posted: gofer could not read the selected items. Reload the radar and select them again."

// Comment handles POST /items/comment, the radar's Post button (same-origin only).
// It posts the comment to each selected item still on the radar, in order, then
// redirects to the radar, which shows the results (/?comment=<key>). The form's
// one-time key (minted with the radar page) makes it post at most once: a resubmit
// of a used form shows that batch's results and posts nothing, and a key gofer does
// not hold (expired, from before a restart, or rendered for another account) is
// refused. Each checked row names the destination it showed, and a row whose item
// now names another is skipped.
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
	if !sameOrigin(r) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	now := h.now()
	key := r.PostForm.Get("key")
	issued, check := h.commentKeyIssued(key, user.ID, now)
	valid := check == keyOK
	if valid {
		// A used form's results are the owner's to see again without a live grant;
		// only starting a batch needs one.
		if bt := h.batches.lookup(user.ID, key, now); bt != nil {
			h.awaitBatch(w, r, bt, key, true)
			return
		}
	}
	body := strings.ReplaceAll(r.PostForm.Get("content"), "\r\n", "\n")
	pins, readable := parseSelections(r.PostForm["item"])
	if !connected {
		// Keep the comment and selection through the connect flow: the radar offers
		// them again once commenting is connected, to post with an explicit Post.
		h.batches.saveDraft(user.ID, body, pinIDs(pins), now)
		http.Redirect(w, r, "/?commenting=connect", http.StatusSeeOther)
		return
	}
	until := issued.Add(commentKeyTTL)
	if !valid {
		// Posts nothing, but keeps the comment and selection for the radar to offer
		// again, under a key of gofer's own.
		key = h.newCommentKey(user.ID, now)
		until = now
	}
	if min := now.Add(resultTTL); until.Before(min) {
		until = min
	}
	bt, mine, ok := h.batches.claim(user.ID, key, until, now)
	if !ok {
		h.commentFailure(w, user, http.StatusServiceUnavailable, "gofer is holding too many recent comments to post another right now. Try again later.")
		return
	}
	if mine {
		switch {
		case check == keyOtherAccount:
			bt.finish(commentView{Body: body, Selected: pinIDs(pins), Error: otherAccountForm})
		case !valid:
			bt.finish(commentView{Body: body, Selected: pinIDs(pins), Error: staleCommentForm})
		case !readable:
			bt.finish(commentView{Body: body, Selected: pinIDs(pins), Error: badSelection})
		default:
			go h.runBatch(context.WithoutCancel(r.Context()), bt, user.ID, body, pins)
		}
	}
	h.awaitBatch(w, r, bt, key, !mine)
}

// awaitBatch waits for bt, then sends the browser to the radar to see its results.
// A browser that gives up first leaves the batch running; its results show when the
// radar is next reached under the same key.
func (h *Handler) awaitBatch(w http.ResponseWriter, r *http.Request, bt *batch, key string, repeat bool) {
	select {
	case <-bt.done:
	case <-r.Context().Done():
		return
	}
	loc := "/?comment=" + key
	if repeat {
		loc += "&again=1"
	}
	http.Redirect(w, r, loc, http.StatusSeeOther)
}

// commentOutcome is the radar's view of the owner's batch under the comment query
// parameter, or nil.
func (h *Handler) commentOutcome(r *http.Request, user *session.User) *commentView {
	q := r.URL.Query()
	key := q.Get("comment")
	if key == "" || user == nil {
		return nil
	}
	bt := h.batches.lookup(user.ID, key, h.now())
	if bt == nil {
		return &commentView{Error: "gofer no longer holds the results of that comment. Check the items on GitHub to see what was posted."}
	}
	select {
	case <-bt.done:
	default:
		return &commentView{Running: true}
	}
	view := bt.view
	view.Repeat = q.Get("again") == "1"
	return &view
}

// commentReturnURL is the radar URL that shows the bulk comment outcome r names, or
// "" (the plain radar).
func commentReturnURL(r *http.Request) string {
	q := r.URL.Query()
	key := q.Get("comment")
	if key == "" {
		return ""
	}
	out := url.Values{"comment": {key}}
	if q.Get("again") == "1" {
		out.Set("again", "1")
	}
	return "/?" + out.Encode()
}

// runBatch checks the form, resolves its items against the owner's radar now, posts,
// and records the outcome on bt.
func (h *Handler) runBatch(ctx context.Context, bt *batch, userID, body string, pins []comment.Target) {
	ctx, cancel := context.WithTimeout(ctx, postTimeout)
	defer cancel()
	ids := pinIDs(pins)
	view := commentView{Body: body, Selected: ids}
	defer func() { bt.finish(view) }()
	switch {
	case strings.TrimSpace(body) == "":
		view.Error = "Write a comment first."
		return
	case len(body) > maxMessageBytes:
		view.Error = messageTooLong
		return
	case len(pins) == 0:
		view.Error = "Select at least one item to comment on."
		return
	case len(pins) > comment.MaxTargets:
		view.Error = "Select at most 50 items for one comment."
		return
	}
	targets, skipped, err := h.pinnedTargets(ctx, userID, pins)
	view.Targets, view.Skipped = targets, skipped
	switch {
	case err != nil:
		view.Error = "gofer could not read your worklist right now; nothing was posted. Try again in a moment."
		return
	case len(targets) == 0:
		view.Error = "None of the selected items can take a comment now; nothing was posted."
		return
	}
	results, err := h.commenter.Post(ctx, userID, targets, body)
	switch {
	case errors.Is(err, comment.ErrNotConnected):
		view.Error = "gofer has no GitHub access to post with. Connect commenting, then try again; nothing was posted."
		// The radar shows the connect prompt, not the comment box: keep the comment
		// for after reconnecting.
		h.batches.saveDraft(userID, body, ids, h.now())
	case err != nil:
		slog.WarnContext(ctx, "bulk comment failed", "user", userID, "err", err)
		view.Error = "gofer could not post right now; nothing was posted. Try again in a moment."
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

// selectionValue is a radar checkbox's value: the item and the destination the row
// showed, so a write-back that changes the item's repository or number under the
// same ID is caught, never followed (pinnedTargets).
func selectionValue(it worklist.WorkItem) string {
	return url.Values{"id": {it.ID}, "repo": {it.GitHub.Repo}, "n": {strconv.Itoa(it.GitHub.Number)}}.Encode()
}

// parseSelections reads the checked rows, first per item. ok is false if any could
// not be read (a page from before rows carried their destination); the readable ones
// are still returned, to check again on the radar. At most MaxTargets+1 are kept:
// one more is enough to refuse the form.
func parseSelections(values []string) (pins []comment.Target, ok bool) {
	ok = true
	seen := map[string]bool{}
	for _, v := range values {
		q, err := url.ParseQuery(v)
		n, nerr := strconv.Atoi(q.Get("n"))
		id := q.Get("id")
		if err != nil || nerr != nil || id == "" {
			ok = false
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		if len(pins) > comment.MaxTargets {
			break
		}
		pins = append(pins, comment.Target{ID: id, Repo: q.Get("repo"), Number: n})
	}
	return pins, ok
}

// pinIDs is the item ids of pins, at most MaxTargets, to re-check on the radar.
func pinIDs(pins []comment.Target) []string {
	ids := make([]string, 0, len(pins))
	for i, p := range pins {
		if i == comment.MaxTargets {
			break
		}
		ids = append(ids, p.ID)
	}
	return ids
}

// pinnedTargets resolves the checked rows against the owner's stored items now. A
// row is kept only if its item is still on the radar AND still names the repository
// and number the row showed: a write-back may have changed an item's GitHub fields
// under the same ID, and the user checked the destination, not the ID.
func (h *Handler) pinnedTargets(ctx context.Context, ownerID string, pins []comment.Target) ([]comment.Target, []skippedTarget, error) {
	current, skipped, err := h.commentTargets(ctx, ownerID, pinIDs(pins))
	if err != nil {
		return nil, nil, err
	}
	now := make(map[string]comment.Target, len(current))
	for _, t := range current {
		now[t.ID] = t
	}
	var targets []comment.Target
	for _, p := range pins {
		t, ok := now[p.ID]
		switch {
		case !ok:
			// skipped by commentTargets already (gone, hidden, closed)
		case !strings.EqualFold(t.Repo, p.Repo) || t.Number != p.Number:
			skipped = append(skipped, skippedTarget{
				Label:  p.Repo + " #" + itoa(p.Number),
				Reason: "now points at " + t.Repo + " #" + itoa(t.Number) + "; not the item you checked",
			})
		default:
			targets = append(targets, t)
		}
	}
	return targets, skipped, nil
}

// draft is an unsent comment kept through reconnecting commenting.
type draft struct {
	body     string
	selected []string
	saved    time.Time
}

// draftTTL is how long an unsent comment is kept for the owner to reconnect.
const draftTTL = 2 * time.Hour

// saveDraft keeps the owner's unsent comment (the newest only), if it has any text
// or selection.
func (b *batches) saveDraft(owner, body string, selected []string, now time.Time) {
	if strings.TrimSpace(body) == "" && len(selected) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.drafts == nil {
		b.drafts = map[string]draft{}
	}
	for k, d := range b.drafts {
		if now.Sub(d.saved) > draftTTL {
			delete(b.drafts, k)
		}
	}
	for len(b.drafts) >= maxRecords {
		var oldest string
		for k, d := range b.drafts {
			if oldest == "" || d.saved.Before(b.drafts[oldest].saved) {
				oldest = k
			}
		}
		delete(b.drafts, oldest)
	}
	b.drafts[owner] = draft{body: body, selected: selected, saved: now}
}

// takeDraft removes and returns the owner's unsent comment, if one is kept.
func (b *batches) takeDraft(owner string, now time.Time) (draft, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	d, ok := b.drafts[owner]
	delete(b.drafts, owner)
	return d, ok && now.Sub(d.saved) <= draftTTL
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

func itoa(n int) string { return strconv.Itoa(n) }

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
