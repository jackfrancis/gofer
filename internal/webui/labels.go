package webui

import (
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// GitHub labels are drawn in their own colors. The CSP allows no inline style, so a
// color cannot go in a style attribute: each label carries a class naming its color
// (ghl-<hex>), and the page links /labels.css?c=<hex>,<hex>,… — a stylesheet holding a
// rule for each color the page uses, generated from the query alone. Only six hex
// digits ever reach it (worklist.NormalizeLabelColor), so the stylesheet carries
// nothing a stored value could smuggle in. A label with no known color (an item
// stored before gofer kept colors) keeps .zz-ghlabel's neutral outline.

// LabelStylesheetPath is where LabelCSS is served.
const LabelStylesheetPath = "/labels.css"

// maxLabelColors bounds the rules one stylesheet holds (and the colors one page
// asks for); a color past it falls back to the neutral outline.
const maxLabelColors = 512

// ghLabel is one GitHub label as the radar draws it.
type ghLabel struct {
	Name  string
	Class string // the color class, or "" for none
}

// ghLabels returns the item's labels in GitHub's order, each with its color class.
func ghLabels(it worklist.WorkItem) []ghLabel {
	out := make([]ghLabel, 0, len(it.Signals.Labels))
	for _, name := range it.Signals.Labels {
		l := ghLabel{Name: name}
		if c := it.GitHub.LabelColor(name); c != "" {
			l.Class = "ghl-" + c
		}
		out = append(out, l)
	}
	return out
}

// labelStylesheet returns the URL of the stylesheet coloring the items' labels, or
// "" if none has a color. The colors are sorted so one set of colors is one URL,
// which the browser caches.
func labelStylesheet(items []worklist.WorkItem) string {
	seen := map[string]bool{}
	var colors []string
	for _, it := range items {
		for _, name := range it.Signals.Labels {
			if c := it.GitHub.LabelColor(name); c != "" && !seen[c] {
				seen[c] = true
				colors = append(colors, c)
			}
		}
	}
	if len(colors) == 0 {
		return ""
	}
	slices.Sort(colors)
	return LabelStylesheetPath + "?c=" + strings.Join(colors[:min(len(colors), maxLabelColors)], ",")
}

// LabelCSS serves the label stylesheet: a rule per color the c query names. It is a
// pure function of the URL, so it is cacheable for good and needs no session.
func (h *Handler) LabelCSS(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	seen := map[string]bool{}
	for _, raw := range strings.Split(r.URL.Query().Get("c"), ",") {
		c := worklist.NormalizeLabelColor(raw)
		if c == "" || seen[c] {
			continue
		}
		if len(seen) == maxLabelColors {
			break
		}
		seen[c] = true
		b.WriteString(labelRule(c))
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write([]byte(b.String()))
}

// labelRule is the rule for color c (six lowercase hex digits), drawn as GitHub's
// light theme draws a label: filled with the color, with black text on a light
// color and white on a dark one, and an outline in a darker shade only when the
// color is nearly white, so it still stands off the page.
func labelRule(c string) string {
	rgb, _ := strconv.ParseUint(c, 16, 32)
	r, g, b := float64(rgb>>16&0xff), float64(rgb>>8&0xff), float64(rgb&0xff)
	lightness := (0.2126*r + 0.7152*g + 0.0722*b) / 255 // perceived
	text := "#ffffff"
	if lightness > 0.453 {
		text = "#000000"
	}
	border := "#" + c
	if lightness > 0.96 {
		border = darken(r, g, b, 0.25)
	}
	return fmt.Sprintf(".ghl-%s{background-color:#%s;color:%s;border-color:%s}\n", c, c, text, border)
}

// darken returns the color lowered by by in HSL lightness, as #rrggbb.
func darken(r, g, b, by float64) string {
	r, g, b = r/255, g/255, b/255
	hi, lo := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	l := (hi + lo) / 2
	var h, s float64
	if d := hi - lo; d > 0 {
		s = d / (1 - math.Abs(2*l-1))
		switch hi {
		case r:
			h = math.Mod((g-b)/d, 6)
		case g:
			h = (b-r)/d + 2
		default:
			h = (r-g)/d + 4
		}
		h *= 60
		if h < 0 {
			h += 360
		}
	}
	l = math.Max(0, l-by)
	// HSL back to RGB.
	ch := (1 - math.Abs(2*l-1)) * s
	x := ch * (1 - math.Abs(math.Mod(h/60, 2)-1))
	m := l - ch/2
	var r1, g1, b1 float64
	switch {
	case h < 60:
		r1, g1, b1 = ch, x, 0
	case h < 120:
		r1, g1, b1 = x, ch, 0
	case h < 180:
		r1, g1, b1 = 0, ch, x
	case h < 240:
		r1, g1, b1 = 0, x, ch
	case h < 300:
		r1, g1, b1 = x, 0, ch
	default:
		r1, g1, b1 = ch, 0, x
	}
	to := func(v float64) int { return int(math.Round((v + m) * 255)) }
	return fmt.Sprintf("#%02x%02x%02x", to(r1), to(g1), to(b1))
}
