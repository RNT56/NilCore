// Package model is the canonical, vendor-neutral message + tool format the native
// loop speaks, plus the Provider seam every model vendor implements. Concrete
// adapters (Anthropic, OpenAI, OpenRouter) live in internal/provider and
// translate this format to/from each vendor's wire shape, so the loop, the
// tools, and the verifier never depend on a vendor. Stdlib only (invariant I6).
package model

import (
	"context"
	"encoding/json"
)

// Message is one turn in the conversation.
type Message struct {
	Role    string  `json:"role"` // "user" | "assistant"
	Content []Block `json:"content"`
}

// Block is a single content block. One struct covers the three shapes the loop
// uses — text, tool_use (from the model), and tool_result (back to the model).
type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// Tool is a tool definition advertised to the model.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Response is the model's reply: the content blocks, why it stopped, and usage.
type Response struct {
	Content    []Block `json:"content"`
	StopReason string  `json:"stop_reason"`
	Usage      Usage   `json:"usage"`
}

// Usage reports token counts for the call.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Provider is one model vendor behind a uniform call. Model selection is
// role → provider:model; an executor, advisor, or planner can be any provider.
type Provider interface {
	// Complete sends one request and returns the model's response. It honors ctx.
	Complete(ctx context.Context, system string, msgs []Message, tools []Tool, maxTokens int) (Response, error)
	// Model is the provider:model string this provider was configured with.
	Model() string
}
