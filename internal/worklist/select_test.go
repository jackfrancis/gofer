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
	// A single item over the budget is projected: the page still holds it, and still
	// fits.
	huge := []WorkItem{{ID: "a", Signals: Signals{Labels: []string{strings.Repeat("x", 5<<20)}}}, {ID: "b"}}
	page, next := Select(huge, Selection{}, now)
	if b, _ := json.Marshal(page); ids(page) != "ab" || next != "" || len(b) > MaxSelectBytes {
		t.Fatalf("oversized item: page %q (%d bytes) next %q", ids(page), len(b), next)
	}
	if page[0].Projected == nil || encodedSize(page[0]) > MaxBriefBytes {
		t.Fatalf("oversized item was not projected: %d bytes", encodedSize(page[0]))
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

// ThreadWindow bounds the whole encoded item and keeps what a run needs: the turn it
// answers and the newest turns before it that fit.
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

	for _, turn := range []string{"ask", "", "u10"} {
		w := ThreadWindow(item, turn, MaxItemBytes)
		if n := encodedSize(w); n > MaxItemBytes {
			t.Fatalf("turn %q: window is %d bytes, want ≤ %d", turn, n, MaxItemBytes)
		}
	}
	w := ThreadWindow(item, "ask", MaxItemBytes)
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
	// An answered turn's window is the turn and its reply, so the run sees it is done.
	w = ThreadWindow(item, "u10", MaxItemBytes)
	if len(w.Thread) != 2 || w.Thread[0].ID != "u10" || w.Thread[1].ID != "a10" || !w.Answered("u10") {
		t.Fatalf("answered turn window = %+v", w.Thread)
	}
	// Without a turn, the window is the newest turns.
	w = ThreadWindow(item, "", MaxItemBytes)
	if w.Thread[len(w.Thread)-1].ID != "ask" || w.Thread[len(w.Thread)-2].ID != "a599" {
		t.Fatalf("newest window ends %q", w.Thread[len(w.Thread)-1].ID)
	}
	// A short thread is returned whole.
	short := WorkItem{Thread: th[:4]}
	if got := ThreadWindow(short, "", MaxItemBytes); len(got.Thread) != 4 {
		t.Fatalf("short thread cut to %d turns", len(got.Thread))
	}
}

// A delayed Discuss turn keeps the history it refers to, however much the thread
// grew after it: later turns are not what the model reads, so they do not take the
// window.
func TestThreadWindowKeepsHistoryForADelayedTurn(t *testing.T) {
	th := []Message{
		{ID: "q1", Role: RoleUser, Content: "is this safe to merge?"},
		{ID: "r1", Role: RoleAgent, Content: "not yet: the migration is unguarded", ReplyTo: "q1"},
		{ID: "q2", Role: RoleUser, Content: "what did you mean about the migration?"},
	}
	big := strings.Repeat("y", 16<<10)
	for i := range 400 { // later turns ≈ 6.3 MiB
		th = append(th,
			Message{ID: fmt.Sprintf("u%d", i), Role: RoleUser, Content: big},
			Message{ID: fmt.Sprintf("a%d", i), Role: RoleAgent, Content: "ok", ReplyTo: fmt.Sprintf("u%d", i)})
	}
	w := ThreadWindow(WorkItem{ID: "x", Thread: th}, "q2", MaxItemBytes)
	if got := ids2(w.Thread); got != "q1 r1 q2" {
		t.Fatalf("window = %s, want the turn and the history before it", got)
	}
	if w.Answered("q2") {
		t.Fatal("q2 must read as unanswered")
	}
}

func ids2(th []Message) string {
	var s []string
	for _, m := range th {
		s = append(s, m.ID)
	}
	return strings.Join(s, " ")
}

// A message too large for any read (one stored before the form limit) is cut with a
// note that says so, whether it is the turn being answered, its reply, or the newest
// turn; the whole item still fits.
func TestThreadWindowCutsAnOversizedMessage(t *testing.T) {
	huge := strings.Repeat("<z>", 3<<20) // 9 MiB, and JSON escaping grows it further
	cases := map[string]struct {
		thread []Message
		turn   string
	}{
		"unanswered turn": {[]Message{{ID: "ok", Role: RoleUser, Content: "hi"}, {ID: "big", Role: RoleUser, Content: huge}}, "big"},
		"answered turn":   {[]Message{{ID: "big", Role: RoleUser, Content: huge}, {ID: "r", Role: RoleAgent, Content: huge, ReplyTo: "big"}}, "big"},
		"newest turn":     {[]Message{{ID: "a", Role: RoleUser, Content: "q"}, {ID: "big", Role: RoleAgent, Content: huge, ReplyTo: "a"}}, ""},
	}
	for name, c := range cases {
		item := WorkItem{ID: "x", Signals: Signals{Labels: []string{strings.Repeat("l", 1<<20)}}, Thread: c.thread}
		w := ThreadWindow(item, c.turn, MaxItemBytes)
		if n := encodedSize(w); n > MaxItemBytes {
			t.Fatalf("%s: item is %d bytes, want ≤ %d", name, n, MaxItemBytes)
		}
		i, ok := w.TurnIndex("big")
		if !ok || !strings.Contains(w.Thread[i].Content, "[gofer: this message was cut from") || !strings.HasPrefix(w.Thread[i].Content, "<z>") {
			t.Fatalf("%s: the oversized turn must be kept, cut, with a note", name)
		}
		if name == "answered turn" && !w.Answered("big") {
			t.Fatalf("%s: the reply must still answer the turn", name)
		}
		if w.Projected == nil {
			t.Fatalf("%s: the oversized item fields must be projected", name)
		}
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
	if n := encodedSize(w); n > 1<<20 {
		t.Fatalf("window is %d bytes", n)
	}
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

// The top N is chosen by the whole item's score, before projection shortens the
// signals it reads: a label past the projection's cut still counts.
func TestSelectTopScoresTheWholeItem(t *testing.T) {
	labels := make([]string, 0, projectedList+1)
	for i := range projectedList {
		labels = append(labels, fmt.Sprintf("area/%d", i))
	}
	labels = append(labels, "security") // past the cut
	big := WorkItem{ID: "big", Signals: Signals{Labels: labels}, Meta: Metadata{Rationale: strings.Repeat("r", MaxBriefBytes)}}
	plain := WorkItem{ID: "plain", Signals: Signals{Labels: []string{"kind/bug"}}}
	now := time.Now().UTC()
	full, cut, other := Score(big, now).Rank, Score(Project(big), now).Rank, Score(plain, now).Rank
	if !(cut < other && other < full) {
		t.Fatalf("fixture: want projected %.3f < plain %.3f < whole %.3f", cut, other, full)
	}
	page, _ := Select([]WorkItem{plain, big}, Selection{Top: 1}, now)
	if len(page) != 1 || page[0].ID != "big" || page[0].Projected == nil {
		t.Fatalf("top 1 = %v, want the (projected) item whose whole score is higher", ids(page))
	}
}

// A Discuss run dispatched without a turn answers the newest user turn. The read
// resolves it against the stored thread, so an out-of-order reply to an older turn
// that lands after a large question cannot crowd the question out of the window.
func TestThreadWindowFindsTheLatestUserTurn(t *testing.T) {
	th := []Message{
		{ID: "q1", Role: RoleUser, Content: "first question"},
		{ID: "q2", Role: RoleUser, Content: strings.Repeat("w", MaxItemBytes)}, // stored before the form limit
		{ID: "r1", Role: RoleAgent, Content: "late answer to q1", ReplyTo: "q1"},
	}
	item := WorkItem{ID: "x", Thread: th}
	if got := ids2(ThreadWindow(item, "", MaxItemBytes).Thread); got != "r1" {
		t.Fatalf("fixture: the newest-turns window = %s, want only r1", got)
	}
	w := ThreadWindow(item, LatestUserTurn, MaxItemBytes)
	i, ok := w.LastUserTurn()
	if !ok || w.TurnKey(i) != "q2" || w.Answered("q2") || !strings.HasPrefix(w.Thread[i].Content, "www") {
		t.Fatalf("window = %s, want the newest user turn q2, unanswered", ids2(w.Thread))
	}
	if b, _ := json.Marshal(w); len(b) > MaxItemBytes {
		t.Fatalf("window encodes to %d bytes, over %d", len(b), MaxItemBytes)
	}
	if w := ThreadWindow(WorkItem{ID: "y", Thread: th[2:]}, LatestUserTurn, MaxItemBytes); ids2(w.Thread) != "r1" {
		t.Fatalf("with no user turn the window = %s, want the newest turns", ids2(w.Thread))
	}
}
