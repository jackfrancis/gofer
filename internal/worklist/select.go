package worklist

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"time"
)

// A run never reads the whole worklist: the threads grow without bound, and the agent
// plane caps a response at 8 MiB. It reads one item, with a window of its thread
// (ReadItem, GET /agent/worklist/item), or a Selection of items without their threads
// (SelectFrom, GET /agent/worklist/select). Every read is bounded by count and by
// encoded size, whatever the stored data holds: an item too large to read whole is
// projected (Project), and an oversized message is cut (ThreadWindow). The same
// functions serve the in-process sink and the agent plane, so the bounds hold on
// every backend.
const (
	// MaxSelectLimit bounds the items in one selection page and the N of a top-N
	// read. A larger request is clamped to it.
	MaxSelectLimit = 1000
	// MaxSelectBytes bounds the encoded items in one selection page. A page that
	// would pass it ends early, with a cursor to the rest; each item is at most
	// MaxBriefBytes (Project), so every page fits. It leaves half the agent plane's
	// 8 MiB response limit as headroom.
	MaxSelectBytes = 4 << 20
	// MaxItemBytes bounds the encoded item ReadItem returns, thread window included
	// (ThreadWindow).
	MaxItemBytes = 4 << 20
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
	// (BaselineScore(item, now).Rank, without the model signals a ranking pass
	// replaces, which may rest on a classification since changed),
	// highest first, instead of paging by ID; Limit is ignored. Equal scores keep the store's first-insertion order. It is clamped to
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
// threads and projected to MaxBriefBytes; items is not changed. A page holds at least
// one item when any remain, and at most MaxSelectBytes of them.
func Select(items []WorkItem, sel Selection, now time.Time) (page []WorkItem, next string) {
	sel = sel.normalized()
	out := make([]WorkItem, 0, len(items))
	for _, it := range items {
		if sel.Open && !it.Meta.CompletedAt.IsZero() {
			continue
		}
		out = append(out, it)
	}
	if sel.Top > 0 {
		// Rank by the whole item: projection would shorten the signals the score reads
		// (a label past the cut), so it runs only on what is selected.
		rank := make(map[string]float64, len(out))
		for _, it := range out {
			rank[it.ID] = BaselineScore(it, now).Rank
		}
		// Stable on ties: a fresh backfill stores the most recently active work
		// first, so a tied cutoff favors fresh work.
		slices.SortStableFunc(out, func(a, b WorkItem) int { return cmp.Compare(rank[b.ID], rank[a.ID]) })
		out = out[:min(len(out), sel.Top)]
		offset, _ := strconv.Atoi(sel.After) // ParseSelection rejects a bad one
		if offset < 0 || offset > len(out) {
			offset = len(out)
		}
		out = projectAll(out[offset:])
		n := fitBytes(out, MaxSelectBytes)
		if n < len(out) {
			next = strconv.Itoa(offset + n)
		}
		return out[:n], next
	}
	slices.SortFunc(out, func(a, b WorkItem) int { return cmp.Compare(a.ID, b.ID) })
	i, _ := slices.BinarySearchFunc(out, sel.After, func(it WorkItem, id string) int { return cmp.Compare(it.ID, id) })
	for i < len(out) && out[i].ID <= sel.After {
		i++
	}
	out = out[i:]
	page = projectAll(out[:min(len(out), sel.Limit)])
	n := fitBytes(page, MaxSelectBytes)
	if n < len(out) {
		next = page[n-1].ID
	}
	return page[:n], next
}

// projectAll returns a copy of items, each projected (Project): without its thread
// and cut to MaxBriefBytes. The read cursors stay: they are not bodies.
func projectAll(items []WorkItem) []WorkItem {
	out := make([]WorkItem, len(items))
	for i, it := range items {
		out[i] = Project(it)
	}
	return out
}

// fitBytes returns how many of items, from the start, fit in budget bytes of JSON —
// at least one, so a page always makes progress (each item is projected, so one
// item is far under the budget).
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

// LatestUserTurn is the turn an item read (ReadItem, ThreadWindow) is given to name
// the stored thread's newest user turn. It is never a turn's key: those are
// generated IDs or "pos:N".
const LatestUserTurn = "@latest-user"

// ReadItem returns the owner's item with id and a window of its thread
// (ThreadWindow) that keeps the turn named by turn, at most MaxItemBytes encoded. ok
// is false if the owner has no such item: another owner's item is never found.
func ReadItem(ctx context.Context, store Store, ownerID, id, turn string) (WorkItem, bool, error) {
	it, ok, err := store.Get(ctx, ownerID, id)
	if err != nil || !ok {
		return WorkItem{}, false, err
	}
	return ThreadWindow(it, turn, MaxItemBytes), true, nil
}

// ThreadWindow returns a copy of item, projected (Project), whose thread is cut so
// the whole encoded item is at most budget bytes: one long conversation, or one
// oversized message, cannot outgrow a read. A key or reply link that a turn's
// position implied (TurnKey, a legacy reply) is first written into the turn, so the
// window answers TurnKey, Answered and ReplyTo for its turns as the whole thread
// does, and a reply keyed from it lands on the right stored turn.
//
// With turn naming a user turn (a Discuss run), the window is what answering it
// needs. If the stored thread already answers it, the window is the turn and its
// first reply, so the run sees it is done. Otherwise it is the turn and, before it,
// as many of the visible turns as fit, newest first; turns after it are left out
// (none of them answers it, and the model sees only what precedes it).
//
// LatestUserTurn names the stored thread's newest user turn (a Discuss run
// dispatched without a turn answers it), resolved here against the whole thread,
// so the window is built for it as for a named turn. A window of the newest turns
// could hold only later replies and lose the question.
//
// Without a turn (research), the window is the newest visible turns that fit, oldest
// first: a hidden turn is withheld from the model, so it is not sent at all.
//
// A turn the window must keep that does not fit is cut (truncateMessage), with a
// note saying so.
func ThreadWindow(item WorkItem, turn string, budget int) WorkItem {
	full := item
	th := slices.Clone(item.Thread)
	for i := range th {
		th[i].ID = full.TurnKey(i)
		if th[i].Role == RoleAgent && th[i].ReplyTo == "" && th[i].Notice == "" && i > 0 && full.Thread[i-1].Role == RoleUser {
			th[i].ReplyTo = full.TurnKey(i - 1)
		}
	}
	out := Project(item)
	left := budget - encodedSize(out) - len(`,"thread":[]`)
	if len(th) == 0 || left <= 0 {
		return out
	}
	keyed := item
	keyed.Thread = th
	if turn == LatestUserTurn {
		turn = ""
		if i, ok := keyed.LastUserTurn(); ok {
			turn = th[i].ID
		}
	}
	var window []Message
	if anchor, ok := keyed.TurnIndex(turn); turn != "" && ok && th[anchor].Role == RoleUser {
		if reply, answered := keyed.replies()[th[anchor].ID]; answered {
			ask := truncateMessage(th[anchor], left/2)
			window = []Message{ask, truncateMessage(th[reply], left-messageSize(ask))}
		} else {
			ask := th[anchor]
			if messageSize(ask) > left {
				ask = truncateMessage(ask, left/2)
			}
			used := messageSize(ask)
			window = []Message{ask}
			for i := anchor - 1; i >= 0; i-- {
				if th[i].Hidden {
					continue // withheld from the model
				}
				s := messageSize(th[i])
				if used+s > left {
					break
				}
				used += s
				window = append(window, th[i])
			}
			slices.Reverse(window)
		}
	} else {
		used := 0
		for i := len(th) - 1; i >= 0; i-- {
			m := th[i]
			if m.Hidden {
				continue // withheld from the model, as research never sends it
			}
			s := messageSize(m)
			if used+s > left {
				if len(window) > 0 {
					break
				}
				m = truncateMessage(m, left)
				s = messageSize(m)
			}
			used += s
			window = append(window, m)
		}
		slices.Reverse(window)
	}
	out.Thread = window
	return out
}

// truncationNote ends a message cut to fit a read.
const truncationNote = "\n\n[gofer: this message was cut from %d bytes to fit a read]"

// truncateMessage returns m cut to at most budget encoded bytes (messageSize). It
// cuts the content, ending it with a note that says so; if even the empty message
// does not fit, it returns it without content.
func truncateMessage(m Message, budget int) Message {
	if messageSize(m) <= budget {
		return m
	}
	original := len(m.Content)
	note := fmt.Sprintf(truncationNote, original)
	content := m.Content
	n := budget - messageSize(Message{ID: m.ID, ReplyTo: m.ReplyTo, Role: m.Role, Model: m.Model, Kind: m.Kind, Verdict: m.Verdict, Hidden: m.Hidden, Notice: m.Notice, At: m.At}) - len(note)
	for range 64 {
		if n <= 0 {
			break
		}
		m.Content = truncateUTF8(content, n) + note
		size := messageSize(m)
		if size <= budget {
			return m
		}
		// JSON escaping can make the content longer than its bytes: shrink in
		// proportion to the overshoot.
		n = min(n-1, int(float64(n)*float64(budget)/float64(size)))
	}
	m.Content = ""
	return m
}

// messageSize is a turn's encoded size in a thread, separator included.
func messageSize(m Message) int { return encodedSize(m) + 1 }

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
