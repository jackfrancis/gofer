package worklist

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
				r := slices.Clone(w.Signals.Reasons[:min(len(w.Signals.Reasons), projectedList)])
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
		func(w *WorkItem) { w.Signals.Labels = cutStrings(w.Signals.Labels) }},
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

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	c := *p
	return &c
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
