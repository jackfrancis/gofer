package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/llm"
	"github.com/jackfrancis/gofer/internal/worklist"
)

// rankedItem is a PR on the radar for a review request, whose stored Meta predates
// its latest research pass: the landing page re-scores it at read time, so the user
// sees the research rationale and its rank, not the stored ones.
func rankedItem() worklist.WorkItem {
	return worklist.WorkItem{
		ID:     "github:o/r#7",
		Type:   worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Repo: "o/r", Number: 7, Title: "Fix the scheduler", State: "open"},
		Signals: worklist.Signals{
			Reasons:      []worklist.Reason{worklist.ReasonReviewRequested, worklist.ReasonAssignee},
			Labels:       []string{"kind/bug"},
			Comments:     4,
			Participants: 3,
			Research: &worklist.ResearchAdjustment{
				Relevance: 1, Impact: 1.5, Engagement: 1, Urgency: 1.5,
				Rationale: "the release is blocked on this fix",
			},
		},
		Meta: worklist.Metadata{
			Rank: 0.1, Priority: worklist.PriorityLow, Rationale: "stale stored rationale",
			Origin: worklist.OriginAgent,
		},
		Thread: []worklist.Message{{ID: "q", Role: worklist.RoleUser, Content: "why is this PR high on my radar?"}},
	}
}

// The assistant is handed the ranking the user sees: the item re-scored as the
// landing page does, not its stored Meta. What the run writes back is untouched.
func TestRunConverseGivesTheRankingTheUserSees(t *testing.T) {
	item := rankedItem()
	want := worklist.Rescore(item, time.Now())
	sink := &memSink{items: []worklist.WorkItem{item}}
	fc := &fakeConverser{reply: "It is blocking the release."}
	if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: item.ID, Converser: fc}, fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	got := fc.gotItem.Meta
	if got.Rationale != "the release is blocked on this fix" || got.Priority != want.Priority || got.Rank != want.Rank {
		t.Fatalf("converser got Meta %+v, want the re-scored %+v", got, want)
	}
	if sink.items[0].Meta.Rationale != "stale stored rationale" {
		t.Fatalf("the stored Meta must not be rewritten by a Discuss turn: %+v", sink.items[0].Meta)
	}
}

// A ranking the user set by hand is explained as set, never re-derived.
func TestRunConversePassesHumanOverrideVerbatim(t *testing.T) {
	item := rankedItem()
	item.Meta = worklist.Metadata{Rank: 0.9, Priority: worklist.PriorityHigh, Rationale: "I care about this", Origin: worklist.OriginUser}
	sink := &memSink{items: []worklist.WorkItem{item}}
	fc := &fakeConverser{reply: "ok"}
	if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: item.ID, Converser: fc}, fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	if got := fc.gotItem.Meta; got.Rank != 0.9 || got.Priority != worklist.PriorityHigh || got.Rationale != "I care about this" {
		t.Fatalf("a human override must reach the assistant verbatim, got %+v", got)
	}
}

// Asking Discuss why an item ranks where it does sends the model the reasons and
// rationale gofer holds for it, with the rank and priority the user sees, inside the
// untrusted-data framing — end to end through the real converser.
func TestConverseWhyRankedSendsReasonsAndRationale(t *testing.T) {
	var system, user string
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, m := range req.Messages {
			switch m.Role {
			case "system":
				system = m.Content
			case "user":
				user = m.Content
			}
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"You were asked to review it, and it blocks the release."}}]}`))
	}))
	defer model.Close()

	item := rankedItem()
	want := worklist.Rescore(item, time.Now())
	sink := &memSink{items: []worklist.WorkItem{item}}
	conv := llm.NewConverser(llm.Config{Endpoint: model.URL, Token: "t", Client: model.Client()})
	if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: item.ID, Converser: conv}, fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	if user != "why is this PR high on my radar?" {
		t.Fatalf("the question did not reach the model: %q", user)
	}
	open := strings.Index(system, "<<<\non the radar because:")
	if open < 0 {
		t.Fatalf("system prompt has no delimited ranking block: %q", system)
	}
	block := system[open:]
	block = block[:strings.Index(block, ">>>")]
	for _, s := range []string{
		"on the radar because: review_requested, assignee",
		"labels: kind/bug",
		"discussion: 4 comments, 3 participants",
		fmt.Sprintf("rank: %.2f of 1 (priority %s)", want.Rank, want.Priority),
		"rationale: the release is blocked on this fix",
		"relevance 1.00 (on your radar as",
	} {
		if !strings.Contains(block, s) {
			t.Errorf("ranking block missing %q:\n%s", s, block)
		}
	}
	if strings.Contains(system, "stale stored rationale") || strings.Contains(system, "priority low") {
		t.Errorf("the model was given the stale stored ranking:\n%s", system)
	}
	th := sink.items[0].Thread
	if len(th) != 2 || !strings.Contains(th[1].Content, "blocks the release") {
		t.Fatalf("the answer was not stored: %+v", th)
	}
}
