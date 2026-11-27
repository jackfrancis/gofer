package worklist

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

// testDiff is the digest of the whole diff a review read (github.DiffDigest).
var testDiff = PatchDigest("the whole diff")

// pinned is a draft pinned to o/r#1 at testCommit against main, with a patch
// digest for every commented file.
func pinned(body string, comments ...DraftComment) *DraftReview {
	d := &DraftReview{Repo: "o/r", Number: 1, Commit: testCommit, BaseRef: "main", Body: body, Comments: comments}
	for _, c := range comments {
		if d.Patches == nil {
			d.Patches = map[string]string{}
		}
		d.Patches[c.Path] = PatchDigest("patch of " + c.Path)
	}
	return d
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
	// Digests are kept for commented files only, and a comment whose file has no
	// valid digest is dropped: it could not be checked before the review is created.
	if len(d.Patches) != 1 || d.Patches["a.go"] == "" {
		t.Errorf("patches = %v", d.Patches)
	}
	unchecked := pinned("", DraftComment{Path: "a.go", Line: 1, Body: "x"}, DraftComment{Path: "b.go", Line: 1, Body: "y"})
	unchecked.Patches["b.go"] = "not-a-digest"
	unchecked.Patches["c.go"] = PatchDigest("uncommented")
	if got := BoundDraft(unchecked); len(got.Comments) != 1 || got.Comments[0].Path != "a.go" || len(got.Patches) != 1 {
		t.Errorf("unchecked comment kept: %+v", got)
	}
	many := pinned("", DraftComment{Path: "a.go", Line: 1, Body: "x"})
	for i := range MaxDraftComments + 5 {
		many.Comments = append(many.Comments, DraftComment{Path: "a.go", Line: i + 2, Body: "x"})
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
	if _, err := AppendReply(&item, "d", Message{ID: "a2", Content: "drafted", Draft: draft, DraftReceipt: receipt, HiddenRev: 99, ReadRev: 99, ReadRevKnown: true}); err != nil {
		t.Fatal(err)
	}
	if got := item.Thread[5]; got.Draft == nil || len(got.Draft.Comments) != 1 || got.DraftReceipt != nil || got.HiddenRev != 0 || got.ReadRev != 0 || got.ReadRevKnown {
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

// Drafting again is redundant only while the drafter would read the conversation it
// read: any visible turn after the draft request but its own reply, a visibility
// change before it since the request, a receipt, or a retired draft opens drafting
// again; hidden turns after it, and toggles before the request, do not.
func TestRedraftRedundant(t *testing.T) {
	asked := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	q0 := Message{ID: "q0", Role: RoleUser, Content: "leave out the rename"}
	d1 := Message{ID: "d1", Role: RoleUser, Kind: KindDraftReviewRequest, Content: "draft", At: asked, ReadRevKnown: true}
	a1 := Message{ID: "a1", ReplyTo: "d1", Role: RoleAgent, Content: "drafted", Draft: pinned("x")}
	if !draftPR(q0, d1, a1).RedraftRedundant() {
		t.Error("a thread ending with its draft is not redundant to redraft")
	}
	// A request saved before revisions were recorded says nothing about toggles of
	// its time: its draft never counts as redundant.
	unrecorded := d1
	unrecorded.ReadRevKnown = false
	if draftPR(q0, unrecorded, a1).RedraftRedundant() {
		t.Error("a draft whose request recorded no revision counts as redundant")
	}
	q := Message{ID: "q", Role: RoleUser, Content: "what about y?"}
	if draftPR(d1, a1, q).RedraftRedundant() {
		t.Error("a turn after the draft left redrafting redundant")
	}
	// A correction posted while the drafter worked, answered before the draft landed:
	// the draft is last, but it never read the correction.
	qa := Message{ID: "qa", ReplyTo: "q", Role: RoleAgent, Content: "noted"}
	if draftPR(d1, q, qa, a1).RedraftRedundant() {
		t.Error("a correction posted while drafting left redrafting redundant")
	}
	hiddenQ := q
	hiddenQ.Hidden = true
	if !draftPR(d1, a1, hiddenQ).RedraftRedundant() || !draftPR(d1, hiddenQ, a1).RedraftRedundant() {
		t.Error("a hidden turn after the request, which the drafter never reads, opened redrafting")
	}
	// Hiding or unhiding an earlier turn since the request was saved (a revision
	// above the request's) changes what the drafter would read; a toggle saved
	// before the request does not.
	toggled := q0
	toggled.HiddenRev = 3
	read := d1
	read.ReadRev = 2
	if draftPR(toggled, read, a1).RedraftRedundant() {
		t.Error("an earlier turn's visibility changed since the request, yet redrafting is redundant")
	}
	read.ReadRev = 3
	if !draftPR(toggled, read, a1).RedraftRedundant() {
		t.Error("a toggle saved before the request opened redrafting")
	}
	// A retry restamps the request's time but keeps its revision: the slow first
	// run's draft, landing last, is still judged by what it could have read.
	retried := draftPR(q0, d1, a1)
	retried.SetHidden(0, true)
	retried.Restamp(1, asked.Add(time.Hour))
	if retried.RedraftRedundant() {
		t.Error("a retry made a hide the first run never saw look older than the request")
	}
	confirmed := a1
	confirmed.DraftReceipt = &DraftReceipt{State: DraftCreated, At: time.Now()}
	if draftPR(d1, confirmed).RedraftRedundant() {
		t.Error("a confirmed draft left redrafting closed")
	}
	empty := Message{ID: "a1", ReplyTo: "d1", Role: RoleAgent, Content: "Nothing to draft."}
	if draftPR(d1, empty).RedraftRedundant() || draftPR(d1).RedraftRedundant() || draftPR().RedraftRedundant() {
		t.Error("no offered draft, yet redrafting is redundant")
	}
	other := draftPR(d1, a1)
	other.GitHub.Number = 2
	if other.RedraftRedundant() {
		t.Error("a draft pinned to another pull request left redrafting closed")
	}
	// A legacy reply (no ReplyTo) answers the request directly before it.
	legacy := a1
	legacy.ReplyTo = ""
	if !draftPR(q0, d1, legacy).RedraftRedundant() {
		t.Error("a legacy draft reply is not recognized as answering its request")
	}
}

// SetHidden gives each change of visibility, and only a change, the thread's next
// revision, whichever turn it changes.
func TestSetHiddenRevisions(t *testing.T) {
	it := draftPR(Message{ID: "q", Role: RoleUser, Content: "hi"}, Message{ID: "r", Role: RoleAgent, Content: "hello"})
	if it.SetHidden(0, false) || it.VisibilityRev() != 0 {
		t.Error("unhiding a visible turn changed it")
	}
	if !it.SetHidden(0, true) || !it.Thread[0].Hidden || it.Thread[0].HiddenRev != 1 {
		t.Errorf("hide: %+v", it.Thread[0])
	}
	if it.SetHidden(0, true) || it.Thread[0].HiddenRev != 1 {
		t.Error("hiding a hidden turn took a revision")
	}
	if !it.SetHidden(1, true) || it.Thread[1].HiddenRev != 2 || !it.SetHidden(0, false) || it.Thread[0].HiddenRev != 3 || it.VisibilityRev() != 3 {
		t.Errorf("revisions do not follow the order of changes: %+v", it.Thread)
	}
	if it.SetHidden(2, true) || it.SetHidden(-1, true) {
		t.Error("an out-of-range turn was changed")
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
	if _, _, err := ClaimDraft(ctx, store, "o", "github:o/r#1", "a", "stale", "", t1); !errors.Is(err, ErrDraftNotOffered) {
		t.Fatalf("a stale digest was claimed: %v", err)
	}
	it, prior, err := ClaimDraft(ctx, store, "o", "github:o/r#1", "a", digest, "", t1)
	if err != nil || prior != nil || it.Thread[1].DraftReceipt == nil || it.Thread[1].DraftReceipt.State != DraftCreating {
		t.Fatalf("claim: %+v %v %v", it.Thread[1].DraftReceipt, prior, err)
	}
	// A replay while the claim stands writes nothing and gets it back.
	if _, prior, err := ClaimDraft(ctx, store, "o", "github:o/r#1", "a", digest, "", t1.Add(time.Second)); err != nil || prior == nil || prior.State != DraftCreating {
		t.Fatalf("replay: %+v %v", prior, err)
	}
	// Someone else's settle (another claim time) changes nothing.
	_ = SettleDraft(ctx, store, "o", "github:o/r#1", "a", t1.Add(time.Second), nil)
	// Release, then claim again.
	if err := SettleDraft(ctx, store, "o", "github:o/r#1", "a", t1, nil); err != nil {
		t.Fatal(err)
	}
	t2 := t1.Add(time.Minute)
	if _, prior, err := ClaimDraft(ctx, store, "o", "github:o/r#1", "a", digest, "", t2); err != nil || prior != nil {
		t.Fatalf("reclaim after release: %+v %v", prior, err)
	}
	created := &DraftReceipt{State: DraftCreated, URL: "https://github.com/o/r/pull/1#pullrequestreview-1", At: t2}
	if err := SettleDraft(ctx, store, "o", "github:o/r#1", "a", t2, created); err != nil {
		t.Fatal(err)
	}
	// Replayed after the review was submitted or discarded on GitHub: still the receipt.
	if _, prior, err := ClaimDraft(ctx, store, "o", "github:o/r#1", "a", digest, "", t2.Add(time.Hour)); err != nil || prior == nil || prior.State != DraftCreated || prior.URL != created.URL {
		t.Fatalf("replay after creation: %+v %v", prior, err)
	}
	// The item now names another pull request: the draft is not offered.
	_ = store.Update(ctx, "o", func(cur []WorkItem) ([]WorkItem, error) {
		cur[0].GitHub.Number = 2
		cur[0].Thread[1].DraftReceipt = nil
		return cur, nil
	})
	if _, _, err := ClaimDraft(ctx, store, "o", "github:o/r#1", "a", digest, "", t2.Add(2*time.Hour)); !errors.Is(err, ErrDraftNotOffered) {
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

// An approving draft with nothing else is still a draft: its approval is kept apart
// from the summary, so the summary's bound never cuts it, and the review body puts
// the commands after the summary, each on a line of its own.
func TestApprovingDraft(t *testing.T) {
	d := pinned("")
	d.Approve, d.Diff = true, testDiff
	got := BoundDraft(d)
	if got == nil || !got.Approve || got.Body != "" || len(got.Comments) != 0 {
		t.Fatalf("approval-only draft = %+v", got)
	}
	long := pinned(strings.Repeat("b", MaxDraftBodyBytes+10))
	long.Approve, long.Diff = true, testDiff
	if got := BoundDraft(long); !got.Approve || len(got.Body) > MaxDraftBodyBytes {
		t.Fatalf("long approving draft: approve %v, body %d bytes", got.Approve, len(got.Body))
	}
	if d.Digest() == pinned("").Digest() {
		t.Error("the approval is not covered by the digest")
	}
	for _, tc := range []struct {
		summary string
		approve bool
		want    string
	}{
		{"", false, ""},
		{"- Add a test.", false, "- Add a test."},
		{"", true, "/lgtm\n/approve"},
		{"- Add a test.", true, "- Add a test.\n\n/lgtm\n/approve"},
	} {
		if got := ReviewBody(tc.summary, tc.approve); got != tc.want {
			t.Errorf("ReviewBody(%q, %v) = %q, want %q", tc.summary, tc.approve, got, tc.want)
		}
	}
}

// The store judges an approval by the latest review before the draft request, the
// one the draft's run read: a review that landed after the request neither makes
// nor unmakes it.
func TestAppendReplyApprovalJudgedByTheReviewBeforeTheRequest(t *testing.T) {
	const later = "fedcba9876543210fedcba9876543210fedcba98"
	item := draftPR(
		Message{ID: "q", Role: RoleUser, Content: "review?", Kind: KindReviewRequest},
		Message{ID: "r", ReplyTo: "q", Role: RoleAgent, Content: "LGTM", Head: testCommit, Base: "main", Diff: testDiff},
		Message{ID: "d", Role: RoleUser, Content: "draft", Kind: KindDraftReviewRequest, ReadRevKnown: true},
		Message{ID: "q2", Role: RoleUser, Content: "review?", Kind: KindReviewRequest},
		Message{ID: "r2", ReplyTo: "q2", Role: RoleAgent, Content: "LGTM", Head: later, Base: "main", Diff: testDiff},
	)
	d := pinned("")
	d.Approve, d.Diff = true, testDiff
	if _, err := AppendReply(&item, "d", Message{ID: "a", Content: "approve", Draft: d, Omitted: true}); err != nil {
		t.Fatal(err)
	}
	m := item.Thread[len(item.Thread)-1]
	if m.Draft == nil || !m.Draft.Approve {
		t.Fatalf("approval of the review before the request = %+v", m.Draft)
	}
	if m.Omitted {
		t.Error("a read's Omitted mark was stored")
	}
}

// The store cannot rebuild a read made before a turn was hidden or unhidden: an
// approval whose request saw an earlier turn's visibility change since it was saved
// is withheld, even when the read the store would serve now fits (a run that missed
// a visible hold, then unrelated turns hidden while it worked). The reply's text
// says so, and the comments stand.
func TestAppendReplyWithholdsAnApprovalAfterAVisibilityChange(t *testing.T) {
	th := []Message{
		{ID: "q", Role: RoleUser, Content: "review?", Kind: KindReviewRequest},
		{ID: "hold", Role: RoleUser, Content: "Do not approve yet."},
		{ID: "r", ReplyTo: "q", Role: RoleAgent, Content: "LGTM", Head: testCommit, Base: "main", Diff: testDiff},
	}
	for i := range 24 { // about 4.8 MB since the review: more than a read holds
		th = append(th, Message{ID: "u" + strconv.Itoa(i), Role: RoleUser, Content: strings.Repeat("chat ", 40000)})
	}
	item := draftPR(append(th, Message{ID: "d", Role: RoleUser, Content: "draft", Kind: KindDraftReviewRequest, ReadRevKnown: true})...)
	for i := range 24 { // hidden while the run worked: now the read fits
		item.SetHidden(3+i, true)
	}
	if !item.readsReviewWhole(len(item.Thread) - 1) {
		t.Fatal("fixture: the read should fit once the filler is hidden")
	}
	d := pinned("", DraftComment{Path: "a.go", Line: 1, Side: SideRight, Body: "nit"})
	d.Approve, d.Diff = true, testDiff
	text := "**Drafted 1 inline review comment** from the points this conversation agrees on." + ApprovalAlso + " Nothing is posted until you create the pending review below.\n\n1. `a.go` line 1:\n\n   nit\n" + ApprovalBlock
	if _, err := AppendReply(&item, "d", Message{ID: "a", Content: text, Draft: d}); err != nil {
		t.Fatal(err)
	}
	m := item.Thread[len(item.Thread)-1]
	if m.Draft == nil || m.Draft.Approve || len(m.Draft.Comments) != 1 {
		t.Fatalf("approval after a visibility change: %+v", m.Draft)
	}
	if strings.Contains(m.Content, ApprovalAlso) || strings.Contains(m.Content, ApprovalCommands) || !strings.Contains(m.Content, "hidden or unhidden after this draft was asked for") || !strings.Contains(m.Content, "   nit") {
		t.Errorf("the text does not match the stored draft:\n%s", m.Content)
	}
	if item.RedraftRedundant() {
		t.Error("drafting again is not offered")
	}
}

// When the approval was the whole draft, withholding it leaves no draft, and the
// text keeps no line about one: no pending review is offered, and none is described.
func TestWithheldApprovalOnlyDraftLeavesNoClaim(t *testing.T) {
	d := pinned("")
	d.Approve, d.Diff = true, testDiff
	reply := Message{Content: ApprovalLead + " of commit `0123`: this conversation agrees. Nothing is posted until you create the pending review below.\n\n**Left out — no consensus yet**\n\n- the rename\n" + ApprovalBlock, Draft: d}
	withholdApproval(&reply, NoApprovalUnread)
	if reply.Draft != nil || strings.Contains(reply.Content, ApprovalLead) || strings.Contains(reply.Content, "pending review") || strings.Contains(reply.Content, ApprovalCommands) {
		t.Errorf("withheld approval-only draft: %+v\n%s", reply.Draft, reply.Content)
	}
	if !strings.Contains(reply.Content, "- the rename") || !strings.HasSuffix(reply.Content, NoApprovalUnread) {
		t.Errorf("the rest of the text, or why, is gone:\n%s", reply.Content)
	}
}

// A run that withheld what drafting again may draft (an approval it could not check
// against an older web tier's read) keeps the button.
func TestRedraftOfferedWhenTheRunSaysItHelps(t *testing.T) {
	d1 := Message{ID: "d1", Role: RoleUser, Kind: KindDraftReviewRequest, Content: "draft", ReadRevKnown: true}
	a1 := Message{ID: "a1", ReplyTo: "d1", Role: RoleAgent, Content: "drafted", Draft: pinned("x"), RedraftHelps: true}
	if draftPR(d1, a1).RedraftRedundant() {
		t.Error("a draft whose run said drafting again helps counts as redundant")
	}
}

// The store keeps an approval only if the run's read held the latest review and
// every visible turn since it was asked for, judged on the read it serves, whatever
// the run sent: a run that never checked (one from before the check) cannot approve
// past an owner's word it did not read, even one posted while the review stalled.
func TestAppendReplyApprovalNeedsTheWholeConversationSinceTheReview(t *testing.T) {
	build := func(filler int) WorkItem {
		th := []Message{
			{ID: "q", Role: RoleUser, Content: "review?", Kind: KindReviewRequest},
			{ID: "hold", Role: RoleUser, Content: "Do not approve yet."},
			{ID: "r", ReplyTo: "q", Role: RoleAgent, Content: "LGTM", Head: testCommit, Base: "main", Diff: testDiff},
		}
		for i := range filler {
			th = append(th, Message{ID: "u" + strconv.Itoa(i), Role: RoleUser, Content: strings.Repeat("chat ", 40000)})
		}
		return draftPR(append(th, Message{ID: "d", Role: RoleUser, Content: "draft", Kind: KindDraftReviewRequest, ReadRevKnown: true})...)
	}
	approve := func(it *WorkItem) *DraftReview {
		d := pinned("")
		d.Approve, d.Diff = true, testDiff
		if _, err := AppendReply(it, "d", Message{ID: "a", Content: "approve", Draft: d}); err != nil {
			t.Fatal(err)
		}
		return it.Thread[len(it.Thread)-1].Draft
	}
	short := build(2)
	if d := approve(&short); d == nil || !d.Approve {
		t.Fatalf("approval on a whole read = %+v", d)
	}
	long := build(24) // about 4.8 MB since the review: more than a read holds
	if d := approve(&long); d != nil {
		t.Errorf("approval past an unread hold kept: %+v", d)
	}
}

// A draft asked for before the latest review landed is what is stale, even when its
// reply lands after that review: the thread offers drafting again, never a review
// of the revision that review already read.
func TestLateDraftDoesNotOutdateANewerReview(t *testing.T) {
	const pushed = "fedcba9876543210fedcba9876543210fedcba98"
	it := draftPR(
		Message{ID: "q", Role: RoleUser, Content: "review?", Kind: KindReviewRequest},
		Message{ID: "r", ReplyTo: "q", Role: RoleAgent, Content: "LGTM", Head: testCommit, Base: "main", Diff: testDiff},
		Message{ID: "d", Role: RoleUser, Content: "draft", Kind: KindDraftReviewRequest, ReadRevKnown: true},
		Message{ID: "q2", Role: RoleUser, Content: "again", Kind: KindReviewRequest},
		Message{ID: "r2", ReplyTo: "q2", Role: RoleAgent, Content: "LGTM", Head: pushed, Base: "main", Diff: testDiff},
		Message{ID: "a", ReplyTo: "d", Role: RoleAgent, Content: "drafted", Head: testCommit, Base: "main", Diff: testDiff},
	)
	if it.ReviewOutdated() {
		t.Error("a late draft makes the newer review outdated")
	}
	if it.RedraftRedundant() {
		t.Error("a late draft is redundant to redraft")
	}
}

// The store keeps an approval only on a draft pinned to the commit the latest
// visible review read, whatever the run sent: a review of an earlier head, one that
// recorded none, or one the owner hid since does not approve this commit. A reply's
// head is kept only when it is a full commit id.
func TestAppendReplyApprovalNeedsTheReviewedHead(t *testing.T) {
	const older = "fedcba9876543210fedcba9876543210fedcba98"
	approving := func(comments ...DraftComment) *DraftReview {
		d := pinned("", comments...)
		d.Approve, d.Diff = true, testDiff
		return d
	}
	thread := func(reviewHead string) WorkItem {
		return draftPR(
			Message{ID: "q", Role: RoleUser, Content: "review?", Kind: KindReviewRequest},
			Message{ID: "r", ReplyTo: "q", Role: RoleAgent, Content: "LGTM", Head: reviewHead, Base: "main", Diff: testDiff},
			Message{ID: "d", Role: RoleUser, Content: "draft", Kind: KindDraftReviewRequest, ReadRevKnown: true},
		)
	}
	item := thread(testCommit)
	if _, err := AppendReply(&item, "d", Message{ID: "a", Content: "approve", Draft: approving()}); err != nil {
		t.Fatal(err)
	}
	if d := item.Thread[3].Draft; d == nil || !d.Approve {
		t.Fatalf("approval of the reviewed head = %+v", d)
	}
	for name, it := range map[string]WorkItem{"older head": thread(older), "no head": thread("")} {
		if _, err := AppendReply(&it, "d", Message{ID: "a", Content: "approve", Draft: approving()}); err != nil {
			t.Fatal(err)
		}
		if d := it.Thread[3].Draft; d != nil {
			t.Errorf("%s: approval-only draft kept: %+v", name, d)
		}
	}
	it := thread(older)
	if _, err := AppendReply(&it, "d", Message{ID: "a", Content: "approve", Draft: approving(DraftComment{Path: "a.go", Line: 1, Body: "nit"})}); err != nil {
		t.Fatal(err)
	}
	if d := it.Thread[3].Draft; d == nil || d.Approve || d.Diff != "" || len(d.Comments) != 1 || !d.Current() {
		t.Errorf("stale approval beside a comment: %+v (current %v)", d, d.Current())
	}
	// A newer review of another head, landing after the approved one, is the latest.
	it = draftPR(
		Message{ID: "q", Role: RoleUser, Content: "review?", Kind: KindReviewRequest},
		Message{ID: "q2", Role: RoleUser, Content: IndependentReviewPrompt, Kind: KindReviewRequest},
		Message{ID: "r", ReplyTo: "q", Role: RoleAgent, Content: "LGTM", Head: testCommit, Base: "main", Diff: testDiff},
		Message{ID: "r2", ReplyTo: "q2", Role: RoleAgent, Content: "LGTM", Head: older, Base: "main", Diff: testDiff},
	)
	if got := it.ReviewedRevision().Head; got != older {
		t.Errorf("ReviewedRevision = %q, want the review that landed last", got)
	}
	it.Thread[3].Hidden = true
	if got := it.ReviewedRevision().Head; got != testCommit {
		t.Errorf("ReviewedRevision with the latest hidden = %q", got)
	}
	// Revisions are normalized or dropped, as a pair, on the way in.
	it = draftPR(
		Message{ID: "q", Role: RoleUser, Content: "review?", Kind: KindReviewRequest},
		Message{ID: "q2", Role: RoleUser, Content: "again", Kind: KindReviewRequest},
		Message{ID: "q3", Role: RoleUser, Content: "and again", Kind: KindReviewRequest},
	)
	for key, m := range map[string]Message{
		"q":  {ID: "a", Content: "ok", Head: strings.ToUpper(testCommit), Base: "main"},
		"q2": {ID: "b", Content: "ok", Head: "main", Base: "main"},
		"q3": {ID: "c", Content: "ok", Head: testCommit},
	} {
		if _, err := AppendReply(&it, key, m); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range it.Thread[3:] {
		switch m.ID {
		case "a":
			if m.Head != testCommit || m.Base != "main" {
				t.Errorf("valid revision stored as %q %q", m.Head, m.Base)
			}
		default:
			if m.Head != "" || m.Base != "" {
				t.Errorf("reply %s kept an invalid revision %q %q", m.ID, m.Head, m.Base)
			}
		}
	}
}

// A retarget changes what would merge without moving the head: an approval of the
// head against the old base does not stand on a draft against the new one.
func TestAppendReplyApprovalNeedsTheReviewedBase(t *testing.T) {
	it := draftPR(
		Message{ID: "q", Role: RoleUser, Content: "review?", Kind: KindReviewRequest},
		Message{ID: "r", ReplyTo: "q", Role: RoleAgent, Content: "LGTM", Head: testCommit, Base: "main", Diff: testDiff},
		Message{ID: "d", Role: RoleUser, Content: "draft", Kind: KindDraftReviewRequest},
	)
	d := pinned("")
	d.Approve, d.BaseRef, d.Diff = true, "release", testDiff
	if _, err := AppendReply(&it, "d", Message{ID: "a", Content: "approve", Draft: d, Head: testCommit, Base: "release", Diff: testDiff}); err != nil {
		t.Fatal(err)
	}
	if it.Thread[3].Draft != nil {
		t.Fatalf("approval against a retargeted base kept: %+v", it.Thread[3].Draft)
	}
	// The draft reply recorded the revision it read, which the latest review did
	// not: the thread offers a review of the current revision until one lands.
	if !it.ReviewOutdated() {
		t.Error("ReviewOutdated = false after a retarget")
	}
	it.Thread = append(it.Thread,
		Message{ID: "q2", Role: RoleUser, Content: "again", Kind: KindReviewRequest},
		Message{ID: "r2", ReplyTo: "q2", Role: RoleAgent, Content: "LGTM", Head: testCommit, Base: "release", Diff: testDiff},
	)
	if it.ReviewOutdated() {
		t.Error("ReviewOutdated = true after a review of the current revision")
	}
}

// A draft stored under older rules (here, before command lines were neutralized) is
// not offered or confirmable, and drafting again is not redundant; one stored under
// today's rules is.
func TestOutdatedDraftIsNotOffered(t *testing.T) {
	old := pinned("Ship it.\n/lgtm")
	if old.Current() {
		t.Fatal("a draft with a live command is current")
	}
	if !BoundDraft(old).Current() {
		t.Fatal("a bounded draft is not current")
	}
	thread := func(d *DraftReview) WorkItem {
		return draftPR(
			Message{ID: "q", Role: RoleUser, Content: "review?", Kind: KindReviewRequest},
			Message{ID: "r", ReplyTo: "q", Role: RoleAgent, Content: "LGTM"},
			Message{ID: "d", Role: RoleUser, Content: "draft", Kind: KindDraftReviewRequest, ReadRevKnown: true},
			Message{ID: "a", ReplyTo: "d", Role: RoleAgent, Content: "drafted", Draft: d},
		)
	}
	it := thread(old)
	if _, ok := it.OfferedDraft("a", old.Digest()); ok {
		t.Error("an outdated draft is offered")
	}
	if it.RedraftRedundant() {
		t.Error("drafting again over an outdated draft counts as redundant")
	}
	cur := BoundDraft(old)
	it = thread(cur)
	if _, ok := it.OfferedDraft("a", cur.Digest()); !ok || !it.RedraftRedundant() {
		t.Error("a current draft is not offered, or a redraft over it is not redundant")
	}
}

// Model-written draft text never carries a live bot command: a line a bot would
// read as one (any command, approval or not) has its command wrapped in backticks,
// in the summary and in every comment, so the approval checkbox is the only source
// of /lgtm and /approve. Paths and ordinary prose are left alone.
func TestDraftTextCarriesNoCommands(t *testing.T) {
	for in, want := range map[string]string{
		"/lgtm":                          "`/lgtm`",
		"Looks good.\n/approve\nThanks!": "Looks good.\n`/approve`\nThanks!",
		"  /LGTM cancel":                 "  `/LGTM` cancel",
		"/hold\r\n/retest":               "`/hold`\n`/retest`",
		"/usr/bin/env is fine":           "/usr/bin/env is fine",
		"run `/lgtm` when ready":         "run `/lgtm` when ready",
		"- /approve once tests pass":     "- /approve once tests pass",
		"// a Go comment":                "// a Go comment",
		"ratio is a/b":                   "ratio is a/b",
	} {
		if got := NeutralizeCommands(in); got != want {
			t.Errorf("NeutralizeCommands(%q) = %q, want %q", in, got, want)
		}
	}
	// A suggestion is applied verbatim, so one holding a command line is never altered
	// and left applicable: it becomes plain code, with a note. One without stays as
	// written, and so does a plain block's ordinary code.
	for in, want := range map[string]string{
		"Ignore the build output:\n```suggestion\n/dist\nnode_modules/\n```": "Ignore the build output:\n```text\n`/dist`\nnode_modules/\n```\n" + SuggestionNote,
		"````Suggestion\n/approve\n````\nafter":                              "````text\n`/approve`\n````\n" + SuggestionNote + "\nafter",
		"```suggestion\nreturn nil\n```":                                     "```suggestion\nreturn nil\n```",
		"```go\nx := a / b\n```":                                             "```go\nx := a / b\n```",
		"```\n/hold\n```":                                                    "```\n`/hold`\n```",
		"```suggestion\n/approve":                                            "```text\n`/approve`\n" + SuggestionNote,
	} {
		got := NeutralizeCommands(in)
		if got != want {
			t.Errorf("NeutralizeCommands(%q) = %q, want %q", in, got, want)
		}
		if again := NeutralizeCommands(got); again != got {
			t.Errorf("NeutralizeCommands is not a fixed point on %q: %q", got, again)
		}
	}
	d := pinned("Ship it.\n/lgtm", DraftComment{Path: "a.go", Line: 1, Body: "nit\n/approve"})
	got := BoundDraft(d)
	if got.Body != "Ship it.\n`/lgtm`" || got.Comments[0].Body != "nit\n`/approve`" || got.Approve {
		t.Errorf("bounded draft = %+v", got)
	}
}

// Line endings are normalized before commands are looked for, so a bare CR (which a
// browser turns into LF before the copy button reads the text) can never hide a
// command from the check and reveal it in what is pasted.
func TestNeutralizeNormalizesLineEndings(t *testing.T) {
	for in, want := range map[string]string{
		"Looks good.\r/approve": "Looks good.\n`/approve`",
		"a\r\nb\rc":             "a\nb\nc",
		"Ship it.\r\n/lgtm\r\n": "Ship it.\n`/lgtm`\n",
	} {
		if got := NeutralizeCommands(in); got != want {
			t.Errorf("NeutralizeCommands(%q) = %q, want %q", in, got, want)
		}
	}
	d := pinned("Ship it.\r/lgtm")
	if got := BoundDraft(d); got.Body != "Ship it.\n`/lgtm`" {
		t.Errorf("stored body %q", got.Body)
	}
	if d.Current() {
		t.Error("a draft stored with a bare CR is current")
	}
}

// Suggestion blocks are found as Markdown parses them: one nested in a list, its
// fence indented past three spaces, is demoted (not altered and left applicable),
// with the note indented to stay in the list item.
func TestNeutralizeFindsSuggestionsInLists(t *testing.T) {
	in := "1. Ignore the build output:\n\n    ```suggestion\n    /dist\n    node_modules/\n    ```\n2. Next point."
	want := "1. Ignore the build output:\n\n    ```text\n    `/dist`\n    node_modules/\n    ```\n    " + SuggestionNote + "\n2. Next point."
	got := NeutralizeCommands(in)
	if got != want {
		t.Fatalf("NeutralizeCommands = %q\nwant %q", got, want)
	}
	if again := NeutralizeCommands(got); again != got {
		t.Errorf("not a fixed point: %q", again)
	}
	// A list-nested suggestion without a command line is left exactly as written.
	clean := "- Fix:\n\n    ```suggestion\n    return nil\n    ```"
	if got := NeutralizeCommands(clean); got != clean {
		t.Errorf("clean nested suggestion changed: %q", got)
	}
}

// An approval is of the whole diff the latest review read: a stacked pull request
// whose dependency merged into its base shows a new diff with the same head and base
// name, and an approval of the old diff does not stand on it. An approving draft
// with no diff to pin approves nothing.
func TestApprovalNeedsTheReviewedDiff(t *testing.T) {
	moved := PatchDigest("a diff with the dependency's files gone")
	it := draftPR(
		Message{ID: "q", Role: RoleUser, Content: "review?", Kind: KindReviewRequest},
		Message{ID: "r", ReplyTo: "q", Role: RoleAgent, Content: "LGTM", Head: testCommit, Base: "main", Diff: testDiff},
		Message{ID: "d", Role: RoleUser, Content: "draft", Kind: KindDraftReviewRequest},
	)
	if !it.Reviewed(Revision{testCommit, "main", testDiff}) || it.Reviewed(Revision{testCommit, "main", moved}) || it.Reviewed(Revision{testCommit, "main", ""}) {
		t.Fatal("Reviewed does not compare the diff")
	}
	d := pinned("")
	d.Approve, d.Diff = true, moved
	if _, err := AppendReply(&it, "d", Message{ID: "a", Content: "approve", Draft: d, Head: testCommit, Base: "main", Diff: moved}); err != nil {
		t.Fatal(err)
	}
	if it.Thread[3].Draft != nil {
		t.Errorf("approval of a moved diff kept: %+v", it.Thread[3].Draft)
	}
	if !it.ReviewOutdated() {
		t.Error("a moved diff does not make the review outdated")
	}
	nodiff := pinned("")
	nodiff.Approve = true
	if BoundDraft(nodiff) != nil {
		t.Error("an approving draft with no diff to pin was kept")
	}
	review := draftPR(Message{ID: "q", Role: RoleUser, Content: "review?", Kind: KindReviewRequest})
	if _, err := AppendReply(&review, "q", Message{ID: "a", Content: "ok", Head: testCommit, Base: "main", Diff: "not-a-digest"}); err != nil {
		t.Fatal(err)
	}
	if m := review.Thread[1]; m.Head != testCommit || m.Diff != "" {
		t.Errorf("invalid diff stored: %+v", m)
	}
}

// A review that landed after the draft is the newer reading: even if it read yet
// another revision (pushed again since the draft), the review is current and the
// draft is what is stale, so no further review is offered.
func TestNewerReviewIsNotOutdated(t *testing.T) {
	const b, c = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccccccccccccccccccccccc"
	it := draftPR(
		Message{ID: "q", Role: RoleUser, Content: "review?", Kind: KindReviewRequest},
		Message{ID: "r", ReplyTo: "q", Role: RoleAgent, Content: "LGTM", Head: testCommit, Base: "main", Diff: testDiff},
		Message{ID: "d", Role: RoleUser, Content: "draft", Kind: KindDraftReviewRequest},
		Message{ID: "a", ReplyTo: "d", Role: RoleAgent, Content: "out of date", Head: b, Base: "main", Diff: testDiff},
	)
	if !it.ReviewOutdated() {
		t.Fatal("a review older than the draft, of another head, is not outdated")
	}
	it.Thread = append(it.Thread,
		Message{ID: "q2", Role: RoleUser, Content: "again", Kind: KindReviewRequest},
		Message{ID: "r2", ReplyTo: "q2", Role: RoleAgent, Content: "LGTM", Head: c, Base: "main", Diff: testDiff},
	)
	if it.ReviewOutdated() {
		t.Error("a review newer than the draft is outdated")
	}
}

// A refused current attempt is neither pending nor waiting to stall: it is retryable
// at once. A retry's new attempt is pending as usual.
func TestRefusedAttemptIsNotPending(t *testing.T) {
	now := time.Now()
	it := draftPR(Message{ID: "q", Role: RoleUser, Content: "review?", Kind: KindReviewRequest, At: now})
	if !it.ReplyPending(now, 5*time.Minute) {
		t.Fatal("a fresh turn is not pending")
	}
	if !it.MarkRefused("q", "") || it.ReplyPending(now, 5*time.Minute) || !it.ReplyStalled(now, 5*time.Minute) || !it.Ended("q") {
		t.Fatal("a refused attempt is pending, or not retryable at once")
	}
	if it.MarkRefused("q", "") {
		t.Error("marking the same refusal again changed something")
	}
	it.Restamp(0, now)
	if !it.ReplyPending(now, 5*time.Minute) || it.Ended("q") {
		t.Error("a retry's new attempt is covered by the earlier refusal")
	}
}
