// Package llm is the AxisRanker backed by a chat model. It is imported only by
// agent runtimes — never by gofer core — because gofer is a credential broker,
// not a model broker: the ranking model is called from the runtime, behind the
// worklist.AxisRanker seam.
//
// It speaks two OpenAI-compatible transports, chosen per endpoint by its path:
// chat-completions (POST /chat/completions) and the Responses API (POST /responses).
// Either works against any endpoint that exposes it (GitHub Copilot, OpenAI, Azure
// OpenAI, a self-hosted gateway); the provider and transport are config, not code.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackfrancis/gofer/internal/httpretry"
	"github.com/jackfrancis/gofer/internal/worklist"
)

const (
	// copilotHost is GitHub Copilot's chat API host. Requests to it carry the
	// Copilot integration header; the client is otherwise provider-neutral.
	copilotHost = "api.githubcopilot.com"
	// copilotIntegrationID is required by GitHub Copilot's chat endpoint. It is a
	// public, non-secret constant, sent only when targeting Copilot.
	copilotIntegrationID = "copilot-developer-cli"
	maxBody              = 1 << 20 // 1 MiB; ranking responses are tiny
)

// defaultModelTimeout bounds a single model HTTP call. It is a SAFETY NET against a
// hung connection, NOT the work budget: the caller's context — in the runtime, the
// run deadline — is what actually bounds a turn, and it is far larger (minutes).
//
// It is deliberately generous. A reasoning model re-reading a whole tool-call
// transcript (up to maxToolIterations rounds, each carrying up to maxToolResultBytes
// of tool output) routinely spends minutes on one round trip. A tight client timeout
// here does not degrade gracefully: it aborts the turn while the run still has most
// of its deadline left, and — because the client's transport is wrapped by httpretry,
// so net/http cannot take its "known transport" path — it surfaces as a bare
// "context deadline exceeded" that reads exactly like a run-deadline failure.
const defaultModelTimeout = 5 * time.Minute

// Config configures the chat-model ranker.
type Config struct {
	Endpoint string       // chat-completions URL (required; no default)
	Model    string       // model identifier (required; no default)
	Token    string       // bearer token
	Client   *http.Client // shared HTTP client; nil gets a default
	// Logger, when set, records the converser's tool-call loop (one line per
	// model turn and tool call); nil disables that logging. Only the Converser
	// consults it.
	Logger *slog.Logger
}

// Ranker implements worklist.AxisRanker by asking a chat model to score one
// item's four axes. gofer blends the proposal into Rank itself, so a misbehaving
// model cannot hijack ordering.
type Ranker struct {
	endpoint string
	model    string
	token    string
	client   *http.Client
}

var _ worklist.AxisRanker = (*Ranker)(nil)

// modelClient returns the HTTP client every model call shares: the caller's when it
// supplied one, otherwise a default bounded by defaultModelTimeout. Either way it is
// routed through httpretry, so a 429 (rate limit) is retried with Retry-After-aware,
// jittered backoff instead of failing the run — and the jitter spreads a concurrent
// burst (e.g. "Review all PRs") across the retry window.
func modelClient(cfg Config) *http.Client {
	c := cfg.Client
	if c == nil {
		c = &http.Client{Timeout: defaultModelTimeout}
	}
	return httpretry.Wrap(c)
}

// NewRanker builds a Ranker from an explicit endpoint + model (no defaults; the
// caller validates them). A nil Client gets a default.
func NewRanker(cfg Config) *Ranker {
	return &Ranker{endpoint: cfg.Endpoint, model: cfg.Model, token: cfg.Token, client: modelClient(cfg)}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls is set on an assistant message that requests tool invocations;
	// ToolCallID links a role=="tool" result back to the call it answers.
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	// FinishReason is response-only diagnostic context ("stop", "tool_calls",
	// ...); it is never serialized into a request.
	FinishReason string `json:"-"`
	// raw is the unparsed response body, retained for diagnostics when a tool-call
	// turn does not parse as expected (unexported -> never serialized).
	raw []byte
}

// chatToolCall is one tool invocation the model requested.
type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON-encoded arguments
	} `json:"function"`
}

// chatTool advertises a callable function to the model.
type chatTool struct {
	Type     string           `json:"type"` // "function"
	Function chatToolFunction `json:"function"`
}

type chatToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	Temperature    float64         `json:"temperature"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	Tools          []chatTool      `json:"tools,omitempty"`
	ToolChoice     string          `json:"tool_choice,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content   string         `json:"content"`
			ToolCalls []chatToolCall `json:"tool_calls"`
			// FunctionCall is the legacy single-function shape some OpenAI-compatible
			// gateways still return instead of tool_calls; parsed as a fallback.
			FunctionCall *struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function_call"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// axesDoc is the strict JSON the model is asked to return.
type axesDoc struct {
	Relevance  float64 `json:"relevance"`
	Impact     float64 `json:"impact"`
	Engagement float64 `json:"engagement"`
	Urgency    float64 `json:"urgency"`
	Confidence float64 `json:"confidence"`
	Rationale  string  `json:"rationale"`
}

// Propose asks the model to score the item and maps the response onto an
// AxisProposal. The values are clamped to [0,1] defensively.
func (r *Ranker) Propose(ctx context.Context, item worklist.WorkItem) (worklist.AxisProposal, error) {
	content, err := chatComplete(ctx, r.client, r.endpoint, r.token, chatRequest{
		Model: r.model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt(item)},
		},
		Temperature:    0,
		ResponseFormat: &responseFormat{Type: "json_object"},
	})
	if err != nil {
		return worklist.AxisProposal{}, err
	}

	var doc axesDoc
	if err := json.Unmarshal([]byte(stripFences(content)), &doc); err != nil {
		return worklist.AxisProposal{}, fmt.Errorf("parse axes JSON: %w", err)
	}
	return worklist.AxisProposal{
		Relevance:  clamp01(doc.Relevance),
		Impact:     clamp01(doc.Impact),
		Engagement: clamp01(doc.Engagement),
		Urgency:    clamp01(doc.Urgency),
		Confidence: clamp01(doc.Confidence),
		Rationale:  strings.TrimSpace(doc.Rationale),
	}, nil
}

// targetsCopilot reports whether endpoint is GitHub Copilot's chat API, so the
// Copilot-specific integration header is sent only then.
func targetsCopilot(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && strings.EqualFold(u.Host, copilotHost)
}

// chatComplete posts an OpenAI-compatible chat-completions request and returns
// the assistant message content. It requires non-empty content, so it is for the
// no-tools case (the ranker and research).
func chatComplete(ctx context.Context, httpClient *http.Client, endpoint, token string, body chatRequest) (string, error) {
	msg, err := chat(ctx, httpClient, endpoint, token, body)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(msg.Content) == "" {
		return "", fmt.Errorf("chat returned no content")
	}
	return msg.Content, nil
}

// chat posts a request to the model and returns the assistant's message, including
// any tool calls. Unlike chatComplete it does not require content, since a
// tool-calling turn returns tool_calls with empty content. It dispatches on the
// endpoint's API shape: an OpenAI Responses endpoint (path .../responses) is spoken
// over the Responses transport, every other endpoint over OpenAI chat-completions.
// Both send the Copilot integration header when the host is GitHub Copilot.
func chat(ctx context.Context, httpClient *http.Client, endpoint, token string, body chatRequest) (chatMessage, error) {
	if isResponsesEndpoint(endpoint) {
		return responsesChat(ctx, httpClient, endpoint, token, body)
	}
	reqBody, err := json.Marshal(body)
	if err != nil {
		return chatMessage{}, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return chatMessage{}, err
	}
	setAuthHeaders(req, endpoint, token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return chatMessage{}, fmt.Errorf("chat request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return chatMessage{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return chatMessage{}, fmt.Errorf("chat status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return chatMessage{}, fmt.Errorf("decode response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return chatMessage{}, fmt.Errorf("chat returned no choices")
	}
	// Aggregate across choices. The OpenAI standard returns a single choice, but
	// some gateways (Copilot serving Claude) split one assistant turn into
	// several choices — a text block in one, each tool call in its own — so the
	// tool calls can live beyond choices[0]. Collect content and tool calls from
	// all choices so a multi-choice tool turn parses like a single-choice one.
	var (
		content   strings.Builder
		toolCalls []chatToolCall
	)
	for i := range cr.Choices {
		cm := cr.Choices[i].Message
		if s := strings.TrimSpace(cm.Content); s != "" {
			if content.Len() > 0 {
				content.WriteString("\n")
			}
			content.WriteString(cm.Content)
		}
		toolCalls = append(toolCalls, cm.ToolCalls...)
		// Fallback for the legacy single-function shape (function_call) some
		// gateways return instead of tool_calls.
		if cm.FunctionCall != nil && cm.FunctionCall.Name != "" {
			tc := chatToolCall{ID: fmt.Sprintf("call_%d", i), Type: "function"}
			tc.Function.Name = cm.FunctionCall.Name
			tc.Function.Arguments = cm.FunctionCall.Arguments
			toolCalls = append(toolCalls, tc)
		}
	}
	return chatMessage{
		Role:         "assistant",
		Content:      content.String(),
		ToolCalls:    toolCalls,
		FinishReason: cr.Choices[0].FinishReason,
		raw:          raw,
	}, nil
}

// stripFences removes a ```json ... ``` markdown fence if the model wrapped its
// JSON in one despite the response_format request.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

func clamp01(f float64) float64 {
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 1
	default:
		return f
	}
}
