package worklist

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"time"
)

// A run never reads the whole worklist: the threads grow without bound, and the agent
// plane caps a response at 8 MiB. It reads one item, with a window of its thread
// (ReadItem, GET /agent/worklist/item), or a Selection of items without their threads
// (SelectFrom, GET /agent/worklist/select). Every read is bounded by count and by
// encoded size, so what crosses to a runtime stays small however large the worklist
// grows. The same functions serve the in-process sink and the agent plane, so the
// bounds hold on every backend.
const (
	// MaxSelectLimit bounds the items in one selection page and the N of a top-N
	// read. A larger request is clamped to it.
	MaxSelectLimit = 1000
	// MaxSelectBytes bounds the encoded items in one selection page. A page that
	// would pass it ends early, with a cursor to the rest. It leaves half the agent
	// plane's 8 MiB response limit as headroom.
	MaxSelectBytes = 4 << 20
	// MaxThreadBytes bounds the encoded turns of the one item ReadItem returns (see
	// ThreadWindow).
	MaxThreadBytes = 4 << 20
)

// Selection names a bounded read of one owner's items, without their threads.
// Enrichment and ingest page through every item; ranking reads the top N.
type Selection struct {
	// Open keeps only items not yet retired (Meta.CompletedAt unset).
	Open bool
	// After is the cursor of the page to read: empty for the first, then the next
	// value the previous page returned.
	After string
	// Limit bounds a page of a paged read, which runs in ID order (keyset paging:
	// an item added or removed between pages neither shifts nor repeats the rest).
	// Zero, or more than MaxSelectLimit, is MaxSelectLimit.
	Limit int
	// Top, when > 0, reads the Top items with the highest baseline score
	// (Score(item, now).Rank), highest first, instead of paging by ID; Limit is
	// ignored. Equal scores keep the store's first-insertion order. It is clamped to
	// MaxSelectLimit, and its pages are positions in that ranking, so a score that
	// changes between pages can shift an item across a page boundary: a reader
	// drops an ID it already has.
	Top int
}

// normalized applies the bounds every selection is held to.
func (sel Selection) normalized() Selection {
	if sel.Top > 0 {
		return Selection{Open: sel.Open, After: sel.After, Top: min(sel.Top, MaxSelectLimit)}
	}
	sel.Top = 0
	if sel.Limit <= 0 || sel.Limit > MaxSelectLimit {
		sel.Limit = MaxSelectLimit
	}
	return sel
}

// Select applies sel to an owner's items and returns the page and the cursor of the
// next one (empty when this page is the last). It copies what it returns, without
// threads; items is not changed. A page holds at least one item when any remain, and
// stops before MaxSelectBytes.
func Select(items []WorkItem, sel Selection, now time.Time) (page []WorkItem, next string) {
	sel = sel.normalized()
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
		// Stable on ties: a fresh backfill stores the most recently active work
		// first, so a tied cutoff favors fresh work.
		slices.SortStableFunc(out, func(a, b WorkItem) int { return cmp.Compare(rank[b.ID], rank[a.ID]) })
		out = out[:min(len(out), sel.Top)]
		offset, _ := strconv.Atoi(sel.After) // ParseSelection rejects a bad one
		if offset < 0 || offset > len(out) {
			offset = len(out)
		}
		n := fitBytes(out[offset:], MaxSelectBytes)
		if offset+n < len(out) {
			next = strconv.Itoa(offset + n)
		}
		return out[offset : offset+n], next
	}
	slices.SortFunc(out, func(a, b WorkItem) int { return cmp.Compare(a.ID, b.ID) })
	i, _ := slices.BinarySearchFunc(out, sel.After, func(it WorkItem, id string) int { return cmp.Compare(it.ID, id) })
	for i < len(out) && out[i].ID <= sel.After {
		i++
	}
	out = out[i:]
	n := fitBytes(out[:min(len(out), sel.Limit)], MaxSelectBytes)
	if n < len(out) {
		next = out[n-1].ID
	}
	return out[:n], next
}

// fitBytes returns how many of items, from the start, fit in budget bytes of JSON —
// at least one, so a page always makes progress.
func fitBytes(items []WorkItem, budget int) int {
	used := 0
	for i, it := range items {
		b, err := json.Marshal(it)
		if err != nil {
			b = nil
		}
		used += len(b) + 1
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// SelectFrom applies sel to the owner's items in store, reading only what the
// selection needs: a paged read fetches the page's rows after the cursor, a top-N
// read the owner's items without threads.
func SelectFrom(ctx context.Context, store Store, ownerID string, sel Selection, now time.Time) ([]WorkItem, string, error) {
	sel = sel.normalized()
	q := BriefQuery{Open: sel.Open}
	if sel.Top == 0 {
		// One row past the page tells Select whether another page follows.
		q.After, q.Limit = sel.After, sel.Limit+1
	}
	items, err := store.Briefs(ctx, ownerID, q)
	if err != nil {
		return nil, "", err
	}
	page, next := Select(items, sel, now)
	return page, next, nil
}

// BriefQuery names the items Store.Briefs returns, without their threads.
type BriefQuery struct {
	// Open keeps only items not yet retired (Meta.CompletedAt unset).
	Open bool
	// With Limit > 0, Briefs returns up to Limit items whose ID sorts after After,
	// in ID order. Otherwise it returns every item, in first-insertion order.
	After string
	Limit int
}

// ApplyBriefQuery is Store.Briefs over items already in memory (in first-insertion
// order): the reference the stores' own implementations match.
func ApplyBriefQuery(items []WorkItem, q BriefQuery) []WorkItem {
	out := make([]WorkItem, 0, len(items))
	for _, it := range items {
		if q.Open && !it.Meta.CompletedAt.IsZero() {
			continue
		}
		if q.Limit > 0 && it.ID <= q.After {
			continue
		}
		it.Thread = nil
		out = append(out, it)
	}
	if q.Limit > 0 {
		slices.SortFunc(out, func(a, b WorkItem) int { return cmp.Compare(a.ID, b.ID) })
		out = out[:min(len(out), q.Limit)]
	}
	return out
}

// ReadItem returns the owner's item with id and a window of its thread
// (ThreadWindow) that keeps the turn named by turn. ok is false if the owner has no
// such item: another owner's item is never found.
func ReadItem(ctx context.Context, store Store, ownerID, id, turn string) (WorkItem, bool, error) {
	it, ok, err := store.Get(ctx, ownerID, id)
	if err != nil || !ok {
		return WorkItem{}, false, err
	}
	return ThreadWindow(it, turn, MaxThreadBytes), true, nil
}

// ThreadWindow returns a copy of item whose thread is cut to at most budget bytes of
// encoded turns, so one long conversation cannot outgrow a read. It keeps the window
// a run needs:
//
//   - the anchor: the turn named by turn, or, if turn is empty or gone, the most
//     recent user turn (the turn Discuss answers);
//   - every turn after the anchor, so a reply to it is still seen. One that does not
//     fit keeps its links and loses its content;
//   - as many visible turns before the anchor as fit, newest first. Older turns, and
//     hidden ones (which no model sees), are dropped.
//
// A key or reply link that a turn's position implied (TurnKey, a legacy reply) is
// written into the turn first, so the window answers TurnKey, Answered and ReplyTo
// exactly as the whole thread does, and a reply to a window turn lands on the right
// turn of the stored thread.
func ThreadWindow(item WorkItem, turn string, budget int) WorkItem {
	full := item
	th := slices.Clone(item.Thread)
	for i := range th {
		th[i].ID = full.TurnKey(i)
		if th[i].Role == RoleAgent && th[i].ReplyTo == "" && th[i].Notice == "" && i > 0 && full.Thread[i-1].Role == RoleUser {
			th[i].ReplyTo = full.TurnKey(i - 1)
		}
	}
	item.Thread = th
	if len(th) == 0 {
		return item
	}
	anchor, ok := -1, false
	if turn != "" {
		anchor, ok = item.TurnIndex(turn)
	}
	if !ok {
		if anchor, ok = item.LastUserTurn(); !ok {
			anchor = len(th) - 1
		}
	}
	size := func(m Message) int {
		b, _ := json.Marshal(m)
		return len(b) + 1
	}
	used := size(th[anchor])
	tail := []Message{th[anchor]}
	for _, m := range th[anchor+1:] {
		if s := size(m); used+s <= budget {
			used += s
		} else {
			m.Content = "" // keeps the links that say what it answers
			used += size(m)
		}
		tail = append(tail, m)
	}
	var head []Message
	for i := anchor - 1; i >= 0; i-- {
		m := th[i]
		if m.Hidden {
			continue
		}
		s := size(m)
		if used+s > budget {
			break
		}
		used += s
		head = append(head, m)
	}
	slices.Reverse(head)
	item.Thread = append(head, tail...)
	return item
}

// Find returns a copy of the item with id, thread included, and whether it is in
// items.
func Find(items []WorkItem, id string) (WorkItem, bool) {
	for _, it := range items {
		if it.ID == id {
			it.Thread = slices.Clone(it.Thread)
			return it, true
		}
	}
	return WorkItem{}, false
}

// Query is the query a Selection travels as on GET /agent/worklist/select.
func (sel Selection) Query() url.Values {
	q := url.Values{}
	if sel.Open {
		q.Set("open", "1")
	}
	if sel.After != "" {
		q.Set("after", sel.After)
	}
	if sel.Top > 0 {
		q.Set("top", strconv.Itoa(sel.Top))
	} else if sel.Limit > 0 {
		q.Set("limit", strconv.Itoa(sel.Limit))
	}
	return q
}

// ParseSelection reads a Selection from a GET /agent/worklist/select query. The
// bounds are applied when the selection is read (SelectFrom), not here.
func ParseSelection(q url.Values) (Selection, error) {
	var sel Selection
	switch q.Get("open") {
	case "", "0":
	case "1":
		sel.Open = true
	default:
		return Selection{}, errors.New("worklist: open must be 0 or 1")
	}
	for name, dst := range map[string]*int{"top": &sel.Top, "limit": &sel.Limit} {
		v := q.Get(name)
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return Selection{}, errors.New("worklist: " + name + " must be a non-negative integer")
		}
		*dst = n
	}
	sel.After = q.Get("after")
	if sel.Top > 0 && sel.After != "" {
		if n, err := strconv.Atoi(sel.After); err != nil || n < 0 {
			return Selection{}, errors.New("worklist: a top-N cursor must be a non-negative integer")
		}
	}
	return sel, nil
}
