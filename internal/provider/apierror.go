package provider

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"nilcore/internal/model"
)

// newAPIError builds a typed model.APIError from a non-2xx HTTP response so the
// resilience wrapper can fast-fail a terminal status (400/401/403/404/422) and honor
// a 429/5xx Retry-After hint, instead of blindly retrying and failing over on a bad
// key. It parses the vendor error envelope — Anthropic and OpenAI-compatible both use
// {"error":{"type","code","message"}} — for the failure CLASS only. The body text is
// vendor-authored and key-free; we never include the request, headers, or the API key
// (I3). A body that does not parse still yields a typed error with the status + a
// bounded raw tail so the classifier and the operator both get a usable signal.
func newAPIError(status int, header http.Header, body []byte) error {
	var env struct {
		Error struct {
			Type    string          `json:"type"`
			Code    json.RawMessage `json:"code"` // string OR number across vendors
			Message string          `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &env)

	code := ""
	if len(env.Error.Code) > 0 && string(env.Error.Code) != "null" {
		if s, err := strconv.Unquote(string(env.Error.Code)); err == nil {
			code = s // it was a JSON string
		} else {
			code = strings.TrimSpace(string(env.Error.Code)) // a number/other scalar
		}
	}

	msg := env.Error.Message
	if msg == "" {
		// No structured message: fall back to a bounded raw tail (still key-free — it
		// is the server's response body, never our request) so the error is not blank.
		msg = tail(string(body), 1000)
	}

	retryAfter := header.Get("Retry-After")
	if retryAfter == "" {
		retryAfter = header.Get("retry-after")
	}
	return model.NewAPIError(status, env.Error.Type, code, msg, retryAfter)
}
