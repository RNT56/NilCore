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

// Block is a single content block. One struct covers the shapes the loop uses —
// text, tool_use (from the model), tool_result (back to the model), and image (a
// screenshot or picture handed to a vision-capable model, e.g. by a behavioral
// browser check). The image shape is purely additive: Source is nil for every
// other type, so it is omitted and a text/tool_use/tool_result block marshals
// byte-identically to before. Adding it does not touch the backend.CodingBackend
// contract or Provider.Complete (invariant I1) — images ride inside the existing
// []Block.
type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	// Source carries an image block's payload (set only when Type == "image"). Its
	// JSON field names match Anthropic's image-source shape, so the near-identity
	// Anthropic marshal needs no special case; the OpenAI adapter translates it to
	// an image_url content part.
	Source *ImageSource `json:"source,omitempty"`
}

// ImageSource is the payload of an image block: base64-encoded image bytes and
// their media type. The encoding is always "base64" — the loop never embeds a
// remote URL; an image is produced/fetched inside the sandbox and handed in as
// data (invariant I7: content is data, never an instruction).
type ImageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // e.g. "image/png", "image/jpeg"
	Data      string `json:"data"`       // base64-encoded image bytes
}

// ImageBlock builds an image content block from base64-encoded bytes and a media
// type (e.g. "image/png"). The single constructor keeps the "base64" tag in one
// place.
func ImageBlock(mediaType, base64Data string) Block {
	return Block{Type: "image", Source: &ImageSource{Type: "base64", MediaType: mediaType, Data: base64Data}}
}

// Tool is a tool definition advertised to the model.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	// Builtin, when non-nil, marks this as a provider built-in tool (Anthropic's
	// `computer` beta tool, Path A — CU-T12). The Anthropic provider serializes the
	// typed shape + sets the beta header; other providers ignore it. nil in every
	// existing path ⇒ a normal tool, serialized byte-identically (see builtin.go).
	Builtin *BuiltinTool `json:"-"`
}

// Response is the model's reply: the content blocks, why it stopped, and usage.
//
// ServedModel is the id of the model that ACTUALLY served the call, when the
// provider reports it (e.g. OpenRouter echoes the top-level "model" field, which
// can differ from the requested id when a models[] fallback chain routes to a
// later entry). It is purely additive and omitempty: a provider that does not
// report a served id leaves it empty, so the JSON is byte-identical to before and
// every existing consumer is unchanged. A meter MAY price the served id rather than
// the requested one when this is set.
type Response struct {
	Content     []Block `json:"content"`
	StopReason  string  `json:"stop_reason"`
	Usage       Usage   `json:"usage"`
	ServedModel string  `json:"served_model,omitempty"`
}

// Usage reports token counts (and, where a vendor reports it, cost) for the call.
// The first two fields are the original, frozen shape: an InputTokens-only/
// OutputTokens-only Usage marshals byte-identically to before. The trailing three
// are purely additive and `omitempty`, so a provider that does not populate them
// emits the exact same JSON it always has — existing providers are unchanged.
//   - ReasoningTokens: tokens spent on hidden reasoning/thinking (e.g. extended
//     thinking), reported separately from the visible OutputTokens by vendors that
//     break it out.
//   - CachedTokens: input tokens served from a prompt cache (a billing discount on
//     the InputTokens total), surfaced by vendors with prompt caching.
//   - CostUSD: the call's cost in US dollars when a vendor (or a meter) computes it;
//     0 means "not reported", which is why it is omitempty.
type Usage struct {
	InputTokens     int     `json:"input_tokens"`
	OutputTokens    int     `json:"output_tokens"`
	ReasoningTokens int     `json:"reasoning_tokens,omitempty"`
	CachedTokens    int     `json:"cached_tokens,omitempty"`
	CostUSD         float64 `json:"cost_usd,omitempty"`
}

// Provider is one model vendor behind a uniform call. Model selection is
// role → provider:model; an executor, advisor, or planner can be any provider.
type Provider interface {
	// Complete sends one request and returns the model's response. It honors ctx.
	Complete(ctx context.Context, system string, msgs []Message, tools []Tool, maxTokens int) (Response, error)
	// Model is the provider:model string this provider was configured with.
	Model() string
}
