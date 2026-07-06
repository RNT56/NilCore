package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"nilcore/internal/model"
)

// TestTuningIsZero pins the byte-identity fast path: a Tuning with no knob set is
// zero, and any single knob makes it non-zero.
func TestTuningIsZero(t *testing.T) {
	if !(Tuning{}).IsZero() {
		t.Error("empty Tuning must be zero")
	}
	yes := true
	nonZero := []Tuning{
		{ReasoningEffort: "high"},
		{MaxTokensField: "max_completion_tokens"},
		{ServiceTier: "flex"},
		{PromptCacheKey: "k"},
		{ParallelToolCalls: &yes},
		{OpenRouterReferer: "https://app"},
		{OpenRouterTitle: "App"},
		{ResponseFormat: &ResponseFormat{Name: "s"}},
		{ToolChoice: json.RawMessage(`"auto"`)},
		{OpenRouterProvider: &OpenRouterProvider{Sort: "price"}},
		{OpenRouterModels: []string{"a/b"}},
		{OpenRouterReasoning: &OpenRouterReasoning{Effort: "high"}},
		{OpenRouterTransforms: []string{"middle-out"}},
		{OpenRouterPlugins: []OpenRouterPlugin{{ID: "web"}}},
	}
	for i, tn := range nonZero {
		if tn.IsZero() {
			t.Errorf("case %d: Tuning %+v reported zero, want non-zero", i, tn)
		}
	}
}

// TestResolveWithTuningReachesRequest is the regression for the unwired-options gap:
// a configured Tuning must actually reach the wire. It resolves an OpenAI provider,
// points it at a capture server, and asserts the request body carries every set knob
// (reasoning_effort the brief's named example, plus service_tier / prompt_cache_key /
// parallel_tool_calls) — proving the option chain is wired end to end.
func TestResolveWithTuningReachesRequest(t *testing.T) {
	off := false
	tuning := Tuning{
		ReasoningEffort:   "high",
		ServiceTier:       "flex",
		PromptCacheKey:    "cache-1",
		ParallelToolCalls: &off,
	}

	p, err := ResolveWithTuning("openai:gpt-4o", staticEnv("OPENAI_API_KEY", "k"), tuning)
	if err != nil {
		t.Fatalf("ResolveWithTuning: %v", err)
	}
	oa, ok := p.(*OpenAI)
	if !ok {
		t.Fatalf("provider type = %T, want *OpenAI", p)
	}

	var body map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	oa.baseURL = srv.URL

	if _, err := oa.Complete(context.Background(), "sys",
		[]model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: "go"}}}}, nil, 100); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	assertJSONString(t, body, "reasoning_effort", `"high"`)
	assertJSONString(t, body, "service_tier", `"flex"`)
	assertJSONString(t, body, "prompt_cache_key", `"cache-1"`)
	assertJSONString(t, body, "parallel_tool_calls", `false`)
}

// TestResolveWithTuningZeroByteIdentical proves a zero Tuning leaves the request
// body identical to plain ResolveWith — none of the optional fields appear.
func TestResolveWithTuningZeroByteIdentical(t *testing.T) {
	p, err := ResolveWithTuning("openai:gpt-4o", staticEnv("OPENAI_API_KEY", "k"), Tuning{})
	if err != nil {
		t.Fatalf("ResolveWithTuning: %v", err)
	}
	oa := p.(*OpenAI)
	body := captureBody(t, oa, 100, false)

	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	for _, k := range []string{"reasoning_effort", "service_tier", "prompt_cache_key", "parallel_tool_calls"} {
		if _, present := m[k]; present {
			t.Errorf("zero Tuning leaked %q into the body: %s", k, body)
		}
	}
}

// TestResolveWithTuningAttributionHeaders proves the OpenRouter attribution knobs
// ride per-request headers on the OpenRouter base, and are NOT sent for a plain
// OpenAI provider (gated on isOpenRouter inside the adapter).
func TestResolveWithTuningAttributionHeaders(t *testing.T) {
	tuning := Tuning{OpenRouterReferer: "https://app.example", OpenRouterTitle: "MyApp"}

	t.Run("openrouter-sends", func(t *testing.T) {
		p, err := ResolveWithTuning("openrouter:meta-llama/llama-3.1-70b", staticEnv("OPENROUTER_API_KEY", "k"), tuning)
		if err != nil {
			t.Fatalf("ResolveWithTuning: %v", err)
		}
		oa := p.(*OpenAI)
		var referer, title string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			referer = r.Header.Get("HTTP-Referer")
			title = r.Header.Get("X-Title")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
		}))
		defer srv.Close()
		oa.baseURL = srv.URL
		if _, err := oa.Complete(context.Background(), "", nil, nil, 10); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if referer != "https://app.example" || title != "MyApp" {
			t.Errorf("attribution headers = %q/%q, want https://app.example/MyApp", referer, title)
		}
	})

	t.Run("openai-omits", func(t *testing.T) {
		p, err := ResolveWithTuning("openai:gpt-4o", staticEnv("OPENAI_API_KEY", "k"), tuning)
		if err != nil {
			t.Fatalf("ResolveWithTuning: %v", err)
		}
		oa := p.(*OpenAI)
		var referer, title string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			referer = r.Header.Get("HTTP-Referer")
			title = r.Header.Get("X-Title")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
		}))
		defer srv.Close()
		oa.baseURL = srv.URL
		if _, err := oa.Complete(context.Background(), "", nil, nil, 10); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if referer != "" || title != "" {
			t.Errorf("plain OpenAI must not send attribution headers, got %q/%q", referer, title)
		}
	})
}

// TestResolveWithTuningAnthropicUntouched proves the Anthropic adapter is returned
// unmodified by a non-zero Tuning (it has no equivalent request-shaping surface).
func TestResolveWithTuningAnthropicUntouched(t *testing.T) {
	p, err := ResolveWithTuning("anthropic:claude-x", staticEnv("ANTHROPIC_API_KEY", "k"), Tuning{ReasoningEffort: "high"})
	if err != nil {
		t.Fatalf("ResolveWithTuning: %v", err)
	}
	if _, ok := p.(*Anthropic); !ok {
		t.Fatalf("provider type = %T, want *Anthropic", p)
	}
}

// captureTuningBody resolves spec with the given key + tuning, points the adapter
// at a capture server, sends one Complete, and returns the decoded request body.
func captureTuningBody(t *testing.T, spec, keyEnv string, tuning Tuning) map[string]json.RawMessage {
	t.Helper()
	p, err := ResolveWithTuning(spec, staticEnv(keyEnv, "k"), tuning)
	if err != nil {
		t.Fatalf("ResolveWithTuning(%q): %v", spec, err)
	}
	oa, ok := p.(*OpenAI)
	if !ok {
		t.Fatalf("provider type = %T, want *OpenAI", p)
	}
	var body map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	oa.baseURL = srv.URL
	if _, err := oa.Complete(context.Background(), "sys",
		[]model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: "go"}}}}, nil, 100); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return body
}

// TestResolveWithTuningResponseFormatAndToolChoice proves the OpenAI-family
// response_format and tool_choice knobs ride the request body when set on a Tuning.
func TestResolveWithTuningResponseFormatAndToolChoice(t *testing.T) {
	body := captureTuningBody(t, "openai:gpt-4o", "OPENAI_API_KEY", Tuning{
		ResponseFormat: &ResponseFormat{Name: "answer", Strict: true, Schema: json.RawMessage(`{"type":"object"}`)},
		ToolChoice:     json.RawMessage(`"required"`),
	})
	assertJSONString(t, body, "tool_choice", `"required"`)
	rf, ok := body["response_format"]
	if !ok {
		t.Fatalf("body missing response_format: %v", body)
	}
	want := `{"type":"json_schema","json_schema":{"name":"answer","strict":true,"schema":{"type":"object"}}}`
	if !jsonEqual(t, rf, json.RawMessage(want)) {
		t.Errorf("response_format = %s, want %s", rf, want)
	}
}

// TestResolveWithTuningOpenRouterExtras proves each OpenRouter routing extra
// (provider, models, reasoning, transforms, plugins) reaches the request body as a
// top-level key when set on a Tuning against the OpenRouter base.
func TestResolveWithTuningOpenRouterExtras(t *testing.T) {
	body := captureTuningBody(t, "openrouter:x/y", "OPENROUTER_API_KEY", Tuning{
		OpenRouterProvider:   &OpenRouterProvider{Sort: "throughput"},
		OpenRouterModels:     []string{"a/b", "c/d"},
		OpenRouterReasoning:  &OpenRouterReasoning{Effort: "high", Exclude: boolPtr(true)},
		OpenRouterTransforms: []string{"middle-out"},
		OpenRouterPlugins:    []OpenRouterPlugin{{ID: "web", MaxResults: 3}},
	})

	// provider carries the default-true require_parameters injected for a present
	// provider object, alongside the set sort.
	if raw, ok := body["provider"]; !ok || !jsonEqual(t, raw, json.RawMessage(`{"require_parameters":true,"sort":"throughput"}`)) {
		t.Errorf("provider = %s, want sort+require_parameters", raw)
	}
	if raw, ok := body["models"]; !ok || !jsonEqual(t, raw, json.RawMessage(`["a/b","c/d"]`)) {
		t.Errorf("models = %s", raw)
	}
	if raw, ok := body["reasoning"]; !ok || !jsonEqual(t, raw, json.RawMessage(`{"effort":"high","exclude":true}`)) {
		t.Errorf("reasoning = %s", raw)
	}
	if raw, ok := body["transforms"]; !ok || !jsonEqual(t, raw, json.RawMessage(`["middle-out"]`)) {
		t.Errorf("transforms = %s", raw)
	}
	if raw, ok := body["plugins"]; !ok || !jsonEqual(t, raw, json.RawMessage(`[{"id":"web","max_results":3}]`)) {
		t.Errorf("plugins = %s", raw)
	}
}

// staticEnv returns a getenv seam that yields val only for name (every other lookup
// is empty), mirroring the SecretStore-backed resolver the composition root passes.
func staticEnv(name, val string) func(string) string {
	return func(n string) string {
		if n == name {
			return val
		}
		return ""
	}
}

func assertJSONString(t *testing.T, m map[string]json.RawMessage, key, want string) {
	t.Helper()
	got, ok := m[key]
	if !ok {
		t.Errorf("body missing %q (want %s)", key, want)
		return
	}
	if string(got) != want {
		t.Errorf("%q = %s, want %s", key, got, want)
	}
}
