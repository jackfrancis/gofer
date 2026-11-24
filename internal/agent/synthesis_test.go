package agent

import (
	"context"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// synthesisItem is a PR whose two reviews the consensus turn s1 names.
func synthesisItem(now time.Time) worklist.WorkItem {
	return worklist.WorkItem{
		ID:     "github:o/r#1",
		Type:   worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1},
		Thread: []worklist.Message{
			{ID: "q1", Role: worklist.RoleUser, Kind: worklist.KindReviewRequest, Content: "Can you review this PR?", At: now},
			{ID: "r1", ReplyTo: "q1", Role: worklist.RoleAgent, Content: "first review", Model: "a", At: now},
			{ID: "p1", Role: worklist.RoleUser, Kind: worklist.KindReviewRequest, Content: worklist.IndependentReviewPrompt, At: now},
			{ID: "r2", ReplyTo: "p1", Role: worklist.RoleAgent, Content: "blind review", Model: "b", At: now},
			{ID: "s1", Role: worklist.RoleUser, Kind: worklist.KindSynthesisRequest, Content: worklist.SynthesisPrompt, Reviews: []string{"q1", "p1"}, At: now},
		},
	}
}

// The consensus run compares the reviews its turn names: with both in its read it
// answers with a verdict; with one hidden since the turn was chained it never asks
// the model, and declines with a notice instead of a verdict.
func TestConverseSynthesisNeedsItsReviews(t *testing.T) {
	now := time.Now().UTC()
	t.Run("both reviews visible", func(t *testing.T) {
		sink := &memSink{items: []worklist.WorkItem{synthesisItem(now)}}
		fc := &fakeConverser{reply: "AGREEMENT\nThey both approve."}
		if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: sink.items[0].ID, Turn: "s1", Converser: fc}, fakeVendor{token: "t"}, sink); err != nil {
			t.Fatal(err)
		}
		if len(fc.gotHistory) != 4 {
			t.Fatalf("the model must see both reviews, got %+v", fc.gotHistory)
		}
		if got := sink.items[0].ReviewVerdict(); got != worklist.VerdictAgree {
			t.Fatalf("verdict = %q, want agree", got)
		}
	})
	t.Run("a review hidden", func(t *testing.T) {
		item := synthesisItem(now)
		item.Thread[1].Hidden = true
		sink := &memSink{items: []worklist.WorkItem{item}}
		fc := &fakeConverser{reply: "AGREEMENT\nOnly one review, and it agrees with itself."}
		if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: item.ID, Turn: "s1", Converser: fc}, fakeVendor{token: "t"}, sink); err != nil {
			t.Fatal(err)
		}
		if fc.gotUser != "" {
			t.Fatal("the model must not be asked to compare a missing review")
		}
		got := sink.items[0]
		last := got.Thread[len(got.Thread)-1]
		if last.Notice != worklist.NoticePanelInputs || last.ReplyTo != "s1" || last.Verdict != "" {
			t.Fatalf("want a panel-inputs notice, got %+v", last)
		}
		if got.ReviewVerdict() != "" || got.Answered("s1") {
			t.Fatal("a declined synthesis carries no verdict and stays unanswered")
		}
	})
}
