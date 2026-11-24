package worklist

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

// pinned is a draft pinned to o/r#1 at testCommit against main.
func pinned(body string, comments ...DraftComment) *DraftReview {
	return &DraftReview{Repo: "o/r", Number: 1, Commit: testCommit, BaseRef: "main", Body: body, Comments: comments}
}

// A stored draft is always within its bounds: invalid comments are dropped, text is
// cut, the comment count is capped, and a draft with an invalid pin or nothing left
// is no draft.
func TestBoundDraft(t *testing.T) {
	for name, d := range map[string]*DraftReview{
		"short commit": {Repo: "o/r", Number: 1, Commit: "main", BaseRef: "main", Body: "x"},
		"no repo":      {Number: 1, Commit: testCommit, BaseRef: "main", Body: "x"},
		"bad repo":     {Repo: "o/r/../x", Number: 1, Commit: testCommit, BaseRef: "main", Body: "x"},
		"no number":    {Repo: "o/r", Commit: testCommit, BaseRef: "main", Body: "x"},
		"no base":      {Repo: "o/r", Number: 1, Commit: testCommit, Body: "x"},
		"empty":        pinned(""),
	} {
		if BoundDraft(d) != nil {
			t.Errorf("%s: kept", name)
		}
	}
	d := pinned(strings.Repeat("b", MaxDraftBodyBytes+10),
		DraftComment{Path: "a.go", Line: 3, Body: " keep "},
		DraftComment{Path: "a.go", Line: 0, Body: "line zero"},
		DraftComment{Path: "/etc/passwd", Line: 1, Body: "absolute"},
		DraftComment{Path: "a\n.go", Line: 1, Body: "control"},
		DraftComment{Path: "a.go", Line: 1, Side: "middle", Body: "bad side"},
		DraftComment{Path: "a.go", Line: 1, Side: "left", Body: strings.Repeat("c", MaxDraftCommentBytes+1)},
		DraftComment{Path: "a.go", Line: 2, Body: "  "},
	)
	d.Commit = strings.ToUpper(testCommit)
	d = BoundDraft(d)
	if d == nil || d.Commit != testCommit || d.Repo != "o/r" || d.Number != 1 || d.BaseRef != "main" {
		t.Fatalf("draft = %+v", d)
	}
	if len(d.Body) > MaxDraftBodyBytes || len(d.Comments) != 2 {
		t.Fatalf("body %d bytes, %d comments: %+v", len(d.Body), len(d.Comments), d.Comments)
	}
	if c := d.Comments[0]; c.Body != "keep" || c.Side != SideRight {
		t.Errorf("first comment = %+v", c)
	}
	if c := d.Comments[1]; c.Side != SideLeft || len(c.Body) > MaxDraftCommentBytes {
		t.Errorf("second comment: side %q, %d bytes", c.Side, len(c.Body))
	}
	many := pinned("")
	for i := range MaxDraftComments + 5 {
		many.Comments = append(many.Comments, DraftComment{Path: "a.go", Line: i + 1, Body: "x"})
	}
	if got := len(BoundDraft(many).Comments); got != MaxDraftComments {
		t.Errorf("kept %d comments, want %d", got, MaxDraftComments)
	}
}

func draftPR(thread ...Message) WorkItem {
	return WorkItem{ID: "github:o/r#1", Type: TypePullRequest, GitHub: GitHubRef{Repo: "o/r", Number: 1, State: "open"}, Thread: thread}
}

// Only a reply to a draft-review request keeps a draft, pinned to this item's pull
// request and bounded on the way in. A notice never carries one, and no run writes a
// receipt.
func TestAppendReplyDraftOnlyForDraftRequests(t *testing.T) {
	draft := pinned("", DraftComment{Path: "a.go", Line: 1, Body: "x"}, DraftComment{Path: "", Line: 1, Body: "dropped"})
	item := draftPR(
		Message{ID: "q", Role: RoleUser, Content: "review?", Kind: KindReviewRequest},
		Message{ID: "d", Role: RoleUser, Content: "draft", Kind: KindDraftReviewRequest},
		Message{ID: "d2", Role: RoleUser, Content: "draft again", Kind: KindDraftReviewRequest},
		Message{ID: "d3", Role: RoleUser, Content: "draft elsewhere", Kind: KindDraftReviewRequest},
	)
	if _, err := AppendReply(&item, "q", Message{ID: "a1", Content: "a review", Draft: draft}); err != nil {
		t.Fatal(err)
	}
	if item.Thread[4].Draft != nil {
		t.Error("a reply to a review request kept a draft")
	}
	receipt := &DraftReceipt{State: DraftCreated, URL: "https://github.com/x"}
	if _, err := AppendReply(&item, "d", Message{ID: "a2", Content: "drafted", Draft: draft, DraftReceipt: receipt}); err != nil {
		t.Fatal(err)
	}
	if got := item.Thread[5]; got.Draft == nil || len(got.Draft.Comments) != 1 || got.DraftReceipt != nil {
		t.Fatalf("draft reply = %+v", got)
	}
	if _, err := AppendReply(&item, "d2", Message{ID: "a3", Content: "sign in", Notice: NoticeReconnect, Draft: draft}); err != nil {
		t.Fatal(err)
	}
	if item.Thread[6].Draft != nil {
		t.Error("a notice kept a draft")
	}
	other := pinned("", DraftComment{Path: "a.go", Line: 1, Body: "x"})
	other.Repo = "o/elsewhere"
	if _, err := AppendReply(&item, "d3", Message{ID: "a4", Content: "drafted", Draft: other}); err != nil {
		t.Fatal(err)
	}
	if item.Thread[7].Draft != nil {
		t.Error("a draft pinned to another pull request was kept")
	}
}

// The thread offers the newest request's draft only: a redraft that found nothing
// retires the one before it, a late reply to an older request never supersedes a
// newer result, and a hidden request or reply offers nothing.
func TestLatestDraft(t *testing.T) {
	d1 := Message{ID: "d1", Role: RoleUser, Kind: KindDraftReviewRequest, Content: "draft"}
	a1 := Message{ID: "a1", ReplyTo: "d1", Role: RoleAgent, Content: "drafted", Draft: pinned("x")}
	d2 := Message{ID: "d2", Role: RoleUser, Kind: KindDraftReviewRequest, Content: "draft"}
	a2 := Message{ID: "a2", ReplyTo: "d2", Role: RoleAgent, Content: "Nothing to draft."}
	if got := draftPR(d1, a1).LatestDraft(); got != 1 {
		t.Errorf("one draft: %d", got)
	}
	if got := draftPR(d1, a1, d2, a2).LatestDraft(); got != -1 {
		t.Errorf("an empty redraft left the old draft offered: %d", got)
	}
	if got := draftPR(d1, a1, d2).LatestDraft(); got != -1 {
		t.Errorf("a pending redraft left the old draft offered: %d", got)
	}
	a2late := Message{ID: "a2", ReplyTo: "d2", Role: RoleAgent, Content: "drafted", Draft: pinned("y")}
	if got := draftPR(d1, d2, a2late, a1).LatestDraft(); got != 2 {
		t.Errorf("a late reply to the older request superseded the newer draft: %d", got)
	}
	hidden := a1
	hidden.Hidden = true
	if got := draftPR(d1, hidden).LatestDraft(); got != -1 {
		t.Errorf("a hidden draft is offered: %d", got)
	}
	hiddenReq := d1
	hiddenReq.Hidden = true
	if got := draftPR(hiddenReq, a1).LatestDraft(); got != -1 {
		t.Errorf("a hidden request's draft is offered: %d", got)
	}
}

// A confirmation is claimed in the store before GitHub is asked: a replay gets the
// receipt back and writes nothing; a release lets the owner try again; a settle only
// lands on its own claim; and a draft whose item now names another pull request is
// not offered.
func TestClaimAndSettleDraft(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	draft := pinned("x")
	store.Seed("o", draftPR(
		Message{ID: "d", Role: RoleUser, Kind: KindDraftReviewRequest, Content: "draft"},
		Message{ID: "a", ReplyTo: "d", Role: RoleAgent, Content: "drafted", Draft: draft},
	))
	digest := draft.Digest()
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, _, err := ClaimDraft(ctx, store, "o", "github:o/r#1", "a", "stale", t1); !errors.Is(err, ErrDraftNotOffered) {
		t.Fatalf("a stale digest was claimed: %v", err)
	}
	it, prior, err := ClaimDraft(ctx, store, "o", "github:o/r#1", "a", digest, t1)
	if err != nil || prior != nil || it.Thread[1].DraftReceipt == nil || it.Thread[1].DraftReceipt.State != DraftCreating {
		t.Fatalf("claim: %+v %v %v", it.Thread[1].DraftReceipt, prior, err)
	}
	// A replay while the claim stands writes nothing and gets it back.
	if _, prior, err := ClaimDraft(ctx, store, "o", "github:o/r#1", "a", digest, t1.Add(time.Second)); err != nil || prior == nil || prior.State != DraftCreating {
		t.Fatalf("replay: %+v %v", prior, err)
	}
	// Someone else's settle (another claim time) changes nothing.
	_ = SettleDraft(ctx, store, "o", "github:o/r#1", "a", t1.Add(time.Second), nil)
	// Release, then claim again.
	if err := SettleDraft(ctx, store, "o", "github:o/r#1", "a", t1, nil); err != nil {
		t.Fatal(err)
	}
	t2 := t1.Add(time.Minute)
	if _, prior, err := ClaimDraft(ctx, store, "o", "github:o/r#1", "a", digest, t2); err != nil || prior != nil {
		t.Fatalf("reclaim after release: %+v %v", prior, err)
	}
	created := &DraftReceipt{State: DraftCreated, URL: "https://github.com/o/r/pull/1#pullrequestreview-1", At: t2}
	if err := SettleDraft(ctx, store, "o", "github:o/r#1", "a", t2, created); err != nil {
		t.Fatal(err)
	}
	// Replayed after the review was submitted or discarded on GitHub: still the receipt.
	if _, prior, err := ClaimDraft(ctx, store, "o", "github:o/r#1", "a", digest, t2.Add(time.Hour)); err != nil || prior == nil || prior.State != DraftCreated || prior.URL != created.URL {
		t.Fatalf("replay after creation: %+v %v", prior, err)
	}
	// The item now names another pull request: the draft is not offered.
	_ = store.Update(ctx, "o", func(cur []WorkItem) ([]WorkItem, error) {
		cur[0].GitHub.Number = 2
		cur[0].Thread[1].DraftReceipt = nil
		return cur, nil
	})
	if _, _, err := ClaimDraft(ctx, store, "o", "github:o/r#1", "a", digest, t2.Add(2*time.Hour)); !errors.Is(err, ErrDraftNotOffered) {
		t.Fatalf("a retargeted item's draft was claimed: %v", err)
	}
}

// A run never reads a stored draft or receipt back: the window and the full-list
// read carry the reply's content only, and the store's copy is untouched.
func TestRunReadsLeaveDraftsOut(t *testing.T) {
	item := draftPR(
		Message{ID: "d", Role: RoleUser, Content: "draft", Kind: KindDraftReviewRequest, At: time.Now()},
		Message{ID: "a", ReplyTo: "d", Role: RoleAgent, Content: "drafted", Draft: pinned("b"), DraftReceipt: &DraftReceipt{State: DraftCreated}, At: time.Now()},
		Message{ID: "h", ReplyTo: "d", Role: RoleAgent, Content: "old", Hidden: true, Draft: pinned("c"), At: time.Now()},
	)
	for _, turn := range []string{"", "d"} {
		for _, m := range ThreadWindow(item, turn, MaxItemBytes).Thread {
			if m.Draft != nil || m.DraftReceipt != nil {
				t.Errorf("turn %q: the window carries a draft", turn)
			}
		}
	}
	for _, m := range RedactDrafts([]WorkItem{item})[0].Thread {
		if m.Draft != nil || m.DraftReceipt != nil {
			t.Error("the redacted list carries a draft")
		}
	}
	if item.Thread[1].Draft == nil || item.Thread[1].DraftReceipt == nil || item.Thread[2].Draft == nil {
		t.Error("a run read changed the stored item")
	}
}

// The digest follows the draft's content and its pin, so the thread page's
// confirmation can tell the draft it showed from another.
func TestDraftDigest(t *testing.T) {
	a, b := pinned("b"), pinned("c")
	moved := pinned("b")
	moved.Number = 2
	if a.Digest() == b.Digest() || a.Digest() == moved.Digest() || a.Digest() != pinned("b").Digest() {
		t.Error("digest does not follow content and pin")
	}
	var nilDraft *DraftReview
	if nilDraft.Digest() != "" || !nilDraft.Empty() {
		t.Error("a nil draft has a digest or content")
	}
}
