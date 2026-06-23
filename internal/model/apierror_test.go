package model

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestAPIError_ErrorIsKeyFree proves Error() renders only status/type/code/message
// and never echoes a secret it was not given — the message it carries is the only
// vendor text, and constructing it from a benign message must not surface anything
// resembling a key, header, or request body (I3).
func TestAPIError_ErrorIsKeyFree(t *testing.T) {
	// A realistic vendor error carrying NO secret; Error() must reflect exactly
	// these fields and nothing else.
	e := &APIError{
		StatusCode: 401,
		Retryable:  false,
		Type:       "authentication_error",
		Code:       "invalid_api_key",
		Message:    "the provided key is invalid",
	}
	got := e.Error()

	for _, want := range []string{"401", "authentication_error", "invalid_api_key", "the provided key is invalid"} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q, missing %q", got, want)
		}
	}

	// A secret value the type was never given must never appear. We assert the
	// rendering is built only from the struct's own fields: it cannot contain a
	// token the constructor was not handed, by construction. Guard explicitly
	// against the most dangerous leak shapes.
	secret := "sk-ant-supersecret-key-DO-NOT-LEAK"
	if strings.Contains(got, secret) {
		t.Errorf("Error() leaked a secret it never held: %q", got)
	}
	for _, forbidden := range []string{"Authorization", "Bearer", "x-api-key", "sk-"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("Error() contains secret-shaped substring %q: %q", forbidden, got)
		}
	}
}

// TestAPIError_ErrorIsAs proves the error participates in errors.As so the
// resilience wrapper can type-assert it, including when wrapped with %w.
func TestAPIError_ErrorIsAs(t *testing.T) {
	base := &APIError{StatusCode: 429, Retryable: true}
	wrapped := fmt.Errorf("provider 0 (claude): %w", base)
	var got *APIError
	if !errors.As(wrapped, &got) {
		t.Fatal("errors.As failed to extract *APIError from wrapped error")
	}
	if got.StatusCode != 429 {
		t.Fatalf("extracted StatusCode = %d, want 429", got.StatusCode)
	}
}

// TestNewAPIError_Classifier proves the status -> Retryable mapping is exactly as
// specified: 429/500/502/503/504 retryable; 400/401/403/404/422 terminal; and the
// fallback (unlisted 5xx retryable, other terminal).
func TestNewAPIError_Classifier(t *testing.T) {
	cases := []struct {
		status        int
		wantRetryable bool
	}{
		{429, true},
		{500, true},
		{502, true},
		{503, true},
		{504, true},
		{400, false},
		{401, false},
		{403, false},
		{404, false},
		{422, false},
		// fallback behavior
		{418, false}, // teapot: unlisted 4xx -> terminal
		{599, true},  // unlisted 5xx -> retryable
		{200, false}, // not an error status, but classified terminal
	}
	for _, c := range cases {
		e := NewAPIError(c.status, "", "", "", "")
		if e.Retryable != c.wantRetryable {
			t.Errorf("NewAPIError(%d).Retryable = %v, want %v", c.status, e.Retryable, c.wantRetryable)
		}
	}
}

// TestParseRetryAfter proves both stdlib-supported Retry-After forms parse: a
// non-negative integer number of seconds, and an HTTP-date relative to now. Empty,
// malformed, and past values yield 0 (no hint).
func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2015, time.October, 21, 7, 28, 0, 0, time.UTC)
	cases := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"seconds", "5", 5 * time.Second},
		{"seconds-zero", "0", 0},
		{"seconds-large", "120", 120 * time.Second},
		{"empty", "", 0},
		{"whitespace", "   ", 0},
		{"garbage", "soon", 0},
		{"negative-seconds", "-3", 0},
		// HTTP-date 30s in the future relative to now.
		{"http-date-future", "Wed, 21 Oct 2015 07:28:30 GMT", 30 * time.Second},
		// HTTP-date in the past -> 0.
		{"http-date-past", "Wed, 21 Oct 2015 07:27:00 GMT", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseRetryAfter(c.in, now); got != c.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestNewAPIError_ParsesRetryAfter proves the constructor wires the Retry-After
// header through to the RetryAfter field (seconds form).
func TestNewAPIError_ParsesRetryAfter(t *testing.T) {
	e := NewAPIError(429, "rate_limit_error", "", "slow down", "7")
	if e.RetryAfter != 7*time.Second {
		t.Fatalf("RetryAfter = %v, want 7s", e.RetryAfter)
	}
	if !e.Retryable {
		t.Fatal("429 must be Retryable")
	}
}
