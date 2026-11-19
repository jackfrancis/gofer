package worklist

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func selectFixture() []WorkItem {
	now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	var items []WorkItem
	for _, id := range []string{"c", "a", "e", "b", "d"} {
		items = append(items, WorkItem{ID: id, Thread: []Message{{Role: RoleUser, Content: "body of " + id}}, ThreadReadAt: now})
	}
	items[2].Meta.CompletedAt = now // "e" is retired
	return items
}

func ids(items []WorkItem) string {
	s := ""
	for _, it := range items {
		s += it.ID
	}
	return s
}

// Paging walks every selected item exactly once, in ID order, without threads; the
// cursor is keyset, so an item inserted behind it or removed ahead of it neither
// repeats nor skips the rest.
func TestSelectPagesInIDOrderWithoutThreads(t *testing.T) {
	items := selectFixture()
	now := time.Now()
	page, next := Select(items, Selection{Limit: 2}, now)
	if ids(page) != "ab" || next != "b" {
		t.Fatalf("first page = %q next %q", ids(page), next)
	}
	for _, it := range page {
		if it.Thread != nil || it.ThreadReadAt.IsZero() {
			t.Fatalf("a selected item must carry no thread but keep its read cursor: %+v", it)
		}
	}
	if items[1].Thread == nil {
		t.Fatal("Select changed its input")
	}
	// An item inserted behind the cursor and one removed ahead of it.
	items = append(items, WorkItem{ID: "a0"})
	items = append(items[:0], items[1:]...) // drop "c"
	page, next = Select(items, Selection{After: next, Limit: 2}, now)
	if ids(page) != "de" || next != "" {
		t.Fatalf("last page = %q next %q, want de and no cursor", ids(page), next)
	}
	if page, next := Select(items, Selection{After: "e", Limit: 2}, now); len(page) != 0 || next != "" {
		t.Fatalf("past the end = %q next %q", ids(page), next)
	}
	if page, next := Select(selectFixture(), Selection{Open: true}, now); ids(page) != "abcd" || next != "" {
		t.Fatalf("open = %q next %q", ids(page), next)
	}
	if page, _ := Select(selectFixture(), Selection{After: "bb"}, now); ids(page) != "cde" {
		t.Fatalf("after bb = %q", ids(page))
	}
}

// Every selection is held to the same bounds, however it is read: no limit, or one
// past the cap, is the cap, and so is a top N past it.
func TestSelectAppliesTheBounds(t *testing.T) {
	var items []WorkItem
	for i := range MaxSelectLimit + 500 {
		items = append(items, WorkItem{ID: fmt.Sprintf("i%05d", i)})
	}
	now := time.Now()
	for _, sel := range []Selection{{}, {Limit: MaxSelectLimit * 3}} {
		page, next := Select(items, sel, now)
		if len(page) != MaxSelectLimit || next != page[len(page)-1].ID {
			t.Fatalf("%+v: %d items, next %q; want %d and a cursor", sel, len(page), next, MaxSelectLimit)
		}
	}
	if page, next := Select(items, Selection{Top: MaxSelectLimit * 3}, now); len(page) != MaxSelectLimit || next != "" {
		t.Fatalf("top past the cap: %d items, next %q", len(page), next)
	}
}

// A page ends before MaxSelectBytes of encoded items, with a cursor to the rest, so
// items with large fields (labels, rationales) cannot push a page past the response
// limit; paging still reaches every item. A top-N read pages the same way.
func TestSelectBoundsPageBytes(t *testing.T) {
	const n = 300
	label := strings.Repeat("l", 32<<10) // 300 × 32 KiB ≈ 9.4 MiB
	var items []WorkItem
	for i := range n {
		items = append(items, WorkItem{ID: fmt.Sprintf("i%03d", i), Signals: Signals{Labels: []string{label}}})
	}
	now := time.Now()
	for _, base := range []Selection{{Limit: 500}, {Top: 500}} {
		var all []WorkItem
		sel, pages := base, 0
		for {
			page, next := Select(items, sel, now)
			b, _ := json.Marshal(page)
			if len(b) > MaxSelectBytes {
				t.Fatalf("%+v: page of %d bytes, want ≤ %d", base, len(b), MaxSelectBytes)
			}
			all = append(all, page...)
			pages++
			if next == "" {
				break
			}
			sel.After = next
		}
		seen := map[string]bool{}
		for _, it := range all {
			seen[it.ID] = true
		}
		if len(all) != n || len(seen) != n || pages < 3 {
			t.Fatalf("%+v: read %d items (%d distinct) in %d pages, want all %d in several", base, len(all), len(seen), pages, n)
		}
	}
	// A single item over the budget still makes progress.
	huge := []WorkItem{{ID: "a", Signals: Signals{Labels: []string{strings.Repeat("x", MaxSelectBytes)}}}, {ID: "b"}}
	if page, next := Select(huge, Selection{}, now); ids(page) != "a" || next != "a" {
		t.Fatalf("oversized item: page %q next %q", ids(page), next)
	}
}

// Top selects the N open items with the highest baseline score, highest first, and
// keeps first-insertion order among equal scores (a fresh backfill stores the most
// recently active work first).
func TestSelectTopByBaselineScore(t *testing.T) {
	now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	items := []WorkItem{
		{ID: "z-tie-first"},
		{ID: "high", Signals: Signals{Reasons: []Reason{ReasonReviewRequested}, AwaitingMeSince: now.Add(-72 * time.Hour)}},
		{ID: "a-tie-second"},
		{ID: "done", Signals: Signals{Reasons: []Reason{ReasonReviewRequested}}, Meta: Metadata{CompletedAt: now}},
	}
	page, next := Select(items, Selection{Open: true, Top: 2, Limit: 1}, now)
	if len(page) != 2 || next != "" {
		t.Fatalf("top 2 = %q next %q (Limit must be ignored)", ids(page), next)
	}
	if page[0].ID != "high" || page[1].ID != "z-tie-first" {
		t.Fatalf("top 2 = %v, %v; want high, then the first-inserted of the tie", page[0].ID, page[1].ID)
	}
}

func TestFindCopiesTheOwnersItem(t *testing.T) {
	items := selectFixture()
	it, ok := Find(items, "a")
	if !ok || it.ID != "a" || len(it.Thread) != 1 {
		t.Fatalf("Find(a) = %+v, %v", it, ok)
	}
	it.Thread[0].Content = "changed"
	if items[1].Thread[0].Content != "body of a" {
		t.Fatal("Find returned the stored thread, not a copy")
	}
	if _, ok := Find(items, "zz"); ok {
		t.Fatal("Find(zz) found something")
	}
}

// ThreadWindow bounds one item's thread and keeps what a run needs: the turn it
// answers, every turn after it, and the newest turns before it that fit.
func TestThreadWindowBoundsOneLongConversation(t *testing.T) {
	big := strings.Repeat("x", 16<<10)
	var th []Message
	for i := range 600 { // 600 answered 16 KiB turns ≈ 9.4 MiB of user content alone
		th = append(th,
			Message{ID: fmt.Sprintf("u%d", i), Role: RoleUser, Content: big},
			Message{ID: fmt.Sprintf("a%d", i), Role: RoleAgent, Content: "ok", ReplyTo: fmt.Sprintf("u%d", i)})
	}
	th = append(th, Message{ID: "ask", Role: RoleUser, Content: "what next?"})
	item := WorkItem{ID: "x", Thread: th}

	w := ThreadWindow(item, "ask", MaxThreadBytes)
	if b, _ := json.Marshal(w.Thread); len(b) > MaxThreadBytes {
		t.Fatalf("window is %d bytes, want ≤ %d", len(b), MaxThreadBytes)
	}
	last := w.Thread[len(w.Thread)-1]
	if last.ID != "ask" || w.Answered("ask") {
		t.Fatalf("window must end with the unanswered turn, got %+v", last)
	}
	if prev := w.Thread[len(w.Thread)-2]; prev.ID != "a599" || len(w.Thread) < 100 {
		t.Fatalf("window must keep the newest history (%d turns, then %q)", len(w.Thread), prev.ID)
	}
	if len(item.Thread) != len(th) {
		t.Fatal("ThreadWindow changed its input")
	}
	// An older turn named by the run anchors the window, and the reply after it
	// still answers it, however large the turns after it are.
	w = ThreadWindow(item, "u10", MaxThreadBytes)
	if w.Thread[0].ID != "u10" {
		t.Fatalf("window starts at %q, want the anchor u10 (the turns after it fill the budget)", w.Thread[0].ID)
	}
	if i, ok := w.TurnIndex("u10"); !ok || !w.Answered("u10") || w.Thread[i].Content != big {
		t.Fatalf("u10 must be kept, whole, and still answered")
	}
	if b, _ := json.Marshal(w.Thread); len(b) > MaxThreadBytes+600*2*256 {
		t.Fatalf("anchored window is %d bytes", len(b))
	}
	// A short thread is returned whole.
	short := WorkItem{Thread: th[:4]}
	if got := ThreadWindow(short, "", MaxThreadBytes); len(got.Thread) != 4 {
		t.Fatalf("short thread cut to %d turns", len(got.Thread))
	}
}

// Turns stored before IDs are keyed by position, and a legacy reply answers the user
// turn before it. The window writes those keys and links into the turns it keeps, so
// it answers TurnKey and Answered as the whole thread does once earlier turns are
// gone.
func TestThreadWindowKeepsLegacyKeys(t *testing.T) {
	big := strings.Repeat("x", 64<<10)
	var th []Message
	for range 100 {
		th = append(th, Message{Role: RoleUser, Content: big}, Message{Role: RoleAgent, Content: "ok"})
	}
	th = append(th, Message{Role: RoleUser, Content: "legacy ask"})
	item := WorkItem{Thread: th}
	ask := item.TurnKey(len(th) - 1) // "pos:200"
	answered := item.TurnKey(len(th) - 3)

	w := ThreadWindow(item, ask, 1<<20)
	if len(w.Thread) >= len(th) {
		t.Fatal("expected a cut window")
	}
	i, ok := w.TurnIndex(ask)
	if !ok || w.Thread[i].Content != "legacy ask" || w.Answered(ask) {
		t.Fatalf("legacy turn %s must keep its key and stay unanswered", ask)
	}
	if !w.Answered(answered) {
		t.Fatalf("legacy turn %s must still read as answered", answered)
	}
	// A reply keyed from the window lands on the right turn of the stored thread.
	stored := item
	if ok, err := AppendReply(&stored, w.TurnKey(i), Message{ID: "r", Content: "answer"}); err != nil || !ok || !stored.Answered(ask) {
		t.Fatalf("reply keyed from the window did not answer %s: %v %v", ask, ok, err)
	}
}

func TestReadItemIsOwnerScoped(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	s.Seed("u1", WorkItem{ID: "a", Thread: []Message{{Role: RoleUser, Content: "hi"}}})
	s.Seed("u2", WorkItem{ID: "z"})
	if it, ok, err := ReadItem(ctx, s, "u1", "a", ""); err != nil || !ok || len(it.Thread) != 1 {
		t.Fatalf("own item = %+v %v %v", it, ok, err)
	}
	if _, ok, err := ReadItem(ctx, s, "u1", "z", ""); err != nil || ok {
		t.Fatalf("another owner's item found: %v %v", ok, err)
	}
}

func TestSelectionQueryRoundTrip(t *testing.T) {
	for _, sel := range []Selection{
		{},
		{Open: true, After: "github:o/r#7", Limit: 50},
		{Open: true, Top: 200, After: "40"},
	} {
		got, err := ParseSelection(sel.Query())
		if err != nil || got != sel {
			t.Fatalf("ParseSelection(%v) = (%+v, %v), want %+v", sel.Query(), got, err, sel)
		}
	}
}

func TestParseSelection(t *testing.T) {
	big := strconv.Itoa(MaxSelectLimit * 10)
	if sel, err := ParseSelection(url.Values{"limit": {big}}); err != nil || sel.Limit != MaxSelectLimit*10 {
		t.Fatalf("limit=%s = (%+v, %v); clamping is SelectFrom's", big, sel, err)
	}
	for _, q := range []url.Values{
		{"open": {"yes"}},
		{"limit": {"-1"}},
		{"top": {"x"}},
		{"top": {"5"}, "after": {"github:o/r#1"}},
	} {
		if _, err := ParseSelection(q); err == nil {
			t.Fatalf("%v: want an error", q)
		}
	}
}

// SelectFrom reads a page from the store's briefs and keeps every bound.
func TestSelectFromStore(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	s.Seed("u1", selectFixture()...)
	now := time.Now()
	var got string
	sel := Selection{Open: true, Limit: 3}
	for {
		page, next, err := SelectFrom(ctx, s, "u1", sel, now)
		if err != nil {
			t.Fatal(err)
		}
		got += ids(page) + "|"
		if next == "" {
			break
		}
		sel.After = next
	}
	if got != "abc|d|" {
		t.Fatalf("pages = %q", got)
	}
}
