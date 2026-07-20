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

// Pipeline triggers a user's backfill when the worklist is empty and schedules an
// assistant turn for the Discuss action. It is the same Ingestor the JSON API
// uses.
type Pipeline interface {
	EnsureBackfill(ctx context.Context, ownerID string) error
	Converse(ctx context.Context, ownerID, itemID string) error
}

// Handler renders the landing page and serves its static assets.
type Handler struct {
	tmpl        *template.Template
	sessions    Sessions
	store       worklist.Store
	pipeline    Pipeline
	providers   Providers
	convEnabled bool
	now         func() time.Time
}

// New builds the UI handler. The embedded templates are parsed once; a parse
// failure is a build error in static assets, so it panics (fails fast).
// convEnabled gates the assistive conversation UI: with no chat model configured
// the Discuss affordances are hidden.
func New(sessions Sessions, store worklist.Store, pipeline Pipeline, providers Providers, convEnabled bool) *Handler {
	tmpl := template.Must(template.New("webui").Funcs(funcs).ParseFS(templatesFS, "templates/*.html"))
	return &Handler{
		tmpl:        tmpl,
		sessions:    sessions,
		store:       store,
		pipeline:    pipeline,
		providers:   providers,
		convEnabled: convEnabled,
		now:         time.Now,
	}
}

// Static serves the embedded assets at /static/.
func (h *Handler) Static() http.Handler {
	return http.FileServer(http.FS(staticFS))
}

type pageData struct {
	View        string // signin | processing | error | worklist | thread
	User        *session.User
	Providers   []string
	Items       []worklist.WorkItem
	Item        worklist.WorkItem // the single item, for the thread view
	ConvEnabled bool              // whether the assistive conversation is available
	RefreshSecs int               // when > 0, the page auto-refreshes after this many seconds
	Message     string            // failure detail rendered on the error view
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
			data := pageData{View: "thread", User: user, Item: it, ConvEnabled: h.convEnabled}
			if n := len(it.Thread); n > 0 && it.Thread[n-1].Role == worklist.RoleUser {
				data.RefreshSecs = 3 // an assistant reply is pending
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
				_ = h.pipeline.Converse(r.Context(), user.ID, id)
			}
			break
		}
	}
	http.Redirect(w, r, "/items/thread?id="+url.QueryEscape(id), http.StatusSeeOther)
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
		return pageData{View: "worklist", User: user, Items: res.Items, ConvEnabled: h.convEnabled}, http.StatusOK
	}
}

func (h *Handler) render(w http.ResponseWriter, status int, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = h.tmpl.ExecuteTemplate(w, "page.html", data)
}

var funcs = template.FuncMap{
	"title":         titleProvider,
	"priorityClass": priorityClass,
	"typeLabel":     typeLabel,
	"pctBucket":     pctBucket,
	"axis2":         axis2,
	"markdown":      markdown.ToSafeHTML,
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
