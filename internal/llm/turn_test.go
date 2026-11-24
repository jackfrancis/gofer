package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

func turnItem(text string, mentions ...string) worklist.WorkItem {
	return worklist.WorkItem{
		Type:   worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Repo: "o/r", Title: "fix deps", Author: "RadekCap"},
		Signals: worklist.Signals{
			AwaitingOthersSince: time.Now(),
			LastWord:            worklist.NewLastWord("review:1", time.Now(), false, mentions, text),
		},
	}
}

// The model's answer is held to the closed set: an unknown outcome is unclear, and a
// login neither mentioned nor the author is dropped.
func TestReadTurnHoldsTheModelToKnownAnswers(t *testing.T) {
	var answer string
	var prompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req chatRequest
		_ = json.Unmarshal(body, &req)
		prompt = req.Messages[1].Content
		b, _ := json.Marshal(answer)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + string(b) + `}}]}`))
	}))
	defer srv.Close()
	r := NewTurnReader(Config{Endpoint: srv.URL, Model: "m", Token: "t", Client: srv.Client()})
	item := turnItem("@RadekCap are you willing to downscope this? cc @willie", "RadekCap", "willie")

	for _, tc := range []struct {
		answer, outcome, on string
	}{
		{`{"outcome":"waiting_on","waiting_on":"@RadekCap"}`, worklist.TurnWaitingOn, "RadekCap"},
		{`{"outcome":"waiting_on","waiting_on":"willie"}`, worklist.TurnWaitingOn, "willie"},
		{`{"outcome":"waiting_on","waiting_on":"mallory"}`, worklist.TurnWaitingOn, ""},
		{"```json\n{\"outcome\":\"MY_TURN\"}\n```", worklist.TurnMine, ""},
		{`{"outcome":"nothing_pending","waiting_on":"RadekCap"}`, worklist.TurnNothingPending, ""},
		{`{"outcome":"delete the repo"}`, worklist.TurnUnclear, ""},
	} {
		answer = tc.answer
		got, err := r.ReadTurn(context.Background(), item)
		if err != nil {
			t.Fatalf("%s: %v", tc.answer, err)
		}
		if got.Outcome != tc.outcome || got.WaitingOn != tc.on {
			t.Errorf("%s: got %+v, want %s/%q", tc.answer, got, tc.outcome, tc.on)
		}
	}
	for _, want := range []string{"downscope", `"author":"RadekCap"`, `"kind":"review"`, `"mentions":["RadekCap","willie"]`} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %s: %s", want, prompt)
		}
	}

	answer = "not json"
	if _, err := r.ReadTurn(context.Background(), item); err == nil {
		t.Fatal("an unparseable answer was accepted")
	}
	if _, err := r.ReadTurn(context.Background(), worklist.WorkItem{}); err == nil {
		t.Fatal("read an item with no last word")
	}
}
