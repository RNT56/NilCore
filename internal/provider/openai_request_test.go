package provider

import (
	"encoding/json"
	"sort"
	"testing"
)

// P15-T05 — the SOTA Chat-Completions request fields. These tests pin two
// guarantees: (1) with NONE of the new options configured the marshalled body
// is byte-identical to the pre-T05 shape (no new keys leak in), and (2) each new
// field serializes correctly when set, including the meaningful false case for
// parallel_tool_calls.

// TestSOTAFieldsUnsetByteIdentical proves that with no SOTA option configured
// the full widened oaiRequest marshals to EXACTLY the baseline body — the new
// omitempty fields contribute zero bytes. This extends the T04 byte-identity
// baseline to the widened struct.
func TestSOTAFieldsUnsetByteIdentical(t *testing.T) {
	o := NewOpenAICompatible("gpt-x", WithKey("k")) // no SOTA options
	got := captureBody(t, o, 100, false)

	// Same baseline as TestMaxTokensDefaultByteIdentical: model, max_tokens,
	// messages — and NOTHING from the SOTA widening.
	const baseline = `{"model":"gpt-x","max_tokens":100,"messages":[{"role":"system","content":"sys"},{"role":"user","content":"go"}]}`
	if got != baseline {
		t.Errorf("widened body not byte-identical to baseline:\n got:      %s\n baseline: %s", got, baseline)
	}

	// Belt-and-suspenders: assert no SOTA key appears at the top level.
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("decode body %q: %v", got, err)
	}
	for _, k := range []string{
		"reasoning_effort", "response_format", "parallel_tool_calls",
		"tool_choice", "service_tier", "prompt_cache_key",
	} {
		if _, ok := m[k]; ok {
			t.Errorf("unset SOTA field leaked key %q into body: %s", k, got)
		}
	}
}

// TestSOTAFieldsUnsetByteIdenticalStream proves the same no-leak guarantee on
// the Stream path (Complete and Stream share newRequest), allowing for the
// stream-only keys (stream / stream_options) that have always been present.
func TestSOTAFieldsUnsetByteIdenticalStream(t *testing.T) {
	o := NewOpenAICompatible("gpt-x", WithKey("k"))
	got := captureBody(t, o, 100, true)

	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("decode stream body %q: %v", got, err)
	}
	// The only keys ever present on the stream path are these — no SOTA key.
	allowed := map[string]bool{
		"model": true, "max_tokens": true, "messages": true,
		"stream": true, "stream_options": true,
	}
	var leaked []string
	for k := range m {
		if !allowed[k] {
			leaked = append(leaked, k)
		}
	}
	sort.Strings(leaked)
	if len(leaked) != 0 {
		t.Errorf("stream body carries unexpected keys %v (SOTA leak): %s", leaked, got)
	}
}

// TestSOTAFieldsSerialize is the per-field table: each option, when set,
// produces the expected top-level key with the expected JSON value. The two
// parallel_tool_calls cases (true AND false) prove the pointer carries the
// meaningful false distinctly from unset.
func TestSOTAFieldsSerialize(t *testing.T) {
	cases := []struct {
		name    string
		opt     Option
		key     string          // top-level key expected in the body
		wantRaw json.RawMessage // exact JSON value expected under that key
	}{
		{
			name:    "reasoning_effort",
			opt:     WithReasoningEffort("high"),
			key:     "reasoning_effort",
			wantRaw: json.RawMessage(`"high"`),
		},
		{
			name:    "response_format-json-schema",
			opt:     WithResponseFormat("answer", true, json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`)),
			key:     "response_format",
			wantRaw: json.RawMessage(`{"type":"json_schema","json_schema":{"name":"answer","strict":true,"schema":{"type":"object","properties":{"x":{"type":"string"}}}}}`),
		},
		{
			name:    "parallel_tool_calls-true",
			opt:     WithParallelToolCalls(true),
			key:     "parallel_tool_calls",
			wantRaw: json.RawMessage(`true`),
		},
		{
			name:    "parallel_tool_calls-false",
			opt:     WithParallelToolCalls(false),
			key:     "parallel_tool_calls",
			wantRaw: json.RawMessage(`false`),
		},
		{
			name:    "tool_choice-string",
			opt:     WithToolChoice(json.RawMessage(`"required"`)),
			key:     "tool_choice",
			wantRaw: json.RawMessage(`"required"`),
		},
		{
			name:    "tool_choice-object",
			opt:     WithToolChoice(json.RawMessage(`{"type":"function","function":{"name":"run"}}`)),
			key:     "tool_choice",
			wantRaw: json.RawMessage(`{"type":"function","function":{"name":"run"}}`),
		},
		{
			name:    "service_tier",
			opt:     WithServiceTier("priority"),
			key:     "service_tier",
			wantRaw: json.RawMessage(`"priority"`),
		},
		{
			name:    "prompt_cache_key",
			opt:     WithPromptCacheKey("session-42"),
			key:     "prompt_cache_key",
			wantRaw: json.RawMessage(`"session-42"`),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := NewOpenAICompatible("gpt-x", WithKey("k"), c.opt)
			body := captureBody(t, o, 100, false)

			var m map[string]json.RawMessage
			if err := json.Unmarshal([]byte(body), &m); err != nil {
				t.Fatalf("decode body %q: %v", body, err)
			}
			raw, ok := m[c.key]
			if !ok {
				t.Fatalf("body missing key %q: %s", c.key, body)
			}
			if !jsonEqual(t, raw, c.wantRaw) {
				t.Errorf("key %q = %s, want %s (full body %s)", c.key, raw, c.wantRaw, body)
			}
		})
	}
}

// TestSOTAFieldsSharedAcrossCompleteAndStream proves a configured field is set
// in newRequest and therefore appears on BOTH the Complete and Stream bodies
// (they share newRequest). reasoning_effort stands in for the whole set.
func TestSOTAFieldsSharedAcrossCompleteAndStream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		o := NewOpenAICompatible("gpt-x", WithKey("k"),
			WithReasoningEffort("low"), WithServiceTier("flex"))
		body := captureBody(t, o, 100, stream)

		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatalf("stream=%v decode %q: %v", stream, body, err)
		}
		if string(m["reasoning_effort"]) != `"low"` {
			t.Errorf("stream=%v reasoning_effort = %s, want \"low\"", stream, m["reasoning_effort"])
		}
		if string(m["service_tier"]) != `"flex"` {
			t.Errorf("stream=%v service_tier = %s, want \"flex\"", stream, m["service_tier"])
		}
	}
}

// jsonEqual reports whether two raw JSON values are semantically equal
// (key order / whitespace insensitive), by normalizing both through Unmarshal.
func jsonEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("decode %q: %v", a, err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("decode %q: %v", b, err)
	}
	an, err := json.Marshal(av)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	bn, err := json.Marshal(bv)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	return string(an) == string(bn)
}
