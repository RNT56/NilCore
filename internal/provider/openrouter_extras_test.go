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

// P15-T06 — OpenRouter typed extras. These tests pin three guarantees:
//  1. BYTE-IDENTITY: a bare OpenRouter request with NO extras/attribution
//     configured is byte-identical to today's plain body, and emits no
//     attribution headers; the OpenAI path is wholly unaffected.
//  2. The typed extras serialize as TOP-LEVEL keys (provider, models, reasoning,
//     transforms, plugins) and ONLY on the OpenRouter base.
//  3. require_parameters defaults true within a present provider object, and
//     attribution headers are set only on the OpenRouter base when configured and
//     never carry the key.

// captureWire records the request body AND a couple of headers an adapter puts on
// the wire. It mirrors captureBody but also surfaces the attribution headers and
// the authorization value (so we can prove the key never leaks into attribution).
type wire struct {
	body    string
	referer string
	title   string
	authVal string
	allHdrs http.Header
}

func captureWire(t *testing.T, build func(base string) *OpenAI, stream bool) wire {
	t.Helper()
	var got wire
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.body = string(b)
		got.referer = r.Header.Get("HTTP-Referer")
		got.title = r.Header.Get("X-Title")
		got.authVal = r.Header.Get("authorization")
		got.allHdrs = r.Header.Clone()
		if stream {
			w.Header().Set("content-type", "text/event-stream")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	o := build(srv.URL)
	var err error
	msgs := []model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: "go"}}}}
	if stream {
		_, err = o.Stream(context.Background(), "sys", msgs, nil, 100, nil)
	} else {
		_, err = o.Complete(context.Background(), "sys", msgs, nil, 100)
	}
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	return got
}

// newOpenRouterAt builds a real NewOpenRouter adapter but points it at the test
// server, preserving the isOpenRouter flag (which an httptest URL would not infer
// from its host). It then re-applies any extra options.
func newOpenRouterAt(base, modelID string, opts ...Option) *OpenAI {
	o := NewOpenRouter("k", modelID)
	o.baseURL = base
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// boolPtr / f64Ptr are tiny helpers for the pointer fields.
func boolPtr(b bool) *bool      { return &b }
func f64Ptr(f float64) *float64 { return &f }

// TestOpenRouterBareByteIdentical is the core byte-identity guarantee: a bare
// OpenRouter request (no extras, no attribution) marshals to EXACTLY the same body
// a plain OpenAI-compatible call produces, and sends NO attribution header.
func TestOpenRouterBareByteIdentical(t *testing.T) {
	for _, stream := range []bool{false, true} {
		bare := captureWire(t, func(base string) *OpenAI {
			return newOpenRouterAt(base, "meta-llama/llama-3.1-70b")
		}, stream)

		plain := captureWire(t, func(base string) *OpenAI {
			return NewOpenAICompatible("meta-llama/llama-3.1-70b", WithKey("k"), WithBaseURL(base))
		}, stream)

		if bare.body != plain.body {
			t.Errorf("stream=%v bare OpenRouter body not byte-identical to plain:\n bare:  %s\n plain: %s", stream, bare.body, plain.body)
		}
		if bare.referer != "" || bare.title != "" {
			t.Errorf("stream=%v bare OpenRouter emitted attribution headers: referer=%q title=%q", stream, bare.referer, bare.title)
		}
		// Belt-and-suspenders: no OpenRouter-only key leaked into the body.
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(bare.body), &m); err != nil {
			t.Fatalf("decode %q: %v", bare.body, err)
		}
		for _, k := range []string{"provider", "models", "reasoning", "transforms", "plugins"} {
			if _, ok := m[k]; ok {
				t.Errorf("stream=%v bare OpenRouter leaked key %q: %s", stream, k, bare.body)
			}
		}
	}
}

// TestOpenRouterBareBodyBaseline pins the bare body against the literal captured
// baseline shared with the max_tokens / SOTA byte-identity tests.
func TestOpenRouterBareBodyBaseline(t *testing.T) {
	bare := captureWire(t, func(base string) *OpenAI {
		return newOpenRouterAt(base, "gpt-x")
	}, false)
	const baseline = `{"model":"gpt-x","max_tokens":100,"messages":[{"role":"system","content":"sys"},{"role":"user","content":"go"}]}`
	if bare.body != baseline {
		t.Errorf("bare OpenRouter body not byte-identical to baseline:\n got:      %s\n baseline: %s", bare.body, baseline)
	}
}

// TestOpenAIPathUnaffected proves a NON-OpenRouter adapter never serializes the
// extras even if (defensively) options are applied to it, and never sends
// attribution headers — the OpenAI path is wholly unchanged.
func TestOpenAIPathUnaffected(t *testing.T) {
	got := captureWire(t, func(base string) *OpenAI {
		// A plain OpenAI-compatible adapter with OpenRouter options forced on.
		return NewOpenAICompatible("gpt-x", WithKey("k"), WithBaseURL(base),
			WithOpenRouterModels("a/b", "c/d"),
			WithOpenRouterAttribution("https://app.example", "MyApp"),
		)
	}, false)

	const baseline = `{"model":"gpt-x","max_tokens":100,"messages":[{"role":"system","content":"sys"},{"role":"user","content":"go"}]}`
	if got.body != baseline {
		t.Errorf("OpenAI path body changed by OpenRouter options:\n got:      %s\n baseline: %s", got.body, baseline)
	}
	if got.referer != "" || got.title != "" {
		t.Errorf("OpenAI path emitted attribution headers: referer=%q title=%q", got.referer, got.title)
	}
}

// TestOpenRouterProviderObject proves the provider routing object serializes as a
// top-level "provider" key, with require_parameters defaulting to true when the
// caller did not set it.
func TestOpenRouterProviderObject(t *testing.T) {
	got := captureWire(t, func(base string) *OpenAI {
		return newOpenRouterAt(base, "x/y", WithOpenRouterProvider(&OpenRouterProvider{
			Order:          []string{"openai", "anthropic"},
			AllowFallbacks: boolPtr(false),
			DataCollection: "deny",
			Sort:           "throughput",
			MaxPrice:       &OpenRouterMaxPrice{Prompt: f64Ptr(1.5), Completion: f64Ptr(0)},
		}))
	}, false)

	prov := topLevel(t, got.body, "provider")
	want := `{"order":["openai","anthropic"],"allow_fallbacks":false,"require_parameters":true,"data_collection":"deny","sort":"throughput","max_price":{"prompt":1.5,"completion":0}}`
	if !jsonEqual(t, prov, json.RawMessage(want)) {
		t.Errorf("provider = %s\n want    = %s", prov, want)
	}
}

// TestOpenRouterRequireParametersOverride proves an explicit RequireParameters is
// preserved (NOT clobbered by the default-true rule).
func TestOpenRouterRequireParametersOverride(t *testing.T) {
	got := captureWire(t, func(base string) *OpenAI {
		return newOpenRouterAt(base, "x/y", WithOpenRouterProvider(&OpenRouterProvider{
			RequireParameters: boolPtr(false),
		}))
	}, false)
	prov := topLevel(t, got.body, "provider")
	if !jsonEqual(t, prov, json.RawMessage(`{"require_parameters":false}`)) {
		t.Errorf("provider = %s, want require_parameters:false preserved", prov)
	}
}

// TestOpenRouterDefaultNotInjectedWithoutProvider proves require_parameters is NOT
// auto-injected when only OTHER extras (e.g. models[]) are configured — the
// default lives inside the provider object, never as a free-floating top-level key.
func TestOpenRouterDefaultNotInjectedWithoutProvider(t *testing.T) {
	got := captureWire(t, func(base string) *OpenAI {
		return newOpenRouterAt(base, "x/y", WithOpenRouterModels("a/b", "c/d"))
	}, false)
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(got.body), &m); err != nil {
		t.Fatalf("decode %q: %v", got.body, err)
	}
	if _, ok := m["provider"]; ok {
		t.Errorf("provider object emitted with no provider configured: %s", got.body)
	}
	if _, ok := m["require_parameters"]; ok {
		t.Errorf("require_parameters leaked to top level: %s", got.body)
	}
	models := topLevel(t, got.body, "models")
	if !jsonEqual(t, models, json.RawMessage(`["a/b","c/d"]`)) {
		t.Errorf("models = %s, want [\"a/b\",\"c/d\"]", models)
	}
}

// TestOpenRouterExtrasSerialize is the per-extra table covering reasoning,
// transforms, and plugins as top-level keys.
func TestOpenRouterExtrasSerialize(t *testing.T) {
	cases := []struct {
		name    string
		opt     Option
		key     string
		wantRaw json.RawMessage
	}{
		{
			name:    "reasoning",
			opt:     WithOpenRouterReasoning(&OpenRouterReasoning{Effort: "high", Exclude: boolPtr(true)}),
			key:     "reasoning",
			wantRaw: json.RawMessage(`{"effort":"high","exclude":true}`),
		},
		{
			name:    "transforms",
			opt:     WithOpenRouterTransforms("middle-out"),
			key:     "transforms",
			wantRaw: json.RawMessage(`["middle-out"]`),
		},
		{
			name:    "plugins",
			opt:     WithOpenRouterPlugins(OpenRouterPlugin{ID: "web", MaxResults: 3, Engine: "exa"}),
			key:     "plugins",
			wantRaw: json.RawMessage(`[{"id":"web","max_results":3,"engine":"exa"}]`),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := captureWire(t, func(base string) *OpenAI {
				return newOpenRouterAt(base, "x/y", c.opt)
			}, false)
			raw := topLevel(t, got.body, c.key)
			if !jsonEqual(t, raw, c.wantRaw) {
				t.Errorf("%s = %s, want %s (body %s)", c.key, raw, c.wantRaw, got.body)
			}
		})
	}
}

// TestOpenRouterExtrasSharedAcrossCompleteAndStream proves the extras ride BOTH
// paths (Complete + Stream share newRequest), and max_tokens stays correctly the
// second key even with the promoted extras present.
func TestOpenRouterExtrasSharedAcrossCompleteAndStream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		got := captureWire(t, func(base string) *OpenAI {
			return newOpenRouterAt(base, "x/y", WithOpenRouterModels("a/b"))
		}, stream)
		models := topLevel(t, got.body, "models")
		if !jsonEqual(t, models, json.RawMessage(`["a/b"]`)) {
			t.Errorf("stream=%v models = %s, want [\"a/b\"]", stream, models)
		}
		// model is first, max_tokens second — the T04 splice survives the embed.
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(got.body), &m); err != nil {
			t.Fatalf("decode %q: %v", got.body, err)
		}
		if string(m["max_tokens"]) != "100" {
			t.Errorf("stream=%v max_tokens = %s, want 100 (T04 splice intact with extras): %s", stream, m["max_tokens"], got.body)
		}
	}
}

// TestOpenRouterAttributionHeaders proves the attribution headers are set on the
// OpenRouter base when configured, carry the operator strings verbatim, and NEVER
// carry the API key (invariant I3).
func TestOpenRouterAttributionHeaders(t *testing.T) {
	got := captureWire(t, func(base string) *OpenAI {
		return newOpenRouterAt(base, "x/y", WithOpenRouterAttribution("https://app.example", "NilCore"))
	}, false)
	if got.referer != "https://app.example" {
		t.Errorf("HTTP-Referer = %q, want https://app.example", got.referer)
	}
	if got.title != "NilCore" {
		t.Errorf("X-Title = %q, want NilCore", got.title)
	}
	// The key must never appear in either attribution header.
	if got.referer == "k" || got.title == "k" {
		t.Fatalf("API key leaked into an attribution header (I3 violation)")
	}
	for name, vals := range got.allHdrs {
		if name == "Authorization" {
			continue
		}
		for _, v := range vals {
			if v == "k" || v == "Bearer k" {
				t.Errorf("key leaked into header %q = %q", name, v)
			}
		}
	}
	// And the body is still the bare baseline (attribution touches headers only).
	const baseline = `{"model":"x/y","max_tokens":100,"messages":[{"role":"system","content":"sys"},{"role":"user","content":"go"}]}`
	if got.body != baseline {
		t.Errorf("attribution altered the body:\n got: %s\n base:%s", got.body, baseline)
	}
}

// TestOpenRouterAttributionPartial proves each header is independent: a referer
// alone sets only HTTP-Referer, a title alone sets only X-Title.
func TestOpenRouterAttributionPartial(t *testing.T) {
	refOnly := captureWire(t, func(base string) *OpenAI {
		return newOpenRouterAt(base, "x/y", WithOpenRouterAttribution("https://r.example", ""))
	}, false)
	if refOnly.referer != "https://r.example" || refOnly.title != "" {
		t.Errorf("referer-only: referer=%q title=%q, want referer set, title empty", refOnly.referer, refOnly.title)
	}

	titleOnly := captureWire(t, func(base string) *OpenAI {
		return newOpenRouterAt(base, "x/y", WithOpenRouterAttribution("", "JustTitle"))
	}, false)
	if titleOnly.referer != "" || titleOnly.title != "JustTitle" {
		t.Errorf("title-only: referer=%q title=%q, want referer empty, title set", titleOnly.referer, titleOnly.title)
	}
}

// TestHostIsOpenRouter proves the compat-base inference: a base whose host is
// openrouter.ai (or a subdomain) flips isOpenRouter, while look-alikes do not.
func TestHostIsOpenRouter(t *testing.T) {
	cases := []struct {
		base string
		want bool
	}{
		{"https://openrouter.ai/api/v1", true},
		{"https://OPENROUTER.ai/api/v1", true}, // Hostname lowercases
		{"https://eu.openrouter.ai/api/v1", true},
		{"https://api.openai.com/v1", false},
		{"https://openrouter.ai.evil.example/api/v1", false},
		{"https://notopenrouter.ai/api/v1", false},
		{"://malformed", false},
	}
	for _, c := range cases {
		if got := hostIsOpenRouter(c.base); got != c.want {
			t.Errorf("hostIsOpenRouter(%q) = %v, want %v", c.base, got, c.want)
		}
	}
}

// TestCompatBaseInfersOpenRouter proves an operator-typed compat base pointed at
// openrouter.ai gates the extras (isOpenRouter true via the host check) — but a
// bare such adapter still stays byte-identical (extras only when configured).
func TestCompatBaseInfersOpenRouter(t *testing.T) {
	o := NewOpenAICompatible("x/y", WithKey("k"), WithBaseURL("https://openrouter.ai/api/v1"))
	if !o.isOpenRouter {
		t.Fatalf("compat base openrouter.ai did not infer isOpenRouter")
	}
	// Now with extras configured the provider object appears.
	got := captureWire(t, func(base string) *OpenAI {
		// Build with the real OpenRouter host first to set the flag, then retarget.
		oo := NewOpenAICompatible("x/y", WithKey("k"), WithBaseURL("https://openrouter.ai/api/v1"),
			WithOpenRouterModels("a/b"))
		oo.baseURL = base
		return oo
	}, false)
	models := topLevel(t, got.body, "models")
	if !jsonEqual(t, models, json.RawMessage(`["a/b"]`)) {
		t.Errorf("compat-inferred OpenRouter did not emit models: %s", got.body)
	}
}

// topLevel decodes a body and returns the raw JSON under a top-level key, failing
// if absent.
func topLevel(t *testing.T, body, key string) json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	raw, ok := m[key]
	if !ok {
		t.Fatalf("body missing top-level key %q: %s", key, body)
	}
	return raw
}
