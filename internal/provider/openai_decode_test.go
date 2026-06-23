package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nilcore/internal/model"
)

// P15-T05 — decode widening. The non-stream and stream usage paths now populate
// model.Usage.{ReasoningTokens,CachedTokens,CostUSD} from the nested OpenAI /
// OpenRouter detail fields. A response WITHOUT those nested fields must yield
// exactly the prior Usage (the detail structs are pointers ⇒ nil ⇒ zero).

// completeWithBody drives one Complete call against a server that returns the
// given raw JSON, and returns the decoded Response.
func completeWithBody(t *testing.T, raw string) model.Response {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, raw)
	}))
	defer srv.Close()

	o := NewOpenAICompatible("gpt-x", WithKey("k"), WithBaseURL(srv.URL))
	resp, err := o.Complete(context.Background(), "sys",
		[]model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: "go"}}}}, nil, 100)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return resp
}

// streamWithFrames assembles a Response from the given SSE frames via the
// public assembler (the same path Stream drives).
func streamWithFrames(t *testing.T, frames string) model.Response {
	t.Helper()
	resp, err := assembleOpenAIStream(context.Background(), strings.NewReader(frames), nil)
	if err != nil {
		t.Fatalf("assembleOpenAIStream: %v", err)
	}
	return resp
}

// TestDecodeUsageNonStream covers the non-stream usage decode: a response
// carrying the nested detail fields populates the three new Usage fields; one
// without them yields exactly the prior Usage.
func TestDecodeUsageNonStream(t *testing.T) {
	t.Run("with-details", func(t *testing.T) {
		const body = `{
			"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],
			"usage":{
				"prompt_tokens":120,
				"completion_tokens":45,
				"completion_tokens_details":{"reasoning_tokens":30},
				"prompt_tokens_details":{"cached_tokens":80},
				"cost":0.00123
			}
		}`
		got := completeWithBody(t, body).Usage
		want := model.Usage{
			InputTokens:     120,
			OutputTokens:    45,
			ReasoningTokens: 30,
			CachedTokens:    80,
			CostUSD:         0.00123,
		}
		if got != want {
			t.Errorf("usage = %+v, want %+v", got, want)
		}
	})

	t.Run("without-details-unchanged", func(t *testing.T) {
		const body = `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":45}}`
		got := completeWithBody(t, body).Usage
		// Byte-identical to the pre-widening shape: only the original two counts.
		want := model.Usage{InputTokens: 120, OutputTokens: 45}
		if got != want {
			t.Errorf("usage = %+v, want %+v (no nested details ⇒ prior shape)", got, want)
		}
		if got.ReasoningTokens != 0 || got.CachedTokens != 0 || got.CostUSD != 0 {
			t.Errorf("absent details must stay zero, got %+v", got)
		}
	})

	t.Run("partial-details", func(t *testing.T) {
		// Only reasoning details present (no prompt_tokens_details, no cost) —
		// cached + cost stay zero.
		const body = `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":20,"completion_tokens_details":{"reasoning_tokens":7}}}`
		got := completeWithBody(t, body).Usage
		want := model.Usage{InputTokens: 10, OutputTokens: 20, ReasoningTokens: 7}
		if got != want {
			t.Errorf("usage = %+v, want %+v", got, want)
		}
	})
}

// TestDecodeUsageStream covers the stream usage decode: the trailing usage-only
// frame's nested detail fields populate the new Usage fields, and a usage frame
// without them yields exactly the prior Usage.
func TestDecodeUsageStream(t *testing.T) {
	t.Run("with-details", func(t *testing.T) {
		// A content frame, then the trailing usage-only frame (empty choices),
		// then [DONE].
		frames := strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":200,"completion_tokens":60,"completion_tokens_details":{"reasoning_tokens":15},"prompt_tokens_details":{"cached_tokens":150},"cost":0.0045}}`,
			`data: [DONE]`,
			``,
		}, "\n\n") + "\n\n"

		got := streamWithFrames(t, frames).Usage
		want := model.Usage{
			InputTokens:     200,
			OutputTokens:    60,
			ReasoningTokens: 15,
			CachedTokens:    150,
			CostUSD:         0.0045,
		}
		if got != want {
			t.Errorf("stream usage = %+v, want %+v", got, want)
		}
	})

	t.Run("without-details-unchanged", func(t *testing.T) {
		frames := strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":200,"completion_tokens":60}}`,
			`data: [DONE]`,
			``,
		}, "\n\n") + "\n\n"

		got := streamWithFrames(t, frames).Usage
		want := model.Usage{InputTokens: 200, OutputTokens: 60}
		if got != want {
			t.Errorf("stream usage = %+v, want %+v (no details ⇒ prior shape)", got, want)
		}
	})
}
