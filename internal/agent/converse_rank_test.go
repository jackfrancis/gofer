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
// sees the research rationale and its rank, not the stored ones. The research ran
// after the thread's first question.
func rankedItem(now time.Time) worklist.WorkItem {
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
				Rationale: "the release is blocked on this fix", AppliedAt: now.Add(-time.Hour),
				Turns: []string{"q"},
			},
		},
		Meta: worklist.Metadata{
			Rank: 0.1, Priority: worklist.PriorityLow, Rationale: "stale stored rationale",
			Origin: worklist.OriginAgent,
		},
		Thread: []worklist.Message{{ID: "q", Role: worklist.RoleUser, Content: "why is this PR high on my radar?", At: now.Add(-2 * time.Hour)}},
	}
}

// fakeModel is a chat-completions endpoint that records the last request's system
// and user messages and answers with reply.
func fakeModel(t *testing.T, reply string) (url string, client *http.Client, system, user *string) {
	t.Helper()
	var sys, usr string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, m := range req.Messages {
			switch m.Role {
			case "system":
				sys = m.Content
			case "user":
				usr = m.Content
			}
		}
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": reply}}}})
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, srv.Client(), &sys, &usr
}

// Asking Discuss why an item ranks where it does sends the model the reasons and
// rationale gofer holds for it, with the rank and priority the user sees, inside the
// untrusted-data framing — end to end through the real converser.
func TestConverseWhyRankedSendsReasonsAndRationale(t *testing.T) {
	url, client, system, user := fakeModel(t, "You were asked to review it, and it blocks the release.")
	item := rankedItem(time.Now())
	want := worklist.Rescore(item, time.Now())
	sink := &memSink{items: []worklist.WorkItem{item}}
	conv := llm.NewConverser(llm.Config{Endpoint: url, Token: "t", Client: client})
	if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: item.ID, Converser: conv}, fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	if *user != "why is this PR high on my radar?" {
		t.Fatalf("the question did not reach the model: %q", *user)
	}
	open := strings.Index(*system, "<<<\non the radar because:")
	if open < 0 {
		t.Fatalf("system prompt has no delimited ranking block: %q", *system)
	}
	block := (*system)[open:]
	block = block[:strings.Index(block, ">>>")]
	for _, s := range []string{
		"on the radar because: review_requested, assignee",
		"labels: kind/bug",
		"discussion: 4 comments, 3 participants",
		fmt.Sprintf("rank: %.2f of 1 (priority %s)", want.Rank, want.Priority),
		"discussion adjustment rationale: the release is blocked on this fix",
		"signals behind the axes: relevance 1.00 (on your radar as",
	} {
		if !strings.Contains(block, s) {
			t.Errorf("ranking block missing %q:\n%s", s, block)
		}
	}
	if strings.Contains(*system, "stale stored rationale") || strings.Contains(*system, "priority low") {
		t.Errorf("the model was given the stale stored ranking:\n%s", *system)
	}
	th := sink.items[0].Thread
	if len(th) != 2 || !strings.Contains(th[1].Content, "blocks the release") {
		t.Fatalf("the answer was not stored: %+v", th)
	}
}

// An independent second opinion must not learn the first reviewer's conclusion
// through the research rationale, which summarizes the thread it is blind to. The
// stored thread is untouched.
func TestConverseIndependentReviewGetsNoThreadDerivedRanking(t *testing.T) {
	now := time.Now()
	url, client, system, _ := fakeModel(t, "My own view: it needs tests.")
	item := rankedItem(now)
	item.Signals.Research.Rationale = "the first reviewer approved it"
	item.Signals.Research.Turns = []string{"r1", "a1"}
	item.Thread = []worklist.Message{
		{ID: "r1", Role: worklist.RoleUser, Kind: worklist.KindReviewRequest, Content: "review this", At: now.Add(-3 * time.Hour)},
		{ID: "a1", ReplyTo: "r1", Role: worklist.RoleAgent, Content: "LGTM, approve", At: now.Add(-2 * time.Hour)},
		{ID: "r2", Role: worklist.RoleUser, Kind: worklist.KindReviewRequest, Content: "review this", At: now.Add(-time.Minute)},
	}
	sink := &memSink{items: []worklist.WorkItem{item}}
	conv := llm.NewConverser(llm.Config{Endpoint: url, Token: "t", Client: client})
	if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: item.ID, Turn: "r2", Independent: true, Converser: conv}, fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"the first reviewer approved it", "LGTM"} {
		if strings.Contains(*system, leak) {
			t.Errorf("the independent reviewer saw %q:\n%s", leak, *system)
		}
	}
	if !strings.Contains(*system, "discussion-based adjustment is left out") {
		t.Errorf("the withheld adjustment should be noted:\n%s", *system)
	}
	if th := sink.items[0].Thread; len(th) != 4 || th[1].Content != "LGTM, approve" {
		t.Fatalf("the stored thread must be untouched, got %+v", th)
	}
}

// The converser itself is handed no thread for an independent review.
func TestRunConverseIndependentCopyHasNoThread(t *testing.T) {
	now := time.Now()
	item := rankedItem(now)
	item.Thread = append(item.Thread, worklist.Message{ID: "r2", Role: worklist.RoleUser, Content: "second opinion", At: now})
	sink := &memSink{items: []worklist.WorkItem{item}}
	fc := &fakeConverser{reply: "ok"}
	if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: item.ID, Turn: "r2", Independent: true, Converser: fc}, fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	if len(fc.gotItem.Thread) != 0 {
		t.Fatalf("an independent review's item copy must carry no thread, got %+v", fc.gotItem.Thread)
	}
}

// A turn the user hid is withheld from future model context, and so is the research
// rationale that summarized it, on the next ordinary Discuss turn.
func TestConverseHiddenTurnKeepsItsResearchOut(t *testing.T) {
	now := time.Now()
	url, client, system, _ := fakeModel(t, "ok")
	item := rankedItem(now)
	item.Signals.Research.Rationale = "the assistant concluded it is unsafe"
	item.Signals.Research.Turns = []string{"q1", "a1"}
	item.Thread = []worklist.Message{
		{ID: "q1", Role: worklist.RoleUser, Content: "is it safe?", At: now.Add(-3 * time.Hour)},
		{ID: "a1", ReplyTo: "q1", Role: worklist.RoleAgent, Content: "it is unsafe", Hidden: true, At: now.Add(-2 * time.Hour)},
		{ID: "q2", Role: worklist.RoleUser, Content: "why is it on my radar?", At: now},
	}
	sink := &memSink{items: []worklist.WorkItem{item}}
	conv := llm.NewConverser(llm.Config{Endpoint: url, Token: "t", Client: client})
	if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: item.ID, Converser: conv}, fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(*system, "unsafe") {
		t.Errorf("the hidden turn's conclusion reached the model:\n%s", *system)
	}
	if !strings.Contains(*system, "discussion-based adjustment is left out") {
		t.Errorf("the withheld adjustment should be noted:\n%s", *system)
	}
}

// runWithFakeModel runs one converse turn against a fake model and returns the
// system prompt it was sent.
func runWithFakeModel(t *testing.T, item worklist.WorkItem, turn string) string {
	t.Helper()
	url, client, system, _ := fakeModel(t, "ok")
	sink := &memSink{items: []worklist.WorkItem{item}}
	conv := llm.NewConverser(llm.Config{Endpoint: url, Token: "t", Client: client})
	if err := Run(context.Background(), Params{JobType: JobConverse, GitHubBaseURL: deadGitHub(t), ItemID: item.ID, Turn: turn, Converser: conv}, fakeVendor{token: "t"}, sink); err != nil {
		t.Fatal(err)
	}
	return *system
}

// The user resets the conversation and asks a new question while an old answer is
// in flight: the old reply is dropped, but its research — over the cleared thread —
// is stored after the new question was posted. It must not reach the new
// conversation, whatever the timestamps say.
func TestConverseResetKeepsInFlightResearchOut(t *testing.T) {
	now := time.Now()
	item := rankedItem(now)
	item.Signals.Research.Rationale = "the cleared conversation concluded it is risky"
	item.Signals.Research.AppliedAt = now // after the new question
	item.Signals.Research.Turns = []string{"old-q", "old-a"}
	item.Thread = []worklist.Message{{ID: "new-q", Role: worklist.RoleUser, Content: "fresh start: what is this?", At: now.Add(-time.Minute)}}
	system := runWithFakeModel(t, item, "new-q")
	if strings.Contains(system, "risky") {
		t.Errorf("research over the cleared thread reached the new conversation:\n%s", system)
	}
	if !strings.Contains(system, "discussion-based adjustment is left out") {
		t.Errorf("the withheld adjustment should be noted:\n%s", system)
	}
}

// Question B was answered (and researched) before earlier question A's run started.
// A's model sees history only up to A, so research that read B's answer must not
// reach it either.
func TestConverseResearchScopedToTheAnsweredTurn(t *testing.T) {
	now := time.Now()
	item := rankedItem(now)
	item.Signals.Research.Rationale = "B's answer says it is a duplicate"
	item.Signals.Research.Turns = []string{"A", "B", "b-ans"}
	item.Thread = []worklist.Message{
		{ID: "A", Role: worklist.RoleUser, Content: "question A", At: now.Add(-3 * time.Minute)},
		{ID: "B", Role: worklist.RoleUser, Content: "question B", At: now.Add(-2 * time.Minute)},
		{ID: "b-ans", ReplyTo: "B", Role: worklist.RoleAgent, Content: "it is a duplicate", At: now.Add(-time.Minute)},
	}
	if system := runWithFakeModel(t, item, "A"); strings.Contains(system, "duplicate") {
		t.Errorf("research that read past question A reached A's answer:\n%s", system)
	}
	// Answering B, the same research read only what B may see, and is used.
	item.Thread = item.Thread[:2]
	item.Signals.Research.Turns = []string{"A", "B"}
	if system := runWithFakeModel(t, item, "B"); !strings.Contains(system, "B's answer says it is a duplicate") {
		t.Errorf("research over B's own history should be used:\n%s", system)
	}
}
