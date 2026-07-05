package provider

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"nilcore/internal/model"
)

// TestAnthropicStreamErrorEventRetryable proves an in-band `error` event carrying an
// overloaded_error is surfaced as a typed, RETRYABLE *model.APIError (honoring a
// Retry-After hint) — not an untyped error the resilience wrapper would blindly
// retry AND fail over even for a terminal class.
func TestAnthropicStreamErrorEventRetryable(t *testing.T) {
	frames := "event: error\n" +
		`data: {"type":"error","error":{"type":"overloaded_error","message":"overloaded","retry_after":"7"}}` + "\n\n"

	_, err := assembleAnthropicStream(context.Background(), strings.NewReader(frames), nil)
	if err == nil {
		t.Fatal("want a typed error for an overloaded_error event, got nil")
	}
	var apiErr *model.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want a *model.APIError", err)
	}
	if !apiErr.Retryable {
		t.Errorf("overloaded_error Retryable=%v, want true", apiErr.Retryable)
	}
	if apiErr.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %v, want the in-band 7s hint honored", apiErr.RetryAfter)
	}
}

// TestAnthropicStreamErrorEventTerminal proves a terminal in-band error class
// (invalid_request_error) is classified NON-retryable so the wrapper fast-fails.
func TestAnthropicStreamErrorEventTerminal(t *testing.T) {
	frames := "event: error\n" +
		`data: {"type":"error","error":{"type":"invalid_request_error","message":"bad request"}}` + "\n\n"

	_, err := assembleAnthropicStream(context.Background(), strings.NewReader(frames), nil)
	var apiErr *model.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want a *model.APIError", err)
	}
	if apiErr.Retryable {
		t.Errorf("invalid_request_error Retryable=%v, want false (terminal)", apiErr.Retryable)
	}
}

// TestAnthropicMaxTokensDefaulted proves a non-positive maxTokens is defaulted in
// the provider before the request is sent — a 0 would otherwise be a terminal 400
// no retry can fix. The body must carry a positive max_tokens.
func TestAnthropicMaxTokensDefaulted(t *testing.T) {
	for _, in := range []int{0, -5} {
		req, err := buildAnthropicRequest("claude-x", in, "", nil, nil, false)
		if err != nil {
			t.Fatalf("buildAnthropicRequest(%d): %v", in, err)
		}
		if req.MaxTokens != defaultAnthropicMaxTokens {
			t.Errorf("maxTokens %d -> req.MaxTokens = %d, want default %d", in, req.MaxTokens, defaultAnthropicMaxTokens)
		}
		body, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(body), `"max_tokens":`+strconv.Itoa(defaultAnthropicMaxTokens)) {
			t.Errorf("body = %s, want a positive max_tokens", body)
		}
	}
}

// TestAnthropicMaxTokensRespectsExplicit proves a positive maxTokens is passed
// through unchanged — the default only fills a non-positive value.
func TestAnthropicMaxTokensRespectsExplicit(t *testing.T) {
	req, err := buildAnthropicRequest("claude-x", 123, "", nil, nil, false)
	if err != nil {
		t.Fatalf("buildAnthropicRequest: %v", err)
	}
	if req.MaxTokens != 123 {
		t.Errorf("req.MaxTokens = %d, want 123 (explicit value preserved)", req.MaxTokens)
	}
}
