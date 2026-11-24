package worklist

import (
	"strings"
	"testing"
	"time"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

// A stored draft is always within its bounds: invalid comments are dropped, text is
// cut, the comment count is capped, and a draft with no valid commit or nothing left
// is no draft.
func TestBoundDraft(t *testing.T) {
	if BoundDraft(&DraftReview{Commit: "main", Body: "x"}) != nil {
		t.Error("a draft without a full commit id was kept")
	}
	if BoundDraft(&DraftReview{Commit: testCommit}) != nil {
		t.Error("an empty draft was kept")
	}
	d := BoundDraft(&DraftReview{
		Commit: strings.ToUpper(testCommit),
		Body:   strings.Repeat("b", MaxDraftBodyBytes+10),
		Comments: []DraftComment{
			{Path: "a.go", Line: 3, Body: " keep "},
			{Path: "a.go", Line: 0, Body: "line zero"},
			{Path: "/etc/passwd", Line: 1, Body: "absolute"},
			{Path: "a\n.go", Line: 1, Body: "control"},
			{Path: "a.go", Line: 1, Side: "middle", Body: "bad side"},
			{Path: "a.go", Line: 1, Side: "left", Body: strings.Repeat("c", MaxDraftCommentBytes+1)},
			{Path: "a.go", Line: 2, Body: "  "},
		},
	})
	if d == nil || d.Commit != testCommit {
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
	many := &DraftReview{Commit: testCommit}
	for i := range MaxDraftComments + 5 {
		many.Comments = append(many.Comments, DraftComment{Path: "a.go", Line: i + 1, Body: "x"})
	}
	if got := len(BoundDraft(many).Comments); got != MaxDraftComments {
		t.Errorf("kept %d comments, want %d", got, MaxDraftComments)
	}
}

// Only a reply to a draft-review request keeps a draft, and it is bounded on the way
// in. A notice never carries one.
func TestAppendReplyDraftOnlyForDraftRequests(t *testing.T) {
	draft := &DraftReview{Commit: testCommit, Comments: []DraftComment{{Path: "a.go", Line: 1, Body: "x"}, {Path: "", Line: 1, Body: "dropped"}}}
	item := WorkItem{Thread: []Message{
		{ID: "q", Role: RoleUser, Content: "review?", Kind: KindReviewRequest},
		{ID: "d", Role: RoleUser, Content: "draft", Kind: KindDraftReviewRequest},
		{ID: "d2", Role: RoleUser, Content: "draft again", Kind: KindDraftReviewRequest},
	}}
	if _, err := AppendReply(&item, "q", Message{ID: "a1", Content: "a review", Draft: draft}); err != nil {
		t.Fatal(err)
	}
	if item.Thread[3].Draft != nil {
		t.Error("a reply to a review request kept a draft")
	}
	if _, err := AppendReply(&item, "d", Message{ID: "a2", Content: "drafted", Draft: draft}); err != nil {
		t.Fatal(err)
	}
	if got := item.Thread[4].Draft; got == nil || len(got.Comments) != 1 {
		t.Fatalf("draft reply = %+v", got)
	}
	if _, err := AppendReply(&item, "d2", Message{ID: "a3", Content: "sign in", Notice: NoticeReconnect, Draft: draft}); err != nil {
		t.Fatal(err)
	}
	if item.Thread[5].Draft != nil {
		t.Error("a notice kept a draft")
	}
	if got := item.LatestDraft(); got != 4 {
		t.Errorf("LatestDraft = %d, want 4", got)
	}
	item.Thread[4].Hidden = true
	if got := item.LatestDraft(); got != -1 {
		t.Errorf("a hidden newest draft is offered: LatestDraft = %d", got)
	}
}

// A run never reads a stored draft back: the window it reads carries the reply's
// content only.
func TestThreadWindowLeavesDraftsOut(t *testing.T) {
	item := WorkItem{ID: "i", Thread: []Message{
		{ID: "d", Role: RoleUser, Content: "draft", Kind: KindDraftReviewRequest, At: time.Now()},
		{ID: "a", ReplyTo: "d", Role: RoleAgent, Content: "drafted", Draft: &DraftReview{Commit: testCommit, Body: "b"}, At: time.Now()},
	}}
	for _, turn := range []string{"", "d"} {
		w := ThreadWindow(item, turn, MaxItemBytes)
		for _, m := range w.Thread {
			if m.Draft != nil {
				t.Errorf("turn %q: the window carries a draft", turn)
			}
		}
	}
	if item.Thread[1].Draft == nil {
		t.Error("ThreadWindow changed the stored item")
	}
}

// The digest follows the draft's content, so the thread page's confirmation can tell
// the draft it showed from another.
func TestDraftDigest(t *testing.T) {
	a := &DraftReview{Commit: testCommit, Body: "b"}
	b := &DraftReview{Commit: testCommit, Body: "c"}
	if a.Digest() == b.Digest() || a.Digest() != (&DraftReview{Commit: testCommit, Body: "b"}).Digest() {
		t.Error("digest does not follow content")
	}
	var nilDraft *DraftReview
	if nilDraft.Digest() != "" || !nilDraft.Empty() {
		t.Error("a nil draft has a digest or content")
	}
}
