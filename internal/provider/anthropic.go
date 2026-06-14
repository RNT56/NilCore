package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"nilcore/internal/model"
)

const anthropicVersion = "2023-06-01"

// Anthropic is the Messages API adapter. The canonical model.* format already
// matches Anthropic's wire shape (tool_use/tool_result blocks), so translation
// here is near-identity. The API key is held only to set a per-request header
// (invariant I3).
type Anthropic struct {
	key     string
	model   string
	baseURL string
	http    *http.Client
}

// NewAnthropic returns an Anthropic provider for the given key and model id.
func NewAnthropic(key, modelID string) *Anthropic {
	return &Anthropic{
		key:     key,
		model:   modelID,
		baseURL: "https://api.anthropic.com",
		http:    &http.Client{Timeout: 5 * time.Minute},
	}
}

// Model returns the configured model id.
func (a *Anthropic) Model() string { return a.model }

type anthropicRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    string          `json:"system,omitempty"`
	Messages  []model.Message `json:"messages"`
	Tools     []model.Tool    `json:"tools,omitempty"`
}

// Complete calls the Messages API and returns the canonical response.
func (a *Anthropic) Complete(ctx context.Context, system string, msgs []model.Message, tools []model.Tool, maxTokens int) (model.Response, error) {
	body, err := json.Marshal(anthropicRequest{
		Model:     a.model,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  msgs,
		Tools:     tools,
	})
	if err != nil {
		return model.Response{}, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return model.Response{}, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("x-api-key", a.key)

	resp, err := a.http.Do(req)
	if err != nil {
		return model.Response{}, fmt.Errorf("messages request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return model.Response{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return model.Response{}, fmt.Errorf("anthropic api: %s: %s", resp.Status, tail(string(raw), 1000))
	}

	var out model.Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return model.Response{}, fmt.Errorf("decode response: %w", err)
	}
	return out, nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
