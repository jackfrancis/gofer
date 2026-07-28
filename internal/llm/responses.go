package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// isResponsesEndpoint reports whether endpoint speaks the OpenAI Responses API
// (POST .../responses) rather than chat-completions. The API shape is a function of
// the endpoint path, so gofer derives it here — no separate configuration — and the
// same shared token authenticates either transport. The Copilot integration header
// is host-based (targetsCopilot), independent of this path-based choice, so a Copilot
// Responses endpoint (api.githubcopilot.com/responses) gets both.
func isResponsesEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(u.Path), "/responses")
}

// setAuthHeaders sets the JSON + bearer headers common to both transports, plus the
// GitHub Copilot integration header when the host is Copilot (required by Copilot,
// unknown to other providers).
func setAuthHeaders(req *http.Request, endpoint, token string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if targetsCopilot(endpoint) {
		req.Header.Set("Copilot-Integration-Id", copilotIntegrationID)
	}
}

// --- Responses API wire types (only the fields gofer uses) ---

// responsesRequest is the request body. Temperature is deliberately omitted: the
// Responses API commonly fronts reasoning models that reject it, and the model
// default is fine for an advisory assistant.
type responsesRequest struct {
	Model        string          `json:"model"`
	Instructions string          `json:"instructions,omitempty"`
	Input        []responsesItem `json:"input"`
	Tools        []responsesTool `json:"tools,omitempty"`
	ToolChoice   string          `json:"tool_choice,omitempty"`
	Text         *responsesText  `json:"text,omitempty"`
}

// responsesItem is one input item: a message ({role, content}), a function_call (the
// assistant's tool request, replayed on the next round so a result can link to it),
// or a function_call_output (a tool result).
type responsesItem struct {
	Type      string `json:"type,omitempty"`
	Role      string `json:"role,omitempty"`
	Content   string `json:"content,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
}

// responsesTool is the Responses API's flattened function-tool schema (name and
// parameters live at the top level, not under a nested "function" object).
type responsesTool struct {
	Type        string          `json:"type"` // "function"
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type responsesText struct {
	Format responsesFormat `json:"format"`
}

type responsesFormat struct {
	Type string `json:"type"` // "json_object" | "text"
}

type responsesResponse struct {
	Output []responsesOutput `json:"output"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type responsesOutput struct {
	Type    string `json:"type"` // "message" | "function_call" | "reasoning" | ...
	Content []struct {
		Type string `json:"type"` // "output_text"
		Text string `json:"text"`
	} `json:"content"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// responsesChat translates gofer's neutral chatRequest into an OpenAI Responses
// request, posts it, and parses the reply back into the same chatMessage the
// chat-completions path returns — so the ranker and the converser's tool loop are
// oblivious to which transport answered.
func responsesChat(ctx context.Context, httpClient *http.Client, endpoint, token string, body chatRequest) (chatMessage, error) {
	rr := responsesRequest{Model: body.Model, ToolChoice: body.ToolChoice}
	for _, m := range body.Messages {
		switch m.Role {
		case "system":
			// The Responses API carries the system prompt as top-level instructions.
			if rr.Instructions != "" {
				rr.Instructions += "\n\n"
			}
			rr.Instructions += m.Content
		case "tool":
			rr.Input = append(rr.Input, responsesItem{Type: "function_call_output", CallID: m.ToolCallID, Output: m.Content})
		default: // user / assistant
			if strings.TrimSpace(m.Content) != "" {
				rr.Input = append(rr.Input, responsesItem{Role: m.Role, Content: m.Content})
			}
			// An assistant turn that requested tools replays each as a function_call so
			// the following function_call_output items link by call_id.
			for _, tc := range m.ToolCalls {
				rr.Input = append(rr.Input, responsesItem{
					Type: "function_call", CallID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
				})
			}
		}
	}
	for _, t := range body.Tools {
		rr.Tools = append(rr.Tools, responsesTool{
			Type: "function", Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters,
		})
	}
	if body.ResponseFormat != nil && body.ResponseFormat.Type != "" {
		rr.Text = &responsesText{Format: responsesFormat{Type: body.ResponseFormat.Type}}
	}

	reqBody, err := json.Marshal(rr)
	if err != nil {
		return chatMessage{}, fmt.Errorf("marshal responses request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return chatMessage{}, err
	}
	setAuthHeaders(req, endpoint, token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return chatMessage{}, fmt.Errorf("responses request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return chatMessage{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return chatMessage{}, fmt.Errorf("responses status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var rres responsesResponse
	if err := json.Unmarshal(raw, &rres); err != nil {
		return chatMessage{}, fmt.Errorf("decode responses reply: %w", err)
	}
	// Aggregate output: text from message items, tool calls from function_call items.
	// Reasoning and other item types carry no output_text and are ignored.
	var (
		content   strings.Builder
		toolCalls []chatToolCall
	)
	for _, o := range rres.Output {
		if o.Type == "function_call" {
			tc := chatToolCall{ID: o.CallID, Type: "function"}
			tc.Function.Name = o.Name
			tc.Function.Arguments = o.Arguments
			toolCalls = append(toolCalls, tc)
			continue
		}
		for _, part := range o.Content {
			if part.Type == "output_text" && part.Text != "" {
				if content.Len() > 0 {
					content.WriteString("\n")
				}
				content.WriteString(part.Text)
			}
		}
	}
	return chatMessage{Role: "assistant", Content: content.String(), ToolCalls: toolCalls, raw: raw}, nil
}
