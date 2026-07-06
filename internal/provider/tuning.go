package provider

import (
	"encoding/json"

	"nilcore/internal/model"
)

// Tuning is the operator-configured, OpenAI-family request-shaping surface, lifted
// out of onboard.Config so the composition root can hand the provider package the
// persisted SOTA / OpenRouter-routing knobs WITHOUT this leaf importing onboard or
// any secret (invariant I3 — every field here is plain request DATA, never a key).
//
// It exists to close the "built-but-unwired options" gap (P15-T05/T06): the WithX
// options and the typed OpenRouter extras were fully implemented and adapter-tested
// but unreachable, because ResolveWith constructs adapters with ZERO options. Tuning
// now carries EVERY one of those knobs — reasoning_effort, the max-tokens field,
// service_tier, prompt_cache_key, parallel_tool_calls, response_format, tool_choice,
// the OpenRouter attribution headers, AND the OpenRouter routing extras (provider,
// models[], reasoning, transforms, plugins) — so none is left stranded. The
// composition root builds a Tuning from onboard.Config / env and calls
// ResolveWithTuning; every zero/blank field is skipped, so an unconfigured Tuning
// leaves the request body byte-identical to today.
//
// Only the OpenAI-family adapter (OpenAI / OpenRouter / openai-compatible) reads
// these; the Anthropic adapter ignores a Tuning entirely (its request shape has no
// equivalent here). The knobs that are OpenRouter-only (attribution + routing) are
// gated inside the adapter on the OpenRouter base, so handing them to a plain
// OpenAI adapter is harmless (they are held, never serialized).
type Tuning struct {
	// ReasoningEffort is the reasoning_effort hint for reasoning models:
	// "minimal" | "low" | "medium" | "high". Empty ⇒ omitted.
	ReasoningEffort string

	// MaxTokensField overrides the JSON field name for the token cap ("max_tokens"
	// default; reasoning models use "max_completion_tokens"). Empty ⇒ the adapter's
	// own default + auto-detection apply. A non-empty value here always wins.
	MaxTokensField string

	// ServiceTier selects the provider service tier: "auto" | "default" | "flex" |
	// "priority". Empty ⇒ omitted.
	ServiceTier string

	// PromptCacheKey steers identical-prefix requests to the same cache. Empty ⇒ omitted.
	PromptCacheKey string

	// ParallelToolCalls controls whether the model may emit multiple tool calls in
	// one turn. A pointer because false ("one at a time") is meaningful and distinct
	// from unset; nil ⇒ omitted.
	ParallelToolCalls *bool

	// OpenRouterReferer / OpenRouterTitle populate OpenRouter's optional
	// HTTP-Referer / X-Title attribution headers. Both blank ⇒ neither sent. These
	// are static config strings, NEVER the API key (invariant I3).
	OpenRouterReferer string
	OpenRouterTitle   string

	// ResponseFormat requests OpenAI/-compatible structured output (a json_schema
	// response_format). nil ⇒ omitted (byte-identical). It is applied on every
	// OpenAI-family base (not OpenRouter-gated); an OpenRouter base accepts it too.
	ResponseFormat *ResponseFormat

	// ToolChoice pins how the model selects tools: a raw JSON value, e.g. "auto",
	// "none", "required", or {"type":"function","function":{"name":"x"}}. Empty ⇒
	// omitted (byte-identical).
	ToolChoice json.RawMessage

	// OpenRouterProvider / OpenRouterModels / OpenRouterReasoning /
	// OpenRouterTransforms / OpenRouterPlugins are the OpenRouter-only routing extras.
	// Each is applied only on the OpenRouter base (held-but-not-serialized elsewhere)
	// and only when set, so a nil/empty value contributes nothing. They are plain
	// request DATA (routing knobs), never a secret (invariant I3).
	OpenRouterProvider   *OpenRouterProvider
	OpenRouterModels     []string
	OpenRouterReasoning  *OpenRouterReasoning
	OpenRouterTransforms []string
	OpenRouterPlugins    []OpenRouterPlugin
}

// ResponseFormat is the operator-facing structured-output request carried on a
// Tuning: a JSON Schema the model's reply must conform to. It is translated into
// the adapter's WithResponseFormat option (a json_schema response_format). Kept as
// a small exported struct so the composition root can build one without reaching
// into the adapter's unexported wire shape.
type ResponseFormat struct {
	Name   string          // labels the schema
	Strict bool            // strict-schema enforcement
	Schema json.RawMessage // the raw JSON Schema
}

// IsZero reports whether the Tuning carries no configured knob, so the caller can
// keep the byte-identical fast path (no options applied at all).
func (t Tuning) IsZero() bool {
	return t.ReasoningEffort == "" &&
		t.MaxTokensField == "" &&
		t.ServiceTier == "" &&
		t.PromptCacheKey == "" &&
		t.ParallelToolCalls == nil &&
		t.OpenRouterReferer == "" &&
		t.OpenRouterTitle == "" &&
		t.ResponseFormat == nil &&
		len(t.ToolChoice) == 0 &&
		t.OpenRouterProvider == nil &&
		len(t.OpenRouterModels) == 0 &&
		t.OpenRouterReasoning == nil &&
		len(t.OpenRouterTransforms) == 0 &&
		len(t.OpenRouterPlugins) == 0
}

// options renders the configured knobs as adapter Options, skipping every unset
// field so an unconfigured Tuning contributes nothing. The OpenRouter attribution
// pair is a single option (both strings ride one call); the adapter only emits a
// header for a non-empty leg, and only on the OpenRouter base.
func (t Tuning) options() []Option {
	var opts []Option
	if t.MaxTokensField != "" {
		opts = append(opts, WithMaxTokensField(t.MaxTokensField))
	}
	if t.ReasoningEffort != "" {
		opts = append(opts, WithReasoningEffort(t.ReasoningEffort))
	}
	if t.ServiceTier != "" {
		opts = append(opts, WithServiceTier(t.ServiceTier))
	}
	if t.PromptCacheKey != "" {
		opts = append(opts, WithPromptCacheKey(t.PromptCacheKey))
	}
	if t.ParallelToolCalls != nil {
		opts = append(opts, WithParallelToolCalls(*t.ParallelToolCalls))
	}
	if t.OpenRouterReferer != "" || t.OpenRouterTitle != "" {
		opts = append(opts, WithOpenRouterAttribution(t.OpenRouterReferer, t.OpenRouterTitle))
	}
	if t.ResponseFormat != nil {
		opts = append(opts, WithResponseFormat(t.ResponseFormat.Name, t.ResponseFormat.Strict, t.ResponseFormat.Schema))
	}
	if len(t.ToolChoice) != 0 {
		opts = append(opts, WithToolChoice(t.ToolChoice))
	}
	if t.OpenRouterProvider != nil {
		opts = append(opts, WithOpenRouterProvider(t.OpenRouterProvider))
	}
	if len(t.OpenRouterModels) != 0 {
		opts = append(opts, WithOpenRouterModels(t.OpenRouterModels...))
	}
	if t.OpenRouterReasoning != nil {
		opts = append(opts, WithOpenRouterReasoning(t.OpenRouterReasoning))
	}
	if len(t.OpenRouterTransforms) != 0 {
		opts = append(opts, WithOpenRouterTransforms(t.OpenRouterTransforms...))
	}
	if len(t.OpenRouterPlugins) != 0 {
		opts = append(opts, WithOpenRouterPlugins(t.OpenRouterPlugins...))
	}
	return opts
}

// applyOptions runs the given options over an already-constructed adapter. It lets
// ResolveWithTuning layer operator tuning onto an adapter the existing resolution
// path built, without duplicating that path. Re-running an option is safe — each is
// an idempotent field set — and re-deriving the OpenRouter-base inference is a no-op
// because none of the tuning options change the base URL.
func (o *OpenAI) applyOptions(opts ...Option) {
	for _, opt := range opts {
		opt(o)
	}
}

// ResolveWithTuning is ResolveWith plus the operator's OpenAI-family Tuning. It
// resolves the provider exactly as ResolveWith does (same key lookup, same
// anti-exfiltration checks), then layers the configured tuning options onto the
// OpenAI-family adapter. A zero Tuning is a pure pass-through (byte-identical to
// ResolveWith); the Anthropic adapter is returned untouched (it has no equivalent
// request-shaping surface here).
func ResolveWithTuning(spec string, getenv func(string) string, tuning Tuning) (model.Provider, error) {
	p, err := ResolveWith(spec, getenv)
	if err != nil {
		return nil, err
	}
	if tuning.IsZero() {
		return p, nil
	}
	if oa, ok := p.(*OpenAI); ok {
		oa.applyOptions(tuning.options()...)
	}
	return p, nil
}
