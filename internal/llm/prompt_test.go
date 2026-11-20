package llm

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// promptSummary decodes the JSON the ranker sends the model for item.
func promptSummary(t *testing.T, item worklist.WorkItem) map[string]any {
	t.Helper()
	p := userPrompt(item)
	i := strings.Index(p, "{")
	if i < 0 {
		t.Fatalf("no JSON in prompt %q", p)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(p[i:]), &m); err != nil {
		t.Fatalf("decode prompt: %v", err)
	}
	return m
}

// After the write-back policy classifies the requester, a bot's review request
// reaches the model as an automated assignment with no awaiting-me pressure, and a
// human's request keeps its urgency signal.
func TestUserPromptBotReviewPolicy(t *testing.T) {
	now := time.Now().UTC()
	items := []worklist.WorkItem{
		{ID: "bot", Signals: worklist.Signals{
			Reasons: []worklist.Reason{worklist.ReasonReviewRequested}, ReviewRequestedBy: "k8s-ci-robot",
			AwaitingMeSince: now.Add(-72 * time.Hour)}},
		{ID: "human", Signals: worklist.Signals{
			Reasons: []worklist.Reason{worklist.ReasonReviewRequested}, ReviewRequestedBy: "alice",
			ReviewRequestedByBot: true, // a run's claim; the policy replaces it
			AwaitingMeSince:      now.Add(-72 * time.Hour)}},
	}
	worklist.NewRunPolicy([]string{"k8s-ci-robot"}).Apply(items)

	bot := promptSummary(t, items[0])
	if bot["review_auto_assigned"] != true {
		t.Errorf("bot review: review_auto_assigned = %v, want true", bot["review_auto_assigned"])
	}
	if _, ok := bot["awaiting_me_days"]; ok {
		t.Error("bot review: awaiting_me_days sent; an automated assignment is not a human waiting")
	}

	human := promptSummary(t, items[1])
	if _, ok := human["review_auto_assigned"]; ok {
		t.Error("human review: review_auto_assigned sent")
	}
	if human["awaiting_me_days"] != float64(3) {
		t.Errorf("human review: awaiting_me_days = %v, want 3", human["awaiting_me_days"])
	}
}
