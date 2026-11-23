package webui

import (
	"bytes"
	"context"
	"html/template"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
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

// postFilterRaw submits the filter form and returns the response.
func postFilterRaw(h *Handler, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/items/projects", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.FilterProjects(rec, req)
	return rec
}

// selectionCookie returns the selection cookie rec set, or nil.
func selectionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == projectsCookie {
			return c
		}
	}
	return nil
}

// postFilter submits the filter form and returns the cookie it set (nil when it set
// none).
func postFilter(t *testing.T, h *Handler, form url.Values) *http.Cookie {
	t.Helper()
	rec := postFilterRaw(h, form)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("status = %d, location = %q; want 303 to / (Post/Redirect/Get)", rec.Code, rec.Header().Get("Location"))
	}
	return selectionCookie(rec)
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

// With no saved selection every project is shown and the checklist lists each
// repository once with its item count. "Select all" is checked but rendered hidden and
// disabled: projects.js enables it, and without the script the per-project boxes alone
// decide what Apply saves.
func TestProjectFilterDefaultsToAll(t *testing.T) {
	page := getIndex(t, projectsFixture(t), nil)
	if got := order(page, fixtureTitles...); strings.Join(got, ",") != strings.Join(fixtureTitles, ",") {
		t.Fatalf("full radar order = %v, want %v", got, fixtureTitles)
	}
	for _, want := range []string{
		"Projects: All",
		`<label class="zz-filter-opt zz-filter-all" hidden><input type="checkbox" name="all" value="1" data-filter-all disabled checked>`,
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
	if strings.Contains(page, "data-filter-all disabled checked") {
		t.Error(`"Select all" must be unchecked while filtered`)
	}
}

// "Select all", or checking every project on the radar (all a script-less browser can
// send), clears the saved selection.
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

// Only repositories on the user's radar are saved, under the user's ID; an empty
// selection is kept (and shows no items) rather than falling back to every project.
func TestProjectFilterSanitizesSelection(t *testing.T) {
	h := projectsFixture(t)
	c := postFilter(t, h, url.Values{"project": {"c/three", "z/not-mine", "c/three"}})
	if c == nil {
		t.Fatal("expected a selection cookie")
	}
	v, _ := url.ParseQuery(c.Value)
	if strings.Join(v["p"], ",") != "c/three" || v.Get("u") != "u1" {
		t.Fatalf("saved selection = %v for %q, want [c/three] for u1", v["p"], v.Get("u"))
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

// A selection saved by one account is ignored for another signed in to the same
// browser, which starts from "Select all".
func TestProjectFilterIsPerAccount(t *testing.T) {
	saved := postFilter(t, projectsFixture(t), url.Values{"project": {"c/three"}})
	if saved == nil {
		t.Fatal("expected a selection cookie")
	}
	other := projectsFixture(t)
	other.sessions = fakeSessions{&session.User{ID: "u2"}}
	items, _ := other.store.List(context.Background(), "u1")
	other.store.(*worklist.MemoryStore).Seed("u2", items...)
	page := getIndex(t, other, saved)
	if !strings.Contains(page, "Projects: All") || len(order(page, fixtureTitles...)) != len(fixtureTitles) {
		t.Fatal("another account must not inherit the saved selection")
	}
}

// A selection too large for the cookie is refused: the previous filter is kept and the
// radar says why, instead of silently widening to every project.
func TestProjectFilterTooLargeKeepsPrevious(t *testing.T) {
	store := worklist.NewMemoryStore()
	var repos []string
	var items []worklist.WorkItem
	for i := 0; i < 150; i++ {
		repo := "some-organization/" + strings.Repeat("r", 20) + strconv.Itoa(i)
		repos = append(repos, repo)
		items = append(items, worklist.WorkItem{ID: repo, GitHub: worklist.GitHubRef{Repo: repo, Number: 1, Title: repo}})
	}
	store.Seed("u1", items...)
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, &recordPipeline{}, reconnectProviders{false}, false, nil)

	rec := postFilterRaw(h, url.Values{"project": repos[1:]})
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || loc != "/?projects="+projectsTooMany {
		t.Fatalf("status = %d, location = %q; want 303 to the too-many notice", rec.Code, loc)
	}
	if c := selectionCookie(rec); c != nil {
		t.Fatalf("a refused selection must leave the cookie alone, got %+v", c)
	}

	prev := filterCookie(repos[0])
	req := httptest.NewRequest(http.MethodGet, "/?projects="+projectsTooMany, nil)
	req.AddCookie(prev)
	page := httptest.NewRecorder()
	h.Index(page, req)
	if body := page.Body.String(); !strings.Contains(body, "Too many projects selected to save") || !strings.Contains(body, "Projects: 1 of 150") {
		t.Fatal("expected the notice and the previous filter")
	}
}

// A single-project radar has nothing to filter, so the dropdown is hidden — unless a
// selection is saved, even one that now covers the whole radar (another project's last
// item closed), so it can always be cleared to include future projects.
func TestProjectFilterReachableWhileSaved(t *testing.T) {
	one := []worklist.WorkItem{{ID: "i1", GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1, Title: "t"}}}
	if out := renderPage(t, pageData{View: "worklist", Items: one, Projects: projectOptions(one, nil)}); strings.Contains(out, "zz-filter") {
		t.Fatal("a single-project radar should not show the filter")
	}

	store := worklist.NewMemoryStore()
	store.Seed("u1", one...)
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, &recordPipeline{}, reconnectProviders{false}, false, nil)
	page := getIndex(t, h, filterCookie("o/r"))
	if !strings.Contains(page, "zz-filter") || !strings.Contains(page, "Projects: 1 of 1") {
		t.Fatal("a saved selection must keep the filter on the radar")
	}
}

// inRepo places item in repo.
func inRepo(it worklist.WorkItem, repo string) worklist.WorkItem {
	it.GitHub.Repo = repo
	return it
}

// filterCookie is the cookie FilterProjects saves for user u1 and the given projects.
func filterCookie(repos ...string) *http.Cookie {
	sel := map[string]bool{}
	for _, r := range repos {
		sel[r] = true
	}
	return &http.Cookie{Name: projectsCookie, Value: url.Values{"u": {"u1"}}.Encode() + "&" + encodeSelection(sel)}
}

// scopeOf is the "scope" a bulk form rendered for the given projects carries.
func scopeOf(repos ...string) string {
	sel := map[string]bool{}
	for _, r := range repos {
		sel[r] = true
	}
	return encodeSelection(sel)
}

// The bulk actions act on exactly what the submitting page showed: the projects named
// by the form's scope (never the cookie, which another tab may have changed since)
// and only items on the radar. Reset Conversations also clears hidden and completed
// items in those projects.
func TestBulkActionsFollowSubmittedScope(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	opts := []ModelOption{
		{Value: "c0|default", Endpoint: "https://default", Model: "default"},
		{Value: "c1|alt", Endpoint: "https://alt", Model: "alt"},
	}
	seed := func() *worklist.MemoryStore {
		merged := inRepo(worklist.WorkItem{ID: "merged-a", Type: worklist.TypePullRequest}, "a/one")
		merged.Meta.CompletedAt = now.Add(-time.Hour)
		hidden := inRepo(reviewedByModel("hidden-a", "default", now), "a/one")
		hidden.Meta.HiddenAt = now.Add(-time.Hour)
		store := worklist.NewMemoryStore()
		store.Seed("u1",
			inRepo(worklist.WorkItem{ID: "new-a", Type: worklist.TypePullRequest}, "a/one"),
			inRepo(worklist.WorkItem{ID: "new-b", Type: worklist.TypePullRequest}, "b/two"),
			inRepo(reviewedByModel("rev-a", "default", now), "a/one"),
			inRepo(reviewedByModel("rev-b", "default", now), "b/two"),
			merged, hidden,
		)
		return store
	}
	post := func(store *worklist.MemoryStore, pipe *recordPipeline, path, scope string, c *http.Cookie, handle func(*Handler, http.ResponseWriter, *http.Request)) {
		h := &Handler{sessions: fakeSessions{&session.User{ID: "u1"}}, store: store, pipeline: pipe, convEnabled: true, options: opts, now: func() time.Time { return now }}
		form := url.Values{"choice": {"c1|alt"}}
		if scope != "" {
			form.Set("scope", scope)
		}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if c != nil {
			req.AddCookie(c)
		}
		handle(h, httptest.NewRecorder(), req)
	}
	cleared := func(store *worklist.MemoryStore) string {
		items, _ := store.List(context.Background(), "u1")
		var ids []string
		for _, it := range items {
			if len(it.Thread) == 0 && strings.HasPrefix(it.ID, "rev") || len(it.Thread) == 0 && it.ID == "hidden-a" {
				ids = append(ids, it.ID)
			}
		}
		sort.Strings(ids)
		return strings.Join(ids, ",")
	}

	for _, tc := range []struct {
		name                    string
		scope                   string
		cookie                  *http.Cookie
		review, second, cleared string
	}{
		{"filtered page", scopeOf("a/one"), filterCookie("a/one"), "new-a", "rev-a", "hidden-a,rev-a"},
		// The filter was widened to All in another tab after this page rendered.
		{"filtered page, cookie since cleared", scopeOf("a/one"), nil, "new-a", "rev-a", "hidden-a,rev-a"},
		// The filter was narrowed in another tab after this unfiltered page rendered.
		{"unfiltered page, cookie since narrowed", "", filterCookie("b/two"), "new-a,new-b", "rev-a,rev-b", "hidden-a,rev-a,rev-b"},
		{"every project", scopeOf("a/one", "b/two"), nil, "new-a,new-b", "rev-a,rev-b", "hidden-a,rev-a,rev-b"},
		{"empty selection", "p=", nil, "", "", ""},
		{"garbled scope", "%zz", nil, "", "", ""},
	} {
		pipe := &recordPipeline{}
		post(seed(), pipe, "/items/review-all", tc.scope, tc.cookie, (*Handler).ReviewAllPRs)
		if got := strings.Join(pipe.reviewed, ","); got != tc.review {
			t.Errorf("%s: Review all PRs dispatched %q, want %q", tc.name, got, tc.review)
		}

		pipe = &recordPipeline{}
		post(seed(), pipe, "/items/second-opinion-all", tc.scope, tc.cookie, (*Handler).SecondOpinionAllPRs)
		if got := strings.Join(pipe.secondOpinions, ","); got != tc.second {
			t.Errorf("%s: Get 2nd Opinion dispatched %q, want %q", tc.name, got, tc.second)
		}

		store := seed()
		post(store, &recordPipeline{}, "/items/reset-conversations", tc.scope, tc.cookie, (*Handler).ResetConversations)
		if got := cleared(store); got != tc.cleared {
			t.Errorf("%s: Reset Conversations cleared %q, want %q", tc.name, got, tc.cleared)
		}
	}
}

// The bulk actions' counts on the radar match what each would act on, and each bulk
// form carries the page's selection.
func TestBulkCountsFollowProjectFilter(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	store := worklist.NewMemoryStore()
	mk := func(it worklist.WorkItem, repo string) worklist.WorkItem {
		it = inRepo(it, repo)
		it.Meta.Origin = worklist.OriginUser
		return it
	}
	hidden := mk(reviewedByModel("hidden-a", "default", now), "a/one")
	hidden.Meta.HiddenAt = now.Add(-time.Hour)
	merged := mk(worklist.WorkItem{ID: "merged-b", Type: worklist.TypePullRequest}, "b/two")
	merged.Meta.CompletedAt = now.Add(-time.Hour)
	store.Seed("u1",
		mk(worklist.WorkItem{ID: "new-a", Type: worklist.TypePullRequest}, "a/one"),
		mk(worklist.WorkItem{ID: "new-b1", Type: worklist.TypePullRequest}, "b/two"),
		mk(worklist.WorkItem{ID: "new-b2", Type: worklist.TypePullRequest}, "b/two"),
		mk(reviewedByModel("rev-b", "default", now), "b/two"),
		hidden, merged,
	)
	opts := []ModelOption{{Value: "c0|default", Model: "default"}, {Value: "c1|alt", Model: "alt"}}
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, &recordPipeline{}, reconnectProviders{false}, true, opts)
	h.now = func() time.Time { return now }

	// Hidden and completed PRs are not reviewed, so not counted; Reset also clears the
	// hidden item's thread, so it counts it.
	all := getIndex(t, h, nil)
	for _, s := range []string{"Review all PRs (3)", "Get 2nd Opinion (1)", "Reset Conversations (2)"} {
		if !strings.Contains(all, s) {
			t.Errorf("unfiltered radar lacks %q", s)
		}
	}
	if strings.Contains(all, `name="scope"`) {
		t.Error("an unfiltered page's bulk forms carry no scope")
	}

	a := getIndex(t, h, filterCookie("a/one"))
	for _, s := range []string{"Review all PRs (1)", "Reset Conversations (1)"} {
		if !strings.Contains(a, s) {
			t.Errorf("a/one radar lacks %q", s)
		}
	}
	if strings.Contains(a, "Get 2nd Opinion") {
		t.Error("a/one's only reviewed PR is hidden: Get 2nd Opinion should be hidden")
	}
	want := `<input type="hidden" name="scope" value="` + template.HTMLEscapeString(scopeOf("a/one")) + `">`
	if n := strings.Count(a, want); n != 3 { // Review all PRs (one form per model) and Reset
		t.Errorf("expected the scope on every bulk form (3), found %d of %q", n, want)
	}
}

// Every bulk form variant — Review all PRs (plain and per model), Get 2nd Opinion
// (menu and auto), Reset Conversations — posts the page's selection back.
func TestEveryBulkFormCarriesScope(t *testing.T) {
	pr := worklist.WorkItem{ID: "p", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: "a/one", Number: 1}}
	base := pageData{View: "worklist", Items: []worklist.WorkItem{pr}, ConvEnabled: true, ReviewCount: 1, SecondOpinionEnabled: true,
		SecondOpinionCount: 1, ResetCount: 1, ProjectsFiltered: true, ProjectScope: scopeOf("a/one")}
	field := `name="scope" value="p=a%2fone"`
	for name, tc := range map[string]struct {
		mod  func(*pageData)
		want int
	}{
		"plain review, auto 2nd opinion": {func(d *pageData) { d.SecondOpinionMode = "auto" }, 3},
		"per-model review, menu 2nd opinion": {func(d *pageData) {
			d.ReviewModelOptions = []ModelOption{{Value: "a"}, {Value: "b"}}
			d.SecondOpinionMode, d.SecondOpinionMenuOptions = "menu", []ModelOption{{Value: "b"}}
		}, 4},
	} {
		d := base
		tc.mod(&d)
		out := strings.ToLower(renderPage(t, d))
		forms := 0
		for _, action := range []string{"review-all", "second-opinion-all", "reset-conversations"} {
			forms += strings.Count(out, `<form method="post" action="/items/`+action+`">`)
		}
		if got := strings.Count(out, field); got != tc.want || forms != tc.want {
			t.Errorf("%s: %d of %d bulk forms carry the scope, want all %d", name, got, forms, tc.want)
		}
	}
}

// A saved project whose last visible item closed is no longer in the checklist, so
// nothing the page offers may reach it: its archived thread is neither counted nor in
// the scope the bulk forms post back.
func TestBulkScopeNamesOnlyListedProjects(t *testing.T) {
	archived := inRepo(worklist.WorkItem{ID: "done-a", Type: worklist.TypePullRequest,
		Thread: []worklist.Message{{Role: worklist.RoleAgent, Content: "old"}}}, "a/one")
	archived.Meta.CompletedAt = time.Now()
	open := inRepo(worklist.WorkItem{ID: "open-b", Type: worklist.TypeIssue,
		Thread: []worklist.Message{{Role: worklist.RoleAgent, Content: "hi"}}}, "b/two")
	store := worklist.NewMemoryStore()
	store.Seed("u1", archived, open)
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, &recordPipeline{}, reconnectProviders{false}, true, nil)

	page := getIndex(t, h, filterCookie("a/one"))
	if !strings.Contains(page, "Projects: 0 of 1") {
		t.Fatal("expected the filter to show no listed project selected")
	}
	if strings.Contains(page, "Reset Conversations") {
		t.Fatal("no listed project is selected, so there is nothing to reset")
	}
	if strings.Contains(page, "a%2fone") || strings.Contains(page, "a%2Fone") {
		t.Fatal("the page must not name the unlisted project in any form")
	}
	// Unfiltered, Reset still reaches every project, archived threads included.
	if all := getIndex(t, h, nil); !strings.Contains(all, "Reset Conversations (2)") {
		t.Fatal("an unfiltered radar counts archived threads in every project")
	}
}

// errReader fails every read, like a request body cut off mid-upload.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// A bulk form that cannot be read changes nothing: its lost scope must not read as
// every project. Each body below scopes the action to b/two, which has no items, so
// any change to a/one's items means the scope was dropped. Only a URL-encoded body is
// read: Go's ParseForm leaves multipart and other bodies unread.
func TestBulkActionsRejectUnreadableForm(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	opts := []ModelOption{{Value: "c0|default", Model: "default"}, {Value: "c1|alt", Model: "alt"}}
	scoped := url.Values{"scope": {scopeOf("b/two")}, "choice": {"c1|alt"}}.Encode()
	multipartBody := func() (io.Reader, string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("scope", scopeOf("b/two"))
		_ = mw.WriteField("choice", "c1|alt")
		_ = mw.Close()
		return &buf, mw.FormDataContentType()
	}
	for name, body := range map[string]func() (io.Reader, string){
		"cut-off body": func() (io.Reader, string) { return errReader{}, "application/x-www-form-urlencoded" },
		"multipart":    multipartBody,
		"plain text":   func() (io.Reader, string) { return strings.NewReader(scoped), "text/plain" },
		"untyped":      func() (io.Reader, string) { return strings.NewReader(scoped), "" },
		"bad type":     func() (io.Reader, string) { return strings.NewReader(scoped), "application/x-www-form-urlencoded; =" },
	} {
		store := worklist.NewMemoryStore()
		store.Seed("u1",
			inRepo(worklist.WorkItem{ID: "new-a", Type: worklist.TypePullRequest}, "a/one"),
			inRepo(reviewedByModel("rev-a", "default", now), "a/one"),
		)
		pipe := &recordPipeline{}
		h := &Handler{sessions: fakeSessions{&session.User{ID: "u1"}}, store: store, pipeline: pipe, convEnabled: true, options: opts, now: func() time.Time { return now }}
		for path, handle := range map[string]func(http.ResponseWriter, *http.Request){
			"/items/review-all":          h.ReviewAllPRs,
			"/items/second-opinion-all":  h.SecondOpinionAllPRs,
			"/items/reset-conversations": h.ResetConversations,
		} {
			b, ct := body()
			req := httptest.NewRequest(http.MethodPost, path, b)
			if ct != "" {
				req.Header.Set("Content-Type", ct)
			}
			rec := httptest.NewRecorder()
			handle(rec, req)
			if rec.Code != http.StatusSeeOther {
				t.Errorf("%s %s: status = %d, want 303", name, path, rec.Code)
			}
		}
		if len(pipe.turns) != 0 {
			t.Errorf("%s: dispatched %v", name, pipe.turns)
		}
		items, _ := store.List(context.Background(), "u1")
		for _, it := range items {
			if it.ID == "new-a" && len(it.Thread) != 0 || it.ID == "rev-a" && len(it.Thread) != 2 {
				t.Errorf("%s: %s thread changed to %d turns", name, it.ID, len(it.Thread))
			}
		}
	}
}

// The page's own URL-encoded scope is honored by the same check, so the rejection
// above is about the body, not the scope.
func TestBulkActionsReadURLEncodedScope(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	store := worklist.NewMemoryStore()
	store.Seed("u1", inRepo(reviewedByModel("rev-a", "default", now), "a/one"))
	h := &Handler{sessions: fakeSessions{&session.User{ID: "u1"}}, store: store, now: func() time.Time { return now }}
	req := httptest.NewRequest(http.MethodPost, "/items/reset-conversations", strings.NewReader(url.Values{"scope": {scopeOf("a/one")}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	h.ResetConversations(httptest.NewRecorder(), req)
	if items, _ := store.List(context.Background(), "u1"); len(items[0].Thread) != 0 {
		t.Fatal("a URL-encoded form (with a charset parameter) must be read and applied")
	}
}

// On a radar with no visible project, Apply on an unchanged empty selection keeps it
// saved, so the next repository to arrive stays filtered out.
func TestProjectFilterEmptyRadarKeepsEmptySelection(t *testing.T) {
	archived := inRepo(worklist.WorkItem{ID: "done-a"}, "a/one")
	archived.Meta.CompletedAt = time.Now()
	store := worklist.NewMemoryStore()
	store.Seed("u1", archived)
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, &recordPipeline{}, reconnectProviders{false}, false, nil)
	c := postFilter(t, h, url.Values{})
	if c == nil || c.MaxAge <= 0 {
		t.Fatalf("expected the empty selection to stay saved, got %+v", c)
	}
	if v, _ := url.ParseQuery(c.Value); len(decodeSelection(v)) != 0 || decodeSelection(v) == nil {
		t.Fatalf("saved %q, want an empty selection", c.Value)
	}
}

// A filtered page offers "Show all projects", a plain submit that clears the filter
// without JavaScript — the only way back on a radar with no project left to check.
// Apply stays the form's first (default) button.
func TestProjectFilterShowAllButton(t *testing.T) {
	archived := inRepo(worklist.WorkItem{ID: "done-a"}, "a/one")
	archived.Meta.CompletedAt = time.Now()
	store := worklist.NewMemoryStore()
	store.Seed("u1", archived, inRepo(worklist.WorkItem{ID: "x"}, "b/two"))
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, &recordPipeline{}, reconnectProviders{false}, false, nil)

	page := getIndex(t, h, filterCookie("a/one"))
	apply, showAll := strings.Index(page, ">Apply</button>"), strings.Index(page, `name="all" value="1">Show all projects</button>`)
	if showAll < 0 || apply < 0 || apply > showAll {
		t.Fatalf("expected Apply, then Show all projects, in the filtered page's form (apply=%d, showAll=%d)", apply, showAll)
	}
	if all := getIndex(t, h, nil); strings.Contains(all, "Show all projects") {
		t.Error("an unfiltered page has nothing to show all of")
	}

	store.Seed("u1", archived) // the radar empties
	if c := postFilter(t, h, url.Values{"all": {"1"}}); c == nil || c.MaxAge >= 0 {
		t.Fatalf("Show all projects must clear the filter on an empty radar, got %+v", c)
	}
}

// A narrowed selection lists each selected project under the toolbar, linked to its
// pull-request queue; an unfiltered radar, or an empty selection, lists none.
func TestSelectedProjectsLinkToPullRequests(t *testing.T) {
	h := projectsFixture(t)
	page := getIndex(t, h, postFilter(t, h, url.Values{"project": {"b/two", "a/one"}}))
	list := page[strings.Index(page, `<ul class="zz-selected-projects"`):]
	list = list[:strings.Index(list, "</ul>")]
	for _, want := range []string{
		`<a href="https://github.com/a/one/pulls" target="_blank" rel="noopener noreferrer"`,
		`>a/one</a>`,
		`<a href="https://github.com/b/two/pulls"`,
	} {
		if !strings.Contains(list, want) {
			t.Errorf("selected-projects list lacks %q:\n%s", want, list)
		}
	}
	if strings.Contains(list, "c/three") {
		t.Error("an unselected project is listed")
	}
	if strings.Index(list, "a/one") > strings.Index(list, "b/two") {
		t.Error("selected projects are not listed by name")
	}

	if page := getIndex(t, h, nil); strings.Contains(page, "zz-selected-projects") {
		t.Error("an unfiltered radar lists selected projects")
	}
	if page := getIndex(t, h, postFilter(t, h, url.Values{"project": {"x/none"}})); strings.Contains(page, "zz-selected-projects") {
		t.Error("an empty selection lists selected projects")
	}
}

// A saved selection that no longer narrows the radar — the other project's last item
// closed, so the checklist reads "1 of 1" — lists no tags, though the filter stays
// saved and clearable.
func TestSelectedProjectsHiddenWhenFilterCoversRadar(t *testing.T) {
	store := worklist.NewMemoryStore()
	store.Seed("u1", worklist.WorkItem{
		ID: "github:a/one#1", Source: "github", Type: worklist.TypeIssue,
		GitHub: worklist.GitHubRef{Repo: "a/one", Number: 1, Title: "title-a/one-1"},
		Meta:   worklist.Metadata{Origin: worklist.OriginUser, Urgency: 0.5, Rank: 0.5},
	})
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, &recordPipeline{}, reconnectProviders{false}, false, nil)
	saved := &http.Cookie{Name: projectsCookie, Value: url.Values{"u": {"u1"}, "p": {"a/one", "b/two"}}.Encode()}
	page := getIndex(t, h, saved)
	if !strings.Contains(page, "Projects: 1 of 1") || !strings.Contains(page, "Show all projects") {
		t.Fatalf("expected the saved filter to stay visible and clearable:\n%s", page)
	}
	if strings.Contains(page, "zz-selected-projects") {
		t.Error("tags listed although the selection covers every project on the radar")
	}
}

// Only a well-formed "owner/name" gets a link; anything else is named without one.
func TestProjectPullsURL(t *testing.T) {
	for name, want := range map[string]string{
		"kubernetes-sigs/cluster-api": "https://github.com/kubernetes-sigs/cluster-api/pulls",
		"a/b.go_x-y":                  "https://github.com/a/b.go_x-y/pulls",
		"":                            "",
		"noslash":                     "",
		"a/b/c":                       "",
		"a/..":                        "",
		"a/.":                         "",
		"a/b?x=1":                     "",
		"a/b#c":                       "",
		"javascript:alert(1)/x":       "",
		"a /b":                        "",
	} {
		if got := (ProjectOption{Name: name}).PullsURL(); got != want {
			t.Errorf("PullsURL(%q) = %q, want %q", name, got, want)
		}
	}
}
