package provider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nilcore/internal/model"
)

// TestOpenAIStreamMidStreamErrorRetryable proves the assembler surfaces an in-band
// `data: {"error":{...}}` frame emitted MID-stream (after a 200 OK) as a typed,
// RETRYABLE *model.APIError — instead of skipping it, reaching a clean EOF, and
// returning a partial Response with a nil error (a false success that would defeat
// retry/failover). A `rate_limit`/overloaded class is classified retryable.
func TestOpenAIStreamMidStreamErrorRetryable(t *testing.T) {
	// A content delta arrives, then the server fails mid-stream and closes with NO
	// [DONE] — the exact shape that used to be a silent partial success.
	frames := `data: {"choices":[{"delta":{"content":"Hel"}}]}` + "\n\n" +
		`data: {"error":{"type":"rate_limit_error","message":"slow down","code":"rate_limit_exceeded"}}` + "\n\n"

	var got []string
	resp, err := assembleOpenAIStream(context.Background(), strings.NewReader(frames), func(c model.Chunk) { got = append(got, c.Text) })
	if err == nil {
		t.Fatal("want a typed error for an in-band error frame, got nil (silent partial success)")
	}
	var apiErr *model.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want a *model.APIError", err)
	}
	if !apiErr.Retryable {
		t.Errorf("rate_limit_error classified Retryable=%v, want true", apiErr.Retryable)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("an in-band error is not a context cancellation: %v", err)
	}
	// The partial that DID arrive is preserved on the returned Response (best-effort),
	// but the non-nil error is what governs retry.
	if len(resp.Content) != 1 || resp.Content[0].Text != "Hel" {
		t.Errorf("partial content = %+v, want the one text block that arrived before the error", resp.Content)
	}
}

// TestOpenAIStreamMidStreamErrorTerminal proves a terminal in-band error class
// (e.g. invalid_request_error) is classified NON-retryable, so the resilience
// wrapper fast-fails instead of retrying/failing over a request that cannot succeed.
func TestOpenAIStreamMidStreamErrorTerminal(t *testing.T) {
	frames := `data: {"error":{"type":"invalid_request_error","message":"bad tool schema"}}` + "\n\n"

	_, err := assembleOpenAIStream(context.Background(), strings.NewReader(frames), nil)
	if err == nil {
		t.Fatal("want a typed error, got nil")
	}
	var apiErr *model.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want a *model.APIError", err)
	}
	if apiErr.Retryable {
		t.Errorf("invalid_request_error classified Retryable=%v, want false (terminal)", apiErr.Retryable)
	}
}

// TestOpenAIStreamTruncatedNoContent proves a stream that closes at EOF with NO
// content and no [DONE] (a broken connection dressed up as a 200) is surfaced as a
// RETRYABLE error, not a clean-EOF success — otherwise an empty Response would be
// treated as a valid reply and never retried.
func TestOpenAIStreamTruncatedNoContent(t *testing.T) {
	// A comment/heartbeat line and a blank separator only — no data frames at all.
	frames := ": keep-alive\n\n"

	_, err := assembleOpenAIStream(context.Background(), strings.NewReader(frames), nil)
	if err == nil {
		t.Fatal("want an error for an empty truncated stream, got nil (false success)")
	}
	var apiErr *model.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want a *model.APIError", err)
	}
	if !apiErr.Retryable {
		t.Errorf("truncated no-content stream Retryable=%v, want true", apiErr.Retryable)
	}
}

// TestOpenAIStreamPartialThenEOFStillSucceeds pins that a stream which delivered
// SOME content and then hit EOF without [DONE] is STILL a clean success (nil error)
// — the truncation guard fires only on the no-content case, so the native loop's
// partial-tool_use salvage (which needs a nil-error partial Response) is preserved.
func TestOpenAIStreamPartialThenEOFStillSucceeds(t *testing.T) {
	// A tool-call opens and its args stream partially, then EOF — no finish, no [DONE].
	frames := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"write","arguments":"{\"path\":"}}]}}]}` + "\n\n"

	resp, err := assembleOpenAIStream(context.Background(), strings.NewReader(frames), nil)
	if err != nil {
		t.Fatalf("partial-content EOF should be a clean success, got: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "tool_use" {
		t.Fatalf("content = %+v, want one (partial) tool_use block", resp.Content)
	}
}
