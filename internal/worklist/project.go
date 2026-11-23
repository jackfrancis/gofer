package worklist

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"unicode/utf8"
)

// A run's read is bounded however the stored data grew, so an item too large to read
// whole is PROJECTED: its free-form fields (titles, labels, model rationales, …) are
// cut to a bounded size. Projection only ever happens on the way out to a run, and
// is reversible on the way back: the item records, per cut field, a fingerprint of
// the value the run received (WorkItem.Projected). When the run writes the item back,
// MergeRunItems restores every cut field the run left as it received it from the
// stored item, and keeps a field the run rewrote (rank's fresh proposal, research's
// fresh adjustment). So a projection can never be persisted.
const (
	// MaxBriefBytes bounds the encoded size of one item without its thread. A larger
	// item is projected. It is well under MaxSelectBytes, so a page of selected items
	// always fits its budget.
	MaxBriefBytes = 256 << 10
	// projectedString and projectedList bound a cut string and list.
	projectedString = 512
	projectedList   = 32
)

// projectable is a field projection may cut: its name (the key in
// WorkItem.Projected), and how to read, write and cut it.
type projectable struct {
	name string
	get  func(*WorkItem) any
	set  func(dst, src *WorkItem) // copy the field from src to dst
	cut  func(*WorkItem)
}

// projectables are the item's free-form fields: everything whose size the data,
// rather than the schema, decides. IDs, enums, numbers and times are left whole.
// A cut keeps what the score reads (the strongest label, every distinct reason), so
// a projected item scores as the whole item does: the ranker and Discuss see the
// radar's ranking.
var projectables = []projectable{
	{"github.title", func(w *WorkItem) any { return w.GitHub.Title },
		func(d, s *WorkItem) { d.GitHub.Title = s.GitHub.Title },
		func(w *WorkItem) { w.GitHub.Title = cutString(w.GitHub.Title) }},
	{"github.url", func(w *WorkItem) any { return w.GitHub.URL },
		func(d, s *WorkItem) { d.GitHub.URL = s.GitHub.URL },
		func(w *WorkItem) { w.GitHub.URL = cutString(w.GitHub.URL) }},
	{"signals.reasons", func(w *WorkItem) any { return w.Signals.Reasons },
		func(d, s *WorkItem) { d.Signals.Reasons = slices.Clone(s.Signals.Reasons) },
		func(w *WorkItem) {
			if w.Signals.Reasons != nil {
				// Relevance reads the best reason and the count, which saturates long
				// before the bound: keep every distinct reason, then fill the bound.
				seen := map[Reason]bool{}
				r := keepByPriority(w.Signals.Reasons, func(r Reason) float64 {
					if seen[r] {
						return 0
					}
					seen[r] = true
					return 1
				})
				for i := range r {
					r[i] = Reason(cutString(string(r[i])))
				}
				w.Signals.Reasons = r
			}
		}},
	{"signals.related_active", func(w *WorkItem) any { return w.Signals.RelatedActive },
		func(d, s *WorkItem) { d.Signals.RelatedActive = slices.Clone(s.Signals.RelatedActive) },
		func(w *WorkItem) { w.Signals.RelatedActive = cutStrings(w.Signals.RelatedActive) }},
	{"signals.labels", func(w *WorkItem) any { return w.Signals.Labels },
		func(d, s *WorkItem) { d.Signals.Labels = slices.Clone(s.Signals.Labels) },
		func(w *WorkItem) {
			// Impact reads the strongest label: keep the strongest labels. A label is
			// cut only past projectedString bytes, far longer than GitHub allows.
			if w.Signals.Labels != nil {
				w.Signals.Labels = cutStrings(keepByPriority(w.Signals.Labels, func(l string) float64 {
					weight, _ := labelImpact(l)
					return weight
				}))
			}
		}},
	// After signals.labels, whose cut it follows: it keeps the colors of the labels
	// that survived, and only well-formed ones, so it is bounded as they are.
	{"github.label_colors", func(w *WorkItem) any { return w.GitHub.LabelColors },
		func(d, s *WorkItem) { d.GitHub.LabelColors = maps.Clone(s.GitHub.LabelColors) },
		func(w *WorkItem) {
			if w.GitHub.LabelColors == nil {
				return
			}
			// kept stays nil until a color survives: an empty map would be fingerprinted
			// as {} but reach the run as nothing (omitempty) and come back nil, so the
			// write-back would see a rewrite and drop the stored colors.
			var kept map[string]string
			for _, l := range w.Signals.Labels {
				if c := w.GitHub.LabelColor(l); c != "" {
					if kept == nil {
						kept = map[string]string{}
					}
					kept[l] = c
				}
			}
			w.GitHub.LabelColors = kept
		}},
	// Enrich stores the CI summary bounded (CISummary.AddFailed), but the store holds
	// whatever a run wrote, so the failed-job names (and their links) are cut like any
	// list. The cut
	// counts the names it drops as omitted, and leaves Failed nil rather than empty,
	// as the JSON a run receives would.
	{projectedCI, func(w *WorkItem) any { return w.GitHub.CI },
		func(d, s *WorkItem) { d.GitHub.CI = cloneCI(s.GitHub.CI) },
		func(w *WorkItem) {
			c := cloneCI(w.GitHub.CI)
			if c == nil {
				return
			}
			if len(c.Failed) > projectedList {
				c.FailedOmitted += len(c.Failed) - projectedList
			}
			c.Failed = cutStrings(c.Failed)
			if len(c.Failed) == 0 {
				c.Failed = nil
			}
			// The links follow the names they belong to; a cut link would be a broken
			// one, so a link past projectedString is dropped instead.
			c.FailedURLs = c.FailedURLs[:min(len(c.FailedURLs), len(c.Failed))]
			for i, l := range c.FailedURLs {
				if len(l) > projectedString {
					c.FailedURLs[i] = ""
				}
			}
			if len(c.FailedURLs) == 0 {
				c.FailedURLs = nil
			}
			w.GitHub.CI = c
		}},
	{"signals.roadmap_themes", func(w *WorkItem) any { return w.Signals.RoadmapThemes },
		func(d, s *WorkItem) { d.Signals.RoadmapThemes = slices.Clone(s.Signals.RoadmapThemes) },
		func(w *WorkItem) { w.Signals.RoadmapThemes = cutStrings(w.Signals.RoadmapThemes) }},
	{"signals.topics", func(w *WorkItem) any { return w.Signals.Topics },
		func(d, s *WorkItem) { d.Signals.Topics = slices.Clone(s.Signals.Topics) },
		func(w *WorkItem) { w.Signals.Topics = cutStrings(w.Signals.Topics) }},
	{"signals.proposed", func(w *WorkItem) any { return w.Signals.Proposed },
		func(d, s *WorkItem) { d.Signals.Proposed = clonePtr(s.Signals.Proposed) },
		func(w *WorkItem) {
			if p := clonePtr(w.Signals.Proposed); p != nil {
				p.Rationale = cutString(p.Rationale)
				w.Signals.Proposed = p
			}
		}},
	{"signals.research", func(w *WorkItem) any { return w.Signals.Research },
		func(d, s *WorkItem) {
			d.Signals.Research = clonePtr(s.Signals.Research)
			if d.Signals.Research != nil {
				d.Signals.Research.Turns = slices.Clone(d.Signals.Research.Turns)
			}
		},
		func(w *WorkItem) {
			if r := clonePtr(w.Signals.Research); r != nil {
				r.Rationale = cutString(r.Rationale)
				if len(r.Turns) > projectedList {
					// A partial list of the turns the rationale read would let the
					// visibility check pass without seeing them all (a hidden turn past
					// the cut). With no list the research is unverifiable, so
					// ResearchReflectsVisibleThread withholds it.
					r.Turns = nil
				} else {
					r.Turns = cutStrings(r.Turns)
				}
				w.Signals.Research = r
			}
		}},
	{"gofer.contributions", func(w *WorkItem) any { return w.Meta.Contributions },
		func(d, s *WorkItem) { d.Meta.Contributions = slices.Clone(s.Meta.Contributions) },
		func(w *WorkItem) {
			c := slices.Clone(w.Meta.Contributions[:min(len(w.Meta.Contributions), projectedList)])
			for i := range c {
				c[i].Signal, c[i].Detail = cutString(c[i].Signal), cutString(c[i].Detail)
			}
			w.Meta.Contributions = c
		}},
	{"gofer.rationale", func(w *WorkItem) any { return w.Meta.Rationale },
		func(d, s *WorkItem) { d.Meta.Rationale = s.Meta.Rationale },
		func(w *WorkItem) { w.Meta.Rationale = cutString(w.Meta.Rationale) }},
}

// Project returns item (without its thread) cut to at most about MaxBriefBytes, and
// marks each field it cut (WorkItem.Projected) so a write-back restores it. An item
// that already fits is returned unchanged.
func Project(item WorkItem) WorkItem {
	item.Thread = nil
	if encodedSize(item) <= MaxBriefBytes {
		return item
	}
	projected := map[string]string{}
	for _, f := range projectables {
		before := fingerprint(f.get(&item))
		f.cut(&item)
		if after := fingerprint(f.get(&item)); after != before {
			projected[f.name] = after
		}
	}
	if len(projected) > 0 {
		item.Projected = projected
	}
	return item
}

// restoreProjected undoes a projection on a run's write-back: every field the run
// received cut, and returned unchanged, takes the stored value again. A field the
// run rewrote keeps the run's value. It always clears Projected, which is never
// stored.
func restoreProjected(incoming *WorkItem, stored WorkItem) {
	if len(incoming.Projected) == 0 {
		incoming.Projected = nil
		return
	}
	for _, f := range projectables {
		if fp, ok := incoming.Projected[f.name]; ok && fingerprint(f.get(incoming)) == fp {
			f.set(incoming, &stored)
		}
	}
	incoming.Projected = nil
}

// fingerprint identifies a field's value.
func fingerprint(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(fmt.Sprint(v))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:12])
}

func encodedSize(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

func cutString(s string) string { return truncateUTF8(s, projectedString) }

func cutStrings(list []string) []string {
	if list == nil {
		return nil
	}
	out := slices.Clone(list[:min(len(list), projectedList)])
	for i := range out {
		out[i] = cutString(out[i])
	}
	return out
}

// keepByPriority returns at most projectedList entries of list, in their original
// order: those with the highest priority, the earlier entry first on a tie. priority
// is called once per entry, in order.
func keepByPriority[T any](list []T, priority func(T) float64) []T {
	if len(list) <= projectedList {
		return slices.Clone(list)
	}
	weight := make([]float64, len(list))
	idx := make([]int, len(list))
	for i, v := range list {
		weight[i], idx[i] = priority(v), i
	}
	slices.SortStableFunc(idx, func(a, b int) int { return cmp.Compare(weight[b], weight[a]) })
	idx = idx[:projectedList]
	slices.Sort(idx)
	out := make([]T, len(idx))
	for j, i := range idx {
		out[j] = list[i]
	}
	return out
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	c := *p
	return &c
}

// projectedCI is the CI summary's key in WorkItem.Projected.
const projectedCI = "github.ci"

// CIProjected reports whether the item's CI summary was cut by projection, so it is
// not the stored summary and a comparison against it says nothing.
func (w *WorkItem) CIProjected() bool {
	_, cut := w.Projected[projectedCI]
	return cut
}

// SetCI replaces the item's CI summary with one freshly read and drops its
// projection marker, so the write-back stores ci as it is: otherwise a fresh
// summary that happens to match the cut one (a link projection dropped and GitHub
// since withdrew) would be taken for the unchanged cut and the stored one restored.
func (w *WorkItem) SetCI(ci *CISummary) {
	w.GitHub.CI = ci
	if w.CIProjected() {
		w.Projected = maps.Clone(w.Projected)
		delete(w.Projected, projectedCI)
	}
}

// cloneCI deep-copies a CI summary, so a cut never writes through to the stored
// item's list.
func cloneCI(c *CISummary) *CISummary {
	if c == nil {
		return nil
	}
	out := *c
	out.Failed = slices.Clone(c.Failed)
	out.FailedURLs = slices.Clone(c.FailedURLs)
	return &out
}

// truncateUTF8 cuts s to at most n bytes without splitting a character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 0 {
		return ""
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n-- // s[n] continues the character the cut would split
	}
	return s[:n]
}
