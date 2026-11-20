package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/ingest"
	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/storage"
	"github.com/jackfrancis/gofer/internal/worklist"
)

var _ RefreshProber = (*ingest.Ingestor)(nil)

// phaseDispatcher accepts every run and reports each in the phase it is set to, or
// fails status reads while statusErr is set.
type phaseDispatcher struct {
	mu        sync.Mutex
	phase     string
	statusErr bool
	runs      int
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
	if d.statusErr {
		return ingest.RunStatus{}, errors.New("aei: status unavailable")
	}
	return ingest.RunStatus{Phase: d.phase}, nil
}

func (d *phaseDispatcher) set(phase string) {
	d.mu.Lock()
	d.phase = phase
	d.mu.Unlock()
}

func (d *phaseDispatcher) failStatus(fail bool) {
	d.mu.Lock()
	d.statusErr = fail
	d.mu.Unlock()
}

const owner = "github:1"

func pr(n int, title string) worklist.WorkItem {
	return worklist.WorkItem{ID: "github:o/r#" + string(rune('0'+n)), Type: worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Repo: "o/r", Number: n, Title: title}}
}

// harness is a radar over a real Ingestor and a controllable backend.
type harness struct {
	t     *testing.T
	store worklist.Store
	d     *phaseDispatcher
	ig    *ingest.Ingestor
	h     *Handler
}

func newHarness(t *testing.T, store worklist.Store) *harness {
	d := &phaseDispatcher{phase: "Succeeded"}
	ig := ingest.New(d, "", "", nil, "", "")
	return &harness{t: t, store: store, d: d, ig: ig,
		h: New(fakeSessions{&session.User{ID: owner}}, store, ig, nil, false, nil)}
}

func (x *harness) page() string {
	rec := httptest.NewRecorder()
	x.h.Index(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	return rec.Body.String()
}

var watcherTag = regexp.MustCompile(`<script src="/static/reload.js" data-done="(\d+)" data-active="([01])" data-poll-secs="3" data-idle-poll-secs="30" defer></script>`)

// watcher is the refresh state a rendered page carries for reload.js.
type watcher struct{ done, active string }

func (x *harness) watcherOf(page string) watcher {
	x.t.Helper()
	m := watcherTag.FindStringSubmatch(page)
	if m == nil {
		x.t.Fatalf("the radar has no refresh watcher:\n%s", page)
	}
	return watcher{m[1], m[2]}
}

type status struct{ Active, Reload bool }

// poll asks what reload.js asks, with the state the page was rendered with.
func (x *harness) poll(w watcher) status {
	x.t.Helper()
	rec := httptest.NewRecorder()
	x.h.RefreshStatus(rec, httptest.NewRequest(http.MethodGet,
		"/items/refresh-status?done="+url.QueryEscape(w.done), nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		x.t.Fatalf("refresh-status: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	var s status
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		x.t.Fatal(err)
	}
	return s
}

// A worklist opened while nothing is running notices a scheduled refresh that starts
// and ends between two of its polls, and its reload shows the new work.
func TestIdleWorklistDiscoversAScheduledRefresh(t *testing.T) {
	ctx := context.Background()
	mem := worklist.NewMemoryStore()
	mem.Seed(owner, pr(1, "Old PR"))
	x := newHarness(t, mem)

	first := x.page()
	w := x.watcherOf(first)
	if w.active != "0" || strings.Contains(first, "http-equiv") || !strings.Contains(first, `<span data-refreshing role="status" hidden>`) {
		t.Fatalf("an idle radar renders idle, got:\n%s", first)
	}
	if s := x.poll(w); s.Reload || s.Active {
		t.Fatalf("nothing ran: poll = %+v", s)
	}

	// Between two polls, a tick starts a refresh, it writes new work, and it ends.
	x.d.set("Running")
	if n := ingest.NewRefresher(mem, x.ig, nil, time.Hour, nil).RefreshOnce(ctx); n != 1 {
		t.Fatalf("the tick dispatched %d runs", n)
	}
	if err := mem.Upsert(ctx, owner, pr(2, "New PR")); err != nil {
		t.Fatal(err)
	}
	x.d.set("Succeeded")

	if s := x.poll(w); !s.Reload {
		t.Fatalf("the page missed a refresh that ran between polls: %+v", s)
	}
	next := x.page()
	if !strings.Contains(next, "New PR") || x.watcherOf(next).active != "0" {
		t.Fatalf("the reload must show the new work, idle, got:\n%s", next)
	}
	if s := x.poll(x.watcherOf(next)); s.Reload {
		t.Fatal("the reloaded page must not reload again")
	}
	if x.d.runs != 1 {
		t.Fatalf("rendering and polling dispatched runs: %d, want 1", x.d.runs)
	}
}

// A page open while a refresh runs polls often, shows that it is refreshing, and
// reloads once the run has ended, not before.
func TestWorklistDuringARefresh(t *testing.T) {
	ctx := context.Background()
	mem := worklist.NewMemoryStore()
	mem.Seed(owner, pr(1, "Old PR"))
	x := newHarness(t, mem)
	w0 := x.watcherOf(x.page())

	x.d.set("Running")
	x.h.Refresh(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/items/refresh", nil))
	if s := x.poll(w0); s.Reload || !s.Active {
		t.Fatalf("an idle page must see the click's run start, without reloading yet: %+v", s)
	}
	out := x.page()
	w := x.watcherOf(out)
	if w.active != "1" || !strings.Contains(out, "Old PR") ||
		!strings.Contains(out, `<span data-refreshing role="status"> · refreshing from GitHub…</span>`) ||
		!strings.Contains(out, `<noscript><meta http-equiv="refresh" content="3"></noscript>`) {
		t.Fatalf("during a refresh the radar stays visible and says so, got:\n%s", out)
	}
	if s := x.poll(w); s.Reload || !s.Active {
		t.Fatalf("still running: poll = %+v", s)
	}

	if err := mem.Upsert(ctx, owner, pr(2, "New PR")); err != nil {
		t.Fatal(err)
	}
	x.d.set("Succeeded")
	if s := x.poll(w); !s.Reload || s.Active {
		t.Fatalf("the run ended: poll = %+v", s)
	}
	if out := x.page(); !strings.Contains(out, "New PR") || x.watcherOf(out).active != "0" {
		t.Fatalf("the reload shows the new work and is idle, got:\n%s", out)
	}
}

// lateStore lets a refresh's final write land, and the run end, just after the radar
// has read its items: the race where a page built from the old items would stop
// watching.
type lateStore struct {
	worklist.Store
	once  sync.Once
	write func()
}

func (s *lateStore) List(ctx context.Context, ownerID string) ([]worklist.WorkItem, error) {
	items, err := s.Store.List(ctx, ownerID)
	s.once.Do(s.write)
	return items, err
}

// The page reads the refresh state before its items, so a final write that lands
// after the items were read leaves the page watching, and its next poll reloads.
func TestFinalWriteAfterTheSnapshotStillReloads(t *testing.T) {
	ctx := context.Background()
	mem := worklist.NewMemoryStore()
	mem.Seed(owner, pr(1, "Old PR"))
	store := &lateStore{Store: mem}
	x := newHarness(t, store)
	x.d.set("Running")
	if err := x.ig.Refresh(ctx, owner); err != nil {
		t.Fatal(err)
	}
	store.write = func() {
		_ = mem.Upsert(ctx, owner, pr(2, "New PR"))
		x.d.set("Succeeded")
	}

	out := x.page()
	if strings.Contains(out, "New PR") {
		t.Fatal("the test means the write to land after the snapshot")
	}
	w := x.watcherOf(out)
	if w.active != "1" {
		t.Fatalf("a page built while the run was in flight must keep watching, got active=%s", w.active)
	}
	if s := x.poll(w); !s.Reload {
		t.Fatalf("the next poll must reload: %+v", s)
	}
	if !strings.Contains(x.page(), "New PR") {
		t.Fatal("the reload shows the final write")
	}
}

// A failed status read does not stop the page watching: the run counts as in flight
// until its status is known again (or its deadline passes, see ingest.TestActive).
func TestTransientStatusErrorKeepsWatching(t *testing.T) {
	ctx := context.Background()
	mem := worklist.NewMemoryStore()
	mem.Seed(owner, pr(1, "Old PR"))
	x := newHarness(t, mem)
	x.d.set("Running")
	if err := x.ig.Refresh(ctx, owner); err != nil {
		t.Fatal(err)
	}
	x.d.failStatus(true)
	out := x.page()
	w := x.watcherOf(out)
	if w.active != "1" {
		t.Fatalf("a failed status read must render as still refreshing, got active=%s", w.active)
	}
	if s := x.poll(w); s.Reload || !s.Active {
		t.Fatalf("a failed status read must keep the page polling: %+v", s)
	}
	x.d.failStatus(false)
	x.d.set("Succeeded")
	if s := x.poll(w); !s.Reload {
		t.Fatalf("once the status is known again the page reloads: %+v", s)
	}
}

// Without a session the status answers 401 with reload, so the page goes to sign-in;
// a pipeline that cannot report a refresh leaves the radar without a watcher.
func TestRefreshStatusWithoutSessionAndWithoutProber(t *testing.T) {
	h := New(fakeSessions{}, worklist.NewMemoryStore(), &recordPipeline{}, nil, false, nil)
	rec := httptest.NewRecorder()
	h.RefreshStatus(rec, httptest.NewRequest(http.MethodGet, "/items/refresh-status?done=0", nil))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"reload":true`) {
		t.Fatalf("no session: %d %s", rec.Code, rec.Body.String())
	}

	mem := worklist.NewMemoryStore()
	mem.Seed(owner, pr(1, "A PR"))
	static := New(fakeSessions{&session.User{ID: owner}}, mem, &recordPipeline{}, nil, false, nil)
	rec = httptest.NewRecorder()
	static.Index(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if strings.Contains(rec.Body.String(), "reload.js") {
		t.Fatal("a pipeline without RefreshProber must leave the radar static")
	}
	rec = httptest.NewRecorder()
	static.RefreshStatus(rec, httptest.NewRequest(http.MethodGet, "/items/refresh-status?done=0", nil))
	if strings.Contains(rec.Body.String(), `"reload":true`) {
		t.Fatal("without a prober there is nothing to reload for")
	}
}

// The watcher script is served, and the views that already poll keep their meta
// refresh rather than the worklist's watcher.
func TestReloadScriptServedAndOtherViewsUnchanged(t *testing.T) {
	h := New(fakeSessions{}, worklist.NewMemoryStore(), &recordPipeline{}, nil, false, nil)
	rec := httptest.NewRecorder()
	h.Static().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/reload.js", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/items/refresh-status") || !strings.Contains(rec.Body.String(), "details[open]") {
		t.Fatalf("GET /static/reload.js = %d:\n%s", rec.Code, rec.Body.String())
	}
	out := renderPage(t, pageData{View: "processing", RefreshSecs: 3})
	if !strings.Contains(out, `<meta http-equiv="refresh" content="3">`) || strings.Contains(out, "reload.js") {
		t.Fatalf("the processing view keeps its meta refresh, got:\n%s", out)
	}
}

// A refresh that ends while the next one is already running still reloads the page,
// so its results show while the next pass runs.
func TestCompletedRefreshShowsWhileTheNextRuns(t *testing.T) {
	ctx := context.Background()
	mem := worklist.NewMemoryStore()
	mem.Seed(owner, pr(1, "Old PR"))
	x := newHarness(t, mem)
	w := x.watcherOf(x.page())

	x.d.set("Running")
	if err := x.ig.Refresh(ctx, owner); err != nil { // refresh A
		t.Fatal(err)
	}
	if err := mem.Upsert(ctx, owner, pr(2, "From A")); err != nil {
		t.Fatal(err)
	}
	x.d.set("Succeeded")
	if n := ingest.NewRefresher(mem, x.ig, nil, time.Hour, nil).RefreshOnce(ctx); n != 1 { // refresh B starts
		t.Fatalf("B dispatched %d runs", n)
	}
	x.d.set("Running")

	if s := x.poll(w); !s.Reload || !s.Active {
		t.Fatalf("A ended while B runs: poll = %+v, want reload while active", s)
	}
	out := x.page()
	if !strings.Contains(out, "From A") || x.watcherOf(out).active != "1" {
		t.Fatalf("the reload shows A's work and keeps watching B, got:\n%s", out)
	}
}

// blockingStatus holds every status read until released, as a stalled backend would.
type blockingStatus struct {
	phaseDispatcher
	release chan struct{}
}

func (b *blockingStatus) Status(ctx context.Context, id string) (ingest.RunStatus, error) {
	select {
	case <-b.release:
	case <-ctx.Done():
		return ingest.RunStatus{}, ctx.Err()
	}
	return b.phaseDispatcher.Status(ctx, id)
}

// Opening the radar never waits on the backend: it renders from what gofer already
// knows, and the status poll asks the backend, within statusTimeout.
func TestRenderDoesNotWaitOnTheBackend(t *testing.T) {
	ctx := context.Background()
	mem := worklist.NewMemoryStore()
	mem.Seed(owner, pr(1, "A PR"))
	d := &blockingStatus{phaseDispatcher: phaseDispatcher{phase: "Running"}, release: make(chan struct{})}
	defer close(d.release)
	ig := ingest.New(d, "", "", nil, "", "")
	h := New(fakeSessions{&session.User{ID: owner}}, mem, ig, nil, false, nil)
	if err := ig.Refresh(ctx, owner); err != nil {
		t.Fatal(err)
	}
	rendered := make(chan string, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.Index(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		rendered <- rec.Body.String()
	}()
	select {
	case out := <-rendered:
		if !strings.Contains(out, "A PR") || !strings.Contains(out, `data-active="1"`) {
			t.Fatalf("expected the radar, watching the refresh, got:\n%s", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rendering the radar waited on the backend's status")
	}
}

// A tab open across a web-tier restart still learns that the refresh it was waiting
// for ended: the version is kept in the database, not just in the old process.
func TestOpenTabAcrossRestart(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := db.Worklist()
	if err := store.Upsert(ctx, owner, pr(1, "Old PR")); err != nil {
		t.Fatal(err)
	}
	d := &phaseDispatcher{phase: "Succeeded"}
	process := func() (*ingest.Ingestor, *Handler) {
		ig := ingest.New(d, "", "", nil, "", "")
		if err := ig.UseLedger(ctx, store); err != nil {
			t.Fatal(err)
		}
		return ig, New(fakeSessions{&session.User{ID: owner}}, store, ig, nil, false, nil)
	}
	ig1, h1 := process()
	x := &harness{t: t, store: store, d: d, ig: ig1, h: h1}
	d.set("Running")
	if _, err := ig1.ScheduledRefresh(ctx, owner); err != nil { // generation 1 starts
		t.Fatal(err)
	}
	w := x.watcherOf(x.page()) // the tab, which then sleeps
	if w.done != "0" || w.active != "1" {
		t.Fatalf("rendered %+v, want done 0 while generation 1 runs", w)
	}

	// The web pod restarts; the run goes on in the runtime and writes its results.
	ig2, h2 := process()
	x.ig, x.h = ig2, h2
	if ok, _ := ig2.ScheduledRefresh(ctx, owner); ok {
		t.Fatal("the new process started a second ingest while generation 1 runs")
	}
	if err := store.Upsert(ctx, owner, pr(2, "New PR")); err != nil {
		t.Fatal(err)
	}
	d.set("Succeeded")
	if s := x.poll(w); !s.Reload {
		t.Fatalf("the tab must learn that generation 1 ended, though the process that rendered it is gone: %+v", s)
	}
	if !strings.Contains(x.page(), "New PR") {
		t.Fatal("the reload shows the new work")
	}
}
