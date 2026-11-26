package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jackfrancis/gofer/internal/github"
	"github.com/jackfrancis/gofer/internal/github/githubtest"
	"github.com/jackfrancis/gofer/internal/worklist"
)

const draftHead = "0123456789abcdef0123456789abcdef01234567"

// draftPatch adds lines 11-12 and removes old line 11 in a.go.
const draftPatch = "@@ -10,3 +10,4 @@ func f() {\n ctx\n-old\n+new1\n+new2\n tail"

// draftDiff is the digest of the whole diff draftGitHub serves.
var draftDiff = github.DiffDigest([]github.DiffFile{
	{Path: "a.go", Status: "modified", SHA: "blob-a", Patch: draftPatch},
	{Path: "logo.png", Status: "added", SHA: "blob-logo"},
})

type fakeDrafter struct {
	prop worklist.DraftProposal
	got  worklist.DraftInput
}

func (f *fakeDrafter) DraftReview(_ context.Context, in worklist.DraftInput) (worklist.DraftProposal, error) {
	f.got = in
	return f.prop, nil
}

// draftGitHub serves an open pull request o/r#1 at draftHead with one patched file.
func draftGitHub(t *testing.T, state string) string {
	t.Helper()
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"login":"octocat"}`))
		case "/repos/o/r/pulls/1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": state, "head": map[string]any{"sha": draftHead}, "changed_files": 2,
				"base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": "o/r"}},
			})
		case "/repos/o/r/issues/1":
			_, _ = w.Write([]byte(`{"body":"desc"}`))
		case "/repos/o/r/pulls/1/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"filename": "a.go", "status": "modified", "sha": "blob-a", "patch": draftPatch},
				{"filename": "logo.png", "status": "added", "sha": "blob-logo"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func draftItem() worklist.WorkItem {
	return worklist.WorkItem{
		ID: "github:o/r#1", Type: worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1, Title: "t"},
		Thread: []worklist.Message{
			{ID: "q", Role: worklist.RoleUser, Content: "review?", Kind: worklist.KindReviewRequest},
			{ID: "r", ReplyTo: "q", Role: worklist.RoleAgent, Content: "rename new1", Model: "m1", Head: draftHead, Base: "main", Diff: draftDiff},
			{ID: "h", Role: worklist.RoleUser, Content: "secret aside", Hidden: true},
			{ID: "d", Role: worklist.RoleUser, Content: "draft", Kind: worklist.KindDraftReviewRequest},
		},
	}
}

// A draft request answers with the agreed comments: those on a line the diff shows
// stay inline; one off the diff moves into the summary with its location; the
// points left out are listed; hidden turns never reach the drafter; and the draft is
// anchored to the head commit it read.
func TestRunConverseDraftsReview(t *testing.T) {
	sink := &memSink{items: []worklist.WorkItem{draftItem()}}
	fd := &fakeDrafter{prop: worklist.DraftProposal{
		Comments: []worklist.DraftComment{
			{Path: "a.go", Line: 11, Side: "RIGHT", Body: "Rename `new1`."},
			{Path: "a.go", Line: 11, Side: "LEFT", Body: "Why drop `old`?"},
			{Path: "a.go", Line: 10, Side: "LEFT", Body: "Context is not a removed line."},
			{Path: "a.go", Line: 99, Side: "RIGHT", Body: "Off the diff."},
			{Path: "logo.png", Line: 1, Body: "Binary file."},
		},
		General:  []string{"Add a test."},
		Excluded: []string{"Error wrapping: m1 and m2 disagree."},
	}}
	fc := &fakeConverser{reply: "must not be called"}
	err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: draftGitHub(t, "open"), ItemID: "github:o/r#1", Turn: "d", Converser: fc, Drafter: fd, Model: "m0"},
		fakeVendor{token: "t"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if fc.gotUser != "" {
		t.Error("a draft request was answered by the converser")
	}
	for _, m := range fd.got.History {
		if m.Hidden || m.Content == "secret aside" {
			t.Fatalf("a hidden turn reached the drafter: %+v", fd.got.History)
		}
	}
	if len(fd.got.History) != 2 || fd.got.ViewerLogin != "octocat" {
		t.Errorf("drafter input: history %d turns, viewer %q", len(fd.got.History), fd.got.ViewerLogin)
	}
	for _, want := range []string{"### a.go (modified)", "[R11] +new1", "[L11] -old", "[R10]  ctx", "logo.png (added) — no patch"} {
		if !strings.Contains(fd.got.Diff, want) {
			t.Errorf("diff lacks %q:\n%s", want, fd.got.Diff)
		}
	}
	th := sink.items[0].Thread
	reply := th[len(th)-1]
	if reply.ReplyTo != "d" || reply.Model != "m0" || reply.Draft == nil {
		t.Fatalf("reply = %+v", reply)
	}
	d := reply.Draft
	if d.Commit != draftHead || d.Repo != "o/r" || d.Number != 1 || d.BaseRef != "main" || len(d.Comments) != 2 {
		t.Fatalf("draft = %+v", d)
	}
	// The commented file's patch is recorded, so the web tier can tell if the
	// merge base moved under it.
	if len(d.Patches) != 1 || d.Patches["a.go"] != worklist.PatchDigest(draftPatch) {
		t.Fatalf("draft = %+v", d)
	}
	if d.Comments[0].Side != worklist.SideRight || d.Comments[1].Side != worklist.SideLeft {
		t.Errorf("comments = %+v", d.Comments)
	}
	for _, want := range []string{"Add a test.", "`a.go` line 99: Off the diff.", "`logo.png` line 1: Binary file.", "`a.go` line 10: Context is not a removed line."} {
		if !strings.Contains(d.Body, want) {
			t.Errorf("summary lacks %q: %q", want, d.Body)
		}
	}
	for _, want := range []string{"Drafted 2 inline review comments", "Rename `new1`.", "Left out — no consensus yet", "m1 and m2 disagree", "3 comments pointed at a line the diff does not show", "1 file had no patch"} {
		if !strings.Contains(reply.Content, want) {
			t.Errorf("reply lacks %q:\n%s", want, reply.Content)
		}
	}
}

// A draft with nothing agreed stores no draft, and a closed pull request, a missing
// drafter or an issue draft nothing at all.
func TestRunConverseDraftsNothing(t *testing.T) {
	run := func(t *testing.T, item worklist.WorkItem, state string, drafter worklist.ReviewDrafter) worklist.Message {
		t.Helper()
		sink := &memSink{items: []worklist.WorkItem{item}}
		err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: draftGitHub(t, state), ItemID: item.ID, Turn: "d", Converser: &fakeConverser{}, Drafter: drafter},
			fakeVendor{token: "t"}, sink)
		if err != nil {
			t.Fatal(err)
		}
		th := sink.items[0].Thread
		return th[len(th)-1]
	}
	if m := run(t, draftItem(), "open", &fakeDrafter{prop: worklist.DraftProposal{Excluded: []string{"all disputed"}}}); m.Draft != nil || !strings.Contains(m.Content, "Nothing to draft") || !strings.Contains(m.Content, "all disputed") {
		t.Errorf("nothing agreed: %+v", m)
	}
	if m := run(t, draftItem(), "closed", &fakeDrafter{}); m.Draft != nil || m.Content != draftClosedReply {
		t.Errorf("closed: %+v", m)
	}
	if m := run(t, draftItem(), "open", nil); m.Draft != nil || m.Content != draftUnavailableReply {
		t.Errorf("no drafter: %+v", m)
	}
	issue := draftItem()
	issue.Type = worklist.TypeIssue
	if m := run(t, issue, "open", &fakeDrafter{}); m.Draft != nil || m.Content != draftNotPRReply {
		t.Errorf("issue: %+v", m)
	}
}

// A conversation that agrees the pull request is ready to approve drafts the
// approval as a general comment, alone or beside agreed nits, and says so.
func TestRunConverseDraftsApproval(t *testing.T) {
	run := func(t *testing.T, prop worklist.DraftProposal) worklist.Message {
		t.Helper()
		sink := &memSink{items: []worklist.WorkItem{draftItem()}}
		err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: draftGitHub(t, "open"), ItemID: "github:o/r#1", Turn: "d", Converser: &fakeConverser{}, Drafter: &fakeDrafter{prop: prop}},
			fakeVendor{token: "t"}, sink)
		if err != nil {
			t.Fatal(err)
		}
		th := sink.items[0].Thread
		return th[len(th)-1]
	}
	m := run(t, worklist.DraftProposal{Approve: true})
	if d := m.Draft; d == nil || !d.Approve || d.Body != "" || len(d.Comments) != 0 || d.Commit != draftHead {
		t.Fatalf("approval-only draft = %+v", m.Draft)
	}
	for _, want := range []string{"Drafted an approval", "/lgtm\n/approve"} {
		if !strings.Contains(m.Content, want) {
			t.Errorf("reply lacks %q:\n%s", want, m.Content)
		}
	}
	if strings.Contains(m.Content, "Nothing to draft") {
		t.Errorf("an approval read as nothing to draft:\n%s", m.Content)
	}
	m = run(t, worklist.DraftProposal{Approve: true, Comments: []worklist.DraftComment{{Path: "a.go", Line: 11, Side: "RIGHT", Body: "Nit: rename."}}})
	if d := m.Draft; d == nil || !d.Approve || len(d.Comments) != 1 {
		t.Fatalf("approval with a nit = %+v", m.Draft)
	}
	for _, want := range []string{"Drafted 1 inline review comment", "ready to approve", "**Approval**"} {
		if !strings.Contains(m.Content, want) {
			t.Errorf("reply lacks %q:\n%s", want, m.Content)
		}
	}
}

// An approval is of the commit the latest review read: when the head moved since,
// or that review recorded no commit, the run drafts no approval and says why, while
// the agreed comments still stand.
func TestRunConverseWithholdsStaleApproval(t *testing.T) {
	for name, head := range map[string]string{"pushed since": "fedcba9876543210fedcba9876543210fedcba98", "unrecorded": ""} {
		t.Run(name, func(t *testing.T) {
			item := draftItem()
			item.Thread[1].Head = head
			sink := &memSink{items: []worklist.WorkItem{item}}
			fd := &fakeDrafter{prop: worklist.DraftProposal{Approve: true}}
			err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: draftGitHub(t, "open"), ItemID: item.ID, Turn: "d", Converser: &fakeConverser{}, Drafter: fd},
				fakeVendor{token: "t"}, sink)
			if err != nil {
				t.Fatal(err)
			}
			if fd.got.Head != draftHead {
				t.Errorf("drafter told head %q", fd.got.Head)
			}
			th := sink.items[0].Thread
			m := th[len(th)-1]
			if m.Draft != nil {
				t.Fatalf("stale approval drafted: %+v", m.Draft)
			}
			why := "is now commit `0123456789ab` against `main`"
			if head == "" {
				why = "did not record the revision it read"
			}
			for _, want := range []string{"No approval drafted", why, "Review the current revision"} {
				if !strings.Contains(m.Content, want) {
					t.Errorf("reply lacks %q:\n%s", want, m.Content)
				}
			}
			if strings.Contains(m.Content, "Nothing to draft") {
				t.Errorf("reply says nothing to draft:\n%s", m.Content)
			}
		})
	}
	item := draftItem()
	item.Thread[1].Head = "fedcba9876543210fedcba9876543210fedcba98"
	sink := &memSink{items: []worklist.WorkItem{item}}
	fd := &fakeDrafter{prop: worklist.DraftProposal{Approve: true, Comments: []worklist.DraftComment{{Path: "a.go", Line: 11, Side: "RIGHT", Body: "Rename."}}}}
	if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: draftGitHub(t, "open"), ItemID: item.ID, Turn: "d", Converser: &fakeConverser{}, Drafter: fd},
		fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	th := sink.items[0].Thread
	if d := th[len(th)-1].Draft; d == nil || d.Approve || len(d.Comments) != 1 {
		t.Errorf("comments beside a stale approval = %+v", d)
	}
}

// A drafter that correctly declines to approve an old head still leaves the owner
// a way forward: the reply says the latest review is out of date, the draft reply
// records the revision it read, and the thread offers a review of the current one.
// A retarget (same head, new base) is out of date too.
func TestRunConverseFlagsStaleReviewWithoutApproval(t *testing.T) {
	for name, set := range map[string]func(*worklist.Message){
		"pushed":     func(m *worklist.Message) { m.Head = "fedcba9876543210fedcba9876543210fedcba98" },
		"retargeted": func(m *worklist.Message) { m.Base = "release" },
	} {
		t.Run(name, func(t *testing.T) {
			item := draftItem()
			set(&item.Thread[1])
			sink := &memSink{items: []worklist.WorkItem{item}}
			if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: draftGitHub(t, "open"), ItemID: item.ID, Turn: "d", Converser: &fakeConverser{}, Drafter: &fakeDrafter{}},
				fakeVendor{token: "t"}, sink); err != nil {
				t.Fatal(err)
			}
			th := sink.items[0].Thread
			m := th[len(th)-1]
			for _, want := range []string{"The latest review is out of date", "Review the current revision"} {
				if !strings.Contains(m.Content, want) {
					t.Errorf("reply lacks %q:\n%s", want, m.Content)
				}
			}
			if m.Head != draftHead || m.Base != "main" || !sink.items[0].ReviewOutdated() {
				t.Errorf("draft reply revision %q %q; ReviewOutdated %v", m.Head, m.Base, sink.items[0].ReviewOutdated())
			}
		})
	}
	// A current review: no note, and nothing outdated.
	sink := &memSink{items: []worklist.WorkItem{draftItem()}}
	if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: draftGitHub(t, "open"), ItemID: "github:o/r#1", Turn: "d", Converser: &fakeConverser{}, Drafter: &fakeDrafter{}},
		fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	th := sink.items[0].Thread
	if m := th[len(th)-1]; strings.Contains(m.Content, "out of date") || sink.items[0].ReviewOutdated() {
		t.Errorf("a current review was flagged:\n%s", m.Content)
	}
}

// A review's reply records the head commit the run read, which a later approval
// must match.
func TestRunConverseRecordsReviewedHead(t *testing.T) {
	item := draftItem()
	item.Thread = item.Thread[:1]
	sink := &memSink{items: []worklist.WorkItem{item}}
	if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: draftGitHub(t, "open"), ItemID: item.ID, Turn: "q", Converser: &fakeConverser{reply: "LGTM"}},
		fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	th := sink.items[0].Thread
	if m := th[len(th)-1]; m.ReplyTo != "q" || m.Head != draftHead || m.Base != "main" || m.Diff != draftDiff {
		t.Fatalf("reply = %+v", m)
	}
	if !sink.items[0].Reviewed(worklist.Revision{Head: draftHead, Base: "main", Diff: draftDiff}) {
		t.Errorf("Reviewed(head, base) = false: %+v", th[len(th)-1])
	}
}

// A stacked pull request whose dependency merged into its base shows a new diff
// with the same head and base name: the review of the old diff approves nothing, the
// reply says why, and an approval is pinned to the diff only when it matches.
func TestRunConverseApprovalNeedsTheReviewedDiff(t *testing.T) {
	item := draftItem()
	item.Thread[1].Diff = worklist.PatchDigest("the diff before the dependency merged")
	sink := &memSink{items: []worklist.WorkItem{item}}
	if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: draftGitHub(t, "open"), ItemID: item.ID, Turn: "d", Converser: &fakeConverser{}, Drafter: &fakeDrafter{prop: worklist.DraftProposal{Approve: true}}},
		fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	th := sink.items[0].Thread
	m := th[len(th)-1]
	if m.Draft != nil || !strings.Contains(m.Content, "its changes are no longer the ones that review read") || m.Diff != draftDiff {
		t.Fatalf("reply = %+v", m)
	}
	sink = &memSink{items: []worklist.WorkItem{draftItem()}}
	if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: draftGitHub(t, "open"), ItemID: "github:o/r#1", Turn: "d", Converser: &fakeConverser{}, Drafter: &fakeDrafter{prop: worklist.DraftProposal{Approve: true}}},
		fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	th = sink.items[0].Thread
	if d := th[len(th)-1].Draft; d == nil || !d.Approve || d.Diff != draftDiff {
		t.Errorf("approval of the reviewed diff = %+v", d)
	}
}

// A review's context and its recorded revision are one snapshot: the model is told
// the head and files gofer records, even when a push lands while it works, and then
// the review vouches for no diff (the model's tools may have read the new head), so
// no approval can stand on it.
func TestRunConverseReviewRecordsItsSnapshot(t *testing.T) {
	const pushed = "fedcba9876543210fedcba9876543210fedcba98"
	reads := 0
	srv := httptest.NewServer(githubtest.PublicRepos(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"login":"octocat"}`))
		case "/repos/o/r/issues/1":
			_, _ = w.Write([]byte(`{"body":"desc"}`))
		case "/repos/o/r/pulls/1":
			head := draftHead // the snapshot's two reads; every read after is the push
			if reads >= 2 {
				head = pushed
			}
			reads++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]any{"sha": head}, "changed_files": 2,
				"base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": "o/r"}},
			})
		case "/repos/o/r/pulls/1/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"filename": "a.go", "status": "modified", "sha": "blob-a", "patch": draftPatch},
				{"filename": "logo.png", "status": "added", "sha": "blob-logo"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	item := draftItem()
	item.Thread = item.Thread[:1]
	sink := &memSink{items: []worklist.WorkItem{item}}
	fc := &fakeConverser{reply: "LGTM"}
	if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: srv.URL, ItemID: item.ID, Turn: "q", Converser: fc}, fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fc.gotSource, "Head commit: "+draftHead) || strings.Contains(fc.gotSource, pushed) {
		t.Errorf("the model was not shown the snapshot's head:\n%s", fc.gotSource)
	}
	th := sink.items[0].Thread
	if m := th[len(th)-1]; m.Head != draftHead || m.Diff != "" {
		t.Errorf("a review that saw the pull request move vouches for a diff: %+v", m)
	}
}

// A draft from a discussion with no review never pins an approval: comments need no
// review, so the reply says nothing about one unless the drafter proposed an
// approval, which it withholds and says a review is needed for.
func TestRunConverseDraftsFromADiscussion(t *testing.T) {
	discussion := func() worklist.WorkItem {
		it := draftItem()
		it.Thread[0].Kind = "" // a Discuss question, not a review request
		it.Thread[1].Head, it.Thread[1].Base, it.Thread[1].Diff = draftHead, "main", ""
		return it
	}
	run := func(t *testing.T, prop worklist.DraftProposal) worklist.Message {
		t.Helper()
		sink := &memSink{items: []worklist.WorkItem{discussion()}}
		if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: draftGitHub(t, "open"), ItemID: "github:o/r#1", Turn: "d", Converser: &fakeConverser{}, Drafter: &fakeDrafter{prop: prop}},
			fakeVendor{token: "t"}, sink); err != nil {
			t.Fatal(err)
		}
		th := sink.items[0].Thread
		return th[len(th)-1]
	}
	m := run(t, worklist.DraftProposal{Comments: []worklist.DraftComment{{Path: "a.go", Line: 11, Side: "RIGHT", Body: "Note the override."}}})
	if m.Draft == nil || len(m.Draft.Comments) != 1 || m.Draft.Approve || strings.Contains(m.Content, "review") && strings.Contains(m.Content, "out of date") {
		t.Errorf("comments from a discussion: %+v\n%s", m.Draft, m.Content)
	}
	m = run(t, worklist.DraftProposal{Approve: true})
	if m.Draft != nil || !strings.Contains(m.Content, "this conversation has no visible review") || !strings.Contains(m.Content, "Review this pull request") {
		t.Errorf("an approval from a discussion: %+v\n%s", m.Draft, m.Content)
	}
}

// A conversation since its latest review was asked for that is longer than a read
// leaves turns out of the draft's read: a word the drafter never saw (the owner's
// "do not approve yet", even one posted while the review stalled and answered after
// it) may disagree, so an approval is withheld, and the reply says why. The drafter
// reads only the newest turns, contiguous, never the review kept out of order. A
// stale revision is reported first, since hiding turns cannot make it current.
func TestRunConverseWithholdsAnApprovalOverAnUnreadConversation(t *testing.T) {
	build := func(hold bool, reviewHead string) worklist.WorkItem {
		it := draftItem()
		q, r, draft := it.Thread[0], it.Thread[1], it.Thread[len(it.Thread)-1]
		r.Head = reviewHead
		th := []worklist.Message{q}
		if hold {
			th = append(th, worklist.Message{ID: "hold", Role: worklist.RoleUser, Content: "Do not approve yet."})
		}
		th = append(th, r) // the review, answered after the hold
		for i := range 24 {
			id := strconv.Itoa(i)
			th = append(th,
				worklist.Message{ID: "u" + id, Role: worklist.RoleUser, Content: strings.Repeat("chat ", 40000)},
				worklist.Message{ID: "g" + id, ReplyTo: "u" + id, Role: worklist.RoleAgent, Content: "ok"})
		}
		it.Thread = append(th, draft)
		return it
	}
	run := func(t *testing.T, it worklist.WorkItem) (worklist.Message, *fakeDrafter) {
		t.Helper()
		sink := &memSink{items: []worklist.WorkItem{it}}
		fd := &fakeDrafter{prop: worklist.DraftProposal{Approve: true}}
		if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: draftGitHub(t, "open"), ItemID: it.ID, Turn: "d", Converser: &fakeConverser{}, Drafter: fd},
			fakeVendor{token: "t"}, sink); err != nil {
			t.Fatal(err)
		}
		got := sink.items[0].Thread
		return got[len(got)-1], fd
	}
	m, fd := run(t, build(true, draftHead))
	if m.Draft != nil || !strings.Contains(m.Content, "No approval drafted") || !strings.Contains(m.Content, "longer than gofer reads") {
		t.Errorf("an approval over an unread hold: %+v\n%s", m.Draft, m.Content)
	}
	for _, h := range fd.got.History {
		if h.Content == "rename new1" {
			t.Error("the review was kept out of order for the drafter")
		}
	}
	const pushed = "fedcba9876543210fedcba9876543210fedcba98"
	m, _ = run(t, build(false, pushed))
	if m.Draft != nil || !strings.Contains(m.Content, "read commit `"+pushed[:12]) || strings.Contains(m.Content, "longer than gofer reads") {
		t.Errorf("a stale review over an unread conversation: %+v\n%s", m.Draft, m.Content)
	}
}

// Without GitHub there is no diff to anchor to: the run says so and drafts nothing.
func TestRunConverseDraftWithoutGitHub(t *testing.T) {
	sink := &memSink{items: []worklist.WorkItem{draftItem()}}
	fd := &fakeDrafter{}
	err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: "github:o/r#1", Turn: "d", Converser: &fakeConverser{}, Drafter: fd},
		fakeVendor{token: "t"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	th := sink.items[0].Thread
	if m := th[len(th)-1]; m.Draft != nil || m.Content != draftNoGitHubReply || fd.got.Diff != "" {
		t.Errorf("reply = %+v", m)
	}
}

// Hunk parsing, as GitHub anchors: added and unchanged context lines are RIGHT,
// removed lines LEFT, across hunks. A context line's base number is no anchor: the
// model is never shown it.
func TestPatchAnchors(t *testing.T) {
	a, text := patchAnchors("@@ -1,2 +1,2 @@\n a\n-b\n+B\n@@ -20 +20,2 @@ x\n c\n+d\n\\ No newline at end of file")
	for _, want := range []anchor{{"RIGHT", 1}, {"LEFT", 2}, {"RIGHT", 2}, {"RIGHT", 20}, {"RIGHT", 21}} {
		if !a[want] {
			t.Errorf("missing anchor %+v", want)
		}
	}
	for _, not := range []anchor{{"LEFT", 1}, {"LEFT", 20}, {"LEFT", 21}, {"RIGHT", 3}} {
		if a[not] {
			t.Errorf("unexpected anchor %+v", not)
		}
	}
	if len(a) != 5 || strings.Count(text, "[R") != 4 || strings.Count(text, "[L") != 1 {
		t.Errorf("anchors %v do not match what the model is shown:\n%s", a, text)
	}
}

// A file that does not fit the prompt budget is listed without its patch, and no
// comment can anchor to it.
func TestRenderDiffBudget(t *testing.T) {
	files := []github.DiffFile{
		{Path: "small.go", Status: "modified", Patch: "@@ -1 +1 @@\n-a\n+b"},
		{Path: "big.go", Status: "modified", Patch: "@@ -1 +1,500 @@\n" + strings.Repeat("+x\n", 500)},
	}
	text, anchors, omitted := renderDiff(files, 300)
	if omitted != 1 || anchors["big.go"] != nil || anchors["small.go"] == nil {
		t.Fatalf("omitted %d, anchors %v", omitted, anchors)
	}
	if !strings.Contains(text, "big.go (modified) — patch left out") || len(text) > 300 {
		t.Errorf("text (%d bytes):\n%s", len(text), text)
	}
	// Files without patches count against the budget too, and every one is still
	// counted once the notices stop fitting.
	var binaries []github.DiffFile
	for range 10 {
		binaries = append(binaries, github.DiffFile{Path: "assets/image.png", Status: "added"})
	}
	text, _, omitted = renderDiff(binaries, 128)
	if len(text) > 128 || omitted != 10 {
		t.Errorf("binary-heavy diff: %d bytes of a 128-byte budget, %d omitted", len(text), omitted)
	}
}
