package provider

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

// cannedOpenAISSE is a complete chat-completions streaming exchange: two content
// deltas ("Hel" + "lo"), a tool_call assembled across three fragments (opening
// fragment with id+name, then two argument fragments), a finish_reason on the last
// choice frame, a trailing usage-only frame (stream_options.include_usage), and
// the [DONE] sentinel.
const cannedOpenAISSE = `data: {"choices":[{"delta":{"content":"Hel"}}]}` + "\n\n" +
	`data: {"choices":[{"delta":{"content":"lo"}}]}` + "\n\n" +
	`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"run","arguments":""}}]}}]}` + "\n\n" +
	`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":"}}]}}]}` + "\n\n" +
	`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}` + "\n\n" +
	`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
	`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":9}}` + "\n\n" +
	"data: [DONE]\n\n"

// TestOpenAIStream parses a canned SSE byte stream served over httptest and
// asserts the assembled Response (text + tool_use assembled across fragments +
// usage + stop_reason from finish_reason) matches what Complete would return, with
// onChunk receiving each content delta in order.
func TestOpenAIStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "Bearer k" {
			t.Errorf("authorization = %q, want Bearer k", r.Header.Get("authorization"))
		}
		// The streamed body must set stream:true and ask for the usage frame, but
		// otherwise match the same request Complete sends.
		var req oaiRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !req.Stream {
			t.Errorf("request stream flag = %v, want true", req.Stream)
		}
		if req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
			t.Errorf("stream_options = %+v, want include_usage:true", req.StreamOptions)
		}
		if req.Model != "gpt-x" || len(req.Messages) != 2 || len(req.Tools) != 1 {
			t.Errorf("request = model %q msgs %d tools %d", req.Model, len(req.Messages), len(req.Tools))
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, cannedOpenAISSE)
	}))
	defer srv.Close()

	o := NewOpenAI("k", "gpt-x")
	o.baseURL = srv.URL

	var deltas []string
	resp, err := o.Stream(context.Background(), "sys",
		[]model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: "go"}}}},
		[]model.Tool{{Name: "run", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		100, func(c model.Chunk) { deltas = append(deltas, c.Text) })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// onChunk saw each content delta, in order, and only content deltas (the
	// tool-call argument fragments are NOT forwarded as text).
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
	if tu.Type != "tool_use" || tu.ID != "c1" || tu.Name != "run" {
		t.Errorf("tool_use block = %+v", tu)
	}
	if string(tu.Input) != `{"cmd":"ls"}` {
		t.Errorf("tool_use input = %s, want {\"cmd\":\"ls\"}", tu.Input)
	}

	// Concatenated text deltas equal the output text in the Response (contract).
	if strings.Join(deltas, "") != resp.Content[0].Text {
		t.Errorf("delta concat %q != response text %q", strings.Join(deltas, ""), resp.Content[0].Text)
	}

	// finish_reason tool_calls maps to the canonical tool_use, exactly as Complete.
	if resp.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", resp.StopReason)
	}
	if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 9 {
		t.Errorf("usage = %+v, want {11 9}", resp.Usage)
	}
}

// TestOpenAIStreamMatchesComplete proves Stream assembles the SAME Response that
// Complete returns for an equivalent non-stream reply — text + tool_use + usage +
// stop_reason are identical (Streamer contract: interchangeable delivery).
func TestOpenAIStreamMatchesComplete(t *testing.T) {
	const completeBody = `{"choices":[{"message":{"content":"Hello","tool_calls":[{"id":"c1","type":"function","function":{"name":"run","arguments":"{\"cmd\":\"ls\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":9}}`

	complSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, completeBody)
	}))
	defer complSrv.Close()
	streamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, cannedOpenAISSE)
	}))
	defer streamSrv.Close()

	cmpl := NewOpenAI("k", "gpt-x")
	cmpl.baseURL = complSrv.URL
	strm := NewOpenAI("k", "gpt-x")
	strm.baseURL = streamSrv.URL

	args := func(o *OpenAI, stream bool) (model.Response, error) {
		if stream {
			return o.Stream(context.Background(), "sys", nil, nil, 100, nil)
		}
		return o.Complete(context.Background(), "sys", nil, nil, 100)
	}

	cResp, err := args(cmpl, false)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	sResp, err := args(strm, true)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	cJSON, _ := json.Marshal(cResp)
	sJSON, _ := json.Marshal(sResp)
	if string(cJSON) != string(sJSON) {
		t.Errorf("Stream Response != Complete Response\n complete: %s\n stream:   %s", cJSON, sJSON)
	}
}

// TestOpenAIStreamNilOnChunk proves a nil callback is a no-op and the same
// Response is still assembled.
func TestOpenAIStreamNilOnChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, cannedOpenAISSE)
	}))
	defer srv.Close()

	o := NewOpenAI("k", "gpt-x")
	o.baseURL = srv.URL
	resp, err := o.Stream(context.Background(), "", nil, nil, 100, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(resp.Content) != 2 || resp.Content[0].Text != "Hello" {
		t.Fatalf("content = %+v", resp.Content)
	}
}

// TestOpenAIStreamCancelMidStream cancels ctx partway through the SSE stream and
// asserts Stream returns the PARTIAL text Response assembled so far together with
// the context error (interrupt-but-preserve) — never an empty Response and never a
// nil error.
func TestOpenAIStreamCancelMidStream(t *testing.T) {
	// Each chunk is a full SSE frame (terminated by a blank line) so the scanner
	// yields complete lines from each Read. Two content deltas; we cancel on the
	// Read that would deliver the SECOND delta, so the partial Response must hold
	// "Hel" but not "lo".
	chunks := []string{
		`data: {"choices":[{"delta":{"content":"Hel"}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"content":"lo"}}]}` + "\n\n",
		"data: [DONE]\n\n",
	}

	ctx, cancel := context.WithCancel(context.Background())
	rdr := &framePacedReader{chunks: chunks, cancelAt: 1, cancel: cancel} // cancel on the "lo" Read

	var deltas []string
	resp, err := assembleOpenAIStream(ctx, rdr, func(c model.Chunk) { deltas = append(deltas, c.Text) })

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

// TestOpenAIStreamCancelBeforeBytes proves that when ctx is already cancelled so
// the HTTP round-trip itself fails before any byte arrives, Stream surfaces the
// context error (not a transport-wrapped error).
func TestOpenAIStreamCancelBeforeBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, cannedOpenAISSE)
	}))
	defer srv.Close()

	o := NewOpenAI("k", "gpt-x")
	o.baseURL = srv.URL

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the call

	_, err := o.Stream(ctx, "sys", nil, nil, 100, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestOpenAIStreamHTTPError proves a non-2xx response is surfaced as a non-context
// error with the body tail, mirroring Complete.
func TestOpenAIStreamHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"bad"}}`)
	}))
	defer srv.Close()

	o := NewOpenAI("k", "gpt-x")
	o.baseURL = srv.URL
	_, err := o.Stream(context.Background(), "sys", nil, nil, 100, nil)
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("400 should not be a context error: %v", err)
	}
	if !strings.Contains(err.Error(), "openai api") {
		t.Errorf("err = %v, want openai api prefix", err)
	}
}

// streamerCheckOpenAI is a compile-time assertion that *OpenAI satisfies the
// optional model.Streamer interface (additive to Provider, invariant I1).
var _ model.Streamer = (*OpenAI)(nil)
