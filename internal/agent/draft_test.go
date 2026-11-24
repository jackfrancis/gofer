package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackfrancis/gofer/internal/github"
	"github.com/jackfrancis/gofer/internal/github/githubtest"
	"github.com/jackfrancis/gofer/internal/worklist"
)

const draftHead = "0123456789abcdef0123456789abcdef01234567"

// draftPatch adds lines 11-12 and removes old line 11 in a.go.
const draftPatch = "@@ -10,3 +10,4 @@ func f() {\n ctx\n-old\n+new1\n+new2\n tail"

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
				"state": state, "head": map[string]any{"sha": draftHead},
				"base": map[string]any{"repo": map[string]any{"full_name": "o/r"}},
			})
		case "/repos/o/r/pulls/1/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"filename": "a.go", "status": "modified", "patch": draftPatch},
				{"filename": "logo.png", "status": "added"},
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
			{ID: "r", ReplyTo: "q", Role: worklist.RoleAgent, Content: "rename new1", Model: "m1"},
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
	if d.Commit != draftHead || len(d.Comments) != 2 {
		t.Fatalf("draft = %+v", d)
	}
	if d.Comments[0].Side != worklist.SideRight || d.Comments[1].Side != worklist.SideLeft {
		t.Errorf("comments = %+v", d.Comments)
	}
	for _, want := range []string{"Add a test.", "`a.go` line 99: Off the diff.", "`logo.png` line 1: Binary file."} {
		if !strings.Contains(d.Body, want) {
			t.Errorf("summary lacks %q: %q", want, d.Body)
		}
	}
	for _, want := range []string{"Drafted 2 inline review comments", "Rename `new1`.", "Left out — no consensus yet", "m1 and m2 disagree", "2 comments pointed at a line the diff does not show", "1 file had no patch"} {
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

// Hunk parsing: added lines are RIGHT, removed LEFT, context both, across hunks.
func TestPatchAnchors(t *testing.T) {
	a, _ := patchAnchors("@@ -1,2 +1,2 @@\n a\n-b\n+B\n@@ -20 +20,2 @@ x\n c\n+d\n\\ No newline at end of file")
	for _, want := range []anchor{{"RIGHT", 1}, {"LEFT", 1}, {"LEFT", 2}, {"RIGHT", 2}, {"RIGHT", 20}, {"LEFT", 20}, {"RIGHT", 21}} {
		if !a[want] {
			t.Errorf("missing anchor %+v", want)
		}
	}
	if a[anchor{"LEFT", 21}] || a[anchor{"RIGHT", 3}] {
		t.Errorf("unexpected anchors: %+v", a)
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
}
