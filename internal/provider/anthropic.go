package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// anthropicCacheControl marks an Anthropic prompt-cache breakpoint (5-minute
// ephemeral TTL — the default tier; reads bill at ~0.1x, writes at 1.25x input).
type anthropicCacheControl struct {
	Type string `json:"type"` // always "ephemeral"
}

// ephemeralCache is the single breakpoint value every marker uses.
func ephemeralCache() *anthropicCacheControl { return &anthropicCacheControl{Type: "ephemeral"} }

// anthropicSystemBlock is one system-prompt content block. The system prompt is
// sent in block-array form rather than as a plain string because only a content
// block can carry a cache_control breakpoint.
type anthropicSystemBlock struct {
	Type         string                 `json:"type"` // "text"
	Text         string                 `json:"text"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

// anthropicBlock wraps a canonical content block with the optional cache_control
// member. The embedded model.Block keeps its exact field set and tags, and
// CacheControl is omitempty — so every block except the single moving breakpoint
// marshals byte-identically to the bare model.Block.
type anthropicBlock struct {
	model.Block
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

// anthropicMessage mirrors model.Message with cache-capable blocks.
type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicRequest struct {
	Model     string                 `json:"model"`
	MaxTokens int                    `json:"max_tokens"`
	System    []anthropicSystemBlock `json:"system,omitempty"`
	Messages  []anthropicMessage     `json:"messages"`
	Tools     []json.RawMessage      `json:"tools,omitempty"`
	Stream    bool                   `json:"stream,omitempty"`
}

// buildAnthropicRequest assembles the wire body with prompt-cache breakpoints.
//
// Prompt caching is a prefix match over the rendered request (render order:
// tools → system → messages), so without breakpoints every step of a long native
// drive re-bills the entire growing transcript at the full input rate. We place
// EXACTLY THREE of Anthropic's maximum FOUR cache_control markers:
//
//  1. the LAST tool definition — caches the (stable) tool set;
//  2. the LAST system block — caches tools + system together;
//  3. the last content block of the FINAL message — the MOVING breakpoint: each
//     step's request re-reads the prefix the previous step cached and pays the
//     cache-read rate on everything before the newest turn.
//
// Each rule sets at most one marker, which keeps the 3 ≤ 4 bound structural
// rather than counted. A section that cannot safely carry a marker (zero tools,
// empty system, empty or uncacheable final block) simply skips it — dropping a
// breakpoint is always a valid request; a malformed one is not.
func buildAnthropicRequest(modelID string, maxTokens int, system string, msgs []model.Message, tools []model.Tool, stream bool) (anthropicRequest, error) {
	req := anthropicRequest{
		Model:     modelID,
		MaxTokens: maxTokens,
		Messages:  toAnthropicMessages(msgs),
		Stream:    stream,
	}
	if system != "" {
		// One block ⇒ it is the last block, so it carries the marker.
		req.System = []anthropicSystemBlock{{Type: "text", Text: system, CacheControl: ephemeralCache()}}
	}
	wireTools, err := marshalAnthropicTools(tools)
	if err != nil {
		return anthropicRequest{}, err
	}
	req.Tools = wireTools
	markMovingBreakpoint(req.Messages)
	return req, nil
}

// toAnthropicMessages converts canonical messages to the cache-capable wire shape.
// nil content stays nil so an image-free/empty message marshals byte-identically
// to the pre-caching request.
func toAnthropicMessages(msgs []model.Message) []anthropicMessage {
	if msgs == nil {
		return nil
	}
	out := make([]anthropicMessage, len(msgs))
	for i, m := range msgs {
		var blocks []anthropicBlock
		if m.Content != nil {
			blocks = make([]anthropicBlock, len(m.Content))
			for j, b := range m.Content {
				blocks[j] = anthropicBlock{Block: b}
			}
		}
		out[i] = anthropicMessage{Role: m.Role, Content: blocks}
	}
	return out
}

// markMovingBreakpoint sets the cache_control marker on the last content block of
// the FINAL message only — earlier messages never carry one, so the marker moves
// forward one turn per step. Canonical messages are always block arrays (there is
// no string-content form in model.Message), so no conversion is needed; an empty
// final message or a block type Anthropic does not accept a marker on skips the
// breakpoint instead of risking a 400.
func markMovingBreakpoint(msgs []anthropicMessage) {
	if len(msgs) == 0 {
		return
	}
	content := msgs[len(msgs)-1].Content
	if len(content) == 0 {
		return
	}
	last := &content[len(content)-1]
	if !cacheableBlockType(last.Type) {
		return
	}
	last.CacheControl = ephemeralCache()
}

// cacheableBlockType reports whether Anthropic accepts cache_control on a message
// content block of this type (text, image, tool_use, tool_result, document).
func cacheableBlockType(t string) bool {
	switch t {
	case "text", "image", "tool_use", "tool_result", "document":
		return true
	}
	return false
}

// marshalAnthropicTools serializes each tool via its own MarshalJSON (so builtin
// tools keep their typed shape + beta semantics) and splices the cache_control
// breakpoint into the LAST definition. Splicing into the rendered bytes — rather
// than re-marshaling through a map — keeps every tool's key order byte-identical
// to the pre-caching request, which is exactly what a prefix-matched cache needs.
func marshalAnthropicTools(tools []model.Tool) ([]json.RawMessage, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]json.RawMessage, 0, len(tools))
	for _, t := range tools {
		b, err := json.Marshal(t)
		if err != nil {
			return nil, fmt.Errorf("marshal tool %q: %w", t.Name, err)
		}
		out = append(out, b)
	}
	out[len(out)-1] = spliceCacheControl(out[len(out)-1])
	return out, nil
}

// cacheControlJSON is the rendered member spliceCacheControl appends. It must
// match what ephemeralCache marshals to.
const cacheControlJSON = `"cache_control":{"type":"ephemeral"}`

// spliceCacheControl appends the cache_control member as the final key of a
// rendered JSON object. If the bytes are not a non-empty object (a shape no
// current tool marshal produces), the input is returned unchanged — skipping the
// breakpoint is always safe; corrupting the request is not.
func spliceCacheControl(obj json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(obj)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return obj
	}
	if len(bytes.TrimSpace(trimmed[1:len(trimmed)-1])) == 0 {
		return obj // empty object: a leading comma would be invalid JSON
	}
	spliced := make(json.RawMessage, 0, len(trimmed)+len(cacheControlJSON)+1)
	spliced = append(spliced, trimmed[:len(trimmed)-1]...)
	spliced = append(spliced, ',')
	spliced = append(spliced, cacheControlJSON...)
	spliced = append(spliced, '}')
	return spliced
}

// newRequest marshals the canonical inputs into the Messages API request body and
// builds the authenticated POST. stream toggles SSE delivery; the body is
// otherwise identical between Complete and Stream — so both paths carry the same
// prompt-cache breakpoints. Headers carry the API key per request and never touch
// disk (invariant I3).
func (a *Anthropic) newRequest(ctx context.Context, system string, msgs []model.Message, tools []model.Tool, maxTokens int, stream bool) (*http.Request, error) {
	wire, err := buildAnthropicRequest(a.model, maxTokens, system, msgs, tools, stream)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("x-api-key", a.key)
	// Path A (CU-T12): a built-in tool (Anthropic's `computer` beta) requires its beta
	// header. Set it when present; absent in every default path ⇒ byte-identical.
	for _, t := range tools {
		if h := t.BetaHeader(); h != "" {
			req.Header.Set("anthropic-beta", h)
			break
		}
	}
	return req, nil
}

// Complete calls the Messages API and returns the canonical response.
func (a *Anthropic) Complete(ctx context.Context, system string, msgs []model.Message, tools []model.Tool, maxTokens int) (model.Response, error) {
	req, err := a.newRequest(ctx, system, msgs, tools, maxTokens, false)
	if err != nil {
		return model.Response{}, err
	}

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
		// Typed error so the resilience wrapper fast-fails a terminal 4xx and honors a
		// 429/5xx Retry-After, instead of failing over on a bad key (I3: key-free).
		return model.Response{}, newAPIError(resp.StatusCode, resp.Header, raw)
	}

	var ar anthropicResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return model.Response{}, fmt.Errorf("decode response: %w", err)
	}
	return ar.toModel(), nil
}

// anthropicResponse is the non-streaming Messages response, decoded TOLERANTLY.
// Decoding straight into model.Response used to crash the whole turn whenever native
// web search ran: an Anthropic web_search_tool_result block carries an ARRAY under
// "content", but model.Block.Content is a string, so json.Unmarshal returned
// "cannot unmarshal array ... into ... string". Here the per-block struct simply
// does not declare a "content" field, so a server-tool block's array content is an
// ignored unknown field. We then keep only the blocks the loop consumes — text and
// tool_use — exactly as the streaming assembler's finish() does, dropping
// server_tool_use / web_search_tool_result (the model's text answer already folds in
// the search results; the loop has no handler for server-side tool blocks).
type anthropicResponse struct {
	Content []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      anthropicUsage `json:"usage"`
}

func (ar anthropicResponse) toModel() model.Response {
	out := model.Response{
		StopReason: ar.StopReason,
		Usage:      ar.Usage.toModelUsage(),
	}
	for _, b := range ar.Content {
		switch b.Type {
		case "text":
			out.Content = append(out.Content, model.Block{Type: "text", Text: b.Text})
		case "tool_use":
			out.Content = append(out.Content, model.Block{
				Type:  "tool_use",
				ID:    b.ID,
				Name:  b.Name,
				Input: json.RawMessage(orEmptyObj(string(b.Input))),
			})
		}
	}
	return out
}

// streamEvent is one Messages-API server-sent event frame. Only the fields the
// assembler reads are decoded; unknown fields are ignored. This is the data side
// of invariant I7 — the wire frames are parsed as data, never executed.
type streamEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`

	// message_start / message_delta carry usage and the final stop_reason.
	Message struct {
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
	Delta struct {
		Type        string `json:"type"`         // text_delta | input_json_delta
		Text        string `json:"text"`         // text_delta payload
		PartialJSON string `json:"partial_json"` // input_json_delta payload
		StopReason  string `json:"stop_reason"`  // message_delta payload
	} `json:"delta"`
	Usage anthropicUsage `json:"usage"` // message_delta output-token tally

	// content_block_start announces the block being opened at Index.
	ContentBlock struct {
		Type string `json:"type"` // text | tool_use
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
}

// anthropicUsage decodes the Messages-API usage object. Anthropic reports
// DISJOINT tallies: input_tokens is only the uncached remainder, with cache reads
// and cache writes broken out separately (total prompt = input_tokens +
// cache_read_input_tokens + cache_creation_input_tokens).
type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// toModelUsage folds Anthropic's disjoint tallies into the canonical model.Usage
// convention (shared with the OpenAI adapter, and what the meter's pricer
// assumes): InputTokens is the TOTAL prompt and CachedTokens is the
// discount-billed subset (pricer: fresh = InputTokens − CachedTokens). Passing
// the raw input_tokens through unfolded would under-bill as soon as cache hits
// occur — the pricer clamps CachedTokens to InputTokens, so a 90K cache read on a
// 4K fresh remainder would price as 4K cached + 0 fresh. Cache-creation tokens
// (billed by Anthropic at 1.25x input) have no dedicated model.Usage field; they
// ride in the fresh remainder at the full input rate — the closest honest
// accounting without inventing a new exported field. Absent cache fields ⇒ both
// folds add zero ⇒ model.Usage is byte-identical to before.
func (u anthropicUsage) toModelUsage() model.Usage {
	return model.Usage{
		InputTokens:  u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		OutputTokens: u.OutputTokens,
		CachedTokens: u.CacheReadInputTokens,
	}
}

// streamBlock accumulates one content block as its deltas arrive. For tool_use
// the input JSON args stream as fragments and are joined into Input at stop.
type streamBlock struct {
	typ     string
	id      string
	name    string
	text    string
	jsonBuf []byte
}

// Stream POSTs the same request body as Complete with "stream":true, decodes the
// Messages SSE event stream with bufio, forwards each text delta to onChunk as it
// arrives, and assembles the identical model.Response Complete would return
// (text + tool_use blocks, Usage, StopReason). It honors ctx: on cancellation
// mid-stream it stops reading and returns the partial Response plus ctx.Err()
// (interrupt-but-preserve). onChunk may be nil.
func (a *Anthropic) Stream(ctx context.Context, system string, msgs []model.Message, tools []model.Tool, maxTokens int, onChunk func(model.Chunk)) (model.Response, error) {
	req, err := a.newRequest(ctx, system, msgs, tools, maxTokens, true)
	if err != nil {
		return model.Response{}, err
	}
	req.Header.Set("accept", "text/event-stream")

	resp, err := a.http.Do(req)
	if err != nil {
		// A ctx cancellation before any byte arrives yields no partial; surface
		// the context error so the caller sees the interrupt.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return model.Response{}, ctxErr
		}
		return model.Response{}, fmt.Errorf("messages stream request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return model.Response{}, newAPIError(resp.StatusCode, resp.Header, raw)
	}

	return assembleAnthropicStream(ctx, resp.Body, onChunk)
}

// assembleAnthropicStream drives the SSE read loop and builds the Response. It is
// split out from Stream so it is unit-testable against any io.Reader.
func assembleAnthropicStream(ctx context.Context, body io.Reader, onChunk func(model.Chunk)) (model.Response, error) {
	var (
		out    model.Response
		blocks = map[int]*streamBlock{}
		order  []int // block indices in first-seen order, for stable assembly
	)

	finish := func() model.Response {
		for _, idx := range order {
			b := blocks[idx]
			switch b.typ {
			case "text":
				out.Content = append(out.Content, model.Block{Type: "text", Text: b.text})
			case "tool_use":
				out.Content = append(out.Content, model.Block{
					Type:  "tool_use",
					ID:    b.id,
					Name:  b.name,
					Input: json.RawMessage(orEmptyObj(string(b.jsonBuf))),
				})
			}
		}
		return out
	}

	sc := bufio.NewScanner(body)
	// SSE data lines for a long tool-call argument or assistant turn can be large;
	// raise the scanner's max token size well above the 64 KiB default.
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)

	for sc.Scan() {
		// Interrupt-but-preserve: a cancelled ctx stops the read loop and returns
		// whatever has been assembled so far, paired with the context error.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return finish(), ctxErr
		}

		line := sc.Bytes()
		// SSE frames are "event:"/"data:" lines separated by blank lines. The
		// event type is redundant with the JSON "type" field, so we key off the
		// data payload alone and skip everything else (event:, id:, :comments).
		data, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		data = bytes.TrimSpace(data)
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}

		var ev streamEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return finish(), fmt.Errorf("decode stream event: %w", err)
		}

		switch ev.Type {
		case "message_start":
			// The input side (including the cache tallies) is fixed at the turn's
			// start and rides message_start's usage. Apply the same disjoint→total
			// fold as Complete so both paths report identical accounting.
			out.Usage = ev.Message.Usage.toModelUsage()

		case "content_block_start":
			if _, seen := blocks[ev.Index]; !seen {
				order = append(order, ev.Index)
			}
			blocks[ev.Index] = &streamBlock{
				typ:  ev.ContentBlock.Type,
				id:   ev.ContentBlock.ID,
				name: ev.ContentBlock.Name,
			}

		case "content_block_delta":
			b := blocks[ev.Index]
			if b == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				b.text += ev.Delta.Text
				if ev.Delta.Text != "" && onChunk != nil {
					onChunk(model.Chunk{Text: ev.Delta.Text})
				}
			case "input_json_delta":
				b.jsonBuf = append(b.jsonBuf, ev.Delta.PartialJSON...)
			}

		case "content_block_stop":
			// Block fully received; nothing to flush — assembled lazily at finish.

		case "message_delta":
			if ev.Delta.StopReason != "" {
				out.StopReason = ev.Delta.StopReason
			}
			// The cumulative output-token count rides on message_delta.
			if ev.Usage.OutputTokens != 0 {
				out.Usage.OutputTokens = ev.Usage.OutputTokens
			}
			// message_delta usage is CUMULATIVE. On a server-side-tool turn (the
			// web_search builtin) it restates the FULL input/cache tally that
			// message_start understated, so the input side must fold here too —
			// otherwise a streaming web-search turn under-charges the budget wall
			// and under-fills the context gauge vs Complete. Mirror toModelUsage's
			// disjoint→total fold, gated on the input-side fields being non-zero so
			// an ordinary message_delta (which omits them) leaves the message_start
			// total untouched, preserving the Complete-vs-Stream identical accounting.
			if in := ev.Usage.InputTokens + ev.Usage.CacheReadInputTokens + ev.Usage.CacheCreationInputTokens; in != 0 {
				out.Usage.InputTokens = in
				out.Usage.CachedTokens = ev.Usage.CacheReadInputTokens
			}

		case "message_stop":
			return finish(), nil

		case "error":
			return finish(), fmt.Errorf("anthropic stream error: %s", tail(string(data), 1000))
		}
	}

	if err := sc.Err(); err != nil {
		// A read error caused by ctx cancellation is reported as the context error
		// with the partial Response, honoring interrupt-but-preserve.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return finish(), ctxErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return finish(), err
		}
		return finish(), fmt.Errorf("read stream: %w", err)
	}

	// Clean EOF without an explicit message_stop: return what we assembled.
	return finish(), nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
