package webui

import (
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// The radar's project filter narrows the worklist to the items of the chosen GitHub
// repositories ("projects"). It never changes what is stored or how it is ranked: the
// visible items keep the full list's order (the filter drops items, it never re-sorts
// them).
//
// The saved selection lives in a plain cookie, so it survives the Post/Redirect/Get
// round trips back to "/" (Refresh, Review all PRs, the thread's back link). No cookie
// means "Select all", the default. The cookie names the account that saved it and is
// ignored for any other, so a second account in the same browser starts from "Select
// all". It holds only repository names that were on that user's radar; it grants
// nothing and is read only to filter that user's items.
//
// The cookie drives what the radar shows, never what a bulk action touches. Each bulk
// form (Review all PRs, Get 2nd Opinion, Reset Conversations) carries the selection
// the page was rendered with (form "scope"), and the action covers exactly the items
// that page showed: in those projects and on the radar (worklist.WorkItem.OnRadar). So
// a filter changed in another tab cannot widen an action submitted from this one.

// projectsCookie names the cookie that holds the saved selection.
const projectsCookie = "gofer_projects"

// maxProjectsCookie bounds an encoded filter cookie (the project filter's or the label
// filter's). Browsers drop a cookie past ~4 KB, so a selection that would not fit is
// refused and the previous filter is kept.
const maxProjectsCookie = 3800

// projectsTooMany is the query value Index reads to explain a refused selection.
const projectsTooMany = "too-many"

// ProjectOption is one entry in the project filter's checklist.
type ProjectOption struct {
	Name     string // the repository, "owner/name"
	Count    int    // items the radar lists from this repository (ranked or hidden)
	Selected bool   // shown on the radar
}

// repoName matches a GitHub repository name, "owner/name": an owner of letters,
// digits and hyphens, and a name of letters, digits, '.', '_' and '-'.
var repoName = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)

// PullsURL is the repository's open pull-request queue on GitHub, or "" when Name is
// not a well-formed "owner/name" (the page then names the project without a link).
func (o ProjectOption) PullsURL() string {
	if !repoName.MatchString(o.Name) {
		return ""
	}
	if _, name, _ := strings.Cut(o.Name, "/"); name == "." || name == ".." {
		return ""
	}
	return "https://github.com/" + o.Name + "/pulls"
}

// listedItems returns every item the radar lists: the ranked items, the waiting
// section and the hidden section. The project filter offers their repositories and
// narrows all three.
func listedItems(res worklist.Resolution) []worklist.WorkItem {
	return slices.Concat(res.Items, res.Waiting, res.Hidden)
}

// listed reports whether the radar lists it, ranked or in the hidden section.
func listed(it worklist.WorkItem) bool { return it.OnRadar() || it.InHiddenList() }

// projectOptions lists the repositories present in items, by name, with their item
// counts, marking the ones selected (every one when selected is nil).
func projectOptions(items []worklist.WorkItem, selected map[string]bool) []ProjectOption {
	counts := map[string]int{}
	for _, it := range items {
		counts[it.GitHub.Repo]++
	}
	opts := make([]ProjectOption, 0, len(counts))
	for name, n := range counts {
		opts = append(opts, ProjectOption{Name: name, Count: n, Selected: selected == nil || selected[name]})
	}
	sort.Slice(opts, func(i, j int) bool { return opts[i].Name < opts[j].Name })
	return opts
}

// filterProjects returns the items whose repository is selected, in their original
// order. A nil selection keeps every item.
func filterProjects(items []worklist.WorkItem, selected map[string]bool) []worklist.WorkItem {
	if selected == nil {
		return items
	}
	kept := make([]worklist.WorkItem, 0, len(items))
	for _, it := range items {
		if selected[it.GitHub.Repo] {
			kept = append(kept, it)
		}
	}
	return kept
}

// inScope reports whether it belongs to a selected project (always, with no selection).
func inScope(selected map[string]bool, it worklist.WorkItem) bool {
	return selected == nil || selected[it.GitHub.Repo]
}

// encodeSelection encodes a selection as repeated "p" values, by name. An empty
// selection encodes as "p=", distinct from no selection at all.
func encodeSelection(selected map[string]bool) string {
	names := make([]string, 0, len(selected))
	for name := range selected {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "p="
	}
	return url.Values{"p": names}.Encode()
}

// decodeSelection reads the "p" values of v. It returns nil (every project) when v
// has none, and an empty, non-nil selection for "p=".
func decodeSelection(v url.Values) map[string]bool {
	names, ok := v["p"]
	if !ok {
		return nil
	}
	sel := map[string]bool{}
	for _, name := range names {
		if name != "" {
			sel[name] = true
		}
	}
	return sel
}

// savedSelection reads ownerID's saved selection from the request's cookie. It returns
// nil (every project) with no usable cookie, or one another account saved.
func savedSelection(r *http.Request, ownerID string) map[string]bool {
	c, err := r.Cookie(projectsCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	v, err := url.ParseQuery(c.Value)
	if err != nil || v.Get("u") != ownerID {
		return nil
	}
	return decodeSelection(v)
}

// formSelection reads the selection a bulk form was rendered with (form "scope"). Only
// an absent scope means the page showed every project: a filtered page always posts a
// non-empty encoding, so a present scope that is not exactly one strict encoding
// (parseSelection), empty included, acts on nothing. ok is false when the form cannot be read:
// the caller must then change nothing, since a lost scope would otherwise read as every
// project. The page's forms are URL-encoded, and that is the only body accepted: Go's
// ParseForm leaves any other body (multipart, plain text) unread, which would drop the
// scope silently. A POST with no body at all carries no scope and reads as every
// project, like an unfiltered page's form.
func formSelection(r *http.Request) (selected map[string]bool, ok bool) {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/x-www-form-urlencoded" {
			return nil, false
		}
	} else if r.ContentLength != 0 {
		return nil, false // a body of unstated type
	}
	if err := r.ParseForm(); err != nil {
		return nil, false
	}
	scopes, present := r.PostForm["scope"]
	if !present {
		return nil, true
	}
	if len(scopes) == 1 {
		if sel := parseSelection(scopes[0]); sel != nil {
			return sel, true
		}
	}
	return map[string]bool{}, true // a malformed scope acts on nothing rather than on everything
}

// parseSelection strictly decodes a selection encodeSelection wrote, or returns nil if
// raw is anything else: one or more "p" values and no other key, where an empty value
// is allowed only as the lone "p=" of an empty selection.
func parseSelection(raw string) map[string]bool {
	v, err := url.ParseQuery(raw)
	if err != nil || len(v) != 1 {
		return nil
	}
	names, ok := v["p"]
	if !ok {
		return nil
	}
	if len(names) == 1 && names[0] == "" {
		return map[string]bool{}
	}
	for _, name := range names {
		if name == "" {
			return nil
		}
	}
	return decodeSelection(v)
}

// coversRenderedProjects reports whether chosen (the submitted names) checks every
// project the checklist rendered (form "offered": exactly one encodeSelection value
// naming at least one project).
func coversRenderedProjects(chosen map[string]bool, offered []string) bool {
	if len(offered) != 1 {
		return false
	}
	rendered := parseSelection(offered[0])
	if len(rendered) == 0 {
		return false
	}
	for name := range rendered {
		if !chosen[name] {
			return false
		}
	}
	return true
}

// offeredProjects encodes the projects a checklist renders, which its form posts back
// (form "offered") so Apply can tell whether every rendered project was checked.
func offeredProjects(opts []ProjectOption) string {
	all := make(map[string]bool, len(opts))
	for _, o := range opts {
		all[o.Name] = true
	}
	return encodeSelection(all)
}

// shownSelection narrows a saved selection to the projects the checklist offers
// (opts, the repositories the radar lists). It is what each bulk form posts back, so an
// action never reaches a repository the page does not name — say one whose last
// visible item closed after it was selected, which may still hold archived threads.
// A nil selection (every project) stays nil.
func shownSelection(selected map[string]bool, opts []ProjectOption) map[string]bool {
	if selected == nil {
		return nil
	}
	shown := map[string]bool{}
	for _, o := range opts {
		if selected[o.Name] {
			shown[o.Name] = true
		}
	}
	return shown
}

// FilterProjects handles POST /items/projects, the project filter's Apply. The checked
// projects (form "project", repeated) are saved for the signed-in account, narrowed to
// the projects on the radar now. "Select all" or the "Show all projects" button (both
// form "all"), or checking every project the checklist rendered (form "offered", at
// least one), clears the filter instead. Whether every project was checked is judged
// against the checklist the user saw, never the store at POST time: a project left
// unchecked whose last item retired meanwhile must not turn a narrowed selection into
// "Select all", which would admit the next new project. A form without a readable
// "offered" never clears.
// An empty selection is saved too, and shows no items. A selection too large for the
// cookie is refused and the previous filter kept. It redirects back to the radar
// (Post/Redirect/Get).
func (h *Handler) FilterProjects(w http.ResponseWriter, r *http.Request) {
	user := h.sessions.CurrentUser(r)
	if user == nil || r.ParseForm() != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	clear := r.PostForm.Get("all") != ""
	var value string
	if !clear {
		items, err := h.store.List(r.Context(), user.ID)
		if err != nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		// Keep only repositories the radar lists (ranked or hidden): the cookie never
		// carries a name the page did not offer.
		var onPage []worklist.WorkItem
		for _, it := range items {
			if listed(it) {
				onPage = append(onPage, it)
			}
		}
		opts := projectOptions(onPage, nil)
		present := make(map[string]bool, len(opts))
		for _, o := range opts {
			present[o.Name] = true
		}
		submitted := map[string]bool{}
		chosen := map[string]bool{}
		for _, name := range r.PostForm["project"] {
			submitted[name] = true
			if present[name] {
				chosen[name] = true
			}
		}
		// Checking every rendered project means "Select all" — but only when the page
		// rendered a project to check: on an empty radar an unchanged empty selection
		// must stay saved.
		clear = coversRenderedProjects(submitted, r.PostForm["offered"])
		value = url.Values{"u": {user.ID}}.Encode() + "&" + encodeSelection(chosen)
		if !clear && len(value) > maxProjectsCookie {
			http.Redirect(w, r, "/?projects="+projectsTooMany, http.StatusSeeOther)
			return
		}
	}
	setFilterCookie(w, r, projectsCookie, value, clear)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// isHTTPS reports whether the request reached gofer over HTTPS, directly or through
// the TLS-terminating ingress.
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}
