// Package webui renders the gofer landing page: the user's work, ranked by gofer
// metadata, in a server-rendered HTML page. It reads persisted data through the
// same worklist.Resolve read model the JSON API uses, so the page and the API
// never drift; it triggers no provider calls of its own. It also renders the
// per-item assistive conversation (the Discuss thread), whose assistant replies
// are sanitized Markdown.
package webui

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackfrancis/gofer/internal/markdown"
	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/worklist"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

// Sessions resolves the current interactive user from a request.
type Sessions interface {
	CurrentUser(r *http.Request) *session.User
}

// Providers lists the enabled auth providers for the sign-in view.
type Providers interface {
	Providers() []string
}

// Reconnector reports whether a signed-in user must sign in again before gofer can
// reach their provider: no stored credential, or one from a client gofer no longer
// uses (ADR 0007). auth.Handler implements it; with Providers that do not, the page
// never shows the prompt.
type Reconnector interface {
	SignInRequired(ctx context.Context, userID, provider string) bool
}

// Pipeline triggers a user's backfill when the worklist is empty, schedules an
// assistant turn for the Discuss action, and launches a concurrent batch of review
// turns for the "Review all PRs" action. It is the same Ingestor the JSON API uses.
type Pipeline interface {
	EnsureBackfill(ctx context.Context, ownerID string) error
	Converse(ctx context.Context, ownerID string, ref worklist.TurnRef, endpoint, model string) error
	ReviewAll(ctx context.Context, ownerID string, refs []worklist.TurnRef, endpoint, model string) error
	Refresh(ctx context.Context, ownerID string) error
	SecondOpinion(ctx context.Context, ownerID string, ref worklist.TurnRef, endpoint, model string) error
	SecondOpinionAll(ctx context.Context, ownerID string, refs []worklist.TurnRef, endpoint, model string) error
}

// ModelOption is one entry in the thread's model picker: an opaque Value that
// round-trips the (connection, model) selection through the form, a display Label, and
// the resolved Endpoint + Model the selected turn routes to. The web tier builds these
// from config.ModelChoices, so the picker shows bare model ids, disambiguated by
// connection only on a collision.
type ModelOption struct {
	Value    string // opaque form value identifying the (connection, model) choice
	Label    string // display text (bare model id, or "id (connection)" on collision)
	Endpoint string // the connection endpoint this choice routes to
	Model    string // the model id this choice selects
}

// Handler renders the landing page and serves its static assets.
type Handler struct {
	tmpl        *template.Template
	sessions    Sessions
	store       worklist.Store
	pipeline    Pipeline
	providers   Providers
	convEnabled bool
	options     []ModelOption // model picker options, default first (options[0]); nil when AI is disabled
	reconnect   Reconnector   // nil: never prompt to sign in again
	now         func() time.Time
}

// New builds the UI handler. The embedded templates are parsed once; a parse
// failure is a build error in static assets, so it panics (fails fast).
// convEnabled gates the assistive conversation UI: with no chat model configured
// the Discuss affordances are hidden. options are the model picker entries with the
// default first (options[0]); the thread offers a picker over them and, for any
// non-default option, a "2nd opinion" review.
func New(sessions Sessions, store worklist.Store, pipeline Pipeline, providers Providers, convEnabled bool, options []ModelOption) *Handler {
	tmpl := template.Must(template.New("webui").Funcs(funcs).ParseFS(templatesFS, "templates/*.html"))
	h := &Handler{
		tmpl:        tmpl,
		sessions:    sessions,
		store:       store,
		pipeline:    pipeline,
		providers:   providers,
		convEnabled: convEnabled,
		options:     options,
		now:         time.Now,
	}
	if rc, ok := providers.(Reconnector); ok {
		h.reconnect = rc
	}
	return h
}

// signInRequired reports whether the page should ask the user to sign in again.
func (h *Handler) signInRequired(r *http.Request, user *session.User) bool {
	return h.reconnect != nil && user != nil && user.Provider != "" &&
		h.reconnect.SignInRequired(r.Context(), user.ID, user.Provider)
}

// defaultOption returns the default picker option (the first configured); ok is false
// only when AI is disabled.
func (h *Handler) defaultOption() (ModelOption, bool) {
	if len(h.options) == 0 {
		return ModelOption{}, false
	}
	return h.options[0], true
}

// alternativeOptions returns the non-default options — the pool a "2nd opinion" review
// draws from. Nil for a single-option (or disabled) setup.
func (h *Handler) alternativeOptions() []ModelOption {
	if len(h.options) <= 1 {
		return nil
	}
	return h.options[1:]
}

// secondOpinionEnabled reports whether the thread offers a "2nd opinion" review — true
// when at least one alternative (non-default) option is configured.
func (h *Handler) secondOpinionEnabled() bool { return len(h.options) > 1 }

// resolveOption returns the option with the given value, or the default (first) option
// when the value is unknown or missing (so a tampered form value falls back safely).
// ok is false only when AI is disabled.
func (h *Handler) resolveOption(value string) (ModelOption, bool) {
	for _, o := range h.options {
		if o.Value == value {
			return o, true
		}
	}
	return h.defaultOption()
}

// resolveSecondOpinionOption is like resolveOption but over the non-default options,
// falling back to the first alternative.
func (h *Handler) resolveSecondOpinionOption(value string) (ModelOption, bool) {
	alts := h.alternativeOptions()
	for _, o := range alts {
		if o.Value == value {
			return o, true
		}
	}
	if len(alts) > 0 {
		return alts[0], true
	}
	return ModelOption{}, false
}

// pickDifferentModel returns the first configured option whose model id differs from
// used (a PR's first-review model), so a 2nd opinion is always by a different model. A
// used of "" (unknown first model) simply yields the default option. ok is false only
// when no configured model differs — a degenerate single-model setup.
func (h *Handler) pickDifferentModel(used string) (ModelOption, bool) {
	for _, o := range h.options {
		if o.Model != used {
			return o, true
		}
	}
	return ModelOption{}, false
}

// resolveSecondOpinionChoice resolves the model the user picked from the homogeneous
// 2nd-opinion menu to a configured option, guaranteeing it differs from the single
// first-review model in firstModels. A missing or tampered value falls back to the
// first different model. ok is false only when no different model exists.
func (h *Handler) resolveSecondOpinionChoice(value string, firstModels map[string]struct{}) (ModelOption, bool) {
	var used string
	for m := range firstModels { // len <= 1 in menu mode
		used = m
	}
	for _, o := range h.options {
		if o.Value == value && o.Model != used {
			return o, true
		}
	}
	return h.pickDifferentModel(used)
}

// secondOpinionMenu decides how the bulk "Get 2nd Opinion" button behaves given the
// set of first-review models across the eligible PRs: "auto" (engage immediately,
// per-PR auto-pick) when they are heterogeneous, otherwise "menu" listing every model
// except the single first-review model. It returns "" when no different model can be
// offered (a degenerate single-model setup).
func (h *Handler) secondOpinionMenu(firstModels map[string]struct{}) (mode string, opts []ModelOption) {
	if len(firstModels) > 1 {
		return "auto", nil
	}
	var used string
	for m := range firstModels {
		used = m
	}
	for _, o := range h.options {
		if o.Model == used { // exclude the first-review model (a no-op when used == "")
			continue
		}
		opts = append(opts, o)
	}
	if len(opts) == 0 {
		return "", nil
	}
	return "menu", opts
}

// Static serves the embedded assets at /static/.
func (h *Handler) Static() http.Handler {
	return http.FileServer(http.FS(staticFS))
}

type pageData struct {
	View                     string // signin | processing | error | worklist | thread
	User                     *session.User
	Providers                []string
	Items                    []worklist.WorkItem
	Item                     worklist.WorkItem  // the single item, for the thread view
	ConvEnabled              bool               // whether the assistive conversation is available
	ModelOptions             []ModelOption      // conversation picker options (default first)
	SecondOpinionEnabled     bool               // whether a second-opinion model is configured
	SecondOpinionOptions     []ModelOption      // alternative options offered in the 2nd-opinion picker
	ReviewCount              int                // PRs needing a first review; gates/labels "Review all PRs"
	ReviewModelOptions       []ModelOption      // models for the "Review all PRs" dropdown; nil = plain button (<=1 model)
	SecondOpinionCount       int                // PRs reviewed once without a 2nd opinion; gates/labels "Get 2nd Opinion"
	SecondOpinionMode        string             // "" hidden | "menu" (pick a model) | "auto" (heterogeneous first reviews, immediate)
	SecondOpinionMenuOptions []ModelOption      // menu-mode models (every model except the single first-review model)
	ResetCount               int                // stored items carrying a conversation (hidden and closed too: Reset clears them all); gates/labels "Reset Conversations"
	RefreshSecs              int                // when > 0, the page auto-refreshes after this many seconds
	ReplyStalled             bool               // a pending reply has not arrived within reviewStaleAfter (thread view)
	Message                  string             // failure detail rendered on the error view
	Failure                  string             // a failed user action, rendered on the error view in place of the discovery wording
	JustHidden               *worklist.WorkItem // the item the user just hid, offered for Undo on the radar
	RetryURL                 string             // where the error view's "Try again" goes; empty = the radar
	SignInRequired           bool               // gofer holds no usable credential: prompt the user to sign in again
	Projects                 []ProjectOption    // the project filter's checklist, by name (worklist view)
	ProjectsFiltered         bool               // some project is deselected: Items is a subset of the radar
	SelectedProjects         int                // projects shown, for the filter's label
	TotalItems               int                // items on the whole radar, before the project filter
	ProjectScope             string             // the shown selection, posted back by each bulk form ("" = every project)
	ProjectsError            string             // why the last project selection was not saved
}

// Index handles GET /. It renders the sign-in view for anonymous visitors, the
// "Discovering" view while the first backfill populates an empty worklist, or the
// ranked worklist.
func (h *Handler) Index(w http.ResponseWriter, r *http.Request) {
	data, status := h.view(r)
	h.render(w, status, data)
}

// Thread handles GET /items/thread?id=<item id>. It renders the per-item
// assistive conversation for the signed-in owner. While a user turn awaits its reply,
// the page auto-refreshes so the assistant's answer appears when the converse run
// lands. A GET whose page is delivered marks the newest reply shown as read
// (worklist.MarkThreadRead).
func (h *Handler) Thread(w http.ResponseWriter, r *http.Request) {
	user := h.sessions.CurrentUser(r)
	if user == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	id := r.URL.Query().Get("id")
	items, err := h.store.List(r.Context(), user.ID)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	for _, it := range items {
		if it.ID == id {
			data := pageData{View: "thread", User: user, Item: it, ConvEnabled: h.convEnabled, ModelOptions: h.options, SecondOpinionEnabled: h.secondOpinionEnabled(), SecondOpinionOptions: h.alternativeOptions()}
			data.SignInRequired = h.signInRequired(r, user)
			now := h.now().UTC()
			if it.ReplyPending(now, reviewStaleAfter) {
				data.RefreshSecs = 3 // a reply is plausibly in flight — poll for it
			} else if it.ReplyStalled(now, reviewStaleAfter) {
				data.ReplyStalled = true // no reply within the window — show a retry hint, stop spinning
			}
			// Delivering the thread is the read receipt (the UI has no script to send
			// one). The key is the newest reply on this page, taken from the snapshot
			// it was built from, so a reply that lands meanwhile stays unread.
			seen := it.LatestReplyKey()
			if err := h.render(w, http.StatusOK, data); err != nil || r.Method != http.MethodGet {
				return // a HEAD, or a page that never reached the browser, shows nothing
			}
			// Write only when the receipt can move the cursor: the page's auto-refresh
			// re-renders an already-read thread every few seconds. The cursor only moves
			// forward, so one already at seen on the snapshot is at or past it in the
			// store; MarkThreadRead re-checks inside its update. A failed write only
			// leaves the item unread.
			if snapshot := it; !snapshot.MarkRead(seen) {
				return
			}
			// net/http may still hold a short page in its buffer, to be sent when the
			// handler returns. Push it to the client first: a page that cannot be
			// flushed (the connection is gone, or the writer cannot flush) is not
			// known to be delivered, so it marks nothing.
			if err := http.NewResponseController(w).Flush(); err != nil {
				return
			}
			_ = worklist.MarkThreadRead(r.Context(), h.store, user.ID, it.ID, seen)
			return
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// maxMessageBytes bounds one user message in a thread, as ZZ did. Every turn is
// stored in the item and read back by each Discuss run, so an unbounded message
// would grow what a run must read without limit.
const maxMessageBytes = 16 << 10

// messageTooLong is the answer to an oversized message.
var messageTooLong = fmt.Sprintf("message too long: the limit is %d KiB", maxMessageBytes>>10)

// maxThreadForm bounds the whole POST /items/thread body: the message (URL-encoded,
// so up to three bytes per byte) plus the item ID and model choice.
const maxThreadForm = 3*maxMessageBytes + 4<<10

// parseThreadForm parses a URL-encoded or multipart (FormData) thread post, under
// the body limit the caller set. ParseForm alone would leave a multipart body
// unread, and FormValue would then not parse it either.
func parseThreadForm(r *http.Request) error {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "multipart/form-data" {
		return r.ParseMultipartForm(maxThreadForm)
	}
	return r.ParseForm()
}

// ThreadPost handles POST /items/thread. It appends the user's message to the
// item's thread and schedules an assistant turn, then redirects back to the thread
// (Post/Redirect/Get). SameSite=Lax cookies give baseline CSRF protection. A message
// over maxMessageBytes is refused with 413 and nothing is stored.
func (h *Handler) ThreadPost(w http.ResponseWriter, r *http.Request) {
	user := h.sessions.CurrentUser(r)
	if user == nil || !h.convEnabled {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxThreadForm)
	if err := parseThreadForm(r); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, messageTooLong, http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	id := r.FormValue("id")
	content := strings.TrimSpace(r.FormValue("content"))
	if len(content) > maxMessageBytes {
		http.Error(w, messageTooLong, http.StatusRequestEntityTooLarge)
		return
	}
	if id == "" || content == "" {
		http.Redirect(w, r, "/items/thread?id="+url.QueryEscape(id), http.StatusSeeOther)
		return
	}
	// The thread's model picker selects which configured connection+model answers this
	// turn; an unknown or missing value falls back to the default.
	opt, _ := h.resolveOption(strings.TrimSpace(r.FormValue("choice")))
	// Append the user's turn first, so the spawned converse runtime reads it from the
	// stored thread; the message never rides the run's environment. The append is
	// atomic, so a run's reply landing at the same moment is not lost.
	msg := worklist.Message{ID: worklist.NewMessageID(), Role: worklist.RoleUser, Content: content, At: h.now().UTC()}
	if h.appendTurn(r.Context(), user.ID, id, msg) {
		_ = h.pipeline.Converse(r.Context(), user.ID, worklist.TurnRef{Item: id, Turn: msg.ID}, opt.Endpoint, opt.Model)
	}
	http.Redirect(w, r, "/items/thread?id="+url.QueryEscape(id), http.StatusSeeOther)
}

// Refresh handles POST /items/refresh. It forces a re-ingest of the signed-in
// owner's worklist to pull in newly created or updated work, then redirects back to
// the radar (Post/Redirect/Get). The re-ingest reconciles, so nothing on the radar
// is lost. SameSite=Lax cookies give baseline CSRF protection.
func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	user := h.sessions.CurrentUser(r)
	if user == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	_ = h.pipeline.Refresh(r.Context(), user.ID)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ResetConversations handles POST /items/reset-conversations. It clears the Discuss
// thread and its read cursor on the signed-in owner's items (which ones: below), leaving the
// work itself — the GitHub fields, the signals, and the gofer metadata — untouched,
// then redirects back to the radar (Post/Redirect/Get).
//
// This is a demo and development affordance: with the threads gone every PR is
// unreviewed again, so "Review all PRs" and the review panel can be exercised end to
// end without rebuilding the environment. It clears every item in the projects the
// submitting page was filtered to (form "scope"; every project without one), including
// items hidden or completed on the radar — the page's count and tooltip say so. The
// conversations are not recoverable.
// SameSite=Lax cookies give baseline CSRF protection.
func (h *Handler) ResetConversations(w http.ResponseWriter, r *http.Request) {
	user := h.sessions.CurrentUser(r)
	if user == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	// One atomic update, so a run's write-back cannot restore a thread mid-clear. Only
	// items that actually carry conversation state are written back.
	selected, ok := formSelection(r)
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	_ = h.store.Update(r.Context(), user.ID, func(current []worklist.WorkItem) ([]worklist.WorkItem, error) {
		var cleared []worklist.WorkItem
		for _, it := range current {
			if !inScope(selected, it) || !hasConversation(it) {
				continue // outside the submitting page's projects, or nothing to clear
			}
			it.Thread = nil
			it.ThreadReadAt = time.Time{}
			it.ThreadReadThrough = ""
			cleared = append(cleared, it)
		}
		return cleared, nil
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// HideMessage handles POST /items/thread/hide. It sets or clears the hidden flag on
// one message of an item's thread (identified by its key, WorkItem.TurnKey), so the user can pare a
// long review thread down to the definitive turns. A hidden turn is also withheld from
// the model as future context (see agent.turnToAnswer). It redirects back to the thread
// (Post/Redirect/Get). SameSite=Lax cookies give baseline CSRF protection.
func (h *Handler) HideMessage(w http.ResponseWriter, r *http.Request) {
	user := h.sessions.CurrentUser(r)
	if user == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	id := r.FormValue("id")
	key := r.FormValue("msg")
	hidden := r.FormValue("hidden") == "true"
	if id == "" || key == "" {
		http.Redirect(w, r, "/items/thread?id="+url.QueryEscape(id), http.StatusSeeOther)
		return
	}
	// The turn is named by its key, not its index, so a page rendered before newer
	// turns landed still hides the turn it showed. A turn that is gone (the thread was
	// cleared) is a no-op.
	_ = h.store.Update(r.Context(), user.ID, func(current []worklist.WorkItem) ([]worklist.WorkItem, error) {
		for i := range current {
			if current[i].ID != id {
				continue
			}
			j, ok := current[i].TurnIndex(key)
			if !ok {
				return nil, nil
			}
			current[i].Thread[j].Hidden = hidden
			return current[i : i+1], nil
		}
		return nil, nil
	})
	http.Redirect(w, r, "/items/thread?id="+url.QueryEscape(id), http.StatusSeeOther)
}

// HideItem handles POST /items/hide. It hides one whole item from the owner's radar
// ("hidden", default true) or brings it back ("hidden=false"). A hidden item stays in
// the store with its thread, is skipped by every bulk action, and resurfaces on its
// own once a refresh brings a GitHub update newer than the hide
// (worklist.HiddenAfter). Hiding a hidden item, or unhiding a visible one, changes
// nothing. After a hide it redirects to the radar with an Undo for that item; after
// an unhide, to the radar, or back to the item's thread when the form says so
// ("back=thread"). A failed write renders an error rather than redirecting as if it
// had worked. SameSite=Lax cookies give baseline CSRF protection.
func (h *Handler) HideItem(w http.ResponseWriter, r *http.Request) {
	user := h.sessions.CurrentUser(r)
	if user == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	id := r.FormValue("id")
	hidden := r.FormValue("hidden") != "false"
	if id == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	at := h.now().UTC()
	err := h.store.Update(r.Context(), user.ID, func(current []worklist.WorkItem) ([]worklist.WorkItem, error) {
		for i := range current {
			if current[i].ID != id {
				continue
			}
			switch {
			case hidden && current[i].Meta.HiddenAt.IsZero():
				current[i].Meta.HiddenAt = at
			case !hidden && !current[i].Meta.HiddenAt.IsZero():
				current[i].Meta.HiddenAt = time.Time{}
			default:
				// Already in the asked-for state. A repeated Hide (a stale tab, a double
				// submit) must keep the original hide time: moving it later would swallow
				// GitHub activity that landed in between.
				return nil, nil
			}
			return current[i : i+1], nil
		}
		return nil, nil // item gone
	})
	if err != nil {
		verb := "hide"
		if !hidden {
			verb = "unhide"
		}
		// "Try again" returns to a page that still offers the same action: the radar row
		// (hide), the thread's Unhide (unhide from the thread), or the radar's Undo for
		// the item, which is still hidden (unhide from the radar).
		retry := "/"
		switch {
		case !hidden && r.FormValue("back") == "thread":
			retry = "/items/thread?id=" + url.QueryEscape(id)
		case !hidden:
			retry = "/?hidden=" + url.QueryEscape(id)
		}
		h.render(w, http.StatusInternalServerError, pageData{View: "error", User: user, RetryURL: retry,
			Failure: "gofer couldn't " + verb + " that item. Nothing was changed; try again."})
		return
	}
	switch {
	case !hidden && r.FormValue("back") == "thread":
		http.Redirect(w, r, "/items/thread?id="+url.QueryEscape(id), http.StatusSeeOther)
	case hidden:
		// The radar offers Undo for the item just hidden (signedInView), since its row,
		// and with it the way to its thread's Unhide, is gone.
		http.Redirect(w, r, "/?hidden="+url.QueryEscape(id), http.StatusSeeOther)
	default:
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// reviewPrompt is the message posted to every pull request's thread by the
// "Review all PRs" action. Kept deliberately simple; the converse runtime gathers
// the PR's diff and discussion as source context before answering.
const reviewPrompt = "Can you review this PR?"

// independentReviewPrompt is the message posted by the "Review panel" action to the
// alternative model. It asks for a thorough, blind, independent assessment — the
// runtime answers without the prior thread — so the model forms its own view before
// the default model's consensus synthesis weighs every review.
const independentReviewPrompt = "Give an independent review of this PR. There may be other humans actively reviewing the PR. Your value is to thoroughly review in detail, especially areas of ambiguity, and to make a practical assessment of the PR quality's readiness for acceptance, or if not, some concrete suggestions for next steps."

// reviewStaleAfter is how long a pending user turn (a review awaiting its reply) is
// treated as plausibly in-flight. Past it, the converse run has almost certainly
// finished or failed (a rate-limited converse dies fast), so the thread stops
// spinning and shows a retry hint, and "Review all PRs" re-dispatches the item —
// making the button self-healing. It is a heuristic, not run-status tracking: a
// genuinely slow run re-dispatched here may still reply, and the retry run then finds
// the turn answered and does nothing (or, if both were already under way, the turn
// gets two replies, which is harmless).
const reviewStaleAfter = 5 * time.Minute

// ReviewAllPRs handles POST /items/review-all. For every pull request the submitting
// page showed (on the radar, in its projects: form "scope"), it appends the review prompt to the item's thread and, in one
// concurrent batch, schedules an assistant review turn for each — then redirects
// back to the radar (Post/Redirect/Get). A PR already awaiting a reply is skipped,
// so a double submit does not stack duplicate reviews.
func (h *Handler) ReviewAllPRs(w http.ResponseWriter, r *http.Request) {
	user := h.sessions.CurrentUser(r)
	if user == nil || !h.convEnabled {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	at := h.now().UTC()
	var refs []worklist.TurnRef
	selected, ok := formSelection(r)
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	// Choose and stamp the PRs in one atomic update, against the threads as stored.
	err := h.store.Update(r.Context(), user.ID, func(current []worklist.WorkItem) ([]worklist.WorkItem, error) {
		var changed []worklist.WorkItem
		refs = refs[:0]
		for _, it := range current {
			if it.Type != worklist.TypePullRequest || !it.OnRadar() || !inScope(selected, it) {
				continue // off the radar (hidden or closed) or outside the page's projects: never review it in bulk
			}
			if it.HasReview() {
				continue // already reviewed at least once — a first-review is no longer needed
			}
			if it.ReplyPending(at, reviewStaleAfter) {
				continue // a reply is plausibly in flight
			}
			var turn string
			if it.ReplyStalled(at, reviewStaleAfter) {
				// The last request's run has finished or failed, so retry it — reusing the
				// prompt (refresh its timestamp) rather than stacking a second one. This is
				// what makes the button self-healing after a rate-limited batch.
				i, _ := it.LastUserTurn()
				it.Thread[i].At = at
				turn = it.TurnKey(i)
			} else {
				msg := worklist.Message{ID: worklist.NewMessageID(), Role: worklist.RoleUser, Content: reviewPrompt, Kind: worklist.KindReviewRequest, At: at}
				it.Thread = append(it.Thread, msg)
				turn = msg.ID
			}
			changed = append(changed, it)
			refs = append(refs, worklist.TurnRef{Item: it.ID, Turn: turn})
		}
		return changed, nil
	})
	if err == nil && len(refs) > 0 {
		// Every prompt is persisted first, so each spawned converse runtime reads its
		// request from the stored thread; then launch the runs concurrently with the
		// model picked from the button's dropdown (an empty choice uses the default).
		opt, _ := h.resolveOption(strings.TrimSpace(r.FormValue("choice")))
		_ = h.pipeline.ReviewAll(r.Context(), user.ID, refs, opt.Endpoint, opt.Model)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// SecondOpinion handles POST /items/second-opinion — the "Review panel" action. It
// appends an independent-review prompt to the item's thread and dispatches a blind
// review by the selected alternative model; once that review lands, the ingestor
// chains a consensus synthesis by the default model onto the same thread. It then
// redirects to the item's thread (Post/Redirect/Get) where both replies appear
// attributed to their models. SameSite=Lax cookies give baseline CSRF protection.
func (h *Handler) SecondOpinion(w http.ResponseWriter, r *http.Request) {
	user := h.sessions.CurrentUser(r)
	if user == nil || !h.secondOpinionEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	id := r.FormValue("id")
	opt, ok := h.resolveSecondOpinionOption(strings.TrimSpace(r.FormValue("choice")))
	if id == "" || !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	// Append the independent-review prompt first (atomically), so the spawned converse
	// runtime reads it from the stored thread; then dispatch the blind review by the
	// chosen model. The consensus synthesis is chained by the ingestor after it lands.
	msg := worklist.Message{ID: worklist.NewMessageID(), Role: worklist.RoleUser, Content: independentReviewPrompt, Kind: worklist.KindReviewRequest, At: h.now().UTC()}
	if h.appendTurn(r.Context(), user.ID, id, msg) {
		_ = h.pipeline.SecondOpinion(r.Context(), user.ID, worklist.TurnRef{Item: id, Turn: msg.ID}, opt.Endpoint, opt.Model)
	}
	http.Redirect(w, r, "/items/thread?id="+url.QueryEscape(id), http.StatusSeeOther)
}

// SecondOpinionAllPRs handles POST /items/second-opinion-all. For every pull request
// the submitting page showed (on the radar, in its projects: form "scope") reviewed once but without a consensus synthesis yet, it appends an independent-review
// prompt and runs the review panel (blind review + chained synthesis) by a model that
// differs from that PR's first review, then redirects back to the radar (Post/Redirect/
// Get). When the eligible PRs' first reviews are homogeneous the user picks the model
// (form "choice"); when they are heterogeneous a different model is auto-picked per PR.
// It requires an alternative model to be configured.
func (h *Handler) SecondOpinionAllPRs(w http.ResponseWriter, r *http.Request) {
	user := h.sessions.CurrentUser(r)
	if user == nil || !h.secondOpinionEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	selected, ok := formSelection(r)
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	now := h.now().UTC()
	choice := strings.TrimSpace(r.FormValue("choice"))
	type dispatch struct {
		ref worklist.TurnRef
		opt ModelOption
	}
	var (
		plan  []dispatch
		batch bool // every PR gets the same model: dispatch them as one batch
	)
	// Decide which PRs are eligible AND which model reviews each one against the same
	// snapshot, in one atomic update, so a PR whose first review lands meanwhile
	// cannot be swept into a batch whose model was chosen without it.
	err := h.store.Update(r.Context(), user.ID, func(current []worklist.WorkItem) ([]worklist.WorkItem, error) {
		plan = plan[:0]
		var eligible []worklist.WorkItem
		firstModels := map[string]struct{}{}
		for _, it := range current {
			if it.OnRadar() && inScope(selected, it) && it.NeedsSecondOpinion(now, reviewStaleAfter) {
				eligible = append(eligible, it)
				firstModels[it.FirstReviewModel()] = struct{}{}
			}
		}
		if len(eligible) == 0 {
			return nil, nil
		}
		// Homogeneous first reviews -> the user's pick from the menu (resolved to a model
		// that differs from the shared first-review model); heterogeneous -> a different
		// model auto-picked per PR.
		heterogeneous := len(firstModels) > 1
		var menuOpt ModelOption
		if !heterogeneous {
			var ok bool
			if menuOpt, ok = h.resolveSecondOpinionChoice(choice, firstModels); !ok {
				return nil, nil // no different model exists: a degenerate setup
			}
		}
		batch = !heterogeneous
		var stamped []worklist.WorkItem
		for _, it := range eligible {
			opt := menuOpt
			if heterogeneous {
				var ok bool
				if opt, ok = h.pickDifferentModel(it.FirstReviewModel()); !ok {
					continue // never stamp a prompt no run will answer
				}
			}
			msg := worklist.Message{ID: worklist.NewMessageID(), Role: worklist.RoleUser, Content: independentReviewPrompt, Kind: worklist.KindReviewRequest, At: now}
			it.Thread = append(it.Thread, msg)
			stamped = append(stamped, it)
			plan = append(plan, dispatch{ref: worklist.TurnRef{Item: it.ID, Turn: msg.ID}, opt: opt})
		}
		return stamped, nil
	})
	if err != nil || len(plan) == 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	// Dispatch outside the update, each prompt already persisted for its run to read.
	if batch {
		refs := make([]worklist.TurnRef, len(plan))
		for i, d := range plan {
			refs[i] = d.ref
		}
		_ = h.pipeline.SecondOpinionAll(r.Context(), user.ID, refs, plan[0].opt.Endpoint, plan[0].opt.Model)
	} else {
		for _, d := range plan {
			_ = h.pipeline.SecondOpinion(r.Context(), user.ID, d.ref, d.opt.Endpoint, d.opt.Model)
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// appendTurn appends a new user turn to ownerID's item id in one atomic update,
// reporting whether the item was found.
func (h *Handler) appendTurn(ctx context.Context, ownerID, id string, msg worklist.Message) bool {
	found := false
	err := h.store.Update(ctx, ownerID, func(current []worklist.WorkItem) ([]worklist.WorkItem, error) {
		for i := range current {
			if current[i].ID == id {
				current[i].Thread = append(current[i].Thread, msg)
				found = true
				return current[i : i+1], nil
			}
		}
		return nil, nil
	})
	return err == nil && found
}

func (h *Handler) view(r *http.Request) (pageData, int) {
	user := h.sessions.CurrentUser(r)
	if user == nil {
		return pageData{View: "signin", Providers: h.providers.Providers()}, http.StatusOK
	}
	data, status := h.signedInView(r, user)
	data.SignInRequired = h.signInRequired(r, user)
	return data, status
}

// signedInView renders the views for a signed-in user.
func (h *Handler) signedInView(r *http.Request, user *session.User) (pageData, int) {

	res, err := worklist.Resolve(r.Context(), h.store, h.pipeline, h.now(), user.ID, worklist.DefaultSort, true)
	if err != nil {
		return pageData{View: "error", User: user}, http.StatusBadGateway
	}
	switch res.Status {
	case worklist.StatusProcessing:
		return pageData{View: "processing", User: user, RefreshSecs: 3}, http.StatusOK
	case worklist.StatusFailed:
		// A failed backfill is a stable, explained state: show the reason and do not
		// auto-refresh — the user retries (or an operator fixes the substrate) and reloads.
		return pageData{View: "error", User: user, Message: res.Message}, http.StatusOK
	default:
		now := h.now()
		// The project filter narrows what is shown. The bulk actions' counts below, and
		// the scope each bulk form posts back (ProjectScope), cover only the selected
		// projects the checklist names, so an action acts on what this page shows.
		selected := savedSelection(r, user.ID)
		projects := projectOptions(res.Items, selected)
		shown := filterProjects(res.Items, selected)
		scope := shownSelection(selected, projects)
		selectedCount := 0
		for _, p := range projects {
			if p.Selected {
				selectedCount++
			}
		}
		var reviewCount, secondCount, resetCount int
		firstModels := map[string]struct{}{}
		for _, it := range shown {
			if it.NeedsReview(now, reviewStaleAfter) {
				reviewCount++
			}
			if it.NeedsSecondOpinion(now, reviewStaleAfter) {
				secondCount++
				firstModels[it.FirstReviewModel()] = struct{}{}
			}
			if hasConversation(it) {
				resetCount++
			}
		}
		// The radar lists only items on it, but Reset clears every stored thread in the
		// selected projects the checklist names, hidden and closed items' included; and the item just hidden is
		// off the radar, to be offered for Undo. Both come from Resolve's same read
		// (res.OffRadar), so the page never offers Hide and Undo for one item.
		var justHidden *worklist.WorkItem
		hiddenID := r.URL.Query().Get("hidden")
		for i, it := range res.OffRadar {
			if inScope(scope, it) && hasConversation(it) {
				resetCount++
			}
			if hiddenID != "" && it.ID == hiddenID && !it.Meta.HiddenAt.IsZero() {
				justHidden = &res.OffRadar[i]
			}
		}
		data := pageData{
			View: "worklist", User: user, Items: shown, ConvEnabled: h.convEnabled,
			ReviewCount: reviewCount, SecondOpinionEnabled: h.secondOpinionEnabled(), SecondOpinionCount: secondCount,
			ResetCount: resetCount, JustHidden: justHidden,
			Projects: projects, ProjectsFiltered: selected != nil, SelectedProjects: selectedCount, TotalItems: len(res.Items),
		}
		if scope != nil {
			data.ProjectScope = encodeSelection(scope)
		}
		if r.URL.Query().Get("projects") == projectsTooMany {
			data.ProjectsError = "Too many projects selected to save. Choose fewer, or Select all; your previous filter is unchanged."
		}
		// "Review all PRs" offers every model when more than one is configured; a
		// single-model setup keeps the plain button.
		if h.convEnabled && len(h.options) > 1 {
			data.ReviewModelOptions = h.options
		}
		if data.SecondOpinionEnabled && secondCount > 0 {
			data.SecondOpinionMode, data.SecondOpinionMenuOptions = h.secondOpinionMenu(firstModels)
		}
		return data, http.StatusOK
	}
}

// render writes the page, returning the template's error, which includes a failed
// write to the client.
func (h *Handler) render(w http.ResponseWriter, status int, data pageData) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	return h.tmpl.ExecuteTemplate(w, "page.html", data)
}

var funcs = template.FuncMap{
	"title":                  titleProvider,
	"priorityClass":          priorityClass,
	"typeLabel":              typeLabel,
	"pctBucket":              pctBucket,
	"axis2":                  axis2,
	"markdown":               markdown.ToSafeHTML,
	"discussBadge":           discussBadge,
	"discussTitle":           discussTitle,
	"isSecondOpinionRequest": isSecondOpinionRequest,
	"isSynthesisRequest":     isSynthesisRequest,
	"userHandle":             userHandle,
}

// hasConversation reports whether the item carries conversation state that Reset
// Conversations clears: a thread, or a read cursor.
func hasConversation(it worklist.WorkItem) bool {
	return len(it.Thread) > 0 || !it.ThreadReadAt.IsZero() || it.ThreadReadThrough != ""
}

// isSecondOpinionRequest reports whether a thread turn is the programmatic
// independent-review request the "2nd opinion" action posts. The UI renders it as a
// small system notice rather than a chat bubble, since the user never typed it.
func isSecondOpinionRequest(m worklist.Message) bool {
	return m.Role == worklist.RoleUser && m.Content == independentReviewPrompt
}

// isSynthesisRequest reports whether a thread turn is the programmatic consensus
// prompt the review panel chains after the independent review. The UI hides it
// entirely — only its verdict reply is worth showing.
func isSynthesisRequest(m worklist.Message) bool {
	return m.Role == worklist.RoleUser && m.Kind == worklist.KindSynthesisRequest
}

// userHandle renders a person's @-handle for a system notice, preferring the provider
// login (e.g. GitHub) and falling back to the display name.
func userHandle(u *session.User) string {
	switch {
	case u == nil:
		return "Someone"
	case u.Login != "":
		return "@" + u.Login
	case u.Name != "":
		return u.Name
	default:
		return "Someone"
	}
}

// discussBadge returns the emoji cue shown before a "Discuss" link: 🤝 or 🎭 once a
// consensus synthesis has ruled the reviews in agreement or disagreement, ✨ when the
// item merely has an active thread, or "" when it has none.
func discussBadge(w worklist.WorkItem) string {
	if w.HasSecondOpinion() {
		switch w.ReviewVerdict() {
		case worklist.VerdictAgree:
			return "🤝"
		case worklist.VerdictDisagree:
			return "🎭"
		}
		return "✨"
	}
	if len(w.Thread) > 0 {
		return "✨"
	}
	return ""
}

// discussTitle is the hover text for a "Discuss" link: an unread cue takes precedence,
// then a consensus verdict; otherwise none.
func discussTitle(w worklist.WorkItem) string {
	if w.HasUnreadReply() {
		return "New assistant reply to read"
	}
	if w.HasSecondOpinion() {
		switch w.ReviewVerdict() {
		case worklist.VerdictAgree:
			return "The reviews agree"
		case worklist.VerdictDisagree:
			return "The reviews disagree"
		}
	}
	return ""
}

// pctBucket rounds an axis value (0..1) to the nearest 10% so the rank bar can use
// a static width class instead of an inline style (keeps CSP tight).
func pctBucket(f float64) int {
	b := int(math.Round(f*10)) * 10
	switch {
	case b < 0:
		return 0
	case b > 100:
		return 100
	default:
		return b
	}
}

func axis2(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) }

func priorityClass(p worklist.Priority) string {
	switch p {
	case worklist.PriorityHigh:
		return "Label--danger"
	case worklist.PriorityMedium:
		return "Label--attention"
	default:
		return "Label--secondary"
	}
}

func typeLabel(t worklist.ItemType) string {
	if t == worklist.TypePullRequest {
		return "PR"
	}
	return "Issue"
}

func titleProvider(s string) string {
	switch s {
	case "github":
		return "GitHub"
	case "google":
		return "Google"
	case "microsoft":
		return "Microsoft"
	default:
		if s == "" {
			return s
		}
		return strings.ToUpper(s[:1]) + s[1:]
	}
}
