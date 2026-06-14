// Package model is a minimal Anthropic Messages API client — the one place
// NilCore talks to a model provider. The native loop drives it. Stdlib only
// (invariant I6): net/http + encoding/json, no SDK. The API key is held solely
// to set a per-request header; it is never logged and never placed in a prompt
// (invariant I3). Phase 1 (task P1-T10) generalizes this into a Provider
// interface with OpenAI/OpenRouter adapters; this client becomes the Anthropic
// one without changing the native loop.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	endpoint   = "https://api.anthropic.com/v1/messages"
	apiVersion = "2023-06-01"
)

// Client calls the Messages API for a single configured model.
type Client struct {
	key   string
	model string
	http  *http.Client
}

// New returns a client for the given API key and model id (e.g.
// "claude-sonnet-4-6"). The key originates from the environment (invariant I3);
// this package only forwards it as a request header.
func New(key, model string) *Client {
	return &Client{
		key:   key,
		model: model,
		http:  &http.Client{Timeout: 5 * time.Minute},
	}
}

// Message is one turn in the conversation.
type Message struct {
	Role    string  `json:"role"` // "user" | "assistant"
	Content []Block `json:"content"`
}

// Block is a single content block. One struct covers the three shapes the loop
// uses — text, tool_use (from the model), and tool_result (back to the model) —
// with omitempty so each value marshals to exactly the wire form the API wants.
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

type request struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []Message `json:"messages"`
	Tools     []Tool    `json:"tools,omitempty"`
}

// Complete sends one Messages API request and returns the model's response. It
// honors ctx cancellation. A non-2xx response is surfaced as an error carrying
// the status and a truncated body — never the API key.
func (c *Client) Complete(ctx context.Context, system string, msgs []Message, tools []Tool, maxTokens int) (Response, error) {
	body, err := json.Marshal(request{
		Model:     c.model,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  msgs,
		Tools:     tools,
	})
	if err != nil {
		return Response{}, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", apiVersion)
	req.Header.Set("x-api-key", c.key)

	resp, err := c.http.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("messages request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Response{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return Response{}, fmt.Errorf("messages api: %s: %s", resp.Status, tail(string(raw), 1000))
	}

	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return Response{}, fmt.Errorf("decode response: %w", err)
	}
	return out, nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
