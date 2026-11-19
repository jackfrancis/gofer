package worklist

import (
	"cmp"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"time"
)

// Selection names a bounded read of one owner's worklist, without the items'
// threads. A run never needs the whole worklist with every thread, which grows
// without bound: Discuss and research read one item, thread included (Find, GET
// /agent/worklist/item); ranking reads the top N, and enrichment and ingest page
// through the items, all without threads. The agent plane applies a Selection on the
// web tier (GET /agent/worklist), so what crosses to a runtime stays small however
// large the worklist grows.
type Selection struct {
	// Open keeps only items not yet retired (Meta.CompletedAt unset).
	Open bool
	// After and Limit page the selection in ID order: the page holds up to Limit
	// items whose ID sorts after After. Keyset paging, so an item added or removed
	// between pages neither shifts nor repeats the rest. Limit <= 0 is no limit.
	After string
	Limit int
	// Top, when > 0, selects the Top items with the highest baseline score
	// (Score(item, now).Rank) instead of paging; After and Limit are ignored.
	Top int
}

// Select applies sel to an owner's items and returns the selected items and, for a
// paged selection with more to read, the cursor to pass as the next page's After
// (empty when this page is the last). It copies what it returns; items is not
// changed.
func Select(items []WorkItem, sel Selection, now time.Time) (page []WorkItem, next string) {
	out := make([]WorkItem, 0, len(items))
	for _, it := range items {
		if sel.Open && !it.Meta.CompletedAt.IsZero() {
			continue
		}
		it.Thread = nil // ThreadReadAt stays: it is a timestamp, not a body
		out = append(out, it)
	}
	if sel.Top > 0 {
		rank := make(map[string]float64, len(out))
		for _, it := range out {
			rank[it.ID] = Score(it, now).Rank
		}
		slices.SortStableFunc(out, func(a, b WorkItem) int {
			if c := cmp.Compare(rank[b.ID], rank[a.ID]); c != 0 {
				return c
			}
			return cmp.Compare(a.ID, b.ID)
		})
		if len(out) > sel.Top {
			out = out[:sel.Top]
		}
		return out, ""
	}
	slices.SortFunc(out, func(a, b WorkItem) int { return cmp.Compare(a.ID, b.ID) })
	if sel.After != "" {
		i, _ := slices.BinarySearchFunc(out, sel.After, func(it WorkItem, id string) int { return cmp.Compare(it.ID, id) })
		for i < len(out) && out[i].ID <= sel.After {
			i++
		}
		out = out[i:]
	}
	if sel.Limit > 0 && len(out) > sel.Limit {
		out = out[:sel.Limit]
		return out, out[len(out)-1].ID
	}
	return out, ""
}

// Find returns a copy of the owner's item with id, thread included, and whether it
// was there.
func Find(items []WorkItem, id string) (WorkItem, bool) {
	for _, it := range items {
		if it.ID == id {
			it.Thread = slices.Clone(it.Thread)
			return it, true
		}
	}
	return WorkItem{}, false
}

// MaxSelectLimit bounds one page and one top-N read on the agent plane. A page of
// items without threads at this size is a small fraction of the plane's 8 MiB
// response limit; a larger request is clamped to it and paged.
const MaxSelectLimit = 1000

// Query is the query a Selection travels as on GET /agent/worklist. The threads=0
// parameter is always present: it marks the request as a selection, which the server
// bounds, rather than the legacy whole-worklist read.
func (sel Selection) Query() url.Values {
	q := url.Values{"threads": {"0"}}
	if sel.Open {
		q.Set("open", "1")
	}
	if sel.Top > 0 {
		q.Set("top", strconv.Itoa(sel.Top))
		return q
	}
	if sel.After != "" {
		q.Set("after", sel.After)
	}
	if sel.Limit > 0 {
		q.Set("limit", strconv.Itoa(sel.Limit))
	}
	return q
}

// ParseSelection reads a Selection from a GET /agent/worklist query. ok is false for
// a request that carries none (no threads parameter): the legacy whole-worklist read.
// A selection never carries threads (threads must be 0), and the page size and top N
// are clamped to MaxSelectLimit, so every selection the server answers is bounded.
func ParseSelection(q url.Values) (sel Selection, ok bool, err error) {
	if !q.Has("threads") {
		return Selection{}, false, nil
	}
	flag := func(name string) (bool, error) {
		switch q.Get(name) {
		case "", "0":
			return false, nil
		case "1":
			return true, nil
		}
		return false, errors.New("worklist: " + name + " must be 0 or 1")
	}
	count := func(name string) (int, error) {
		v := q.Get(name)
		if v == "" {
			return 0, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, errors.New("worklist: " + name + " must be a non-negative integer")
		}
		return n, nil
	}
	if q.Get("threads") != "0" {
		return Selection{}, true, errors.New("worklist: a selection omits threads (threads=0); read one item for its thread")
	}
	if sel.Open, err = flag("open"); err != nil {
		return Selection{}, true, err
	}
	if sel.Top, err = count("top"); err != nil {
		return Selection{}, true, err
	}
	if sel.Limit, err = count("limit"); err != nil {
		return Selection{}, true, err
	}
	sel.After = q.Get("after")
	if sel.Top > 0 {
		return Selection{Open: sel.Open, Top: min(sel.Top, MaxSelectLimit)}, true, nil
	}
	if sel.Limit == 0 || sel.Limit > MaxSelectLimit {
		sel.Limit = MaxSelectLimit
	}
	return sel, true, nil
}
