package provider

// fixreview_test.go covers the provider-side defect-review fixes:
//   - #2  a zero-frame clean-EOF Anthropic stream is a retryable stream_truncated error
//   - #4  a server-tool-only (pause_turn) turn is preserved as a safe non-empty marker
//   - LOW anthropic-beta collects ALL beta-carrying tools (deduped), not just the first
//   - LOW OpenRouter web plugin is deduped and appended via a defensive copy
//   - LOW an errored tool_result carries a failure signal into OpenAI's role:"tool"
//   - LOW the token-cap splice survives a model id containing a comma

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nilcore/internal/model"
)

// --- #2: zero-frame clean EOF is retryable -----------------------------------

func TestAnthropicStreamZeroFrameEOFIsRetryable(t *testing.T) {
	// A 200 OK whose body carries NO SSE data frames (a broken connection dressed up
	// as success) must surface a RETRYABLE stream_truncated error — matching the OpenAI
	// assembler — not a silent empty Response the native loop would append as a poisoned
	// assistant turn (which would 400 the NEXT request).
	cases := map[string]string{
		"empty body":         "",
		"comments only":      ": keep-alive\n\n: still alive\n\n",
		"message_start only": "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\n",
	}
	for name, frames := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := assembleAnthropicStream(context.Background(), strings.NewReader(frames), nil)
			var apiErr *model.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v, want a *model.APIError", err)
			}
			if !apiErr.Retryable {
				t.Errorf("Retryable = %v, want true (a broken stream must retry/fail over)", apiErr.Retryable)
			}
			if apiErr.StatusCode != 502 {
				t.Errorf("StatusCode = %d, want 502", apiErr.StatusCode)
			}
		})
	}
}

func TestAnthropicStreamPartialContentEOFStillSalvages(t *testing.T) {
	// The positive control: a stream that DID produce content but was cut before
	// message_stop must still return a clean (nil-error) partial, so the native loop's
	// truncation salvage can act on it. Only a zero-content EOF errors.
	frames := "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}` + "\n\n"
	resp, err := assembleAnthropicStream(context.Background(), strings.NewReader(frames), nil)
	if err != nil {
		t.Fatalf("partial-content EOF must NOT error: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "partial" {
		t.Errorf("Content = %+v, want the salvaged partial text", resp.Content)
	}
}

// --- #4: server-tool-only (pause_turn) turn preserved ------------------------

func TestAnthropicServerToolOnlyTurnPreservedNonStream(t *testing.T) {
	// A pause_turn whose ONLY blocks are server_tool_use / web_search_tool_result (no
	// assistant text) must NOT decode to empty content — that trips the native loop's
	// empty-turn poison. It is preserved as a fixed, non-executable marker carrying NONE
	// of the untrusted search body (I7).
	const body = `{
	  "content": [
	    {"type":"server_tool_use","id":"srv1","name":"web_search","input":{"query":"go release"}},
	    {"type":"web_search_tool_result","tool_use_id":"srv1","content":[
	       {"type":"web_search_result","title":"Go","url":"https://go.dev","encrypted_content":"SECRET-BODY"}
	    ]}
	  ],
	  "stop_reason":"pause_turn",
	  "usage":{"input_tokens":3,"output_tokens":0}
	}`
	var ar anthropicResponse
	if err := json.Unmarshal([]byte(body), &ar); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp := ar.toModel()
	if len(resp.Content) == 0 {
		t.Fatal("server-tool-only turn decoded to EMPTY content (would poison the loop history)")
	}
	if resp.Content[0].Type != "text" {
		t.Fatalf("marker must be a plain (non-executable) text block, got %+v", resp.Content)
	}
	if strings.Contains(resp.Content[0].Text, "SECRET-BODY") || strings.Contains(resp.Content[0].Text, "go.dev") {
		t.Errorf("marker must NOT carry untrusted search content (I7): %q", resp.Content[0].Text)
	}
	if resp.StopReason != "pause_turn" {
		t.Errorf("StopReason = %q, want pause_turn passed through", resp.StopReason)
	}
}

func TestAnthropicServerToolOnlyTurnPreservedStream(t *testing.T) {
	frames := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":3,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srv1","name":"web_search"}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"web_search_tool_result","id":"wsr1"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"pause_turn"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	resp, err := assembleAnthropicStream(context.Background(), strings.NewReader(frames), nil)
	if err != nil {
		t.Fatalf("assembleAnthropicStream: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "text" {
		t.Fatalf("streamed server-tool-only turn must decode to a single text marker, got %+v", resp.Content)
	}
	if resp.StopReason != "pause_turn" {
		t.Errorf("StopReason = %q, want pause_turn", resp.StopReason)
	}
}

func TestAnthropicServerToolWithTextKeepsTextNoMarker(t *testing.T) {
	// Control: when the model's OWN text answer is present, the server-tool blocks are
	// dropped and NO marker is added — byte-identical to the normal web-search path.
	const body = `{
	  "content": [
	    {"type":"text","text":"Go 1.25 is out."},
	    {"type":"server_tool_use","id":"srv1","name":"web_search","input":{"query":"x"}}
	  ],
	  "stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}
	}`
	var ar anthropicResponse
	if err := json.Unmarshal([]byte(body), &ar); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp := ar.toModel()
	if len(resp.Content) != 1 || resp.Content[0].Type != "text" || resp.Content[0].Text != "Go 1.25 is out." {
		t.Errorf("want only the model's text (no marker), got %+v", resp.Content)
	}
}

// --- LOW: anthropic-beta collects all betas ----------------------------------

func TestAnthropicCollectsAllBetaHeadersDeduped(t *testing.T) {
	var gotBeta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()
	a := NewAnthropic("k", "claude-x")
	a.baseURL = srv.URL
	tools := []model.Tool{
		model.NewComputerTool(800, 600), // Beta: computer-use-2025-11-24
		{Name: "extra", Builtin: &model.BuiltinTool{Type: "t", Name: "extra", Beta: "beta-two"}},
		{Name: "dup", Builtin: &model.BuiltinTool{Type: "t2", Name: "dup", Beta: model.ComputerBeta20251124}}, // duplicate beta
	}
	if _, err := a.Complete(context.Background(), "",
		[]model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: "go"}}}}, tools, 100); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !strings.Contains(gotBeta, model.ComputerBeta20251124) || !strings.Contains(gotBeta, "beta-two") {
		t.Errorf("anthropic-beta = %q, want BOTH betas (not just the first)", gotBeta)
	}
	if n := strings.Count(gotBeta, model.ComputerBeta20251124); n != 1 {
		t.Errorf("anthropic-beta = %q, want the duplicate beta deduped (count=%d)", gotBeta, n)
	}
}

// --- LOW: OpenRouter web plugin dedupe + defensive copy ----------------------

func TestOpenRouterWebPluginDeduped(t *testing.T) {
	// Operator already configured a `web` plugin AND native web search is on: the body
	// must carry exactly ONE web plugin (dedup by id), keeping the operator's richer one.
	o := NewOpenAICompatible("x/y",
		WithKey("k"),
		WithBaseURL("https://openrouter.ai/api/v1"),
		WithOpenRouterPlugins(OpenRouterPlugin{ID: "web", MaxResults: 3}),
	)
	body := captureBodyTools(t, o, []model.Tool{model.NewWebSearchTool(0)})
	if n := strings.Count(body, `"id":"web"`); n != 1 {
		t.Fatalf("web plugin count = %d, want exactly 1 (deduped): %s", n, body)
	}
	if !strings.Contains(body, `"max_results":3`) {
		t.Errorf("the operator's configured web plugin should be kept: %s", body)
	}
}

func TestOpenRouterWebPluginAppendDefensiveCopy(t *testing.T) {
	// The operator's plugin slice has SPARE CAPACITY, so a naive in-place append of the
	// `web` plugin would corrupt their backing array for later requests. Prove the append
	// goes into a fresh copy: the operator's array index 1 stays zero after the request.
	base := make([]OpenRouterPlugin, 1, 2)
	base[0] = OpenRouterPlugin{ID: "file-parser"}
	o := NewOpenAICompatible("x/y",
		WithKey("k"),
		WithBaseURL("https://openrouter.ai/api/v1"),
		WithOpenRouterPlugins(base...), // aliases base (len 1, cap 2)
	)
	body := captureBodyTools(t, o, []model.Tool{model.NewWebSearchTool(0)})
	// The request carries both plugins...
	if !strings.Contains(body, `"id":"file-parser"`) || !strings.Contains(body, `"id":"web"`) {
		t.Fatalf("body should carry both the operator plugin and the web plugin: %s", body)
	}
	// ...but the operator's shared backing array was NOT mutated (index 1 still zero).
	if got := base[:2][1].ID; got != "" {
		t.Errorf("operator's plugin backing array was corrupted by an in-place append: index 1 = %q, want empty", got)
	}
	// A second request still produces exactly one web plugin (no accumulation).
	if n := strings.Count(captureBodyTools(t, o, []model.Tool{model.NewWebSearchTool(0)}), `"id":"web"`); n != 1 {
		t.Errorf("second request web plugin count = %d, want 1", n)
	}
}

// --- LOW: errored tool_result carries a failure signal to OpenAI -------------

func TestOpenAIToolResultCarriesErrorSignal(t *testing.T) {
	msgs := []model.Message{{Role: "user", Content: []model.Block{
		{Type: "tool_result", ToolUseID: "call_1", Content: "boom: file not found", IsError: true},
		{Type: "tool_result", ToolUseID: "call_2", Content: "ok output"},
	}}}
	out := toOpenAIMessages("", msgs)
	var errMsg, okMsg string
	for _, m := range out {
		if m.Role != "tool" {
			continue
		}
		s, _ := m.Content.(string)
		switch m.ToolCallID {
		case "call_1":
			errMsg = s
		case "call_2":
			okMsg = s
		}
	}
	if !strings.Contains(errMsg, "error") || !strings.Contains(errMsg, "boom") {
		t.Errorf("an errored tool_result must carry a failure signal into role:tool, got %q", errMsg)
	}
	if okMsg != "ok output" {
		t.Errorf("a successful tool_result must be verbatim, got %q", okMsg)
	}
}

// --- LOW: token-cap splice survives a comma in the model id ------------------

func TestOAIMaxTokensSpliceModelWithComma(t *testing.T) {
	// A model id containing a comma (e.g. a route list) must NOT corrupt the body: the
	// old first-comma splice put the token key inside the model string. The result must
	// be valid JSON with the model intact and the cap a top-level key.
	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		t.Run(field, func(t *testing.T) {
			r := oaiRequest{
				Model:          `vendor/a,vendor/b,vendor/c`,
				MaxTokens:      4096,
				maxTokensField: field,
				Messages:       []oaiMessage{{Role: "user", Content: "hi"}},
			}
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !json.Valid(b) {
				t.Fatalf("body is not valid JSON: %s", b)
			}
			var decoded struct {
				Model         string            `json:"model"`
				MaxTokens     int               `json:"max_tokens"`
				MaxCompletion int               `json:"max_completion_tokens"`
				Messages      []json.RawMessage `json:"messages"`
			}
			if err := json.Unmarshal(b, &decoded); err != nil {
				t.Fatalf("decode: %v (body %s)", err, b)
			}
			if decoded.Model != `vendor/a,vendor/b,vendor/c` {
				t.Errorf("model corrupted: %q (body %s)", decoded.Model, b)
			}
			gotCap := decoded.MaxTokens
			if field == "max_completion_tokens" {
				gotCap = decoded.MaxCompletion
			}
			if gotCap != 4096 {
				t.Errorf("%s = %d, want 4096 (body %s)", field, gotCap, b)
			}
			if len(decoded.Messages) != 1 {
				t.Errorf("messages lost in the splice: %s", b)
			}
		})
	}
}

func TestOAIMaxTokensSpliceModelWithQuoteAndComma(t *testing.T) {
	// An escaped quote inside the (unusual) model id must not fool the string scanner.
	r := oaiRequest{Model: `a",b`, MaxTokens: 100, maxTokensField: "max_tokens", Messages: []oaiMessage{{Role: "user", Content: "hi"}}}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !json.Valid(b) {
		t.Fatalf("body is not valid JSON: %s", b)
	}
	var decoded struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
	}
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("decode: %v (body %s)", err, b)
	}
	if decoded.Model != `a",b` || decoded.MaxTokens != 100 {
		t.Errorf("model/cap corrupted: %q / %d (body %s)", decoded.Model, decoded.MaxTokens, b)
	}
}
