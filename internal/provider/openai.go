package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"nilcore/internal/model"
)

// OpenAI is the Chat Completions adapter. It translates the canonical model.*
// format to/from OpenAI's wire shape (tool_use/tool_result blocks ↔
// tool_calls/tool messages). OpenRouter reuses this adapter with a different base
// URL and key (it is OpenAI-compatible).
type OpenAI struct {
	key     string
	model   string
	baseURL string
	http    *http.Client
}

// NewOpenAI returns an OpenAI Chat Completions provider.
func NewOpenAI(key, modelID string) *OpenAI {
	return &OpenAI{key: key, model: modelID, baseURL: "https://api.openai.com/v1", http: &http.Client{Timeout: 5 * time.Minute}}
}

// DefaultOpenRouterModel is OpenRouter's Fusion alias: a multi-model panel that
// runs the prompt across several frontier models and synthesizes the best answer
// (launched as a public experiment 2026-03-31, since integrated into the API). It
// is the default when the openrouter provider is
// selected without an explicit model — e.g. NILCORE_MODEL="openrouter". Note: it
// bills the cumulative cost of every model in the panel, so it is opt-in via the
// provider, not the global default model.
const DefaultOpenRouterModel = "openrouter/fusion"

// NewOpenRouter returns an OpenRouter provider (OpenAI-compatible). The model id
// carries the `provider/model` namespace, e.g. "meta-llama/llama-3.1-70b"; an
// empty id falls back to DefaultOpenRouterModel.
func NewOpenRouter(key, modelID string) *OpenAI {
	if modelID == "" {
		modelID = DefaultOpenRouterModel
	}
	return &OpenAI{key: key, model: modelID, baseURL: "https://openrouter.ai/api/v1", http: &http.Client{Timeout: 5 * time.Minute}}
}

// Model returns the configured model id.
func (o *OpenAI) Model() string { return o.model }

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content,omitempty"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

type oaiRequest struct {
	Model     string       `json:"model"`
	MaxTokens int          `json:"max_tokens,omitempty"`
	Messages  []oaiMessage `json:"messages"`
	Tools     []oaiTool    `json:"tools,omitempty"`
}

type oaiResponse struct {
	Choices []struct {
		Message struct {
			Content   string        `json:"content"`
			ToolCalls []oaiToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Complete translates, calls /chat/completions, and translates the reply back.
func (o *OpenAI) Complete(ctx context.Context, system string, msgs []model.Message, tools []model.Tool, maxTokens int) (model.Response, error) {
	reqBody := oaiRequest{
		Model:     o.model,
		MaxTokens: maxTokens,
		Messages:  toOpenAIMessages(system, msgs),
		Tools:     toOpenAITools(tools),
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return model.Response{}, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return model.Response{}, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+o.key)

	resp, err := o.http.Do(req)
	if err != nil {
		return model.Response{}, fmt.Errorf("chat completions request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return model.Response{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return model.Response{}, fmt.Errorf("openai api: %s: %s", resp.Status, tail(string(raw), 1000))
	}

	var out oaiResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return model.Response{}, fmt.Errorf("decode response: %w", err)
	}
	return fromOpenAI(out), nil
}

// toOpenAIMessages flattens the canonical conversation into OpenAI messages:
// assistant tool_use → tool_calls; user tool_result → role:"tool" messages.
func toOpenAIMessages(system string, msgs []model.Message) []oaiMessage {
	var out []oaiMessage
	if system != "" {
		out = append(out, oaiMessage{Role: "system", Content: system})
	}
	for _, m := range msgs {
		if m.Role == "assistant" {
			am := oaiMessage{Role: "assistant"}
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					am.Content += b.Text
				case "tool_use":
					tc := oaiToolCall{ID: b.ID, Type: "function"}
					tc.Function.Name = b.Name
					tc.Function.Arguments = rawOrEmptyObj(b.Input)
					am.ToolCalls = append(am.ToolCalls, tc)
				}
			}
			out = append(out, am)
			continue
		}
		// user turn: tool results become role:"tool" messages; text stays user.
		var text string
		for _, b := range m.Content {
			switch b.Type {
			case "tool_result":
				out = append(out, oaiMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: b.Content})
			case "text":
				text += b.Text
			}
		}
		if text != "" {
			out = append(out, oaiMessage{Role: "user", Content: text})
		}
	}
	return out
}

func toOpenAITools(tools []model.Tool) []oaiTool {
	var out []oaiTool
	for _, t := range tools {
		var ot oaiTool
		ot.Type = "function"
		ot.Function.Name = t.Name
		ot.Function.Description = t.Description
		ot.Function.Parameters = t.InputSchema
		out = append(out, ot)
	}
	return out
}

func fromOpenAI(r oaiResponse) model.Response {
	var out model.Response
	if len(r.Choices) == 0 {
		return out
	}
	ch := r.Choices[0]
	if ch.Message.Content != "" {
		out.Content = append(out.Content, model.Block{Type: "text", Text: ch.Message.Content})
	}
	for _, tc := range ch.Message.ToolCalls {
		out.Content = append(out.Content, model.Block{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(orEmptyObj(tc.Function.Arguments)),
		})
	}
	switch ch.FinishReason {
	case "tool_calls":
		out.StopReason = "tool_use"
	case "stop":
		out.StopReason = "end_turn"
	default:
		out.StopReason = ch.FinishReason
	}
	out.Usage = model.Usage{InputTokens: r.Usage.PromptTokens, OutputTokens: r.Usage.CompletionTokens}
	return out
}

func rawOrEmptyObj(r json.RawMessage) string { return orEmptyObj(string(r)) }

func orEmptyObj(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}
