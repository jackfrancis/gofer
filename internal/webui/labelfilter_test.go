package webui

import (
	"context"
	"html/template"
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

// labelsFixture is a radar over two repositories whose labels overlap by name in
// different cases ("bug"/"Bug"), with one unlabeled item and one hidden item carrying a
// label no ranked item has.
func labelsFixture(t *testing.T) *Handler {
	t.Helper()
	item := func(repo string, n int, urgency float64, labels ...string) worklist.WorkItem {
		return worklist.WorkItem{
			ID: "github:" + repo + "#" + strconv.Itoa(n), Source: "github", Type: worklist.TypeIssue,
			GitHub:  worklist.GitHubRef{Repo: repo, Number: n, Title: "t-" + strconv.Itoa(n), LabelColors: map[string]string{"bug": "d73a4a", "Bug": "ee0701"}},
			Signals: worklist.Signals{Labels: labels},
			Meta:    worklist.Metadata{Origin: worklist.OriginUser, Urgency: urgency, Rank: urgency},
		}
	}
	hidden := item("b/two", 6, 0.1, "wontfix")
	hidden.Meta.HiddenAt = time.Now()
	store := worklist.NewMemoryStore()
	store.Seed("u1",
		item("a/one", 1, 0.9, "bug", "area/api"),
		item("b/two", 2, 0.8, "Bug"),
		item("a/one", 3, 0.7, "docs"),
		item("b/two", 4, 0.6),
		item("a/one", 5, 0.5, "area/api"),
		hidden,
	)
	return New(fakeSessions{&session.User{ID: "u1"}}, store, &recordPipeline{}, reconnectProviders{false}, false, nil)
}

var labelTitles = []string{"t-1", "t-2", "t-3", "t-4", "t-5", "t-6"}

func postLabelsRaw(h *Handler, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/items/labels", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.FilterLabels(rec, req)
	return rec
}

// postLabels submits the label filter and returns the cookie it set (nil for none).
func postLabels(t *testing.T, h *Handler, form url.Values) *http.Cookie {
	t.Helper()
	rec := postLabelsRaw(h, form)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("status = %d, location = %q; want 303 to /", rec.Code, rec.Header().Get("Location"))
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == labelsCookie {
			return c
		}
	}
	return nil
}

// getIndexWith renders the radar with the given cookies (a deleting cookie is skipped).
func getIndexWith(t *testing.T, h *Handler, cookies ...*http.Cookie) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range cookies {
		if c != nil && c.MaxAge >= 0 {
			req.AddCookie(c)
		}
	}
	rec := httptest.NewRecorder()
	h.Index(rec, req)
	return rec.Body.String()
}

// labelCookie is the cookie FilterLabels saves for u1 and the given label keys ("" for
// "No label").
func labelCookie(keys ...string) *http.Cookie {
	return &http.Cookie{Name: labelsCookie, Value: url.Values{"u": {"u1"}}.Encode() + "&" + encodeLabels(labelSel(keys...))}
}

func labelSel(keys ...string) *labelSelection {
	sel := &labelSelection{names: map[string]bool{}}
	for _, k := range keys {
		if k == "" {
			sel.none = true
		} else {
			sel.names[k] = true
		}
	}
	return sel
}

// With no saved selection every item shows, and the checklist lists each label once,
// merged by name ignoring case, across projects and the hidden section, sorted, then
// "No label".
func TestLabelFilterDefaultsToAll(t *testing.T) {
	page := getIndexWith(t, labelsFixture(t))
	if got := order(page, labelTitles...); len(got) != len(labelTitles) {
		t.Fatalf("unfiltered radar shows %v", got)
	}
	for _, want := range []string{
		"Labels: All",
		`action="/items/labels"`,
		`name="label" value="area/api" data-filter-label checked`,
		`name="label" value="bug" data-filter-label checked> <span class="zz-filter-name"><span class="Label zz-ghlabel ghl-d73a4a">bug</span></span> <span class="zz-filter-count">2</span>`,
		`name="label" value="wontfix" data-filter-label checked`,
		`name="nolabel" value="1" data-filter-label checked> <span class="zz-filter-name zz-filter-none">No label</span> <span class="zz-filter-count">1</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("default page lacks %q", want)
		}
	}
	if n := strings.Count(page, `name="label" value="bug"`); n != 1 {
		t.Errorf("bug offered %d times, want once", n)
	}
	got := order(page, `area/api`, `bug`, `docs`, `wontfix`, `No label`)
	if strings.Join(got, ",") != "area/api,bug,docs,wontfix,No label" {
		t.Errorf("checklist order = %v", got)
	}
}

// A selection shows items carrying any selected label, case-insensitively, in the
// full list's order; the hidden section is narrowed too.
func TestLabelFilterShowsAnySelectedLabel(t *testing.T) {
	h := labelsFixture(t)
	c := postLabels(t, h, url.Values{"label": {"BUG", "docs"}})
	if c == nil || c.MaxAge <= 0 || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("expected a saved, HttpOnly, SameSite=Lax cookie, got %+v", c)
	}
	page := getIndexWith(t, h, c)
	if got := strings.Join(order(page, labelTitles...), ","); got != "t-1,t-2,t-3" {
		t.Fatalf("filtered radar = %s, want t-1,t-2,t-3", got)
	}
	for _, s := range []string{"Labels: 2 of 5", "3 of 5 items on your radar", `aria-label="Selected labels"`} {
		if !strings.Contains(page, s) {
			t.Errorf("filtered page lacks %q", s)
		}
	}
	if strings.Contains(page, "Hidden (") {
		t.Error("the hidden item carries no selected label, so the hidden section should be empty")
	}

	none := getIndexWith(t, h, labelCookie(""))
	if got := strings.Join(order(none, labelTitles...), ","); got != "t-4" {
		t.Fatalf(`"No label" shows %s, want t-4`, got)
	}
}

// The label filter is downstream of the project filter: it narrows the project's items,
// still offers labels from deselected projects, and counts only the selected projects.
func TestLabelFilterDownstreamOfProjects(t *testing.T) {
	h := labelsFixture(t)
	page := getIndexWith(t, h, filterCookie("a/one"), labelCookie("bug"))
	if got := strings.Join(order(page, labelTitles...), ","); got != "t-1" {
		t.Fatalf("a/one ∩ bug = %s, want t-1", got)
	}
	for _, s := range []string{
		`value="bug" data-filter-label checked> <span class="zz-filter-name"><span class="Label zz-ghlabel ghl-d73a4a">bug</span></span> <span class="zz-filter-count">1</span>`,
		`<span class="Label zz-ghlabel">wontfix</span></span> <span class="zz-filter-count">0</span>`,
		`zz-filter-none">No label</span> <span class="zz-filter-count">0</span>`,
		"1 of 5 items on your radar",
	} {
		if !strings.Contains(page, s) {
			t.Errorf("page lacks %q", s)
		}
	}
	empty := getIndexWith(t, h, filterCookie("a/one"), labelCookie("wontfix"))
	if !strings.Contains(empty, "No items match the selected projects and labels.") {
		t.Error("an empty intersection should say so")
	}
}

// "Select all", "Show all labels", or checking every option clears the saved selection;
// unknown labels are dropped and an empty selection is kept.
func TestLabelFilterSelectAllAndSanitize(t *testing.T) {
	h := labelsFixture(t)
	rendered := encodeLabels(labelSel("area/api", "bug", "docs", "wontfix", ""))
	for name, form := range map[string]url.Values{
		"select all":     {"all": {"1"}, "label": {"bug"}},
		"every option":   {"label": {"bug", "area/api", "docs", "wontfix"}, "nolabel": {"1"}, "offered": {rendered}},
		"every, unknown": {"label": {"Bug", "area/api", "docs", "wontfix", "nope"}, "nolabel": {"1"}, "offered": {rendered}},
	} {
		if c := postLabels(t, h, form); c == nil || c.MaxAge >= 0 {
			t.Errorf("%s: expected the cookie cleared, got %+v", name, c)
		}
	}
	c := postLabels(t, h, url.Values{"label": {"docs", "not-on-radar"}})
	v, _ := url.ParseQuery(c.Value)
	if strings.Join(v["l"], ",") != "docs" || v.Get("u") != "u1" || v.Get("n") != "" {
		t.Fatalf("saved %q, want docs for u1", c.Value)
	}
	empty := postLabels(t, h, url.Values{})
	if empty == nil || empty.MaxAge <= 0 {
		t.Fatalf("expected an empty selection saved, got %+v", empty)
	}
	page := getIndexWith(t, h, empty)
	if len(order(page, labelTitles...)) != 0 || !strings.Contains(page, "Labels: 0 of 5") || !strings.Contains(page, "No items match the selected labels.") {
		t.Fatal("an empty label selection shows nothing and says so")
	}
}

// The label filter is per account and needs a session.
func TestLabelFilterPerAccountAndSession(t *testing.T) {
	anon := &Handler{sessions: fakeSessions{}, store: worklist.NewMemoryStore(), now: time.Now}
	if c := postLabels(t, anon, url.Values{"label": {"bug"}}); c != nil {
		t.Fatalf("anonymous request set %+v", c)
	}
	other := labelsFixture(t)
	other.sessions = fakeSessions{&session.User{ID: "u2"}}
	items, _ := other.store.List(context.Background(), "u1")
	other.store.(*worklist.MemoryStore).Seed("u2", items...)
	if page := getIndexWith(t, other, labelCookie("docs")); !strings.Contains(page, "Labels: All") {
		t.Fatal("another account must not inherit the saved label selection")
	}
}

// A label selection too large for the cookie is refused and the radar says why.
func TestLabelFilterTooLargeKeepsPrevious(t *testing.T) {
	var labels []string
	var items []worklist.WorkItem
	for i := 0; i < 150; i++ {
		l := strings.Repeat("l", 30) + strconv.Itoa(i)
		labels = append(labels, l)
		items = append(items, worklist.WorkItem{ID: l, GitHub: worklist.GitHubRef{Repo: "o/r", Number: i, Title: l}, Signals: worklist.Signals{Labels: []string{l}}})
	}
	store := worklist.NewMemoryStore()
	store.Seed("u1", items...)
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, &recordPipeline{}, reconnectProviders{false}, false, nil)
	rec := postLabelsRaw(h, url.Values{"label": labels[1:]})
	if loc := rec.Header().Get("Location"); loc != "/?labels="+projectsTooMany || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("location = %q, cookies = %v; want the too-many notice and no cookie", loc, rec.Result().Cookies())
	}
	req := httptest.NewRequest(http.MethodGet, "/?labels="+projectsTooMany, nil)
	req.AddCookie(labelCookie(labels[0]))
	page := httptest.NewRecorder()
	h.Index(page, req)
	if body := page.Body.String(); !strings.Contains(body, "Too many labels selected to save") || !strings.Contains(body, "Labels: 1 of 150") {
		t.Fatal("expected the notice and the previous filter")
	}
}

// The bulk actions act on what the page showed: the posted label scope (never the
// cookie), intersected with the project scope. Reset also clears hidden items with a
// selected label.
func TestBulkActionsFollowLabelScope(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	opts := []ModelOption{{Value: "c0|default", Model: "default"}, {Value: "c1|alt", Model: "alt"}}
	labeled := func(it worklist.WorkItem, repo string, labels ...string) worklist.WorkItem {
		it = inRepo(it, repo)
		it.Signals.Labels = labels
		return it
	}
	seed := func() *worklist.MemoryStore {
		hidden := labeled(reviewedByModel("hidden-bug", "default", now), "a/one", "bug")
		hidden.Meta.HiddenAt = now.Add(-time.Hour)
		store := worklist.NewMemoryStore()
		store.Seed("u1",
			labeled(worklist.WorkItem{ID: "new-bug-a", Type: worklist.TypePullRequest}, "a/one", "Bug"),
			labeled(worklist.WorkItem{ID: "new-bug-b", Type: worklist.TypePullRequest}, "b/two", "bug"),
			labeled(worklist.WorkItem{ID: "new-docs", Type: worklist.TypePullRequest}, "a/one", "docs"),
			labeled(worklist.WorkItem{ID: "new-none", Type: worklist.TypePullRequest}, "a/one"),
			labeled(reviewedByModel("rev-bug", "default", now), "a/one", "bug"),
			labeled(reviewedByModel("rev-docs", "default", now), "a/one", "docs"),
			hidden,
		)
		return store
	}
	post := func(store *worklist.MemoryStore, pipe *recordPipeline, path string, form url.Values, handle func(*Handler, http.ResponseWriter, *http.Request)) {
		h := &Handler{sessions: fakeSessions{&session.User{ID: "u1"}}, store: store, pipeline: pipe, convEnabled: true, options: opts, now: func() time.Time { return now }}
		form.Set("choice", "c1|alt")
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(labelCookie("docs")) // another tab's filter: never consulted
		handle(h, httptest.NewRecorder(), req)
	}
	cleared := func(store *worklist.MemoryStore) string {
		items, _ := store.List(context.Background(), "u1")
		var ids []string
		for _, it := range items {
			if len(it.Thread) == 0 && (strings.HasPrefix(it.ID, "rev") || strings.HasPrefix(it.ID, "hidden")) {
				ids = append(ids, it.ID)
			}
		}
		sort.Strings(ids)
		return strings.Join(ids, ",")
	}
	for _, tc := range []struct {
		name                    string
		form                    url.Values
		review, second, cleared string
	}{
		{"bug", url.Values{"labels": {encodeLabels(labelSel("bug"))}}, "new-bug-a,new-bug-b", "rev-bug", "hidden-bug,rev-bug"},
		{"bug in a/one", url.Values{"labels": {encodeLabels(labelSel("bug"))}, "scope": {scopeOf("a/one")}}, "new-bug-a", "rev-bug", "hidden-bug,rev-bug"},
		{"no label", url.Values{"labels": {encodeLabels(labelSel(""))}}, "new-none", "", ""},
		{"no label filter", url.Values{}, "new-bug-a,new-bug-b,new-docs,new-none", "rev-bug,rev-docs", "hidden-bug,rev-bug,rev-docs"},
		{"empty selection", url.Values{"labels": {"f=1"}}, "", "", ""},
		{"garbled", url.Values{"labels": {"%zz"}}, "", "", ""},
		{"unrecognized", url.Values{"labels": {"l=bug"}}, "", "", ""},
		// Only an absent field means every label: a present but empty or malformed one
		// must not widen the action.
		{"present but empty", url.Values{"labels": {""}}, "", "", ""},
		{"repeated", url.Values{"labels": {encodeLabels(labelSel("bug")), encodeLabels(labelSel("docs"))}}, "", "", ""},
		{"bad filter marker", url.Values{"labels": {"f=0&l=bug"}}, "", "", ""},
		{"bad none marker", url.Values{"labels": {"f=1&n=yes"}}, "", "", ""},
		{"empty label", url.Values{"labels": {"f=1&l="}}, "", "", ""},
		{"unknown key", url.Values{"labels": {"f=1&l=bug&x=1"}}, "", "", ""},
	} {
		pipe := &recordPipeline{}
		post(seed(), pipe, "/items/review-all", tc.form, (*Handler).ReviewAllPRs)
		got := append([]string(nil), pipe.reviewed...)
		sort.Strings(got)
		if strings.Join(got, ",") != tc.review {
			t.Errorf("%s: Review all PRs dispatched %v, want %q", tc.name, got, tc.review)
		}
		pipe = &recordPipeline{}
		post(seed(), pipe, "/items/second-opinion-all", tc.form, (*Handler).SecondOpinionAllPRs)
		if got := strings.Join(pipe.secondOpinions, ","); got != tc.second {
			t.Errorf("%s: Get 2nd Opinion dispatched %q, want %q", tc.name, got, tc.second)
		}
		store := seed()
		post(store, &recordPipeline{}, "/items/reset-conversations", tc.form, (*Handler).ResetConversations)
		if got := cleared(store); got != tc.cleared {
			t.Errorf("%s: Reset Conversations cleared %q, want %q", tc.name, got, tc.cleared)
		}
	}
}

// The radar's bulk counts follow the label filter, and every bulk form posts the
// page's label selection, narrowed to the labels the checklist offers.
func TestBulkFormsCarryLabelScope(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	mk := func(id string, labels ...string) worklist.WorkItem {
		it := inRepo(worklist.WorkItem{ID: id, Type: worklist.TypePullRequest, Signals: worklist.Signals{Labels: labels}}, "a/one")
		it.Meta.Origin = worklist.OriginUser
		return it
	}
	store := worklist.NewMemoryStore()
	store.Seed("u1", mk("p1", "bug"), mk("p2", "bug"), mk("p3", "docs"))
	opts := []ModelOption{{Value: "c0|default", Model: "default"}, {Value: "c1|alt", Model: "alt"}}
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, &recordPipeline{}, reconnectProviders{false}, true, opts)
	h.now = func() time.Time { return now }

	page := getIndexWith(t, h, labelCookie("bug", "gone-label"))
	if !strings.Contains(page, "Review all PRs (2)") {
		t.Error("Review all PRs should count only the bug PRs")
	}
	want := `<input type="hidden" name="labels" value="` + template.HTMLEscapeString(encodeLabels(labelSel("bug"))) + `">`
	if n := strings.Count(page, want); n != 2 { // one Review all PRs form per model
		t.Errorf("found %d bulk forms carrying %q, want 2", n, want)
	}
	if strings.Contains(page, `name="scope"`) {
		t.Error("with no project filter the forms carry no project scope")
	}
}

// A single-option radar hides the label menu, unless a selection is saved.
func TestLabelFilterReachableWhileSaved(t *testing.T) {
	one := []worklist.WorkItem{{ID: "i1", GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1, Title: "t"}, Signals: worklist.Signals{Labels: []string{"bug"}}}}
	if out := renderPage(t, pageData{View: "worklist", Items: one, Labels: labelOptions(one, one, nil)}); strings.Contains(out, "/items/labels") {
		t.Fatal("a radar with a single label option should not show the label filter")
	}
	store := worklist.NewMemoryStore()
	store.Seed("u1", one...)
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, &recordPipeline{}, reconnectProviders{false}, false, nil)
	if page := getIndexWith(t, h, labelCookie("bug")); !strings.Contains(page, "Labels: 1 of 1") || !strings.Contains(page, "Show all labels") {
		t.Fatal("a saved label selection must keep the filter reachable")
	}
}

// Whether Apply means "Select all" is judged against the checklist the user saw. An
// option they left unchecked whose last item retired before Apply keeps the selection
// narrowed, so the next new label does not join it; a form that names no rendered
// checklist never clears.
func TestLabelFilterSelectAllJudgedByRenderedChecklist(t *testing.T) {
	store := worklist.NewMemoryStore()
	bug := worklist.WorkItem{ID: "i1", GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1, Title: "t-bug"}, Signals: worklist.Signals{Labels: []string{"bug"}}}
	store.Seed("u1", bug)
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, &recordPipeline{}, reconnectProviders{false}, false, nil)

	// The page offered bug and docs; docs, unchecked, has since left the radar.
	c := postLabels(t, h, url.Values{"label": {"bug"}, "offered": {encodeLabels(labelSel("bug", "docs"))}})
	if c == nil || c.MaxAge <= 0 {
		t.Fatalf("a narrowed selection must stay saved, got %+v", c)
	}
	if v, _ := url.ParseQuery(c.Value); strings.Join(v["l"], ",") != "bug" {
		t.Fatalf("saved %q, want bug", c.Value)
	}
	store.Seed("u1", bug, worklist.WorkItem{ID: "i2", GitHub: worklist.GitHubRef{Repo: "o/r", Number: 2, Title: "t-feature"}, Signals: worklist.Signals{Labels: []string{"feature"}}})
	if got := order(getIndexWith(t, h, c), "t-bug", "t-feature"); strings.Join(got, ",") != "t-bug" {
		t.Fatalf("a label new since the selection must stay filtered out, radar shows %v", got)
	}

	for name, offered := range map[string][]string{"no offered": nil, "garbled": {"%zz"}, "empty": {""}, "repeated": {"f=1&l=bug", "f=1&l=bug"}} {
		form := url.Values{"label": {"bug"}}
		if offered != nil {
			form["offered"] = offered
		}
		if c := postLabels(t, h, form); c == nil || c.MaxAge <= 0 {
			t.Errorf("%s: without a readable rendered checklist Apply must save, not clear; got %+v", name, c)
		}
	}

	// The radar renders the checklist's options for Apply to post back.
	page := getIndexWith(t, labelsFixture(t))
	want := `<input type="hidden" name="offered" value="` + template.HTMLEscapeString(encodeLabels(labelSel("area/api", "bug", "docs", "wontfix", ""))) + `">`
	if !strings.Contains(page, want) {
		t.Errorf("label checklist lacks %q", want)
	}
}

// When only hidden items match, the radar does not claim nothing matches.
func TestLabelFilterHiddenOnlyMatch(t *testing.T) {
	page := getIndexWith(t, labelsFixture(t), labelCookie("wontfix"))
	if !strings.Contains(page, "No ranked items match the selected labels; only hidden ones do.") || !strings.Contains(page, "Hidden (1)") {
		t.Fatal("expected the hidden-only wording and the hidden item")
	}
	if strings.Contains(page, "No items match") {
		t.Fatal("must not say no items match while a hidden item does")
	}
	projects := getIndexWith(t, labelsFixture(t), filterCookie("b/two"), labelCookie("wontfix"))
	if !strings.Contains(projects, "No ranked items match the selected projects and labels; only hidden ones do.") {
		t.Fatal("expected the hidden-only wording with both filters")
	}
}

// Labels are merged by Unicode case folding, as strings.EqualFold compares, not just
// lowercasing: "ΟΣ" and "ος" are one option, and selecting it shows both items.
func TestLabelKeyFoldsUnicodeCase(t *testing.T) {
	for _, pair := range [][2]string{{"ΟΣ", "ος"}, {"Bug", "bUG"}, {"K", "\u212a"}, {"straße", "STRAßE"}} {
		if labelKey(pair[0]) != labelKey(pair[1]) {
			t.Errorf("labelKey(%q) = %q, labelKey(%q) = %q; want equal", pair[0], labelKey(pair[0]), pair[1], labelKey(pair[1]))
		}
	}
	if labelKey("Bug") != "bug" {
		t.Errorf("ASCII keys stay lowercase, got %q", labelKey("Bug"))
	}
	items := []worklist.WorkItem{
		{ID: "a", Signals: worklist.Signals{Labels: []string{"ΟΣ"}}},
		{ID: "b", Signals: worklist.Signals{Labels: []string{"ος"}}},
	}
	opts := labelOptions(items, items, nil)
	if len(opts) != 1 || opts[0].Count != 2 {
		t.Fatalf("options = %+v, want one option counting both items", opts)
	}
	if got := filterLabels(items, labelSel(opts[0].Key)); len(got) != 2 {
		t.Fatalf("selecting %q shows %d items, want 2", opts[0].Key, len(got))
	}
}
