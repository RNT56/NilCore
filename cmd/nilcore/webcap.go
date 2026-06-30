package main

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"

	"nilcore/internal/model"
	"nilcore/internal/tools"
)

// webcap.go is the web-search CAPABILITY SWITCH (Phase 15, T09). Two paths exist and
// are MUTUALLY EXCLUSIVE — exactly one `web_search` tool may be advertised, or the
// model could emit a tool_use with no handler and corrupt the turn:
//
//   - PATH A (native, server-side) — the PROVIDER runs the search and returns the
//     answer; NilCore makes no HTTP call. It rides the model.BuiltinTool seam. It is
//     opt-in via NILCORE_WEB_SEARCH_NATIVE because it reaches the web OUTSIDE the
//     local egress allowlist (the provider fetches), a different trust posture than
//     the sandboxed client path — so it must be a deliberate choice, never implied.
//   - PATH B (client-side fallback) — the already-shipped, sandboxed, guard.Wrap'd
//     tools.WebSearchTool, egress-confined (I4/I7). It is what chat.go registers when
//     Path A is not selected.
//
// The caller (chat.go) registers the Path-A tool from selectNativeWebSearch when it
// is non-nil, and falls through to the Path-B tool otherwise — never both.

// nativeWebSearchTool advertises the provider-native web-search built-in. It has NO
// local Run: the provider fulfils the search server-side, so the harness never
// dispatches it (the result rides back inside the assistant turn — the model's own
// synthesized text, with raw provider result blocks dropped/fenced by the adapter
// decode, never re-entering as trusted instructions, I7). It implements
// tools.BuiltinProvider so Registry.Defs() carries the typed builtin (+ any beta
// header) to the provider.
type nativeWebSearchTool struct{ maxUses int }

func (nativeWebSearchTool) Name() string { return model.WebSearchName }

func (nativeWebSearchTool) Description() string {
	return "Search the web (handled by the model provider server-side)."
}

// Schema is unused for a builtin (the provider sends the typed def); the Tool
// interface requires it, so return an empty object.
func (nativeWebSearchTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (t nativeWebSearchTool) BuiltinDef() *model.BuiltinTool {
	return model.NewWebSearchTool(t.maxUses).Builtin
}

// Run should never be invoked (the provider handles web search server-side); if a
// provider ever did surface it as a client tool_use, this is an inert, honest no-op.
func (nativeWebSearchTool) Run(_ context.Context, _ string, _ json.RawMessage) (string, error) {
	return "web_search is handled by the provider server-side; no local action taken.", nil
}

var _ tools.BuiltinProvider = nativeWebSearchTool{}

// selectNativeWebSearch returns the Path-A native web-search tool for the configured
// model spec, or nil when native search is not opted in or the provider does not
// support it (Path B then applies). modelSpec is the resolved `provider:model`.
func selectNativeWebSearch(modelSpec string) tools.Tool {
	if strings.TrimSpace(os.Getenv("NILCORE_WEB_SEARCH_NATIVE")) == "" {
		return nil
	}
	if !providerSupportsNativeWebSearch(vendorOf(modelSpec), modelSpec) {
		return nil
	}
	return nativeWebSearchTool{maxUses: nativeWebSearchMaxUses()}
}

// providerSupportsNativeWebSearch reports whether the vendor+model can run native
// server-side web search. Conservative on OpenAI: only the documented web-search
// model family ("*-search-preview", the ids that accept web_search_options on chat
// completions) matches — a loose "search" substring would false-match unrelated ids
// like "research-*" and emit web_search_options the model rejects. Broad on Anthropic
// (GA tool) and OpenRouter (the `web` plugin augments ANY model — correct by design).
// Unknown vendors (generic openai-compatible / self-hosted) get no native search →
// Path B fallback.
func providerSupportsNativeWebSearch(vendor, modelSpec string) bool {
	switch vendor {
	case "anthropic":
		return true
	case "openrouter":
		return true
	case "openai":
		return strings.Contains(strings.ToLower(modelSpec), "search-preview")
	default:
		return false
	}
}

// nativeWebSearchMaxUses reads the optional per-turn cap (NILCORE_WEB_SEARCH_MAX_USES),
// defaulting to 5; 0/invalid ⇒ the default.
func nativeWebSearchMaxUses() int {
	if v := strings.TrimSpace(os.Getenv("NILCORE_WEB_SEARCH_MAX_USES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 5
}
