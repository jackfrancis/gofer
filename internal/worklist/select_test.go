package worklist

import (
	"net/url"
	"strconv"
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
	// Open drops retired items; no limit is one page with no cursor.
	if page, next := Select(selectFixture(), Selection{Open: true}, now); ids(page) != "abcd" || next != "" {
		t.Fatalf("open = %q next %q", ids(page), next)
	}
	// A cursor that is not an ID still resumes after it.
	if page, _ := Select(selectFixture(), Selection{After: "bb"}, now); ids(page) != "cde" {
		t.Fatalf("after bb = %q", ids(page))
	}
}

// Top selects the N open items with the highest baseline score, highest first.
func TestSelectTopByBaselineScore(t *testing.T) {
	now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	items := []WorkItem{
		{ID: "low"},
		{ID: "high", Signals: Signals{Reasons: []Reason{ReasonReviewRequested}, AwaitingMeSince: now.Add(-72 * time.Hour)}},
		{ID: "mid", Signals: Signals{Reasons: []Reason{ReasonAuthor}}},
		{ID: "done", Signals: Signals{Reasons: []Reason{ReasonReviewRequested}}, Meta: Metadata{CompletedAt: now}},
	}
	page, next := Select(items, Selection{Open: true, Top: 2, Limit: 1, After: "zzz"}, now)
	if len(page) != 2 || next != "" {
		t.Fatalf("top 2 = %q next %q (After and Limit must be ignored)", ids(page), next)
	}
	if Score(page[0], now).Rank < Score(page[1], now).Rank || page[0].ID != "high" {
		t.Fatalf("top 2 = %q, want the highest-scoring open items, highest first", ids(page))
	}
	for _, it := range page {
		if it.ID == "done" || it.ID == "low" {
			t.Fatalf("top 2 = %q", ids(page))
		}
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

func TestSelectionQueryRoundTrip(t *testing.T) {
	for _, sel := range []Selection{
		{},
		{Open: true, After: "github:o/r#7", Limit: 50},
		{Open: true, Top: 200},
	} {
		got, ok, err := ParseSelection(sel.Query())
		want := sel
		if want.Top == 0 && want.Limit == 0 {
			want.Limit = MaxSelectLimit // the server bounds an unbounded page
		}
		if !ok || err != nil || got != want {
			t.Fatalf("ParseSelection(%v) = (%+v, %v, %v), want %+v", sel.Query(), got, ok, err, want)
		}
	}
}

// The server bounds every selection and refuses one that asks for threads; a
// request with no selection is the legacy read.
func TestParseSelection(t *testing.T) {
	if _, ok, err := ParseSelection(url.Values{}); ok || err != nil {
		t.Fatalf("no parameters = (%v, %v), want the legacy read", ok, err)
	}
	big := strconv.Itoa(MaxSelectLimit * 10)
	sel, ok, err := ParseSelection(url.Values{"threads": {"0"}, "limit": {big}})
	if !ok || err != nil || sel.Limit != MaxSelectLimit {
		t.Fatalf("limit=%s = (%+v, %v, %v), want clamped", big, sel, ok, err)
	}
	sel, _, _ = ParseSelection(url.Values{"threads": {"0"}, "top": {big}})
	if sel.Top != MaxSelectLimit {
		t.Fatalf("top=%s = %+v, want clamped", big, sel)
	}
	for _, q := range []url.Values{
		{"threads": {"1"}},
		{"threads": {"0"}, "open": {"yes"}},
		{"threads": {"0"}, "limit": {"-1"}},
		{"threads": {"0"}, "top": {"x"}},
	} {
		if _, ok, err := ParseSelection(q); !ok || err == nil {
			t.Fatalf("%v = (%v, %v), want an error", q, ok, err)
		}
	}
}
