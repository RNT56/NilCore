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
	"net/url"
	"strings"
	"time"

	"nilcore/internal/model"
)

// OpenAI is the Chat Completions adapter. It translates the canonical model.*
// format to/from OpenAI's wire shape (tool_use/tool_result blocks ↔
// tool_calls/tool messages). The same adapter serves every OpenAI-compatible
// endpoint — OpenRouter, Groq, Fireworks, Azure, local vLLM/Ollama/LM-Studio —
// by varying baseURL and the auth descriptor (it is OpenAI-compatible).
//
// baseURL is the FULL endpoint prefix (it already carries any "/v1"); newRequest
// appends only "/chat/completions". The auth descriptor names the header and its
// value prefix (default "authorization" / "Bearer "; Azure uses "api-key" with no
// prefix; an empty key emits no auth header at all, for keyless local servers).
// maxTokensField records the JSON field name for the token cap so a later task can
// switch it per backend — it is stored but not yet read by the marshaller, so the
// request body stays byte-identical to today.
type OpenAI struct {
	key            string
	model          string
	baseURL        string
	authHeader     string
	authPrefix     string
	maxTokensField string
	http           *http.Client

	// SOTA Chat-Completions request fields (P15-T05). All opt-in: a zero value
	// for every one of these leaves the corresponding oaiRequest field at its
	// zero value, which is omitempty, so the request body stays byte-identical to
	// today when none is configured.
	reasoningEffort   string             // "low" | "medium" | "high" | "minimal"
	responseFormat    *oaiResponseFormat // structured-output json_schema
	parallelToolCalls *bool              // pointer: false is meaningful
	toolChoice        json.RawMessage    // "auto" | "none" | {"type":"function",...}
	serviceTier       string             // "auto" | "default" | "flex" | "priority"
	promptCacheKey    string             // routing hint for prompt caching

	// OpenRouter-only typed extras (P15-T06). These are gated on isOpenRouter:
	// they are merged into the request body / set as headers ONLY when this adapter
	// targets the OpenRouter base, and only when the operator configured them — so a
	// bare OpenRouter request stays byte-identical to a plain OpenAI-compatible one.
	// isOpenRouter is set by NewOpenRouter and by the compat path when the base host
	// is openrouter.ai. openRouterExtras is nil until a WithOpenRouter* option runs;
	// the attribution strings are static config (NEVER the key, invariant I3).
	isOpenRouter      bool
	openRouterExtras  *openRouterExtras
	openRouterReferer string // HTTP-Referer attribution header (empty ⇒ not sent)
	openRouterTitle   string // X-Title attribution header (empty ⇒ not sent)
}

// Option configures an OpenAI-compatible adapter built via NewOpenAICompatible.
// Options are applied in order over the defaults (OpenAI's base URL, Bearer auth,
// "max_tokens").
type Option func(*OpenAI)

// WithBaseURL overrides the endpoint prefix. It is the FULL prefix (including any
// "/v1"); only "/chat/completions" is appended, with no "/v1" injected. A trailing
// slash is trimmed, so there is never a doubled slash.
func WithBaseURL(baseURL string) Option {
	return func(o *OpenAI) { o.baseURL = baseURL }
}

// WithAuth sets the auth header name and the value prefix. Bearer is
// headerName="authorization", valuePrefix="Bearer "; Azure is headerName="api-key",
// valuePrefix="" (raw key). The header is emitted only when the key is non-empty.
func WithAuth(headerName, valuePrefix string) Option {
	return func(o *OpenAI) {
		o.authHeader = headerName
		o.authPrefix = valuePrefix
	}
}

// WithMaxTokensField sets the JSON field name used for the token cap (default
// "max_tokens"). The value is stored for a later task; the body marshals
// unchanged for now.
func WithMaxTokensField(field string) Option {
	return func(o *OpenAI) { o.maxTokensField = field }
}

// WithKey sets the API key. The key is held only to set a per-request header
// (invariant I3): it is never logged, never placed in a prompt, and never given
// to the model.
func WithKey(key string) Option {
	return func(o *OpenAI) { o.key = key }
}

// WithReasoningEffort sets the reasoning_effort hint for reasoning models
// (gpt-5.x / o-series): "minimal" | "low" | "medium" | "high". Empty (the
// default) omits the field, keeping the request body byte-identical to today.
func WithReasoningEffort(effort string) Option {
	return func(o *OpenAI) { o.reasoningEffort = effort }
}

// WithResponseFormat requests structured output: the model must emit JSON
// conforming to the given JSON Schema. name labels the schema; strict toggles
// strict-schema enforcement; schema is the raw JSON Schema. A nil/zero config
// omits response_format entirely (byte-identical to today).
func WithResponseFormat(name string, strict bool, schema json.RawMessage) Option {
	return func(o *OpenAI) {
		o.responseFormat = &oaiResponseFormat{Type: "json_schema"}
		o.responseFormat.JSONSchema.Name = name
		o.responseFormat.JSONSchema.Strict = strict
		o.responseFormat.JSONSchema.Schema = schema
	}
}

// WithParallelToolCalls controls whether the model may emit multiple tool calls
// in one turn. It takes a pointer because false ("force one tool call at a
// time") is a meaningful, distinct value from unset; unset omits the field.
func WithParallelToolCalls(enabled bool) Option {
	return func(o *OpenAI) { o.parallelToolCalls = &enabled }
}

// WithToolChoice pins how the model selects tools: a raw JSON value, e.g.
// "auto", "none", "required", or {"type":"function","function":{"name":"x"}}.
// Empty/nil omits the field (byte-identical to today).
func WithToolChoice(choice json.RawMessage) Option {
	return func(o *OpenAI) { o.toolChoice = choice }
}

// WithServiceTier selects the service tier: "auto" | "default" | "flex" |
// "priority". Empty (the default) omits the field.
func WithServiceTier(tier string) Option {
	return func(o *OpenAI) { o.serviceTier = tier }
}

// WithPromptCacheKey sets a routing hint that improves prompt-cache hit rates by
// steering identical-prefix requests to the same cache. Empty omits the field.
func WithPromptCacheKey(key string) Option {
	return func(o *OpenAI) { o.promptCacheKey = key }
}

// NewOpenAICompatible builds a Chat Completions adapter for any OpenAI-compatible
// endpoint. With no options it targets OpenAI itself (api.openai.com/v1, Bearer
// auth). Pass WithBaseURL / WithAuth / WithKey / WithMaxTokensField to retarget it
// (OpenRouter, Groq, Fireworks, Azure, local vLLM/Ollama, …).
func NewOpenAICompatible(model string, opts ...Option) *OpenAI {
	o := &OpenAI{
		model:          model,
		baseURL:        "https://api.openai.com/v1",
		authHeader:     "authorization",
		authPrefix:     "Bearer ",
		maxTokensField: "max_tokens",
		http:           &http.Client{Timeout: 5 * time.Minute},
	}
	for _, opt := range opts {
		opt(o)
	}
	// Infer the OpenRouter base from the configured URL host so the compat path
	// (NILCORE_COMPAT_BASE_URL pointed at OpenRouter, P15-T02) also gates the typed
	// extras + attribution headers. NewOpenRouter sets isOpenRouter explicitly; this
	// is the belt-and-suspenders host check for an operator-typed base. It NEVER
	// changes the body/headers on its own — extras and attribution are still
	// emitted only when separately configured, so a bare OpenRouter compat base
	// stays byte-identical.
	if !o.isOpenRouter && hostIsOpenRouter(o.baseURL) {
		o.isOpenRouter = true
	}
	return o
}

// hostIsOpenRouter reports whether baseURL's host is openrouter.ai (or a subdomain
// of it). It parses the URL so a path/query containing the string cannot trip it;
// a malformed URL is simply "not OpenRouter".
func hostIsOpenRouter(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	// DNS host names are case-insensitive; fold to lower so an operator-typed base
	// like https://OpenRouter.ai/... still gates the extras.
	host := strings.ToLower(u.Hostname())
	return host == "openrouter.ai" || strings.HasSuffix(host, ".openrouter.ai")
}

// NewOpenAI returns an OpenAI Chat Completions provider.
func NewOpenAI(key, modelID string) *OpenAI {
	return NewOpenAICompatible(modelID, WithKey(key))
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
	return NewOpenAICompatible(modelID, WithKey(key), WithBaseURL("https://openrouter.ai/api/v1"), withOpenRouterBase())
}

// withOpenRouterBase marks the adapter as targeting OpenRouter, gating the typed
// extras and the attribution headers (P15-T06). It is unexported: NewOpenRouter
// sets it directly, and the compat path infers it from the base host, so an
// operator never flips it by hand. With the flag set but no extras/attribution
// configured, the request stays byte-identical to a bare OpenRouter call.
func withOpenRouterBase() Option {
	return func(o *OpenAI) { o.isOpenRouter = true }
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
	Role string `json:"role"`
	// Content is a plain string for text-only messages (byte-identical to before)
	// or a []oaiContentPart array when the message carries an image. It is left nil
	// (omitted) when empty, so an assistant message that is only tool_calls marshals
	// exactly as it did.
	Content    any           `json:"content,omitempty"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

// oaiContentPart is one element of a multimodal message content array. OpenAI
// represents an image as an image_url part whose URL is a base64 data: URI; text
// is a text part. Parts are used only when an image is present.
type oaiContentPart struct {
	Type     string       `json:"type"` // "text" | "image_url"
	Text     string       `json:"text,omitempty"`
	ImageURL *oaiImageURL `json:"image_url,omitempty"`
}

type oaiImageURL struct {
	URL string `json:"url"` // "data:<media_type>;base64,<data>"
}

type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// oaiResponseFormat requests structured output via a JSON Schema. It is a
// pointer on oaiRequest with omitempty, so it vanishes from the body when unset.
type oaiResponseFormat struct {
	Type       string `json:"type"` // "json_schema"
	JSONSchema struct {
		Name   string          `json:"name"`
		Strict bool            `json:"strict,omitempty"`
		Schema json.RawMessage `json:"schema,omitempty"`
	} `json:"json_schema"`
}

// oaiRequest is the chat-completions request body. MaxTokens and maxTokensField
// carry NO json tag of their own (`json:"-"`): the token cap is emitted by the
// custom MarshalJSON in openai_maxtokens.go under the single dynamic key name in
// maxTokensField (default "max_tokens"; reasoning models use
// "max_completion_tokens"). This makes it structurally impossible to emit both
// keys — gpt-5.x / o-series reject a body carrying both. Every other field keeps
// its tag and omitempty exactly as before.
type oaiRequest struct {
	Model          string            `json:"model"`
	MaxTokens      int               `json:"-"`
	maxTokensField string            // json key for the token cap; never serialized directly
	Messages       []oaiMessage      `json:"messages"`
	Tools          []oaiTool         `json:"tools,omitempty"`
	Stream         bool              `json:"stream,omitempty"`
	StreamOptions  *oaiStreamOptions `json:"stream_options,omitempty"`

	// SOTA fields (P15-T05) — all additive and omitempty, so a zero-valued
	// request marshals byte-identically to before. They ride the T04 custom
	// MarshalJSON automatically because the oaiRequestAlias shares this exact
	// struct shape (same tags, same order, same omitempty).
	ReasoningEffort   string             `json:"reasoning_effort,omitempty"`
	ResponseFormat    *oaiResponseFormat `json:"response_format,omitempty"`
	ParallelToolCalls *bool              `json:"parallel_tool_calls,omitempty"`
	ToolChoice        json.RawMessage    `json:"tool_choice,omitempty"`
	ServiceTier       string             `json:"service_tier,omitempty"`
	PromptCacheKey    string             `json:"prompt_cache_key,omitempty"`

	// WebSearchOptions enables OpenAI's server-side web search (Phase 15). Set only
	// when a web-search builtin is advertised AND this is NOT the OpenRouter base
	// (OpenRouter renders web search as a plugin instead); nil otherwise so the body
	// stays byte-identical.
	WebSearchOptions *oaiWebSearchOptions `json:"web_search_options,omitempty"`

	// OpenRouter-only extras (P15-T06), embedded as an ANONYMOUS pointer so
	// encoding/json PROMOTES its tagged fields (provider, models, reasoning,
	// transforms, plugins) to the top level of the request body — exactly where
	// OpenRouter expects them. When nil (every non-OpenRouter call, and a bare
	// OpenRouter call) it promotes nothing, so the body is byte-identical to today.
	// The promotion survives the T04 oaiRequestAlias because the alias preserves the
	// embedded field and its tags.
	*openRouterExtras
}

type oaiResponse struct {
	Choices []struct {
		Message struct {
			Content   string        `json:"content"`
			ToolCalls []oaiToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage oaiUsage `json:"usage"`
}

// oaiUsage is the shared usage shape for both the non-stream response and the
// trailing stream usage frame. The first two fields are the original, frozen
// counts. The nested detail structs and OpenRouter's cost are pointers so a
// response lacking them decodes to nil ⇒ the derived model.Usage stays exactly
// what it was before this widening.
type oaiUsage struct {
	PromptTokens            int `json:"prompt_tokens"`
	CompletionTokens        int `json:"completion_tokens"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details,omitempty"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
	Cost *float64 `json:"cost,omitempty"` // OpenRouter reports call cost in USD
}

// toModelUsage maps the wire usage onto the canonical model.Usage. The three
// trailing fields are populated only when the corresponding nested detail (or
// cost) is present; absent ⇒ they stay zero ⇒ model.Usage is byte-identical to
// the pre-widening shape.
func (u oaiUsage) toModelUsage() model.Usage {
	mu := model.Usage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
	}
	if u.CompletionTokensDetails != nil {
		mu.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	}
	if u.PromptTokensDetails != nil {
		mu.CachedTokens = u.PromptTokensDetails.CachedTokens
	}
	if u.Cost != nil {
		mu.CostUSD = *u.Cost
	}
	return mu
}

// newRequest marshals the canonical inputs into the chat-completions request body
// and builds the authenticated POST. stream toggles SSE delivery (and asks for a
// trailing usage chunk); the body is otherwise identical between Complete and
// Stream. The API key rides a per-request header and never touches disk
// (invariant I3).
func (o *OpenAI) newRequest(ctx context.Context, system string, msgs []model.Message, tools []model.Tool, maxTokens int, stream bool) (*http.Request, error) {
	// Web search rides a request-level field, not the generic tools[] array, so lift
	// any web-search builtin out before rendering function tools (Phase 15).
	funcTools, hasWeb := splitWebSearch(tools)
	reqBody := oaiRequest{
		Model:          o.model,
		MaxTokens:      maxTokens,
		maxTokensField: o.maxTokensField,
		Messages:       toOpenAIMessages(system, msgs),
		Tools:          toOpenAITools(funcTools),
		Stream:         stream,

		// SOTA fields (P15-T05): each copies straight from the configured option,
		// which is the zero value unless set, so unset ⇒ omitempty ⇒ omitted.
		ReasoningEffort:   o.reasoningEffort,
		ResponseFormat:    o.responseFormat,
		ParallelToolCalls: o.parallelToolCalls,
		ToolChoice:        o.toolChoice,
		ServiceTier:       o.serviceTier,
		PromptCacheKey:    o.promptCacheKey,
	}
	if stream {
		reqBody.StreamOptions = &oaiStreamOptions{IncludeUsage: true}
	}

	// Web-search rendering (Phase 15). OpenAI (and bare openai-compatible) emit the
	// top-level web_search_options; OpenRouter renders web search as a `web` plugin
	// merged into its extras instead. Both ONLY when a web-search builtin is present,
	// so a non-web request stays byte-identical.
	if hasWeb && !o.isOpenRouter {
		reqBody.WebSearchOptions = &oaiWebSearchOptions{}
	}

	// OpenRouter extras ride the body ONLY on the OpenRouter base AND only when the
	// operator configured at least one (o.openRouterExtras non-nil) OR a web-search
	// builtin needs the web plugin. On any other base, or a bare OpenRouter call with
	// neither, the embedded pointer stays nil so the body is byte-identical to today.
	// applyDefaults injects require_parameters:true only WITHIN a present provider
	// object — never on a bare call.
	if o.isOpenRouter && (o.openRouterExtras != nil || hasWeb) {
		var extras openRouterExtras
		if o.openRouterExtras != nil {
			extras = *o.openRouterExtras
			if extras.Provider != nil {
				p := *extras.Provider
				extras.Provider = &p
			}
		}
		if hasWeb {
			extras.Plugins = append(extras.Plugins, openRouterPlugin{ID: "web"})
		}
		extras.applyDefaults()
		reqBody.openRouterExtras = &extras
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// baseURL is the full prefix; append only "/chat/completions". TrimRight folds
	// a trailing slash so the join never doubles it and never injects a "/v1".
	endpoint := strings.TrimRight(o.baseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	// Per-request header only (I3) — never logged, never persisted. Emitted only
	// when a key is present, so keyless local servers (vLLM/Ollama/LM-Studio) send
	// no auth header at all.
	if o.key != "" {
		req.Header.Set(o.authHeader, o.authPrefix+o.key)
	}
	// OpenRouter attribution headers (P15-T06): static operator-supplied config
	// strings that credit the calling app on OpenRouter's dashboards. Set ONLY on
	// the OpenRouter base AND only when configured — so a bare OpenRouter request
	// sends no extra header (byte-identical to today). These are NEVER the API key
	// (invariant I3) and are never logged.
	if o.isOpenRouter {
		if o.openRouterReferer != "" {
			req.Header.Set("HTTP-Referer", o.openRouterReferer)
		}
		if o.openRouterTitle != "" {
			req.Header.Set("X-Title", o.openRouterTitle)
		}
	}
	return req, nil
}

// Complete translates, calls /chat/completions, and translates the reply back.
func (o *OpenAI) Complete(ctx context.Context, system string, msgs []model.Message, tools []model.Tool, maxTokens int) (model.Response, error) {
	req, err := o.newRequest(ctx, system, msgs, tools, maxTokens, false)
	if err != nil {
		return model.Response{}, err
	}

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
		// Typed error: fast-fail a terminal 4xx, honor a 429/5xx Retry-After (I3: key-free).
		return model.Response{}, newAPIError(resp.StatusCode, resp.Header, raw)
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
			var atext string
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					atext += b.Text
				case "tool_use":
					tc := oaiToolCall{ID: b.ID, Type: "function"}
					tc.Function.Name = b.Name
					tc.Function.Arguments = rawOrEmptyObj(b.Input)
					am.ToolCalls = append(am.ToolCalls, tc)
				}
			}
			// Assign only when non-empty so an assistant message that is only
			// tool_calls keeps Content nil (omitted), exactly as before.
			if atext != "" {
				am.Content = atext
			}
			out = append(out, am)
			continue
		}
		// user turn: tool results become role:"tool" messages; text + images stay on
		// the user message — a plain string when there is no image (byte-identical to
		// the prior path), a multimodal content-part array when an image is present.
		var text string
		var parts []oaiContentPart
		for _, b := range m.Content {
			switch b.Type {
			case "tool_result":
				out = append(out, oaiMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: b.Content})
			case "text":
				text += b.Text
			case "image":
				if b.Source != nil {
					parts = append(parts, oaiContentPart{
						Type:     "image_url",
						ImageURL: &oaiImageURL{URL: "data:" + b.Source.MediaType + ";base64," + b.Source.Data},
					})
				}
			}
		}
		switch {
		case len(parts) > 0:
			// A leading text part carries the prompt alongside the image(s).
			if text != "" {
				parts = append([]oaiContentPart{{Type: "text", Text: text}}, parts...)
			}
			out = append(out, oaiMessage{Role: "user", Content: parts})
		case text != "":
			out = append(out, oaiMessage{Role: "user", Content: text})
		}
	}
	return out
}

// oaiWebSearchOptions is OpenAI's top-level web-search request object (Phase 15). An
// empty object enables provider-side search on a search-capable model; the optional
// search_context_size tunes recall. It is omitted unless a web-search builtin is
// advertised, so a non-web body is byte-identical.
type oaiWebSearchOptions struct {
	SearchContextSize string `json:"search_context_size,omitempty"`
}

// splitWebSearch separates the web-search builtin (which rides a request-level field,
// not the tools[] array) from the function tools. hasWeb is true when the model was
// offered native web search.
func splitWebSearch(tools []model.Tool) (funcTools []model.Tool, hasWeb bool) {
	for _, t := range tools {
		if t.IsWebSearch() {
			hasWeb = true
			continue
		}
		funcTools = append(funcTools, t)
	}
	return funcTools, hasWeb
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

// oaiStreamChunk is one chat-completions SSE frame. The delta carries incremental
// text and/or tool-call fragments; finish_reason lands on the last choice frame;
// the trailing usage-only frame (requested via stream_options.include_usage) has
// an empty choices array. Only the fields the assembler reads are decoded; the
// frame is parsed as data, never executed (invariant I7).
type oaiStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string             `json:"content"`
			ToolCalls []oaiToolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaiUsage `json:"usage"`
}

// oaiToolCallDelta is a streamed tool-call fragment. id and function.name arrive
// on the opening fragment for a given index; function.arguments arrives in pieces
// across later fragments. index ties the fragments of one call together.
type oaiToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// oaiStreamToolCall accumulates one streamed tool call across its fragments. The
// argument fragments are concatenated and parsed into Input at assembly time.
type oaiStreamToolCall struct {
	id      string
	name    string
	argsBuf []byte
}

// Stream POSTs the same chat-completions body as Complete with "stream":true (and
// stream_options.include_usage so a trailing usage frame is sent), decodes the SSE
// frames with bufio, forwards each content delta to onChunk as it arrives, and
// assembles the identical model.Response Complete would return (text + tool_use
// blocks, Usage, StopReason from finish_reason). It honors ctx: on cancellation
// mid-stream it stops reading and returns the partial Response plus ctx.Err()
// (interrupt-but-preserve). onChunk may be nil. OpenRouter inherits this verbatim
// — it is the same adapter on a different base URL.
func (o *OpenAI) Stream(ctx context.Context, system string, msgs []model.Message, tools []model.Tool, maxTokens int, onChunk func(model.Chunk)) (model.Response, error) {
	req, err := o.newRequest(ctx, system, msgs, tools, maxTokens, true)
	if err != nil {
		return model.Response{}, err
	}
	req.Header.Set("accept", "text/event-stream")

	resp, err := o.http.Do(req)
	if err != nil {
		// A ctx cancellation before any byte arrives yields no partial; surface
		// the context error so the caller sees the interrupt.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return model.Response{}, ctxErr
		}
		return model.Response{}, fmt.Errorf("chat completions stream request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return model.Response{}, newAPIError(resp.StatusCode, resp.Header, raw)
	}

	return assembleOpenAIStream(ctx, resp.Body, onChunk)
}

// assembleOpenAIStream drives the SSE read loop and builds the Response. It is
// split out from Stream so it is unit-testable against any io.Reader.
func assembleOpenAIStream(ctx context.Context, body io.Reader, onChunk func(model.Chunk)) (model.Response, error) {
	var (
		out       model.Response
		textBuf   []byte
		hasText   bool
		finish    string
		toolCalls = map[int]*oaiStreamToolCall{}
		toolOrder []int // tool-call indices in first-seen order, for stable assembly
	)

	assemble := func() model.Response {
		var r model.Response
		if hasText {
			r.Content = append(r.Content, model.Block{Type: "text", Text: string(textBuf)})
		}
		for _, idx := range toolOrder {
			tc := toolCalls[idx]
			r.Content = append(r.Content, model.Block{
				Type:  "tool_use",
				ID:    tc.id,
				Name:  tc.name,
				Input: json.RawMessage(orEmptyObj(string(tc.argsBuf))),
			})
		}
		r.StopReason = stopReasonFromFinish(finish)
		r.Usage = out.Usage
		return r
	}

	sc := bufio.NewScanner(body)
	// A content delta or tool-call argument fragment can exceed the 64 KiB default
	// scanner token size; raise the cap to match the non-stream read limit.
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)

	for sc.Scan() {
		// Interrupt-but-preserve: a cancelled ctx stops the read loop and returns
		// whatever has been assembled so far, paired with the context error.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return assemble(), ctxErr
		}

		line := sc.Bytes()
		// SSE frames are "data:" lines separated by blank lines; skip everything
		// else (event:, id:, :comments, blank separators).
		data, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		data = bytes.TrimSpace(data)
		if len(data) == 0 {
			continue
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			return assemble(), nil
		}

		var chunk oaiStreamChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			return assemble(), fmt.Errorf("decode stream chunk: %w", err)
		}

		// The trailing usage-only frame carries no choices.
		if chunk.Usage != nil {
			out.Usage = chunk.Usage.toModelUsage()
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]
		if choice.FinishReason != "" {
			finish = choice.FinishReason
		}
		if choice.Delta.Content != "" {
			hasText = true
			textBuf = append(textBuf, choice.Delta.Content...)
			if onChunk != nil {
				onChunk(model.Chunk{Text: choice.Delta.Content})
			}
		}
		for _, tcd := range choice.Delta.ToolCalls {
			tc, seen := toolCalls[tcd.Index]
			if !seen {
				tc = &oaiStreamToolCall{}
				toolCalls[tcd.Index] = tc
				toolOrder = append(toolOrder, tcd.Index)
			}
			if tcd.ID != "" {
				tc.id = tcd.ID
			}
			if tcd.Function.Name != "" {
				tc.name = tcd.Function.Name
			}
			tc.argsBuf = append(tc.argsBuf, tcd.Function.Arguments...)
		}
	}

	if err := sc.Err(); err != nil {
		// A read error caused by ctx cancellation is reported as the context error
		// with the partial Response, honoring interrupt-but-preserve.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return assemble(), ctxErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return assemble(), err
		}
		return assemble(), fmt.Errorf("read stream: %w", err)
	}

	// Clean EOF without an explicit [DONE]: return what we assembled.
	return assemble(), nil
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
	out.StopReason = stopReasonFromFinish(ch.FinishReason)
	out.Usage = r.Usage.toModelUsage()
	return out
}

// stopReasonFromFinish maps OpenAI's finish_reason onto the canonical StopReason.
// Shared by the non-stream and stream paths so both assemble an identical reply.
func stopReasonFromFinish(finish string) string {
	switch finish {
	case "tool_calls":
		return "tool_use"
	case "stop":
		return "end_turn"
	default:
		return finish
	}
}

func rawOrEmptyObj(r json.RawMessage) string { return orEmptyObj(string(r)) }

func orEmptyObj(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}
