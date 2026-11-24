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

// maxProjectsCookie bounds the encoded cookie. Browsers drop a cookie past ~4 KB, so
// a selection that would not fit is refused and the previous filter is kept.
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

// covers reports whether selected includes every repository in opts, i.e. the
// selection is no narrower than "Select all".
func covers(selected map[string]bool, opts []ProjectOption) bool {
	for _, o := range opts {
		if !selected[o.Name] {
			return false
		}
	}
	return true
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

// formSelection reads the selection a bulk form was rendered with (form "scope"). No
// scope means the page showed every project. ok is false when the form cannot be read:
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
	scope := r.PostForm.Get("scope")
	if scope == "" {
		return nil, true
	}
	v, err := url.ParseQuery(scope)
	if err != nil {
		return map[string]bool{}, true // a garbled scope acts on nothing rather than on everything
	}
	if sel := decodeSelection(v); sel != nil {
		return sel, true
	}
	return map[string]bool{}, true
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
// projects (form "project", repeated) are saved for the signed-in account; checking
// every project on the radar (at least one), "Select all", or the "Show all projects"
// button (both form "all") clears the filter instead.
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
		chosen := map[string]bool{}
		for _, name := range r.PostForm["project"] {
			if present[name] {
				chosen[name] = true
			}
		}
		// Checking every project means "Select all" — but only when there is a project to
		// check: on an empty radar an unchanged empty selection must stay saved.
		clear = len(opts) > 0 && covers(chosen, opts)
		value = url.Values{"u": {user.ID}}.Encode() + "&" + encodeSelection(chosen)
		if !clear && len(value) > maxProjectsCookie {
			http.Redirect(w, r, "/?projects="+projectsTooMany, http.StatusSeeOther)
			return
		}
	}
	c := &http.Cookie{
		Name:     projectsCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   365 * 24 * 60 * 60,
	}
	if clear {
		c.Value, c.MaxAge = "", -1
	}
	http.SetCookie(w, c)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// isHTTPS reports whether the request reached gofer over HTTPS, directly or through
// the TLS-terminating ingress.
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}
