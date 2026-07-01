package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nilcore/internal/model"
)

// cannedSSE is a complete Messages streaming exchange: a streamed text block
// ("Hel" + "lo") followed by a tool_use block whose JSON args arrive in two
// fragments, with usage seeded on message_start and finalized on message_delta.
const cannedSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"usage":{"input_tokens":11,"output_tokens":1}}}` + "\n" +
	"\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}` + "\n" +
	"\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}` + "\n" +
	"\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}` + "\n" +
	"\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n" +
	"\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"run"}}` + "\n" +
	"\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":"}}` + "\n" +
	"\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}` + "\n" +
	"\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":1}` + "\n" +
	"\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}` + "\n" +
	"\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n" +
	"\n"

// TestAnthropicStream parses a canned SSE byte stream served over httptest and
// asserts the assembled Response (text + tool_use + usage + stop_reason) matches
// what Complete would return, with onChunk receiving each text delta in order.
// TestAnthropicCompleteWebSearchResult proves the non-streaming decode no longer
// crashes on a web_search_tool_result block (whose "content" is an ARRAY) — the
// exact shape native web search returns. Before the tolerant decode this failed the
// whole turn with "cannot unmarshal array ... into ... string". The server-tool
// blocks are dropped; the model's text answer survives.
func TestAnthropicCompleteWebSearchResult(t *testing.T) {
	const body = `{
	  "content": [
	    {"type":"text","text":"Go 1.25 is out."},
	    {"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"go release"}},
	    {"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[
	       {"type":"web_search_result","title":"Go","url":"https://go.dev","encrypted_content":"abc"}
	    ]}
	  ],
	  "stop_reason":"end_turn",
	  "usage":{"input_tokens":12,"output_tokens":7}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	a := NewAnthropic("k", "claude-x")
	a.baseURL = srv.URL

	resp, err := a.Complete(context.Background(), "sys",
		[]model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: "go"}}}}, nil, 100)
	if err != nil {
		t.Fatalf("Complete on a web-search response must not error: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "text" || resp.Content[0].Text != "Go 1.25 is out." {
		t.Fatalf("expected the single text block to survive, got %+v", resp.Content)
	}
	if resp.StopReason != "end_turn" || resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 7 {
		t.Errorf("stop/usage = %q %+v", resp.StopReason, resp.Usage)
	}
}

// TestAnthropicCompleteTypedAPIError proves a non-2xx response yields a typed
// *model.APIError (not a plain fmt.Errorf), so the resilience wrapper can fast-fail a
// terminal 401 and honor a 429 Retry-After. Before this, both were returned as plain
// errors and the typed fast-fail/backoff machinery was dead.
func TestAnthropicCompleteTypedAPIError(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		retryAfter    string
		wantRetryable bool
		wantAfter     time.Duration
	}{
		{"terminal 401", 401, "", false, 0},
		{"rate limited 429", 429, "7", true, 7 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)
			}))
			defer srv.Close()
			a := NewAnthropic("k", "claude-x")
			a.baseURL = srv.URL
			_, err := a.Complete(context.Background(), "sys",
				[]model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: "go"}}}}, nil, 100)
			var apiErr *model.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("want *model.APIError, got %T: %v", err, err)
			}
			if apiErr.StatusCode != tc.status || apiErr.Retryable != tc.wantRetryable || apiErr.RetryAfter != tc.wantAfter {
				t.Errorf("APIError = %+v, want status=%d retryable=%v after=%v", apiErr, tc.status, tc.wantRetryable, tc.wantAfter)
			}
			if strings.Contains(apiErr.Error(), "k") && strings.Contains(apiErr.Error(), "api-key") {
				t.Error("APIError must not leak the key/header (I3)")
			}
		})
	}
}

// TestAnthropicCachedTokens proves the adapter now decodes Anthropic's
// prompt-cache tallies: cache_read_input_tokens maps to model.Usage.CachedTokens
// on both the non-stream and stream paths (parity with the OpenAI adapter, which a
// budget/meter reading CachedTokens relies on). A response without the cache fields
// leaves CachedTokens zero (byte-identical to before).
func TestAnthropicCachedTokens(t *testing.T) {
	t.Run("non-stream", func(t *testing.T) {
		const body = `{
			"content":[{"type":"text","text":"hi"}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":12,"output_tokens":7,"cache_read_input_tokens":90,"cache_creation_input_tokens":5}
		}`
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("content-type", "application/json")
			_, _ = io.WriteString(w, body)
		}))
		defer srv.Close()
		a := NewAnthropic("k", "claude-x")
		a.baseURL = srv.URL
		resp, err := a.Complete(context.Background(), "sys",
			[]model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: "go"}}}}, nil, 100)
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if resp.Usage.CachedTokens != 90 {
			t.Errorf("CachedTokens = %d, want 90 (cache_read_input_tokens)", resp.Usage.CachedTokens)
		}
		if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 7 {
			t.Errorf("base usage = %+v, want input 12 output 7", resp.Usage)
		}
	})

	t.Run("non-stream-absent", func(t *testing.T) {
		const body = `{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":7}}`
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("content-type", "application/json")
			_, _ = io.WriteString(w, body)
		}))
		defer srv.Close()
		a := NewAnthropic("k", "claude-x")
		a.baseURL = srv.URL
		resp, err := a.Complete(context.Background(), "sys",
			[]model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: "go"}}}}, nil, 100)
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if resp.Usage.CachedTokens != 0 {
			t.Errorf("CachedTokens = %d, want 0 (no cache fields ⇒ prior shape)", resp.Usage.CachedTokens)
		}
	})

	t.Run("stream", func(t *testing.T) {
		// cache_read_input_tokens rides message_start's usage; assert it survives to
		// the assembled Response alongside the message_delta output-token update.
		frames := "event: message_start\n" +
			`data: {"type":"message_start","message":{"usage":{"input_tokens":11,"output_tokens":1,"cache_read_input_tokens":77}}}` + "\n\n" +
			"event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
			"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}` + "\n\n" +
			"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n"

		resp, err := assembleAnthropicStream(context.Background(), strings.NewReader(frames), nil)
		if err != nil {
			t.Fatalf("assembleAnthropicStream: %v", err)
		}
		if resp.Usage.CachedTokens != 77 {
			t.Errorf("stream CachedTokens = %d, want 77", resp.Usage.CachedTokens)
		}
		if resp.Usage.OutputTokens != 9 {
			t.Errorf("stream OutputTokens = %d, want 9 (message_delta)", resp.Usage.OutputTokens)
		}
	})
}

func TestAnthropicStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "k" {
			t.Errorf("missing api key header")
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Errorf("missing anthropic-version header")
		}
		// The streamed request body must set stream:true but otherwise match
		// the same request Complete sends.
		var req anthropicRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !req.Stream {
			t.Errorf("request stream flag = %v, want true", req.Stream)
		}
		if req.Model != "claude-x" || len(req.Messages) != 1 || len(req.Tools) != 1 {
			t.Errorf("request = model %q msgs %d tools %d", req.Model, len(req.Messages), len(req.Tools))
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, cannedSSE)
	}))
	defer srv.Close()

	a := NewAnthropic("k", "claude-x")
	a.baseURL = srv.URL

	var deltas []string
	resp, err := a.Stream(context.Background(), "sys",
		[]model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: "go"}}}},
		[]model.Tool{{Name: "run", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		100, func(c model.Chunk) { deltas = append(deltas, c.Text) })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// onChunk saw each text delta, in order, and only text deltas.
	if got := strings.Join(deltas, "|"); got != "Hel|lo" {
		t.Errorf("onChunk deltas = %q, want \"Hel|lo\"", got)
	}

	// Assembled content: one text block then one complete tool_use block.
	if len(resp.Content) != 2 {
		t.Fatalf("content len = %d, want 2: %+v", len(resp.Content), resp.Content)
	}
	if resp.Content[0].Type != "text" || resp.Content[0].Text != "Hello" {
		t.Errorf("text block = %+v", resp.Content[0])
	}
	tu := resp.Content[1]
	if tu.Type != "tool_use" || tu.ID != "t1" || tu.Name != "run" {
		t.Errorf("tool_use block = %+v", tu)
	}
	if string(tu.Input) != `{"cmd":"ls"}` {
		t.Errorf("tool_use input = %s, want {\"cmd\":\"ls\"}", tu.Input)
	}

	// Concatenated text deltas equal the output text in the Response (contract).
	if strings.Join(deltas, "") != resp.Content[0].Text {
		t.Errorf("delta concat %q != response text %q", strings.Join(deltas, ""), resp.Content[0].Text)
	}

	if resp.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", resp.StopReason)
	}
	if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 9 {
		t.Errorf("usage = %+v, want {11 9}", resp.Usage)
	}
}

// TestAnthropicStreamNilOnChunk proves a nil callback is a no-op and the same
// Response is still assembled.
func TestAnthropicStreamNilOnChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, cannedSSE)
	}))
	defer srv.Close()

	a := NewAnthropic("k", "claude-x")
	a.baseURL = srv.URL
	resp, err := a.Stream(context.Background(), "", nil, nil, 100, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(resp.Content) != 2 || resp.Content[0].Text != "Hello" {
		t.Fatalf("content = %+v", resp.Content)
	}
}

// framePacedReader hands the SSE byte stream to the consumer one pre-split chunk
// at a time, one chunk per Read call. Before delivering the chunk at index
// cancelAt it cancels the request context and returns that chunk's bytes — so the
// read loop processes everything up to the cut, then observes the cancelled ctx
// on its next top-of-loop check. This deterministically reproduces a network
// stream interrupted mid-flight with no timing races (safe under -race).
type framePacedReader struct {
	chunks   []string
	cancelAt int // index of the chunk whose Read cancels ctx
	cancel   context.CancelFunc
	mu       sync.Mutex
	i        int
}

func (f *framePacedReader) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.i >= len(f.chunks) {
		return 0, io.EOF
	}
	if f.i == f.cancelAt {
		f.cancel()
	}
	n := copy(p, f.chunks[f.i])
	f.i++
	return n, nil
}

// TestAnthropicStreamCancelMidStream cancels ctx partway through the SSE stream
// and asserts Stream returns the PARTIAL text Response assembled so far together
// with the context error (interrupt-but-preserve) — never an empty Response and
// never a nil error.
func TestAnthropicStreamCancelMidStream(t *testing.T) {
	// Each chunk is a full SSE frame (terminated by a blank line) so the scanner
	// yields complete lines from each Read. The stream is two text deltas across
	// one block; we cancel on the Read that would deliver the SECOND delta, so the
	// partial Response must hold "Hel" but not "lo".
	chunks := []string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":3,"output_tokens":1}}}` + "\n\n",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}` + "\n\n",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}` + "\n\n",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}` + "\n\n",
		`data: {"type":"message_stop"}` + "\n\n",
	}

	ctx, cancel := context.WithCancel(context.Background())
	rdr := &framePacedReader{chunks: chunks, cancelAt: 3, cancel: cancel} // cancel on the "lo" Read

	var deltas []string
	resp, err := assembleAnthropicStream(ctx, rdr, func(c model.Chunk) { deltas = append(deltas, c.Text) })

	// The error MUST be the context error — interrupt is signaled to the caller.
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// The Response MUST be the best-effort partial, NOT empty: "Hel" survives.
	if len(resp.Content) != 1 || resp.Content[0].Type != "text" {
		t.Fatalf("partial content = %+v, want one text block", resp.Content)
	}
	if resp.Content[0].Text != "Hel" {
		t.Errorf("partial text = %q, want \"Hel\" (the second delta must be cut off)", resp.Content[0].Text)
	}
	// onChunk forwarded exactly the deltas that were decoded before the cut.
	if got := strings.Join(deltas, "|"); got != "Hel" {
		t.Errorf("forwarded deltas = %q, want \"Hel\"", got)
	}
}

// TestAnthropicStreamCancelBeforeBytes proves that when ctx is already cancelled
// so the HTTP round-trip itself fails before any byte arrives, Stream surfaces
// the context error (not a transport-wrapped error).
func TestAnthropicStreamCancelBeforeBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, cannedSSE)
	}))
	defer srv.Close()

	a := NewAnthropic("k", "claude-x")
	a.baseURL = srv.URL

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the call

	_, err := a.Stream(ctx, "sys", nil, nil, 100, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestAnthropicStreamHTTPError proves a non-2xx response is surfaced as a
// non-context error with the body tail, mirroring Complete.
func TestAnthropicStreamHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"bad"}}`)
	}))
	defer srv.Close()

	a := NewAnthropic("k", "claude-x")
	a.baseURL = srv.URL
	_, err := a.Stream(context.Background(), "sys", nil, nil, 100, nil)
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("400 should not be a context error: %v", err)
	}
	var apiErr *model.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest || apiErr.Retryable {
		t.Errorf("err = %v, want a terminal (non-retryable) *model.APIError with status 400", err)
	}
}

// streamerCheck is a compile-time assertion that *Anthropic satisfies the
// optional model.Streamer interface (additive to Provider, invariant I1).
var _ model.Streamer = (*Anthropic)(nil)
