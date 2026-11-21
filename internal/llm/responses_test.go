package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackfrancis/gofer/internal/worklist"
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

// openAIResponsesStub mimics the OpenAI Responses API's json_object rule: a request
// whose text.format is json_object is refused with 400 unless an input message (the
// instructions do not count) contains "json". It also refuses two user messages in a
// row, as a self-hosted server does when the model's chat template requires
// alternating roles. Otherwise it answers with reply as the output text. Each decoded
// request is sent on got, when non-nil.
func openAIResponsesStub(t *testing.T, reply string, got chan<- responsesRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req responsesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if got != nil {
			got <- req
		}
		refuse := func(msg string) {
			w.WriteHeader(http.StatusBadRequest)
			out, _ := json.Marshal(map[string]any{"error": map[string]string{"message": msg}})
			_, _ = w.Write(out)
		}
		mentioned := false
		for i, it := range req.Input {
			if it.Role != "" && strings.Contains(strings.ToLower(it.Content), "json") {
				mentioned = true
			}
			if i > 0 && it.Role == "user" && req.Input[i-1].Role == "user" {
				refuse("Conversation roles must alternate user/assistant/user/assistant/...")
				return
			}
		}
		if req.Text != nil && req.Text.Format.Type == "json_object" && !mentioned {
			refuse("Response input messages must contain the word 'json' in some form to use 'text.format' of type 'json_object'.")
			return
		}
		out, _ := json.Marshal(map[string]any{"output": []any{map[string]any{
			"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": reply}},
		}}})
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// captured returns the request the stub recorded, failing the test at once if none
// reached it. Call it only after the call returned without error: the handler records
// the request before it answers, so a successful call never has to wait for it.
func captured(t *testing.T, got <-chan responsesRequest) responsesRequest {
	t.Helper()
	select {
	case req := <-got:
		return req
	default:
		t.Fatal("no request reached the stub")
		return responsesRequest{}
	}
}

// wantJSONMode fails the test unless req asked for json_object output.
func wantJSONMode(t *testing.T, req responsesRequest) {
	t.Helper()
	if req.Text == nil || req.Text.Format.Type != "json_object" {
		t.Fatalf("text.format = %+v, want json_object", req.Text)
	}
}

// A json_object request whose only mention of JSON is in the system prompt (moved to
// instructions) gets the reminder in its closing user message, not in a second user
// turn; the instructions and the caller's own messages are unchanged.
func TestResponsesJSONModeInputMentionsJSON(t *testing.T) {
	got := make(chan responsesRequest, 1)
	srv := openAIResponsesStub(t, `{}`, got)

	msgs := []chatMessage{
		{Role: "system", Content: "Respond with ONLY a JSON object."},
		{Role: "user", Content: "Return the four multipliers."},
	}
	_, err := responsesChat(context.Background(), srv.Client(), srv.URL+"/v1/responses", "tok", chatRequest{
		Model:          "m",
		Messages:       msgs,
		ResponseFormat: &responseFormat{Type: "json_object"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := captured(t, got)
	wantJSONMode(t, req)
	if req.Instructions != "Respond with ONLY a JSON object." {
		t.Errorf("instructions = %q", req.Instructions)
	}
	want := "Return the four multipliers.\n\n" + jsonModeReminder
	if len(req.Input) != 1 || req.Input[0].Role != "user" || req.Input[0].Content != want {
		t.Errorf("input = %+v, want one user message %q", req.Input, want)
	}
	if msgs[1].Content != "Return the four multipliers." {
		t.Errorf("caller's message changed: %q", msgs[1].Content)
	}
}

// When the input does not end with a user message (an assistant turn, a tool output,
// or nothing at all), the reminder is a new closing user message.
func TestResponsesJSONReminderAfterNonUserTurn(t *testing.T) {
	call := chatToolCall{ID: "c0", Type: "function"}
	call.Function.Name = "lookup"
	cases := []struct {
		name string
		msgs []chatMessage
		want int // input items, the last being the reminder
	}{
		{"assistant last", []chatMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}}, 3},
		{"tool output last", []chatMessage{{Role: "user", Content: "hi"}, {Role: "assistant", ToolCalls: []chatToolCall{call}}, {Role: "tool", ToolCallID: "c0", Content: "r"}}, 4},
		{"no input", nil, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan responsesRequest, 1)
			srv := openAIResponsesStub(t, `{}`, got)
			_, err := responsesChat(context.Background(), srv.Client(), srv.URL+"/v1/responses", "tok", chatRequest{
				Model:          "m",
				Messages:       append([]chatMessage{{Role: "system", Content: "JSON only"}}, tc.msgs...),
				ResponseFormat: &responseFormat{Type: "json_object"},
			})
			if err != nil {
				t.Fatal(err)
			}
			req := captured(t, got)
			if len(req.Input) != tc.want {
				t.Fatalf("input = %+v, want %d item(s)", req.Input, tc.want)
			}
			if last := req.Input[len(req.Input)-1]; last.Role != "user" || last.Content != jsonModeReminder {
				t.Errorf("last input = %+v, want the reminder as a user message", last)
			}
		})
	}
}

// The reminder is added only when needed: not when an input message already says
// JSON, and never outside json_object mode.
func TestResponsesJSONReminderOnlyWhenNeeded(t *testing.T) {
	cases := []struct {
		name   string
		user   string
		format *responseFormat
	}{
		{"input already says json", "Reply in Json, please.", &responseFormat{Type: "json_object"}},
		{"text format", "hello", &responseFormat{Type: "text"}},
		{"no format", "hello", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan responsesRequest, 1)
			srv := openAIResponsesStub(t, `{}`, got)
			_, err := responsesChat(context.Background(), srv.Client(), srv.URL+"/v1/responses", "tok", chatRequest{
				Model:          "m",
				Messages:       []chatMessage{{Role: "system", Content: "JSON only"}, {Role: "user", Content: tc.user}},
				ResponseFormat: tc.format,
			})
			if err != nil {
				t.Fatal(err)
			}
			if req := captured(t, got); len(req.Input) != 1 || req.Input[0].Content != tc.user {
				t.Errorf("input = %+v, want the user message unchanged", req.Input)
			}
		})
	}
}

// Research over a Responses endpoint asks for JSON mode, passes the API's json_object
// rule and role alternation, and the reply parses into the adjustment.
func TestResearchOverResponses(t *testing.T) {
	got := make(chan responsesRequest, 1)
	srv := openAIResponsesStub(t, `{"relevance":1.5,"urgency":0.5,"rationale":"x"}`, got)
	r := NewResearchRanker(Config{Endpoint: srv.URL + "/v1/responses", Model: "m", Token: "t", Client: srv.Client()})
	adj, err := r.Research(context.Background(), worklist.WorkItem{
		Thread: []worklist.Message{{Role: worklist.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantJSONMode(t, captured(t, got))
	if adj.Relevance != 1.5 || adj.Urgency != 0.5 || adj.Impact != 1 || adj.Engagement != 1 || adj.Rationale != "x" {
		t.Fatalf("adjustment = %+v", adj)
	}
}

// Ranking over a Responses endpoint has the same exposure (its JSON instruction is
// in the system prompt too) and the same fix.
func TestRankerOverResponses(t *testing.T) {
	got := make(chan responsesRequest, 1)
	srv := openAIResponsesStub(t, `{"relevance":0.9,"impact":0.4,"engagement":0.2,"urgency":0.7,"confidence":0.8,"rationale":"y"}`, got)
	r := NewRanker(Config{Endpoint: srv.URL + "/v1/responses", Model: "m", Token: "t", Client: srv.Client()})
	p, err := r.Propose(context.Background(), worklist.WorkItem{})
	if err != nil {
		t.Fatal(err)
	}
	wantJSONMode(t, captured(t, got))
	if p.Relevance != 0.9 || p.Urgency != 0.7 || p.Confidence != 0.8 || p.Rationale != "y" {
		t.Fatalf("proposal = %+v", p)
	}
}
