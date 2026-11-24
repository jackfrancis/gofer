package webui

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
	"unicode"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// The radar's label filter narrows the worklist, after the project filter, to the items
// carrying any of the chosen GitHub labels. Like the project filter it never changes
// what is stored or how it is ranked: it drops items, never re-sorts them.
//
// The checklist offers every label on the radar (ranked or hidden), across all
// projects, whatever the project filter selects; each label's count is the items it
// would show within the selected projects. Labels are merged by name, ignoring case (as
// GitHub compares them), so "bug" in one repository and "Bug" in another are one entry.
// A "No label" entry selects the items that carry none.
//
// The saved selection lives in its own cookie, bound to the account that saved it, like
// the project filter's: no cookie means "Select all". It is a view preference only. Each
// bulk form posts the label selection its page was rendered with (form "labels"),
// narrowed to the labels the checklist names, beside the project scope, and the action
// covers only the items both select.

// labelsCookie names the cookie that holds the saved label selection.
const labelsCookie = "gofer_labels"

// labelSelection is a label filter's selection. A nil *labelSelection selects every
// item (no filter).
type labelSelection struct {
	names map[string]bool // selected labels, by labelKey
	none  bool            // items that carry no label
}

// labelKey is the name a label is compared by: GitHub compares label names ignoring
// case. It folds case the way strings.EqualFold does (Unicode simple folding), so names
// EqualFold calls equal share one key ("ΟΣ" and "ος" included, which lowercasing alone
// keeps apart). Each rune maps to the lowercase of the smallest rune in its folding
// orbit, which every member of the orbit shares, so ASCII keys stay lowercase.
func labelKey(name string) string { return strings.Map(foldRune, name) }

// foldRune maps r to its case-folding orbit's canonical rune (see labelKey).
func foldRune(r rune) rune {
	m := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		m = min(m, f)
	}
	return unicode.ToLower(m)
}

// matches reports whether the selection shows it: always with no selection; otherwise
// when it carries a selected label, or carries none and "No label" is selected.
func (s *labelSelection) matches(it worklist.WorkItem) bool {
	if s == nil {
		return true
	}
	if len(it.Signals.Labels) == 0 {
		return s.none
	}
	for _, l := range it.Signals.Labels {
		if s.names[labelKey(l)] {
			return true
		}
	}
	return false
}

// LabelOption is one entry in the label filter's checklist.
type LabelOption struct {
	Key      string // labelKey of the label, the form value; "" for the "No label" entry
	Name     string // the label as first seen on the radar
	Class    string // the label's color class (ghl-<hex>), or ""
	Count    int    // items in the selected projects carrying it (or, for None, no label)
	Selected bool   // shown on the radar
	None     bool   // the "No label" entry
}

// labelOptions lists the labels found on all (every listed item, in every project), by
// name, ending with "No label" when some item carries none. Each count comes from
// counted (the listed items in the selected projects). Each option is marked selected
// by sel (every one when sel is nil).
func labelOptions(all, counted []worklist.WorkItem, sel *labelSelection) []LabelOption {
	byKey := map[string]*LabelOption{}
	var keys []string
	anyNone := false
	for _, it := range all {
		if len(it.Signals.Labels) == 0 {
			anyNone = true
		}
		for _, name := range it.Signals.Labels {
			k := labelKey(name)
			o := byKey[k]
			if o == nil {
				o = &LabelOption{Key: k, Name: name, Selected: sel == nil || sel.names[k]}
				byKey[k] = o
				keys = append(keys, k)
			}
			if o.Class == "" {
				if c := it.GitHub.LabelColor(name); c != "" {
					o.Class = "ghl-" + c
				}
			}
		}
	}
	noneCount := 0
	for _, it := range counted {
		if len(it.Signals.Labels) == 0 {
			noneCount++
			continue
		}
		seen := map[string]bool{}
		for _, name := range it.Signals.Labels {
			k := labelKey(name)
			if o := byKey[k]; o != nil && !seen[k] {
				seen[k] = true
				o.Count++
			}
		}
	}
	sort.Strings(keys)
	opts := make([]LabelOption, 0, len(keys)+1)
	for _, k := range keys {
		opts = append(opts, *byKey[k])
	}
	if anyNone {
		opts = append(opts, LabelOption{Name: "No label", None: true, Count: noneCount, Selected: sel == nil || sel.none})
	}
	return opts
}

// filterLabels returns the items sel shows, in their original order. A nil selection
// keeps every item.
func filterLabels(items []worklist.WorkItem, sel *labelSelection) []worklist.WorkItem {
	if sel == nil {
		return items
	}
	kept := make([]worklist.WorkItem, 0, len(items))
	for _, it := range items {
		if sel.matches(it) {
			kept = append(kept, it)
		}
	}
	return kept
}

// selects reports whether sel includes the option.
func (s *labelSelection) selects(o LabelOption) bool {
	if o.None {
		return s.none
	}
	return s.names[o.Key]
}

// shownLabelSelection narrows a saved selection to the options the checklist offers,
// which is what each bulk form posts back. A nil selection stays nil.
func shownLabelSelection(sel *labelSelection, opts []LabelOption) *labelSelection {
	if sel == nil {
		return nil
	}
	shown := &labelSelection{names: map[string]bool{}}
	for _, o := range opts {
		switch {
		case !sel.selects(o):
		case o.None:
			shown.none = true
		default:
			shown.names[o.Key] = true
		}
	}
	return shown
}

// encodeLabels encodes a non-nil selection: "f=1" (a filter is set, so an empty
// selection is distinct from none), the label keys as repeated "l" values, by name, and
// "n=1" when "No label" is selected.
func encodeLabels(sel *labelSelection) string {
	names := make([]string, 0, len(sel.names))
	for k := range sel.names {
		names = append(names, k)
	}
	sort.Strings(names)
	v := url.Values{"f": {"1"}}
	if len(names) > 0 {
		v["l"] = names
	}
	if sel.none {
		v.Set("n", "1")
	}
	return v.Encode()
}

// decodeLabels reads a selection encoded by encodeLabels. It returns nil (every item)
// when v sets no filter.
func decodeLabels(v url.Values) *labelSelection {
	if _, ok := v["f"]; !ok {
		return nil
	}
	sel := &labelSelection{names: map[string]bool{}, none: v.Get("n") != ""}
	for _, k := range v["l"] {
		if k != "" {
			sel.names[labelKey(k)] = true
		}
	}
	return sel
}

// savedLabels reads ownerID's saved label selection from the request's cookie. It
// returns nil (every item) with no usable cookie, or one another account saved.
func savedLabels(r *http.Request, ownerID string) *labelSelection {
	c, err := r.Cookie(labelsCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	v, err := url.ParseQuery(c.Value)
	if err != nil || v.Get("u") != ownerID {
		return nil
	}
	return decodeLabels(v)
}

// formLabels reads the label selection a bulk form was rendered with (form "labels"),
// from a form formSelection has already parsed. Only an absent field means the page
// showed every label: a filtered page always posts a non-empty encoding. Anything else
// that is not exactly one well-formed encoding (an empty value, repeated fields, a
// garbled or unrecognized one) selects nothing rather than everything.
func formLabels(r *http.Request) *labelSelection {
	raws, present := r.PostForm["labels"]
	if !present {
		return nil
	}
	if len(raws) != 1 {
		return &labelSelection{}
	}
	if sel := parseLabels(raws[0]); sel != nil {
		return sel
	}
	return &labelSelection{}
}

// parseLabels strictly decodes a selection encodeLabels wrote, or returns nil if raw is
// anything else: exactly one "f=1", at most one "n=1", non-empty "l" values, and no
// other key.
func parseLabels(raw string) *labelSelection {
	v, err := url.ParseQuery(raw)
	if err != nil {
		return nil
	}
	if f := v["f"]; len(f) != 1 || f[0] != "1" {
		return nil
	}
	n, hasN := v["n"]
	if hasN && (len(n) != 1 || n[0] != "1") {
		return nil
	}
	for k, vals := range v {
		switch k {
		case "f", "n":
		case "l":
			for _, l := range vals {
				if l == "" {
					return nil
				}
			}
		default:
			return nil
		}
	}
	return decodeLabels(v)
}

// viewScope is what a bulk form's page showed: the projects and the labels it was
// filtered to (nil for no filter).
type viewScope struct {
	projects map[string]bool
	labels   *labelSelection
}

// includes reports whether it is in both the scope's projects and its labels.
func (s viewScope) includes(it worklist.WorkItem) bool {
	return inScope(s.projects, it) && s.labels.matches(it)
}

// formScope reads the scope a bulk form posts back (formSelection, then formLabels).
// ok is false when the form cannot be read: the caller must then change nothing.
func formScope(r *http.Request) (viewScope, bool) {
	projects, ok := formSelection(r)
	if !ok {
		return viewScope{}, false
	}
	return viewScope{projects: projects, labels: formLabels(r)}, true
}

// FilterLabels handles POST /items/labels, the label filter's Apply. The checked labels
// (form "label", repeated, by key) and "No label" (form "nolabel") are saved for the
// signed-in account, narrowed to the labels on the radar now. "Select all" or "Show all
// labels" (both form "all"), or checking every option the checklist rendered (form
// "offered", at least one), clears the filter instead. Whether every option was checked
// is judged against the checklist the user saw, never the store at POST time: an option
// left unchecked whose last item retired meanwhile must not turn a narrowed selection
// into "Select all", which would admit the next new label. A form without a readable
// "offered" never clears. An empty selection is saved too, and shows no items. A
// selection too large for the cookie is refused and the previous filter kept. It
// redirects back to the radar (Post/Redirect/Get).
func (h *Handler) FilterLabels(w http.ResponseWriter, r *http.Request) {
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
		// Keep only labels the radar lists (ranked or hidden), in any project: the
		// cookie never carries a label the page did not offer.
		var onPage []worklist.WorkItem
		for _, it := range items {
			if listed(it) {
				onPage = append(onPage, it)
			}
		}
		opts := labelOptions(onPage, nil, nil)
		offered := make(map[string]bool, len(opts))
		noneOffered := false
		for _, o := range opts {
			if o.None {
				noneOffered = true
			} else {
				offered[o.Key] = true
			}
		}
		submitted := &labelSelection{names: map[string]bool{}, none: r.PostForm.Get("nolabel") != ""}
		for _, k := range r.PostForm["label"] {
			submitted.names[labelKey(k)] = true
		}
		clear = coversRendered(submitted, r.PostForm["offered"])
		chosen := &labelSelection{names: map[string]bool{}, none: noneOffered && submitted.none}
		for k := range submitted.names {
			if offered[k] {
				chosen.names[k] = true
			}
		}
		value = url.Values{"u": {user.ID}}.Encode() + "&" + encodeLabels(chosen)
		if !clear && len(value) > maxProjectsCookie {
			http.Redirect(w, r, "/?labels="+projectsTooMany, http.StatusSeeOther)
			return
		}
	}
	setFilterCookie(w, r, labelsCookie, value, clear)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// coversRendered reports whether submitted checks every option of the rendered
// checklist (form "offered": exactly one encodeLabels value naming at least one option).
func coversRendered(submitted *labelSelection, offered []string) bool {
	if len(offered) != 1 {
		return false
	}
	rendered := parseLabels(offered[0])
	if rendered == nil || len(rendered.names) == 0 && !rendered.none {
		return false
	}
	if rendered.none && !submitted.none {
		return false
	}
	for k := range rendered.names {
		if !submitted.names[k] {
			return false
		}
	}
	return true
}

// offeredLabels encodes the options a checklist renders, which its form posts back
// (form "offered") so Apply can tell whether every rendered option was checked.
func offeredLabels(opts []LabelOption) string {
	all := &labelSelection{names: map[string]bool{}}
	for _, o := range opts {
		if o.None {
			all.none = true
		} else {
			all.names[o.Key] = true
		}
	}
	return encodeLabels(all)
}

// setFilterCookie saves a filter's cookie, or deletes it when clear (no filter).
func setFilterCookie(w http.ResponseWriter, r *http.Request, name, value string, clear bool) {
	c := &http.Cookie{
		Name:     name,
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
}
