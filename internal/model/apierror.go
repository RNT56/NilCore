// apierror.go gives the canonical model package a typed, vendor-neutral error a
// provider adapter can return when a model call fails at the HTTP layer. It exists
// so the resilience wrapper can make a *correct* retry decision instead of blindly
// retrying everything: a terminal 4xx (bad key, forbidden, malformed request) must
// fail fast — retrying it only burns latency and budget — while a transient 429/5xx
// should be retried, ideally respecting the server's Retry-After hint.
//
// The type is deliberately small and stdlib-only (I6). Its Error() is KEY-FREE
// (I3): it reports only the status, vendor type/code, and a message — never a
// secret, never a header, never the request body. A provider adapter constructs one
// from an HTTP response; the resilience wrapper inspects it via errors.As. Plain
// (untyped) errors are unaffected and retry exactly as before.
package model

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// APIError is a typed model-call failure carrying just enough structure for the
// resilience wrapper to decide retry-vs-terminal and to honor a server backoff
// hint. Type, Code, and Message are vendor-supplied strings (e.g. Anthropic's
// "rate_limit_error"); they describe the failure class, never carry credentials.
type APIError struct {
	// StatusCode is the HTTP status the vendor returned (e.g. 429, 503, 401).
	StatusCode int
	// Retryable reports whether retrying this call could plausibly succeed. It is
	// set by the classifier from StatusCode but can be overridden by a constructor.
	Retryable bool
	// RetryAfter is the server's requested minimum wait before retrying, parsed
	// from a Retry-After header. Zero means the server gave no hint.
	RetryAfter time.Duration
	// Type and Code are the vendor's machine-readable error classifiers (free of
	// secrets). Message is the vendor's human-readable message.
	Type, Code, Message string
}

// Error renders the error WITHOUT any secret: only the status, type/code, and the
// vendor message. It never includes an API key, header, or request payload (I3).
func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "model: api error: status %d", e.StatusCode)
	if e.Type != "" {
		fmt.Fprintf(&b, " type=%s", e.Type)
	}
	if e.Code != "" {
		fmt.Fprintf(&b, " code=%s", e.Code)
	}
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	return b.String()
}

// retryableStatus is the fixed classification of HTTP statuses into transient
// (retryable) vs terminal. 429 (rate limited) and the 5xx gateway/availability
// family are transient; the 4xx client errors below are terminal — retrying them
// cannot succeed without a different request.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	case http.StatusBadRequest, // 400
		http.StatusUnauthorized,        // 401
		http.StatusForbidden,           // 403
		http.StatusNotFound,            // 404
		http.StatusUnprocessableEntity: // 422
		return false
	default:
		// Unlisted statuses: treat any other 5xx as transient, everything else as
		// terminal. This keeps the explicit table authoritative while still doing
		// the sane thing for an unanticipated server-side status.
		return status >= 500
	}
}

// NewAPIError builds a typed error from a status, vendor classifiers, message, and
// a raw Retry-After header value (which may be empty). Retryability is derived from
// the status via retryableStatus. The header is parsed leniently: a bad/empty value
// just yields a zero RetryAfter, never an error.
func NewAPIError(status int, typ, code, message, retryAfter string) *APIError {
	return &APIError{
		StatusCode: status,
		Retryable:  retryableStatus(status),
		RetryAfter: parseRetryAfter(retryAfter, time.Now()),
		Type:       typ,
		Code:       code,
		Message:    message,
	}
}

// parseRetryAfter parses an HTTP Retry-After header value into a duration. The
// header has two stdlib-supported forms (RFC 7231): a non-negative integer number
// of seconds, or an HTTP-date. For the date form the wait is the date minus `now`.
// An empty, malformed, or past value yields 0 (no hint). A negative result is
// clamped to 0. `now` is a parameter so the date branch is testable.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	// Integer seconds form.
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	// HTTP-date form (e.g. "Wed, 21 Oct 2015 07:28:00 GMT").
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
