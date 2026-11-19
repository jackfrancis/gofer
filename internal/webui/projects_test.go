package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// projectsFixture is a radar spanning three repositories, with distinct urgencies so
// the full list has a definite order that interleaves the repositories.
func projectsFixture(t *testing.T) *Handler {
	t.Helper()
	store := worklist.NewMemoryStore()
	item := func(repo string, n int, urgency float64) worklist.WorkItem {
		return worklist.WorkItem{
			ID: "github:" + repo + "#" + string(rune('0'+n)), Source: "github", Type: worklist.TypeIssue,
			GitHub: worklist.GitHubRef{Repo: repo, Number: n, Title: "title-" + repo + "-" + string(rune('0'+n))},
			Meta:   worklist.Metadata{Origin: worklist.OriginUser, Urgency: urgency, Rank: urgency},
		}
	}
	store.Seed("u1",
		item("a/one", 1, 0.9),
		item("b/two", 2, 0.8),
		item("a/one", 3, 0.7),
		item("c/three", 4, 0.6),
		item("b/two", 5, 0.5),
	)
	return New(fakeSessions{&session.User{ID: "u1"}}, store, &recordPipeline{}, reconnectProviders{false}, false, nil)
}

// postFilter submits the filter form and returns the cookie it set (nil when it set
// none).
func postFilter(t *testing.T, h *Handler, form url.Values) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/items/projects", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.FilterProjects(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("status = %d, location = %q; want 303 to / (Post/Redirect/Get)", rec.Code, rec.Header().Get("Location"))
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == projectsCookie {
			return c
		}
	}
	return nil
}

func getIndex(t *testing.T, h *Handler, c *http.Cookie) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if c != nil && c.MaxAge >= 0 {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.Index(rec, req)
	return rec.Body.String()
}

// order returns the titles found in page, in page order.
func order(page string, titles ...string) []string {
	type hit struct {
		at    int
		title string
	}
	var hits []hit
	for _, title := range titles {
		if i := strings.Index(page, ">"+title+"<"); i >= 0 {
			hits = append(hits, hit{i, title})
		}
	}
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hits[j].at < hits[j-1].at; j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.title
	}
	return out
}

var fixtureTitles = []string{"title-a/one-1", "title-b/two-2", "title-a/one-3", "title-c/three-4", "title-b/two-5"}

// With no saved selection every project is shown, "Select all" is checked, and the
// checklist lists each repository once with its item count.
func TestProjectFilterDefaultsToAll(t *testing.T) {
	page := getIndex(t, projectsFixture(t), nil)
	if got := order(page, fixtureTitles...); strings.Join(got, ",") != strings.Join(fixtureTitles, ",") {
		t.Fatalf("full radar order = %v, want %v", got, fixtureTitles)
	}
	for _, want := range []string{
		"Projects: All",
		`name="all" value="1" data-filter-all checked`,
		`value="a/one" data-filter-project checked`,
		`<span class="zz-filter-name">c/three</span> <span class="zz-filter-count">1</span>`,
		"5 items on your radar",
		`action="/items/projects"`,
		`src="/static/projects.js"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("default page lacks %q", want)
		}
	}
	if n := strings.Count(page, `value="b/two"`); n != 1 {
		t.Errorf("b/two listed %d times, want once", n)
	}
}

// A narrowed selection shows only those projects' items, in the full list's order
// (gaps in rank are simply skipped), and the label and count say it is filtered.
func TestProjectFilterKeepsFullListOrder(t *testing.T) {
	h := projectsFixture(t)
	c := postFilter(t, h, url.Values{"project": {"b/two", "a/one"}})
	if c == nil || c.MaxAge <= 0 || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("expected a saved, HttpOnly, SameSite=Lax selection cookie, got %+v", c)
	}
	page := getIndex(t, h, c)
	want := []string{"title-a/one-1", "title-b/two-2", "title-a/one-3", "title-b/two-5"}
	if got := order(page, fixtureTitles...); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("filtered order = %v, want %v", got, want)
	}
	for _, s := range []string{"Projects: 2 of 3", "4 of 5 items on your radar", `value="c/three" data-filter-project>`} {
		if !strings.Contains(page, s) {
			t.Errorf("filtered page lacks %q", s)
		}
	}
	if strings.Contains(page, "data-filter-all checked") {
		t.Error(`"Select all" must be unchecked while filtered`)
	}
}

// "Select all", or checking every project on the radar, clears the saved selection.
func TestProjectFilterSelectAllClears(t *testing.T) {
	h := projectsFixture(t)
	for name, form := range map[string]url.Values{
		"select all":      {"all": {"1"}, "project": {"a/one"}},
		"every project":   {"project": {"a/one", "b/two", "c/three"}},
		"every + unknown": {"project": {"a/one", "b/two", "c/three", "z/gone"}},
	} {
		c := postFilter(t, h, form)
		if c == nil || c.MaxAge >= 0 {
			t.Errorf("%s: expected the selection cookie to be cleared, got %+v", name, c)
		}
	}
}

// Only repositories on the user's radar are saved; an empty selection is kept (and
// shows no items) rather than falling back to every project.
func TestProjectFilterSanitizesSelection(t *testing.T) {
	h := projectsFixture(t)
	c := postFilter(t, h, url.Values{"project": {"c/three", "z/not-mine", "c/three"}})
	if c == nil {
		t.Fatal("expected a selection cookie")
	}
	if v, _ := url.ParseQuery(c.Value); strings.Join(v["p"], ",") != "c/three" {
		t.Fatalf("saved selection = %v, want [c/three]", v["p"])
	}

	empty := postFilter(t, h, url.Values{})
	if empty == nil || empty.MaxAge <= 0 {
		t.Fatalf("expected an empty selection to be saved, got %+v", empty)
	}
	page := getIndex(t, h, empty)
	if got := order(page, fixtureTitles...); len(got) != 0 {
		t.Fatalf("empty selection shows %v, want nothing", got)
	}
	for _, s := range []string{"No items in the selected projects.", "Projects: 0 of 3", "0 of 5 items on your radar"} {
		if !strings.Contains(page, s) {
			t.Errorf("empty-selection page lacks %q", s)
		}
	}
}

// The filter is a view preference: an anonymous request saves nothing.
func TestProjectFilterRequiresSession(t *testing.T) {
	h := &Handler{sessions: fakeSessions{}, store: worklist.NewMemoryStore(), now: time.Now}
	if c := postFilter(t, h, url.Values{"project": {"a/one"}}); c != nil {
		t.Fatalf("anonymous request set %+v", c)
	}
}

// A radar with a single project has nothing to filter, so the dropdown is hidden —
// unless a saved selection is narrowing it, so it can always be undone.
func TestProjectFilterHiddenForOneProject(t *testing.T) {
	one := []worklist.WorkItem{{ID: "i1", GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1, Title: "t"}}}
	if out := renderPage(t, pageData{View: "worklist", Items: one, Projects: projectOptions(one, nil)}); strings.Contains(out, "zz-filter") {
		t.Fatal("a single-project radar should not show the filter")
	}
	if out := renderPage(t, pageData{View: "worklist", Projects: projectOptions(one, map[string]bool{}), ProjectsFiltered: true}); !strings.Contains(out, "zz-filter") {
		t.Fatal("a filtered radar must always show the filter")
	}
}

// inRepo places item in repo.
func inRepo(it worklist.WorkItem, repo string) worklist.WorkItem {
	it.GitHub.Repo = repo
	return it
}

// filterCookie is the cookie FilterProjects saves for the given projects.
func filterCookie(repos ...string) *http.Cookie {
	return &http.Cookie{Name: projectsCookie, Value: url.Values{"p": repos}.Encode()}
}

// The bulk actions act only on the items the project filter shows, and on every item
// without a filter.
func TestBulkActionsFollowProjectFilter(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	opts := []ModelOption{
		{Value: "c0|default", Endpoint: "https://default", Model: "default"},
		{Value: "c1|alt", Endpoint: "https://alt", Model: "alt"},
	}
	seed := func() *worklist.MemoryStore {
		store := worklist.NewMemoryStore()
		store.Seed("u1",
			inRepo(worklist.WorkItem{ID: "new-a", Type: worklist.TypePullRequest}, "a/one"),
			inRepo(worklist.WorkItem{ID: "new-b", Type: worklist.TypePullRequest}, "b/two"),
			inRepo(reviewedByModel("rev-a", "default", now), "a/one"),
			inRepo(reviewedByModel("rev-b", "default", now), "b/two"),
		)
		return store
	}
	post := func(store *worklist.MemoryStore, pipe *recordPipeline, path string, c *http.Cookie, handle func(*Handler, http.ResponseWriter, *http.Request)) {
		h := &Handler{sessions: fakeSessions{&session.User{ID: "u1"}}, store: store, pipeline: pipe, convEnabled: true, options: opts, now: func() time.Time { return now }}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("choice=c1|alt"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if c != nil {
			req.AddCookie(c)
		}
		handle(h, httptest.NewRecorder(), req)
	}
	threads := func(store *worklist.MemoryStore) map[string]int {
		items, _ := store.List(context.Background(), "u1")
		n := map[string]int{}
		for _, it := range items {
			n[it.ID] = len(it.Thread)
		}
		return n
	}

	for _, tc := range []struct {
		name                    string
		cookie                  *http.Cookie
		review, second, cleared string
	}{
		{"filtered", filterCookie("a/one"), "new-a", "rev-a", "rev-a"},
		{"every project selected", filterCookie("a/one", "b/two"), "new-a,new-b", "rev-a,rev-b", "rev-a,rev-b"},
		{"no filter", nil, "new-a,new-b", "rev-a,rev-b", "rev-a,rev-b"},
	} {
		pipe := &recordPipeline{}
		post(seed(), pipe, "/items/review-all", tc.cookie, (*Handler).ReviewAllPRs)
		if got := strings.Join(pipe.reviewed, ","); got != tc.review {
			t.Errorf("%s: Review all PRs dispatched %q, want %q", tc.name, got, tc.review)
		}

		pipe = &recordPipeline{}
		post(seed(), pipe, "/items/second-opinion-all", tc.cookie, (*Handler).SecondOpinionAllPRs)
		if got := strings.Join(pipe.secondOpinions, ","); got != tc.second {
			t.Errorf("%s: Get 2nd Opinion dispatched %q, want %q", tc.name, got, tc.second)
		}

		store := seed()
		post(store, &recordPipeline{}, "/items/reset-conversations", tc.cookie, (*Handler).ResetConversations)
		var cleared []string
		for _, id := range []string{"rev-a", "rev-b"} {
			if threads(store)[id] == 0 {
				cleared = append(cleared, id)
			}
		}
		if got := strings.Join(cleared, ","); got != tc.cleared {
			t.Errorf("%s: Reset Conversations cleared %q, want %q", tc.name, got, tc.cleared)
		}
	}
}

// The bulk actions' counts on the radar cover only the shown items.
func TestBulkCountsFollowProjectFilter(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	store := worklist.NewMemoryStore()
	user := worklist.Metadata{Origin: worklist.OriginUser}
	mk := func(it worklist.WorkItem, repo string) worklist.WorkItem {
		it = inRepo(it, repo)
		it.Meta = user
		return it
	}
	store.Seed("u1",
		mk(worklist.WorkItem{ID: "new-a", Type: worklist.TypePullRequest}, "a/one"),
		mk(worklist.WorkItem{ID: "new-b1", Type: worklist.TypePullRequest}, "b/two"),
		mk(worklist.WorkItem{ID: "new-b2", Type: worklist.TypePullRequest}, "b/two"),
		mk(reviewedByModel("rev-b", "default", now), "b/two"),
	)
	opts := []ModelOption{{Value: "c0|default", Model: "default"}, {Value: "c1|alt", Model: "alt"}}
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, &recordPipeline{}, reconnectProviders{false}, true, opts)
	h.now = func() time.Time { return now }
	get := func(c *http.Cookie) string {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if c != nil {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		h.Index(rec, req)
		return rec.Body.String()
	}

	all := get(nil)
	for _, s := range []string{"Review all PRs (3)", "Get 2nd Opinion (1)", "Reset Conversations (1)"} {
		if !strings.Contains(all, s) {
			t.Errorf("unfiltered radar lacks %q", s)
		}
	}
	a := get(filterCookie("a/one"))
	if !strings.Contains(a, "Review all PRs (1)") {
		t.Error("filtered radar should count only a/one's unreviewed PR")
	}
	if strings.Contains(a, "Get 2nd Opinion") || strings.Contains(a, "Reset Conversations") {
		t.Error("a/one has no reviewed PR or conversation: those actions should be hidden")
	}
}
