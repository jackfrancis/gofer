package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/worklist"
)

var readNow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// readFixture is an owner with two PRs: "high" outranks "low", and "low" carries a
// completed Review-all review the owner has not opened.
func readFixture() (worklist.WorkItem, worklist.WorkItem) {
	high := worklist.WorkItem{
		ID: "github:o/r#1", Type: worklist.TypePullRequest,
		GitHub:  worklist.GitHubRef{Number: 1, Repo: "o/r", Title: "High PR", UpdatedAt: readNow.Add(-time.Hour)},
		Signals: worklist.Signals{Reasons: []worklist.Reason{worklist.ReasonReviewRequested}, Comments: 20},
	}
	low := worklist.WorkItem{
		ID: "github:o/r#2", Type: worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Number: 2, Repo: "o/r", Title: "Low PR", UpdatedAt: readNow.Add(-time.Hour)},
		Thread: []worklist.Message{
			{ID: "u1", Role: worklist.RoleUser, Content: reviewPrompt, Kind: worklist.KindReviewRequest, At: readNow.Add(-10 * time.Minute)},
			{ID: "r1", ReplyTo: "u1", Role: worklist.RoleAgent, Content: "Looks good.", At: readNow.Add(-5 * time.Minute)},
		},
	}
	return high, low
}

func readHandler(store worklist.Store) *Handler {
	h := New(fakeSessions{&session.User{ID: "u1"}}, store, &recordPipeline{}, nil, true, nil)
	h.now = func() time.Time { return readNow }
	return h
}

func openThread(t *testing.T, h *Handler, id string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Thread(rec, httptest.NewRequest(http.MethodGet, "/items/thread?id="+id, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("thread status = %d", rec.Code)
	}
	return rec.Body.String()
}

// worklistOrder renders the radar and returns whether title a is listed before b,
// and whether any Discuss button carries the unread cue.
func worklistOrder(t *testing.T, h *Handler, a, b string) (aFirst, unread bool) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Index(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	ia, ib := strings.Index(body, a), strings.Index(body, b)
	if ia < 0 || ib < 0 {
		t.Fatalf("worklist is missing %q or %q:\n%s", a, b, body)
	}
	return ia < ib, strings.Contains(body, "zz-discuss--unread")
}

func stored(t *testing.T, store worklist.Store, id string) worklist.WorkItem {
	t.Helper()
	items, err := store.List(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("item %s not stored", id)
	return worklist.WorkItem{}
}

// Opening a reviewed PR's thread clears its unread badge and returns it to its
// ranked position on the radar; a later reply makes it unread and floats it again.
func TestThreadRenderMarksReplyRead(t *testing.T) {
	high, low := readFixture()
	store := worklist.NewMemoryStore()
	store.Seed("u1", high, low)
	h := readHandler(store)

	if lowFirst, unread := worklistOrder(t, h, "Low PR", "High PR"); !lowFirst || !unread {
		t.Fatalf("an unread review should float first with the unread cue (lowFirst=%v unread=%v)", lowFirst, unread)
	}

	openThread(t, h, low.ID)
	got := stored(t, store, low.ID)
	if got.ThreadReadThrough != "r1" || got.HasUnreadReply() {
		t.Fatalf("opening the thread must acknowledge the reply shown, cursor = %q", got.ThreadReadThrough)
	}
	if highFirst, unread := worklistOrder(t, h, "High PR", "Low PR"); !highFirst || unread {
		t.Fatalf("a read review must return to its ranked position without the cue (highFirst=%v unread=%v)", highFirst, unread)
	}

	// Re-opening the read thread changes nothing.
	openThread(t, h, low.ID)
	if got := stored(t, store, low.ID); got.ThreadReadThrough != "r1" {
		t.Fatalf("re-opening moved the cursor to %q", got.ThreadReadThrough)
	}

	// A follow-up is answered: the new reply is unread and floats the item again.
	err := store.Update(context.Background(), "u1", func(cur []worklist.WorkItem) ([]worklist.WorkItem, error) {
		for i := range cur {
			if cur[i].ID == low.ID {
				cur[i].Thread = append(cur[i].Thread,
					worklist.Message{ID: "u2", Role: worklist.RoleUser, Content: "and the tests?", At: readNow},
					worklist.Message{ID: "r2", ReplyTo: "u2", Role: worklist.RoleAgent, Content: "They pass.", At: readNow})
				return cur[i : i+1], nil
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if lowFirst, unread := worklistOrder(t, h, "Low PR", "High PR"); !lowFirst || !unread {
		t.Fatalf("a later reply must be unread and float first (lowFirst=%v unread=%v)", lowFirst, unread)
	}
	openThread(t, h, low.ID)
	if got := stored(t, store, low.ID); got.ThreadReadThrough != "r2" || got.HasUnreadReply() {
		t.Fatalf("opening again must acknowledge the later reply, cursor = %q", got.ThreadReadThrough)
	}
}

// landingStore delivers a reply to item just after the thread page has read the
// store and before it acknowledges, as a run finishing mid-request would.
type landingStore struct {
	worklist.Store
	item    string
	landing worklist.Message
	landed  bool
}

func (s *landingStore) Update(ctx context.Context, owner string, fn func([]worklist.WorkItem) ([]worklist.WorkItem, error)) error {
	if !s.landed {
		s.landed = true
		if err := worklist.ApplyReply(ctx, s.Store, owner, s.item, s.landing.ReplyTo, s.landing); err != nil {
			return err
		}
	}
	return s.Store.Update(ctx, owner, fn)
}

// A reply that lands after the thread page was built is not on the page, so it
// stays unread — even when its run stamped it earlier than the reply shown.
func TestThreadRenderLeavesLateReplyUnread(t *testing.T) {
	item := worklist.WorkItem{
		ID: "github:o/r#3", Type: worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Number: 3, Repo: "o/r", Title: "Busy PR"},
		Thread: []worklist.Message{
			{ID: "u1", Role: worklist.RoleUser, Content: "first question", At: readNow.Add(-10 * time.Minute)},
			{ID: "u2", Role: worklist.RoleUser, Content: "second question", At: readNow.Add(-9 * time.Minute)},
			{ID: "r2", ReplyTo: "u2", Role: worklist.RoleAgent, Content: "Second answer.", At: readNow.Add(-2 * time.Minute)},
		},
	}
	mem := worklist.NewMemoryStore()
	mem.Seed("u1", item)
	store := &landingStore{Store: mem, item: item.ID, landing: worklist.Message{
		ID: "r1", ReplyTo: "u1", Role: worklist.RoleAgent, Content: "First answer.", At: readNow.Add(-5 * time.Minute),
	}}

	body := openThread(t, readHandler(store), item.ID)
	if strings.Contains(body, "First answer.") || !strings.Contains(body, "Second answer.") {
		t.Fatal("the page must show only the replies stored when it was built")
	}
	got := stored(t, mem, item.ID)
	if len(got.Thread) != 4 {
		t.Fatalf("the late reply did not land: %d turns", len(got.Thread))
	}
	if got.ThreadReadThrough != "r2" {
		t.Fatalf("the page must acknowledge the reply it showed, cursor = %q", got.ThreadReadThrough)
	}
	if !got.HasUnreadReply() {
		t.Fatal("a reply that landed after the page was built must stay unread")
	}
}

// Only the signed-in owner's open acknowledges anything; a signed-out request
// writes nothing.
func TestThreadRenderSignedOutMarksNothing(t *testing.T) {
	_, low := readFixture()
	store := worklist.NewMemoryStore()
	store.Seed("u1", low)
	h := readHandler(store)
	h.sessions = fakeSessions{nil}
	rec := httptest.NewRecorder()
	h.Thread(rec, httptest.NewRequest(http.MethodGet, "/items/thread?id="+low.ID, nil))
	if got := stored(t, store, low.ID); got.ThreadReadThrough != "" || !got.HasUnreadReply() {
		t.Fatal("a signed-out request must not mark a thread read")
	}
}
