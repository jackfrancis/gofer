package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackfrancis/gofer/internal/worklist"
)

func TestConverserReply(t *testing.T) {
	var gotSystem, gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, m := range req.Messages {
			switch m.Role {
			case "system":
				gotSystem = m.Content
			case "user":
				gotUser = m.Content
			}
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"You could ask for a re-review."}}]}`))
	}))
	defer srv.Close()

	c := NewConverser(Config{Endpoint: srv.URL, Token: "t", Client: srv.Client()})
	item := worklist.WorkItem{GitHub: worklist.GitHubRef{Repo: "o/r", Title: "Fix bug"}, Type: worklist.TypePullRequest}
	reply, err := c.Reply(context.Background(), item, "alice",
		"PR body: changes the parser", // untrusted source context
		[]worklist.Message{{Role: worklist.RoleUser, Content: "hi"}, {Role: worklist.RoleAgent, Content: "hello"}},
		"what should I do?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "re-review") {
		t.Fatalf("unexpected reply: %q", reply)
	}
	if !strings.Contains(gotSystem, "o/r") || !strings.Contains(gotSystem, "@alice") {
		t.Fatalf("system prompt missing item/viewer context: %q", gotSystem)
	}
	if !strings.Contains(gotSystem, "never instructions") {
		t.Fatalf("system prompt should frame source context as untrusted: %q", gotSystem)
	}
	if gotUser != "what should I do?" {
		t.Fatalf("user turn not passed through: %q", gotUser)
	}
}

// fakeToolBox is a worklist.ToolBox that records invocations and returns a canned
// result (or error), so the tool loop can be exercised without a provider.
type fakeToolBox struct {
	invoked []string
	result  string
	err     error
}

func (f *fakeToolBox) Definitions() []worklist.ToolDef {
	return []worklist.ToolDef{{
		Name:        "github_read_file",
		Description: "read a file",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
	}}
}

func (f *fakeToolBox) Invoke(_ context.Context, name string, _ json.RawMessage) (string, error) {
	f.invoked = append(f.invoked, name)
	if f.err != nil {
		return "", f.err
	}
	return f.result, nil
}

// TestConverserReplyToolLoop drives the bounded tool-call loop: the model asks
// for a tool on the first turn, the toolbox answers, and the model produces the
// final reply on the second turn. It asserts the tools were offered, the tool was
// invoked, and its result was fed back before the model answered.
func TestConverserReplyToolLoop(t *testing.T) {
	var (
		sawTools       bool
		sawToolMsg     bool
		lastToolResult string
		turns          int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tools    []json.RawMessage `json:"tools"`
			Messages []struct {
				Role       string `json:"role"`
				Content    string `json:"content"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		turns++
		if len(req.Tools) > 0 {
			sawTools = true
		}
		hasTool := false
		for _, m := range req.Messages {
			if m.Role == "tool" {
				hasTool = true
				sawToolMsg = true
				lastToolResult = m.Content
			}
		}
		if !hasTool {
			// First turn: request a tool call with empty content.
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"github_read_file","arguments":"{\"path\":\"go.mod\"}"}}]},"finish_reason":"tool_calls"}]}`))
			return
		}
		// Second turn: the tool result is present; answer in text.
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Your go.mod already bumped it."}}]}`))
	}))
	defer srv.Close()

	box := &fakeToolBox{result: "module foo\n\ngo 1.26.0"}
	c := NewConverser(Config{Endpoint: srv.URL, Token: "t", Client: srv.Client()})
	item := worklist.WorkItem{GitHub: worklist.GitHubRef{Repo: "o/r", Title: "Bump dep"}, Type: worklist.TypePullRequest}
	reply, err := c.Reply(context.Background(), item, "alice", "", nil, "did we already bump it?", box)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "already bumped") {
		t.Fatalf("unexpected reply: %q", reply)
	}
	if strings.Contains(reply, "incomplete investigation") {
		t.Errorf("a self-stopped answer must not carry the partial-research note: %q", reply)
	}
	if len(box.invoked) != 1 || box.invoked[0] != "github_read_file" {
		t.Fatalf("tool not invoked as expected: %v", box.invoked)
	}
	if !sawTools {
		t.Fatal("tools were not offered to the model")
	}
	if !sawToolMsg {
		t.Fatal("tool result was not fed back to the model")
	}
	if lastToolResult != box.result {
		t.Fatalf("tool result fed back = %q, want %q", lastToolResult, box.result)
	}
	if turns != 2 {
		t.Fatalf("expected 2 model turns, got %d", turns)
	}
}

// TestConverserReplyForcesFinalAnswer: a model that keeps requesting tools every
// round is forced to answer on the final round via tool_choice="none" (the schema
// stays present so the history's tool_calls remain valid), so Reply returns text
// rather than the empty message an abrupt tool-drop can produce.
func TestConverserReplyForcesFinalAnswer(t *testing.T) {
	var finalToolChoice string
	var turns int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ToolChoice string            `json:"tool_choice"`
			Tools      []json.RawMessage `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		turns++
		if req.ToolChoice == "none" {
			finalToolChoice = "none"
			if len(req.Tools) == 0 {
				t.Error("final round dropped the tool schema; the history's tool_calls would be invalid")
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Here is the summary."},"finish_reason":"stop"}]}`))
			return
		}
		// Every non-final round: keep asking for a tool so the loop runs to the cap.
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"github_read_file","arguments":"{\"path\":\"go.mod\"}"}}]},"finish_reason":"tool_calls"}]}`))
	}))
	defer srv.Close()

	box := &fakeToolBox{result: "module foo"}
	c := NewConverser(Config{Endpoint: srv.URL, Token: "t", Client: srv.Client()})
	item := worklist.WorkItem{GitHub: worklist.GitHubRef{Repo: "o/r", Title: "Bump dep"}, Type: worklist.TypePullRequest}
	reply, err := c.Reply(context.Background(), item, "alice", "", nil, "summarize", box)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "summary") {
		t.Fatalf("forced final answer not returned: %q", reply)
	}
	if !strings.Contains(reply, "incomplete investigation") {
		t.Errorf("a forced answer should carry the partial-research note: %q", reply)
	}
	if finalToolChoice != "none" {
		t.Fatal("final round did not force tool_choice=none")
	}
	if turns != maxToolIterations+1 {
		t.Fatalf("expected %d model turns, got %d", maxToolIterations+1, turns)
	}
}

// TestConverserReplyBudgetExhaustedMessage: if the model spends its whole tool
// budget and still returns nothing on the forced final round, Reply surfaces a
// clear note rather than an empty string.
func TestConverserReplyBudgetExhaustedMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ToolChoice string `json:"tool_choice"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ToolChoice == "none" {
			// Forced final round: return nothing — the failure the note guards against.
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`))
			return
		}
		// Every non-final round: keep asking for a tool so the loop runs to the cap.
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"github_read_file","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`))
	}))
	defer srv.Close()

	box := &fakeToolBox{result: "x"}
	c := NewConverser(Config{Endpoint: srv.URL, Token: "t", Client: srv.Client()})
	item := worklist.WorkItem{GitHub: worklist.GitHubRef{Repo: "o/r", Title: "T"}, Type: worklist.TypePullRequest}
	reply, err := c.Reply(context.Background(), item, "alice", "", nil, "summarize", box)
	if err != nil {
		t.Fatal(err)
	}
	if reply != budgetExhaustedReply {
		t.Fatalf("reply = %q, want the budget-exhausted note", reply)
	}
}

// TestConverserReplyToolError verifies a tool failure is relayed to the model as
// a "tool error:" message rather than aborting the reply.
func TestConverserReplyToolError(t *testing.T) {
	var relayed string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		hasTool := false
		for _, m := range req.Messages {
			if m.Role == "tool" {
				hasTool = true
				relayed = m.Content
			}
		}
		if !hasTool {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"github_read_file","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"I could not read it, so here is what I can tell."}}]}`))
	}))
	defer srv.Close()

	box := &fakeToolBox{err: errors.New("path is required")}
	c := NewConverser(Config{Endpoint: srv.URL, Token: "t", Client: srv.Client()})
	item := worklist.WorkItem{GitHub: worklist.GitHubRef{Repo: "o/r"}, Type: worklist.TypePullRequest}
	reply, err := c.Reply(context.Background(), item, "", "", nil, "read it", box)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "could not read") {
		t.Fatalf("unexpected reply: %q", reply)
	}
	if !strings.HasPrefix(relayed, "tool error: ") || !strings.Contains(relayed, "path is required") {
		t.Fatalf("tool error not relayed to model: %q", relayed)
	}
}
