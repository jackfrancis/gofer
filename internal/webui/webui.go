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
	"html/template"
	"math"
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

// Pipeline triggers a user's backfill when the worklist is empty, schedules an
// assistant turn for the Discuss action, and launches a concurrent batch of review
// turns for the "Review all PRs" action. It is the same Ingestor the JSON API uses.
type Pipeline interface {
	EnsureBackfill(ctx context.Context, ownerID string) error
	Converse(ctx context.Context, ownerID, itemID, endpoint, model string) error
	ReviewAll(ctx context.Context, ownerID string, itemIDs []string, endpoint, model string) error
	Refresh(ctx context.Context, ownerID string) error
	SecondOpinion(ctx context.Context, ownerID, itemID, endpoint, model string) error
	SecondOpinionAll(ctx context.Context, ownerID string, itemIDs []string, endpoint, model string) error
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
	return &Handler{
		tmpl:        tmpl,
		sessions:    sessions,
		store:       store,
		pipeline:    pipeline,
		providers:   providers,
		convEnabled: convEnabled,
		options:     options,
		now:         time.Now,
	}
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
	Item                     worklist.WorkItem // the single item, for the thread view
	ConvEnabled              bool              // whether the assistive conversation is available
	ModelOptions             []ModelOption     // conversation picker options (default first)
	SecondOpinionEnabled     bool              // whether a second-opinion model is configured
	SecondOpinionOptions     []ModelOption     // alternative options offered in the 2nd-opinion picker
	ReviewCount              int               // PRs needing a first review; gates/labels "Review all PRs"
	ReviewModelOptions       []ModelOption     // models for the "Review all PRs" dropdown; nil = plain button (<=1 model)
	SecondOpinionCount       int               // PRs reviewed once without a 2nd opinion; gates/labels "Get 2nd Opinion"
	SecondOpinionMode        string            // "" hidden | "menu" (pick a model) | "auto" (heterogeneous first reviews, immediate)
	SecondOpinionMenuOptions []ModelOption     // menu-mode models (every model except the single first-review model)
	ResetCount               int               // visible items carrying a conversation; gates/labels "Reset Conversations"
	RefreshSecs              int               // when > 0, the page auto-refreshes after this many seconds
	ReplyStalled             bool              // a pending reply has not arrived within reviewStaleAfter (thread view)
	Message                  string            // failure detail rendered on the error view
}

// Index handles GET /. It renders the sign-in view for anonymous visitors, the
// "Discovering" view while the first backfill populates an empty worklist, or the
// ranked worklist.
func (h *Handler) Index(w http.ResponseWriter, r *http.Request) {
	data, status := h.view(r)
	h.render(w, status, data)
}

// Thread handles GET /items/thread?id=<item id>. It renders the per-item
// assistive conversation for the signed-in owner. While the last turn is the
// user's (a reply is pending), the page auto-refreshes so the assistant's answer
// appears when the converse run lands.
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
			if n := len(it.Thread); n > 0 && it.Thread[n-1].Role == worklist.RoleUser {
				if h.now().UTC().Sub(it.Thread[n-1].At) < reviewStaleAfter {
					data.RefreshSecs = 3 // reply plausibly in-flight — poll for it
				} else {
					data.ReplyStalled = true // no reply within the window — show a retry hint, stop spinning
				}
			}
			h.render(w, http.StatusOK, data)
			return
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ThreadPost handles POST /items/thread. It appends the user's message to the
// item's thread and schedules an assistant turn, then redirects back to the thread
// (Post/Redirect/Get). SameSite=Lax cookies give baseline CSRF protection.
func (h *Handler) ThreadPost(w http.ResponseWriter, r *http.Request) {
	user := h.sessions.CurrentUser(r)
	if user == nil || !h.convEnabled {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	id := r.FormValue("id")
	content := strings.TrimSpace(r.FormValue("content"))
	if id == "" || content == "" {
		http.Redirect(w, r, "/items/thread?id="+url.QueryEscape(id), http.StatusSeeOther)
		return
	}
	// The thread's model picker selects which configured connection+model answers this
	// turn; an unknown or missing value falls back to the default.
	opt, _ := h.resolveOption(strings.TrimSpace(r.FormValue("choice")))
	items, err := h.store.List(r.Context(), user.ID)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	for _, it := range items {
		if it.ID == id {
			// Append the user's turn first, so the spawned converse runtime reads it
			// from the stored thread; the message never rides the run's environment.
			it.Thread = append(it.Thread, worklist.Message{Role: worklist.RoleUser, Content: content, At: h.now().UTC()})
			if err := h.store.Upsert(r.Context(), user.ID, it); err == nil {
				_ = h.pipeline.Converse(r.Context(), user.ID, id, opt.Endpoint, opt.Model)
			}
			break
		}
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
// thread and its read cursor on every one of the signed-in owner's items, leaving the
// work itself — the GitHub fields, the signals, and the gofer metadata — untouched,
// then redirects back to the radar (Post/Redirect/Get).
//
// This is a demo and development affordance: with the threads gone every PR is
// unreviewed again, so "Review all PRs" and the review panel can be exercised end to
// end without rebuilding the environment. It clears EVERY item the owner has, including
// ones hidden or completed on the radar, and the conversations are not recoverable.
// SameSite=Lax cookies give baseline CSRF protection.
func (h *Handler) ResetConversations(w http.ResponseWriter, r *http.Request) {
	user := h.sessions.CurrentUser(r)
	if user == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	items, err := h.store.List(r.Context(), user.ID)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	// Range yields a copy of each item, so dropping the Thread reference here cannot
	// race a concurrent reader of the store's backing array; the store persists the
	// copy under its own lock. Only items that actually carry conversation state are
	// written back.
	var cleared []worklist.WorkItem
	for _, it := range items {
		if len(it.Thread) == 0 && it.ThreadReadAt.IsZero() {
			continue
		}
		it.Thread = nil
		it.ThreadReadAt = time.Time{}
		cleared = append(cleared, it)
	}
	if len(cleared) > 0 {
		_ = h.store.Upsert(r.Context(), user.ID, cleared...)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// HideMessage handles POST /items/thread/hide. It sets or clears the hidden flag on
// one message of an item's thread (identified by its index), so the user can pare a
// long review thread down to the definitive turns. A hidden turn is also withheld from
// the model as future context (see agent.splitThread). It redirects back to the thread
// (Post/Redirect/Get). SameSite=Lax cookies give baseline CSRF protection.
func (h *Handler) HideMessage(w http.ResponseWriter, r *http.Request) {
	user := h.sessions.CurrentUser(r)
	if user == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	id := r.FormValue("id")
	idx, err := strconv.Atoi(r.FormValue("msg"))
	hidden := r.FormValue("hidden") == "true"
	if id == "" || err != nil || idx < 0 {
		http.Redirect(w, r, "/items/thread?id="+url.QueryEscape(id), http.StatusSeeOther)
		return
	}
	items, lerr := h.store.List(r.Context(), user.ID)
	if lerr != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	for _, it := range items {
		if it.ID == id {
			if idx < len(it.Thread) {
				// Clone the thread before mutating: List returns items whose Thread slice
				// still aliases the store's backing array, so an in-place write would race a
				// concurrent reader. The store persists the clone under its own lock.
				thread := make([]worklist.Message, len(it.Thread))
				copy(thread, it.Thread)
				thread[idx].Hidden = hidden
				it.Thread = thread
				_ = h.store.Upsert(r.Context(), user.ID, it)
			}
			break
		}
	}
	http.Redirect(w, r, "/items/thread?id="+url.QueryEscape(id), http.StatusSeeOther)
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
// genuinely slow run re-dispatched here just yields a duplicate reply, which is
// harmless.
const reviewStaleAfter = 5 * time.Minute

// ReviewAllPRs handles POST /items/review-all. For every pull request on the
// user's radar it appends the review prompt to the item's thread and, in one
// concurrent batch, schedules an assistant review turn for each — then redirects
// back to the radar (Post/Redirect/Get). A PR already awaiting a reply is skipped,
// so a double submit does not stack duplicate reviews.
func (h *Handler) ReviewAllPRs(w http.ResponseWriter, r *http.Request) {
	user := h.sessions.CurrentUser(r)
	if user == nil || !h.convEnabled {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	items, err := h.store.List(r.Context(), user.ID)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	at := h.now().UTC()
	var (
		changed []worklist.WorkItem
		ids     []string
	)
	for _, it := range items {
		if it.Type != worklist.TypePullRequest {
			continue
		}
		if it.HasReview() {
			continue // already reviewed at least once — a first-review is no longer needed
		}
		if n := len(it.Thread); n > 0 && it.Thread[n-1].Role == worklist.RoleUser {
			// A reply is pending. Skip it only while still plausibly in-flight; once the
			// pending prompt is older than reviewStaleAfter the prior run has finished or
			// failed, so retry it — reusing the existing prompt (just refresh its
			// timestamp) rather than stacking a second one. This is what makes the button
			// self-healing after a rate-limited batch.
			if at.Sub(it.Thread[n-1].At) < reviewStaleAfter {
				continue
			}
			it.Thread[n-1].At = at
		} else {
			it.Thread = append(it.Thread, worklist.Message{Role: worklist.RoleUser, Content: reviewPrompt, Kind: worklist.KindReviewRequest, At: at})
		}
		changed = append(changed, it)
		ids = append(ids, it.ID)
	}
	if len(changed) > 0 {
		// Persist every appended prompt first, so each spawned converse runtime reads
		// its request from the stored thread; then launch the runs concurrently with the
		// model picked from the button's dropdown (an empty choice uses the default).
		if err := h.store.Upsert(r.Context(), user.ID, changed...); err == nil {
			opt, _ := h.resolveOption(strings.TrimSpace(r.FormValue("choice")))
			_ = h.pipeline.ReviewAll(r.Context(), user.ID, ids, opt.Endpoint, opt.Model)
		}
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
	items, err := h.store.List(r.Context(), user.ID)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	for _, it := range items {
		if it.ID == id {
			// Append the independent-review prompt first, so the spawned converse runtime
			// reads it from the stored thread; then dispatch the blind review by the chosen
			// model. The consensus synthesis is chained by the ingestor after it lands.
			it.Thread = append(it.Thread, worklist.Message{Role: worklist.RoleUser, Content: independentReviewPrompt, Kind: worklist.KindReviewRequest, At: h.now().UTC()})
			if err := h.store.Upsert(r.Context(), user.ID, it); err == nil {
				_ = h.pipeline.SecondOpinion(r.Context(), user.ID, id, opt.Endpoint, opt.Model)
			}
			break
		}
	}
	http.Redirect(w, r, "/items/thread?id="+url.QueryEscape(id), http.StatusSeeOther)
}

// SecondOpinionAllPRs handles POST /items/second-opinion-all. For every pull request
// reviewed once but without a consensus synthesis yet, it appends an independent-review
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
	items, err := h.store.List(r.Context(), user.ID)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	now := h.now().UTC()
	var eligible []worklist.WorkItem
	firstModels := map[string]struct{}{}
	for _, it := range items {
		if it.NeedsSecondOpinion(now, reviewStaleAfter) {
			eligible = append(eligible, it)
			firstModels[it.FirstReviewModel()] = struct{}{}
		}
	}
	if len(eligible) == 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	// Homogeneous first reviews -> the user picked one model (already excluding the
	// first-review model); heterogeneous -> auto-pick a different model per PR below.
	// Resolve the menu choice before mutating, so a degenerate setup bails cleanly.
	heterogeneous := len(firstModels) > 1
	var menuOpt ModelOption
	if !heterogeneous {
		var ok bool
		if menuOpt, ok = h.resolveSecondOpinionChoice(strings.TrimSpace(r.FormValue("choice")), firstModels); !ok {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}

	// Append the independent-review prompt to each eligible PR, then persist so each
	// spawned converse runtime reads its request from the stored thread.
	var (
		changed []worklist.WorkItem
		ids     []string
	)
	for _, it := range eligible {
		it.Thread = append(it.Thread, worklist.Message{Role: worklist.RoleUser, Content: independentReviewPrompt, Kind: worklist.KindReviewRequest, At: now})
		changed = append(changed, it)
		ids = append(ids, it.ID)
	}
	if err := h.store.Upsert(r.Context(), user.ID, changed...); err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	if heterogeneous {
		// Per PR, dispatch a review by a model different from that PR's first review.
		for _, it := range eligible {
			if opt, ok := h.pickDifferentModel(it.FirstReviewModel()); ok {
				_ = h.pipeline.SecondOpinion(r.Context(), user.ID, it.ID, opt.Endpoint, opt.Model)
			}
		}
	} else {
		_ = h.pipeline.SecondOpinionAll(r.Context(), user.ID, ids, menuOpt.Endpoint, menuOpt.Model)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) view(r *http.Request) (pageData, int) {
	user := h.sessions.CurrentUser(r)
	if user == nil {
		return pageData{View: "signin", Providers: h.providers.Providers()}, http.StatusOK
	}

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
		var reviewCount, secondCount, resetCount int
		firstModels := map[string]struct{}{}
		for _, it := range res.Items {
			if it.NeedsReview(now, reviewStaleAfter) {
				reviewCount++
			}
			if it.NeedsSecondOpinion(now, reviewStaleAfter) {
				secondCount++
				firstModels[it.FirstReviewModel()] = struct{}{}
			}
			if len(it.Thread) > 0 {
				resetCount++
			}
		}
		data := pageData{
			View: "worklist", User: user, Items: res.Items, ConvEnabled: h.convEnabled,
			ReviewCount: reviewCount, SecondOpinionEnabled: h.secondOpinionEnabled(), SecondOpinionCount: secondCount,
			ResetCount: resetCount,
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

func (h *Handler) render(w http.ResponseWriter, status int, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = h.tmpl.ExecuteTemplate(w, "page.html", data)
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
