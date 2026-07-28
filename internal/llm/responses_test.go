package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsResponsesEndpoint(t *testing.T) {
	cases := map[string]bool{
		"https://api.githubcopilot.com/chat/completions":    false,
		"https://api.openai.com/v1/responses":               true,
		"https://api.githubcopilot.com/responses":           true,
		"https://gw.example/v1/chat/completions":            false,
		"https://gw.example/openai/deployments/x/responses": true,
	}
	for endpoint, want := range cases {
		if got := isResponsesEndpoint(endpoint); got != want {
			t.Errorf("isResponsesEndpoint(%q) = %v, want %v", endpoint, got, want)
		}
	}
}

// responsesChat translates gofer's neutral chatRequest into the Responses wire shape
// (system -> instructions; user -> input message; an assistant tool-call turn ->
// function_call; a tool result -> function_call_output; tools flattened) and parses
// the Responses reply (text message + function_call) back into one chatMessage.
func TestResponsesChatTranslatesBothWays(t *testing.T) {
	var got responsesRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer auth: %q", r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":[
			{"type":"reasoning","summary":[]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi there"}]},
			{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{\"q\":\"x\"}"}
		]}`)
	}))
	defer srv.Close()

	priorCall := chatToolCall{ID: "c0", Type: "function"}
	priorCall.Function.Name = "lookup"
	priorCall.Function.Arguments = `{"q":"y"}`

	body := chatRequest{
		Model: "gpt-5.6",
		Messages: []chatMessage{
			{Role: "system", Content: "be nice"},
			{Role: "user", Content: "hello"},
			{Role: "assistant", ToolCalls: []chatToolCall{priorCall}},
			{Role: "tool", ToolCallID: "c0", Content: "a result"},
		},
		Tools:      []chatTool{{Type: "function", Function: chatToolFunction{Name: "lookup", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}}},
		ToolChoice: "auto",
	}

	msg, err := responsesChat(context.Background(), srv.Client(), srv.URL+"/v1/responses", "tok", body)
	if err != nil {
		t.Fatal(err)
	}

	// Reply parsed: both the text and the requested tool call.
	if msg.Content != "hi there" {
		t.Errorf("content = %q, want %q", msg.Content, "hi there")
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].ID != "c1" || msg.ToolCalls[0].Function.Name != "lookup" {
		t.Fatalf("tool calls = %+v", msg.ToolCalls)
	}

	// Request translated: system -> instructions, tools flattened, tool_choice kept.
	if got.Instructions != "be nice" {
		t.Errorf("instructions = %q, want 'be nice'", got.Instructions)
	}
	if got.Model != "gpt-5.6" || got.ToolChoice != "auto" {
		t.Errorf("model/tool_choice wrong: %+v", got)
	}
	if len(got.Tools) != 1 || got.Tools[0].Type != "function" || got.Tools[0].Name != "lookup" {
		t.Fatalf("tools = %+v", got.Tools)
	}
	// input: the user message, then the replayed function_call, then its output.
	if len(got.Input) != 3 {
		t.Fatalf("input items = %+v", got.Input)
	}
	if got.Input[0].Role != "user" || got.Input[0].Content != "hello" {
		t.Errorf("input[0] = %+v, want user/hello", got.Input[0])
	}
	if got.Input[1].Type != "function_call" || got.Input[1].CallID != "c0" || got.Input[1].Name != "lookup" {
		t.Errorf("input[1] = %+v, want function_call c0/lookup", got.Input[1])
	}
	if got.Input[2].Type != "function_call_output" || got.Input[2].CallID != "c0" || got.Input[2].Output != "a result" {
		t.Errorf("input[2] = %+v, want function_call_output c0/'a result'", got.Input[2])
	}
}

// A non-200 from the Responses endpoint surfaces as an error carrying the body.
func TestResponsesChatError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"model not accessible via /responses"}}`)
	}))
	defer srv.Close()

	_, err := responsesChat(context.Background(), srv.Client(), srv.URL+"/v1/responses", "tok", chatRequest{Model: "x", Messages: []chatMessage{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("expected an error on a 400 response")
	}
}
