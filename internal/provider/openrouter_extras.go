package provider

// OpenRouter typed extras (P15-T06). OpenRouter is OpenAI-compatible at the wire
// level, but it accepts a handful of OpenRouter-only request fields that steer
// which upstream provider serves a call, a model fallback chain, reasoning
// controls, and prompt transforms / plugins. This file gives those fields TYPED
// Go structs (no raw maps) and the Option setters that populate them.
//
// The extras are merged onto the request body ONLY on the OpenRouter base (see
// openai.go: o.isOpenRouter), and the attribution headers (HTTP-Referer /
// X-Title) are emitted ONLY there and ONLY when the operator configured them.
// With nothing configured, a bare OpenRouter request stays byte-identical to a
// plain OpenAI-compatible call — the embedded pointer is nil, so it contributes
// zero bytes, and no attribution header is set.

// openRouterExtras carries the OpenRouter-only request fields. It is embedded as
// an ANONYMOUS pointer in oaiRequest: when nil it promotes nothing (byte-identical
// to today); when non-nil, encoding/json flattens its tagged fields to the top
// level of the request body, exactly where OpenRouter expects `provider`,
// `models`, `reasoning`, `transforms`, and `plugins`. Every field is omitempty,
// so configuring one knob never drags the others onto the wire.
type openRouterExtras struct {
	Provider   *OpenRouterProvider  `json:"provider,omitempty"`
	Models     []string             `json:"models,omitempty"`     // model fallback chain
	Reasoning  *OpenRouterReasoning `json:"reasoning,omitempty"`  // reasoning controls
	Transforms []string             `json:"transforms,omitempty"` // prompt transforms, e.g. "middle-out"
	Plugins    []OpenRouterPlugin   `json:"plugins,omitempty"`    // plugin chain, e.g. web search
}

// OpenRouterProvider is OpenRouter's provider-routing object. Every field is
// optional and omitempty; an empty object never reaches the wire because the
// whole *OpenRouterProvider stays nil until WithOpenRouterProvider runs.
type OpenRouterProvider struct {
	Order             []string            `json:"order,omitempty"`              // preferred upstream order
	AllowFallbacks    *bool               `json:"allow_fallbacks,omitempty"`    // pointer: false is meaningful
	RequireParameters *bool               `json:"require_parameters,omitempty"` // defaults true when extras present
	DataCollection    string              `json:"data_collection,omitempty"`    // "allow" | "deny"
	ZDR               *bool               `json:"zdr,omitempty"`                // zero-data-retention only
	Sort              string              `json:"sort,omitempty"`               // "price" | "throughput" | "latency"
	Only              []string            `json:"only,omitempty"`               // restrict to these upstreams
	Ignore            []string            `json:"ignore,omitempty"`             // skip these upstreams
	Quantizations     []string            `json:"quantizations,omitempty"`      // e.g. "fp8", "int4"
	MaxPrice          *OpenRouterMaxPrice `json:"max_price,omitempty"`          // per-token price ceiling
}

// OpenRouterMaxPrice caps the per-million-token price OpenRouter will pay when
// routing. Both legs are pointers so a 0 ceiling ("free only") is distinct from
// unset; an all-nil struct is never emitted because MaxPrice itself stays nil.
type OpenRouterMaxPrice struct {
	Prompt     *float64 `json:"prompt,omitempty"`
	Completion *float64 `json:"completion,omitempty"`
}

// OpenRouterReasoning controls reasoning-model behavior on OpenRouter (a
// normalized shape across upstream vendors). Effort and MaxTokens are mutually
// exclusive upstream; we just carry whatever the operator set. Exclude:true asks
// OpenRouter to run reasoning but withhold the reasoning text from the reply.
type OpenRouterReasoning struct {
	Effort    string `json:"effort,omitempty"`     // "low" | "medium" | "high"
	MaxTokens int    `json:"max_tokens,omitempty"` // explicit reasoning-token budget
	Exclude   *bool  `json:"exclude,omitempty"`    // run reasoning but omit it from output
	Enabled   *bool  `json:"enabled,omitempty"`    // pointer: false disables distinctly from unset
}

// OpenRouterPlugin is one entry in OpenRouter's plugin chain (e.g. the web-search
// plugin). ID is the only required field; MaxResults / Engine are optional knobs.
type OpenRouterPlugin struct {
	ID         string `json:"id"`
	MaxResults int    `json:"max_results,omitempty"`
	Engine     string `json:"engine,omitempty"`
}

// ensureExtras returns o.openRouterExtras, lazily allocating it so the Option
// setters can compose (each WithOpenRouter* adds to the same struct).
func (o *OpenAI) ensureExtras() *openRouterExtras {
	if o.openRouterExtras == nil {
		o.openRouterExtras = &openRouterExtras{}
	}
	return o.openRouterExtras
}

// WithOpenRouterProvider sets the provider-routing object. It is applied to the
// request body ONLY on the OpenRouter base; on any other base it is held but never
// serialized. require_parameters defaults to true WHEN a provider object is
// present (callers that pass an explicit RequireParameters override it). A nil
// argument is a no-op so the call site can pass through an unset config safely.
func WithOpenRouterProvider(p *OpenRouterProvider) Option {
	return func(o *OpenAI) {
		if p == nil {
			return
		}
		o.ensureExtras().Provider = p
	}
}

// WithOpenRouterModels sets the model fallback chain (`models[]`): OpenRouter
// tries each in order until one succeeds. An empty slice is a no-op (omitempty).
func WithOpenRouterModels(models ...string) Option {
	return func(o *OpenAI) {
		if len(models) == 0 {
			return
		}
		o.ensureExtras().Models = models
	}
}

// WithOpenRouterReasoning sets the reasoning controls object. A nil argument is a
// no-op. Applied only on the OpenRouter base.
func WithOpenRouterReasoning(r *OpenRouterReasoning) Option {
	return func(o *OpenAI) {
		if r == nil {
			return
		}
		o.ensureExtras().Reasoning = r
	}
}

// WithOpenRouterTransforms sets the prompt-transform chain (e.g. "middle-out").
func WithOpenRouterTransforms(transforms ...string) Option {
	return func(o *OpenAI) {
		if len(transforms) == 0 {
			return
		}
		o.ensureExtras().Transforms = transforms
	}
}

// WithOpenRouterPlugins sets the plugin chain (e.g. the web-search plugin).
func WithOpenRouterPlugins(plugins ...OpenRouterPlugin) Option {
	return func(o *OpenAI) {
		if len(plugins) == 0 {
			return
		}
		o.ensureExtras().Plugins = plugins
	}
}

// WithOpenRouterAttribution sets the HTTP-Referer and X-Title attribution headers
// OpenRouter uses to credit the calling app on its dashboards/leaderboards. These
// are STATIC operator-supplied config strings — never the API key (invariant I3).
// They are emitted ONLY on the OpenRouter base and ONLY when set here; an empty
// string for either leaves that header off. The values are stored as-is and ride
// per-request headers; they are never logged or persisted.
func WithOpenRouterAttribution(referer, title string) Option {
	return func(o *OpenAI) {
		o.openRouterReferer = referer
		o.openRouterTitle = title
	}
}

// applyOpenRouterDefaults fills in the OpenRouter-only defaults that apply WHEN
// extras are present. Today that is exactly one rule: when a provider object is
// configured but RequireParameters is left unset, default it to true (OpenRouter's
// recommended setting so a route that cannot honor the request's parameters is
// skipped rather than silently degrading). It is NOT auto-injected on a bare call:
// this runs only when o.openRouterExtras is non-nil AND carries a Provider object.
func (e *openRouterExtras) applyDefaults() {
	if e == nil || e.Provider == nil {
		return
	}
	if e.Provider.RequireParameters == nil {
		t := true
		e.Provider.RequireParameters = &t
	}
}
