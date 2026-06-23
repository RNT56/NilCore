package main

import "testing"

func TestProviderSupportsNativeWebSearch(t *testing.T) {
	cases := []struct {
		vendor, spec string
		want         bool
	}{
		{"anthropic", "claude-sonnet-4-6", true},
		{"openrouter", "anthropic/claude-3.5", true},
		{"openai", "gpt-4o-search-preview", true}, // search-capable model
		{"openai", "gpt-4o", false},               // conservative: no "search" → no native
		{"openai-compatible", "llama-3", false},   // self-hosted → client-side
		{"", "x", false},
	}
	for _, c := range cases {
		if got := providerSupportsNativeWebSearch(c.vendor, c.spec); got != c.want {
			t.Errorf("providerSupportsNativeWebSearch(%q,%q) = %v, want %v", c.vendor, c.spec, got, c.want)
		}
	}
}

func TestSelectNativeWebSearchGating(t *testing.T) {
	// Off by default (opt-in env unset) — even for a supporting provider.
	t.Setenv("NILCORE_WEB_SEARCH_NATIVE", "")
	if selectNativeWebSearch("claude-sonnet-4-6") != nil {
		t.Fatal("native web search must be off unless opted in")
	}

	// Opted in + supporting provider → a native tool, advertised as web_search.
	t.Setenv("NILCORE_WEB_SEARCH_NATIVE", "1")
	tool := selectNativeWebSearch("claude-sonnet-4-6")
	if tool == nil || tool.Name() != "web_search" {
		t.Fatalf("expected a native web_search tool, got %v", tool)
	}

	// Opted in but UNsupported provider (bare gpt-4o) → nil (Path B fallback applies).
	if selectNativeWebSearch("openai:gpt-4o") != nil {
		t.Fatal("a non-search OpenAI model must not get native web search")
	}
	// Self-hosted compat → nil.
	if selectNativeWebSearch("openai-compatible:llama-3") != nil {
		t.Fatal("an openai-compatible endpoint must fall back to client-side")
	}
}

func TestNativeWebSearchMaxUses(t *testing.T) {
	t.Setenv("NILCORE_WEB_SEARCH_MAX_USES", "")
	if nativeWebSearchMaxUses() != 5 {
		t.Fatal("default max-uses should be 5")
	}
	t.Setenv("NILCORE_WEB_SEARCH_MAX_USES", "12")
	if nativeWebSearchMaxUses() != 12 {
		t.Fatal("env override should win")
	}
	t.Setenv("NILCORE_WEB_SEARCH_MAX_USES", "bad")
	if nativeWebSearchMaxUses() != 5 {
		t.Fatal("invalid override should fall back to default")
	}
}
