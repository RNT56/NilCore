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

// captureBody spins an httptest server, drives one Complete (or Stream) call
// through it, and returns the raw request body the adapter put on the wire.
func captureBody(t *testing.T, o *OpenAI, maxTokens int, stream bool) string {
	t.Helper()
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		if stream {
			w.Header().Set("content-type", "text/event-stream")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	o.baseURL = srv.URL

	var err error
	if stream {
		_, err = o.Stream(context.Background(), "sys",
			[]model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: "go"}}}}, nil, maxTokens, nil)
	} else {
		_, err = o.Complete(context.Background(), "sys",
			[]model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: "go"}}}}, nil, maxTokens)
	}
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	return body
}

// keysPresent decodes a marshalled request body and reports whether each of the
// two token-cap key names is present at the top level.
func keysPresent(t *testing.T, body string) (hasMaxTokens, hasMaxCompletion bool) {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	_, hasMaxTokens = m["max_tokens"]
	_, hasMaxCompletion = m["max_completion_tokens"]
	return
}

// TestMaxTokensDefaultByteIdentical pins the default-key body against a captured
// baseline: the marshalled body must be byte-for-byte what the original
// `MaxTokens int `+"`json:\"max_tokens,omitempty\"`"+` struct produced. The baseline is the
// literal wire shape the prior code emitted for this exact input.
func TestMaxTokensDefaultByteIdentical(t *testing.T) {
	o := NewOpenAICompatible("gpt-x", WithKey("k")) // default maxTokensField "max_tokens"
	got := captureBody(t, o, 100, false)

	// Baseline: model, then max_tokens (the second field, exactly where the old
	// struct tag placed it), then messages (no omitempty, so always present).
	const baseline = `{"model":"gpt-x","max_tokens":100,"messages":[{"role":"system","content":"sys"},{"role":"user","content":"go"}]}`
	if got != baseline {
		t.Errorf("default body not byte-identical to baseline:\n got:      %s\n baseline: %s", got, baseline)
	}
}

// TestMaxTokensExactlyOneKey is the core invariant: for each configuration the
// body carries EXACTLY ONE of the two token-cap keys (never both), and it is the
// configured one.
func TestMaxTokensExactlyOneKey(t *testing.T) {
	cases := []struct {
		name          string
		field         string // "" => use default (max_tokens)
		wantMaxTokens bool
		wantMaxComp   bool
	}{
		{"default-max-tokens", "", true, false},
		{"reasoning-max-completion", "max_completion_tokens", false, true},
		{"explicit-max-tokens", "max_tokens", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := []Option{WithKey("k")}
			if c.field != "" {
				opts = append(opts, WithMaxTokensField(c.field))
			}
			o := NewOpenAICompatible("gpt-x", opts...)
			body := captureBody(t, o, 100, false)

			hasMT, hasMC := keysPresent(t, body)
			if hasMT && hasMC {
				t.Fatalf("body carries BOTH keys (rejected by reasoning models): %s", body)
			}
			if !hasMT && !hasMC {
				t.Fatalf("body carries NEITHER key, want exactly one: %s", body)
			}
			if hasMT != c.wantMaxTokens {
				t.Errorf("max_tokens present = %v, want %v (body %s)", hasMT, c.wantMaxTokens, body)
			}
			if hasMC != c.wantMaxComp {
				t.Errorf("max_completion_tokens present = %v, want %v (body %s)", hasMC, c.wantMaxComp, body)
			}
		})
	}
}

// TestMaxTokensOmittedWhenUnset proves a non-positive cap omits the key entirely
// under BOTH key configurations (mirrors the prior omitempty on a zero int).
func TestMaxTokensOmittedWhenUnset(t *testing.T) {
	for _, field := range []string{"", "max_completion_tokens"} {
		opts := []Option{WithKey("k")}
		if field != "" {
			opts = append(opts, WithMaxTokensField(field))
		}
		o := NewOpenAICompatible("gpt-x", opts...)
		for _, mt := range []int{0, -1} {
			body := captureBody(t, o, mt, false)
			if hasMT, hasMC := keysPresent(t, body); hasMT || hasMC {
				t.Errorf("field=%q maxTokens=%d: body carries a token key, want none: %s", field, mt, body)
			}
		}
	}
}

// TestMaxTokensStreamPathToo proves the same single-key behavior holds on the
// Stream path (both Complete and Stream share newRequest).
func TestMaxTokensStreamPathToo(t *testing.T) {
	o := NewOpenAICompatible("gpt-x", WithKey("k"), WithMaxTokensField("max_completion_tokens"))
	body := captureBody(t, o, 100, true)
	hasMT, hasMC := keysPresent(t, body)
	if hasMT {
		t.Errorf("stream body carries max_tokens, want only max_completion_tokens: %s", body)
	}
	if !hasMC {
		t.Errorf("stream body missing max_completion_tokens: %s", body)
	}
}

// TestOaiRequestZeroValueRoundTrip guards the byte-identity safety net: a
// zero-value oaiRequest (empty maxTokensField) must marshal with the default
// "max_tokens" key when a cap is set, never panicking on the empty field name.
func TestOaiRequestZeroValueRoundTrip(t *testing.T) {
	r := oaiRequest{Model: "m", MaxTokens: 7} // maxTokensField left "" deliberately
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	hasMT, hasMC := keysPresent(t, string(b))
	if !hasMT || hasMC {
		t.Errorf("zero-value oaiRequest body = %s, want max_tokens only", b)
	}
}
