package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/ingest"
	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/worklist"
)

var _ RefreshProber = (*ingest.Ingestor)(nil)

// phaseDispatcher accepts every run and reports each in the phase it is set to.
type phaseDispatcher struct {
	mu    sync.Mutex
	phase string
	runs  int
}

func (d *phaseDispatcher) Submit(context.Context, ingest.RunSpec) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runs++
	return "run", nil
}

func (d *phaseDispatcher) Status(context.Context, string) (ingest.RunStatus, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return ingest.RunStatus{Phase: d.phase}, nil
}

func (d *phaseDispatcher) set(phase string) {
	d.mu.Lock()
	d.phase = phase
	d.mu.Unlock()
}

const reloadScript = `<script src="/static/reload.js" data-reload-secs="3" defer></script>`

// New work appears on an open worklist page with no click: a scheduled tick starts a
// refresh, the page renders with its self-reload while the run is in flight, and the
// reload after the run writes back shows the new item, then stops reloading.
func TestScheduledRefreshReachesAnOpenWorklist(t *testing.T) {
	const owner = "github:1"
	ctx := context.Background()
	store := worklist.NewMemoryStore()
	store.Seed(owner, worklist.WorkItem{ID: "github:o/r#1", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1, Title: "Old PR"}})
	d := &phaseDispatcher{phase: "Succeeded"}
	ig := ingest.New(d, "", "", nil, "", "")
	h := New(fakeSessions{&session.User{ID: owner}}, store, ig, nil, false, nil)
	page := func() string {
		rec := httptest.NewRecorder()
		h.Index(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		return rec.Body.String()
	}

	// Before any refresh the radar is static.
	if out := page(); strings.Contains(out, "reload.js") || strings.Contains(out, "http-equiv") {
		t.Fatalf("a radar with no refresh running must not reload itself, got:\n%s", out)
	}

	// A tick starts a refresh: the open page keeps its list and reloads itself.
	d.set("Running")
	r := ingest.NewRefresher(store, ig, nil, time.Hour, nil)
	if n := r.RefreshOnce(ctx); n != 1 {
		t.Fatalf("the tick dispatched %d runs, want 1", n)
	}
	out := page()
	if !strings.Contains(out, "Old PR") || !strings.Contains(out, reloadScript) ||
		!strings.Contains(out, "refreshing from GitHub") ||
		!strings.Contains(out, `<noscript><meta http-equiv="refresh" content="3"></noscript>`) {
		t.Fatalf("during the refresh the radar must stay visible and reload itself, got:\n%s", out)
	}

	// The run writes back new work and ends; the page's next reload shows it and stops.
	if err := store.Upsert(ctx, owner, worklist.WorkItem{ID: "github:o/r#2", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: "o/r", Number: 2, Title: "New PR"}}); err != nil {
		t.Fatal(err)
	}
	d.set("Succeeded")
	out = page()
	if !strings.Contains(out, "New PR") || !strings.Contains(out, "Old PR") {
		t.Fatalf("the reload after the refresh must show the new work, got:\n%s", out)
	}
	if strings.Contains(out, "reload.js") || strings.Contains(out, "refreshing from GitHub") {
		t.Fatalf("the radar must stop reloading once the refresh has ended, got:\n%s", out)
	}
	if d.runs != 1 {
		t.Fatalf("rendering the page dispatched runs: %d, want 1", d.runs)
	}
}

// A Refresh click reloads the radar the same way until its run lands, and a pipeline
// that cannot report a refresh leaves the radar static.
func TestWorklistReloadsDuringARefreshClick(t *testing.T) {
	const owner = "github:1"
	store := worklist.NewMemoryStore()
	store.Seed(owner, worklist.WorkItem{ID: "github:o/r#1", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1, Title: "A PR"}})
	ig := ingest.New(&phaseDispatcher{phase: "Running"}, "", "", nil, "", "")
	h := New(fakeSessions{&session.User{ID: owner}}, store, ig, nil, false, nil)
	h.Refresh(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/items/refresh", nil))
	rec := httptest.NewRecorder()
	h.Index(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(rec.Body.String(), reloadScript) {
		t.Fatalf("expected the radar to reload during a Refresh, got:\n%s", rec.Body.String())
	}

	static := New(fakeSessions{&session.User{ID: owner}}, store, &recordPipeline{}, nil, false, nil)
	rec = httptest.NewRecorder()
	static.Index(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if strings.Contains(rec.Body.String(), "reload.js") {
		t.Fatal("a pipeline without RefreshProber must leave the radar static")
	}
}

// The reload script is served, and the views that already poll keep their meta
// refresh rather than the worklist's script.
func TestReloadScriptServedAndOtherViewsUnchanged(t *testing.T) {
	h := New(fakeSessions{}, worklist.NewMemoryStore(), &recordPipeline{}, nil, false, nil)
	rec := httptest.NewRecorder()
	h.Static().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/reload.js", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "details[open]") {
		t.Fatalf("GET /static/reload.js = %d:\n%s", rec.Code, rec.Body.String())
	}
	out := renderPage(t, pageData{View: "processing", RefreshSecs: 3})
	if !strings.Contains(out, `<meta http-equiv="refresh" content="3">`) || strings.Contains(out, "reload.js") {
		t.Fatalf("the processing view keeps its meta refresh, got:\n%s", out)
	}
}
