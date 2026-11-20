package worklist

import (
	"context"
	"testing"
)

func user(id, content string) Message {
	return Message{ID: id, Role: RoleUser, Content: content, At: t0}
}

// Reading the newest reply clears the unread cue and the unread-first sort
// preference; a later reply is unread again.
func TestMarkReadClearsUnreadUntilNextReply(t *testing.T) {
	it := WorkItem{ID: "a", Thread: []Message{user("u1", "q"), {ID: "r1", ReplyTo: "u1", Role: RoleAgent, Content: "a", At: t1}}}
	if !it.HasUnreadReply() {
		t.Fatal("a reply never read must be unread")
	}
	if got := it.LatestReplyKey(); got != "r1" {
		t.Fatalf("LatestReplyKey = %q, want r1", got)
	}
	if !it.MarkRead("r1") {
		t.Fatal("marking the newest reply read must move the cursor")
	}
	if it.HasUnreadReply() {
		t.Fatal("a read reply must not be unread")
	}

	// The user's follow-up is not a reply: still read.
	it.Thread = append(it.Thread, user("u2", "and?"))
	if it.HasUnreadReply() {
		t.Fatal("the user's own turn must not make the thread unread")
	}
	// Its answer is.
	it.Thread = append(it.Thread, Message{ID: "r2", ReplyTo: "u2", Role: RoleAgent, Content: "b", At: t2})
	if !it.HasUnreadReply() {
		t.Fatal("a reply after the cursor must be unread")
	}
}

// A reply that lands after the page was built stays unread, even when the run
// stamped it earlier than the reply the page showed (replies land out of order).
func TestReplyLandingAfterRenderStaysUnread(t *testing.T) {
	// Two turns in flight; the answer to the second lands first.
	it := WorkItem{ID: "a", Thread: []Message{
		user("u1", "first"),
		user("u2", "second"),
		{ID: "r2", ReplyTo: "u2", Role: RoleAgent, Content: "b", At: t3},
	}}
	shown := it.LatestReplyKey() // what the page rendered
	// The answer to the first turn lands while the page is on its way, stamped
	// before the reply shown.
	it.Thread = append(it.Thread, Message{ID: "r1", ReplyTo: "u1", Role: RoleAgent, Content: "a", At: t1})
	if !it.MarkRead(shown) {
		t.Fatal("acknowledging the shown reply must move the cursor")
	}
	if !it.HasUnreadReply() {
		t.Fatal("a reply that landed after the page was built must stay unread")
	}
}

// The cursor only moves forward: a page built before a newer one cannot undo it.
func TestMarkReadNeverMovesBack(t *testing.T) {
	it := WorkItem{ID: "a", Thread: []Message{
		user("u1", "q"),
		{ID: "r1", ReplyTo: "u1", Role: RoleAgent, At: t1},
		user("u2", "q2"),
		{ID: "r2", ReplyTo: "u2", Role: RoleAgent, At: t2},
	}}
	if !it.MarkRead("r2") {
		t.Fatal("first acknowledgement must move the cursor")
	}
	if it.MarkRead("r1") || it.ThreadReadThrough != "r2" {
		t.Fatalf("an older page moved the cursor back to %q", it.ThreadReadThrough)
	}
	if it.MarkRead("r2") {
		t.Fatal("acknowledging the same reply again must report no change")
	}
	if it.MarkRead("u2") || it.MarkRead("nope") || it.MarkRead("") {
		t.Fatal("only an agent turn in the thread can be acknowledged")
	}
}

// A cursor whose turn is gone (the thread was cleared and restarted) reads as
// nothing seen, and the next acknowledgement replaces it.
func TestStaleCursorReadsUnread(t *testing.T) {
	it := WorkItem{ID: "a", ThreadReadThrough: "gone", Thread: []Message{user("u1", "q"), {ID: "r1", ReplyTo: "u1", Role: RoleAgent, At: t1}}}
	if !it.HasUnreadReply() {
		t.Fatal("a reply after a cleared thread's stale cursor must be unread")
	}
	if !it.MarkRead("r1") || it.HasUnreadReply() {
		t.Fatal("acknowledging must replace a stale cursor")
	}
}

// Turns stored before turns had IDs are acknowledged by position.
func TestMarkReadLegacyTurns(t *testing.T) {
	it := WorkItem{ID: "a", Thread: []Message{{Role: RoleUser, At: t0}, {Role: RoleAgent, At: t1}}}
	if got := it.LatestReplyKey(); got != "pos:1" {
		t.Fatalf("LatestReplyKey = %q, want pos:1", got)
	}
	if !it.MarkRead("pos:1") || it.HasUnreadReply() {
		t.Fatal("a legacy reply must be acknowledged by its position")
	}
}

// Without a cursor, the legacy time-based read time still applies.
func TestHasUnreadReplyLegacyReadTime(t *testing.T) {
	it := WorkItem{ID: "a", ThreadReadAt: t2, Thread: []Message{{Role: RoleUser, At: t0}, {Role: RoleAgent, At: t1}}}
	if it.HasUnreadReply() {
		t.Fatal("a reply before the legacy read time is read")
	}
	it.Thread = append(it.Thread, Message{Role: RoleAgent, At: t3})
	if !it.HasUnreadReply() {
		t.Fatal("a reply after the legacy read time is unread")
	}
	if (WorkItem{}).HasUnreadReply() || (WorkItem{}).LatestReplyKey() != "" {
		t.Fatal("an item with no thread has nothing to read")
	}
}

// Once its review is read, a reviewed PR leaves the unread group and returns to
// its ranked position.
func TestReadReviewReturnsToRankedPosition(t *testing.T) {
	reviewed := WorkItem{ID: "low", Meta: Metadata{Rank: 0.1}, Thread: []Message{
		{ID: "u1", Role: RoleUser, Kind: KindReviewRequest, Content: "review", At: t0},
		{ID: "r1", ReplyTo: "u1", Role: RoleAgent, Content: "lgtm", At: t1},
	}}
	items := []WorkItem{{ID: "high", Meta: Metadata{Rank: 0.9}}, reviewed}
	if err := Sort(items, SortRank, true); err != nil {
		t.Fatal(err)
	}
	if items[0].ID != "low" {
		t.Fatalf("an unread review should sort first, got %q", items[0].ID)
	}
	items[0].MarkRead(items[0].LatestReplyKey())
	if err := Sort(items, SortRank, true); err != nil {
		t.Fatal(err)
	}
	if items[0].ID != "high" || items[1].ID != "low" {
		t.Fatalf("a read review must sort by rank, got %q then %q", items[0].ID, items[1].ID)
	}
}

// countingStore records how many items each Update writes.
type countingStore struct {
	Store
	writes []int
}

func (s *countingStore) Update(ctx context.Context, owner string, fn func([]WorkItem) ([]WorkItem, error)) error {
	return s.Store.Update(ctx, owner, func(cur []WorkItem) ([]WorkItem, error) {
		out, err := fn(cur)
		s.writes = append(s.writes, len(out))
		return out, err
	})
}

// MarkThreadRead stores the cursor on the owner's item only, and writes nothing
// when the cursor does not move (a re-render of a read thread).
func TestMarkThreadRead(t *testing.T) {
	mem := NewMemoryStore()
	item := WorkItem{ID: "a", Thread: []Message{user("u1", "q"), {ID: "r1", ReplyTo: "u1", Role: RoleAgent, At: t1}}}
	mem.Seed("owner", item)
	mem.Seed("other", item)
	store := &countingStore{Store: mem}
	ctx := context.Background()

	if err := MarkThreadRead(ctx, store, "owner", "a", "r1"); err != nil {
		t.Fatal(err)
	}
	if err := MarkThreadRead(ctx, store, "owner", "a", "r1"); err != nil {
		t.Fatal(err)
	}
	if err := MarkThreadRead(ctx, store, "owner", "missing", "r1"); err != nil {
		t.Fatal(err)
	}
	if len(store.writes) != 3 || store.writes[0] != 1 || store.writes[1] != 0 || store.writes[2] != 0 {
		t.Fatalf("writes = %v, want [1 0 0]", store.writes)
	}
	if got, _ := mem.List(ctx, "owner"); got[0].ThreadReadThrough != "r1" || got[0].HasUnreadReply() {
		t.Fatalf("owner's item not marked read: %+v", got[0])
	}
	if got, _ := mem.List(ctx, "other"); got[0].ThreadReadThrough != "" || !got[0].HasUnreadReply() {
		t.Fatal("marking one owner's item read must not touch another owner's")
	}
}
