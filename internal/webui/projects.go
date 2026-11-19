package webui

import (
	"net/http"
	"net/url"
	"sort"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// The radar's project filter narrows the worklist to the items of the chosen GitHub
// repositories ("projects"). It never changes what is stored or how it is ranked: the
// visible items keep the full list's order (the filter drops items, it never re-sorts
// them). The radar's bulk actions (Review all PRs, Get 2nd Opinion, Reset
// Conversations) act on, and count, only the items the filter shows.
//
// The selection lives in a plain cookie, so it survives the Post/Redirect/Get round
// trips back to "/" (Refresh, Review all PRs, the thread's back link). No cookie means
// "Select all", the default. The cookie holds only repository names already on the
// user's own radar; it grants nothing and is read only to filter that user's items.

// projectsCookie names the cookie that holds the selected projects.
const projectsCookie = "gofer_projects"

// maxProjectsCookie bounds the encoded selection. Browsers drop a cookie past ~4 KB;
// a selection that would not fit is not saved, so the radar shows every project
// rather than a truncated choice.
const maxProjectsCookie = 3800

// ProjectOption is one entry in the project filter's checklist.
type ProjectOption struct {
	Name     string // the repository, "owner/name"
	Count    int    // items on the radar from this repository
	Selected bool   // shown on the radar
}

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

// selectedProjects reads the saved selection from the request. It returns nil (every
// project) when there is no usable cookie.
func selectedProjects(r *http.Request) map[string]bool {
	c, err := r.Cookie(projectsCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	v, err := url.ParseQuery(c.Value)
	if err != nil {
		return nil
	}
	sel := map[string]bool{}
	for _, name := range v["p"] {
		if name != "" {
			sel[name] = true
		}
	}
	return sel
}

// scopeTo returns the selection that applies to items: nil (every item) when there is
// no selection or it covers every repository in items. The view and the bulk actions
// both narrow through it, so an action covers exactly the items the radar shows.
func scopeTo(selected map[string]bool, items []worklist.WorkItem) map[string]bool {
	if selected != nil && covers(selected, projectOptions(items, nil)) {
		return nil
	}
	return selected
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

// FilterProjects handles POST /items/projects, the project filter's Apply. A checked
// "Select all" (form "all"), or a selection that covers every project on the radar,
// clears the filter; otherwise the checked projects (form "project", repeated) are
// saved. An empty selection is saved too, and shows no items. It redirects back to the
// radar (Post/Redirect/Get).
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
		// Keep only repositories on the user's radar: the cookie never carries a name
		// the page did not offer.
		opts := projectOptions(items, nil)
		present := make(map[string]bool, len(opts))
		for _, o := range opts {
			present[o.Name] = true
		}
		chosen := map[string]bool{}
		var names []string
		for _, name := range r.PostForm["project"] {
			if present[name] && !chosen[name] {
				chosen[name] = true
				names = append(names, name)
			}
		}
		sort.Strings(names)
		value = url.Values{"p": names}.Encode()
		if len(names) == 0 {
			value = "p=" // an explicit empty selection, distinct from no cookie
		}
		clear = covers(chosen, opts) || len(value) > maxProjectsCookie
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
