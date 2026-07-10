package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// MarshalJSON emits the chat-completions request body with the token cap under a
// single, configurable key name. gpt-5.x / o-series reject "max_tokens" and
// require "max_completion_tokens", and reject a body carrying BOTH — so the body
// must emit EXACTLY ONE of the two. We make that structurally impossible: the
// oaiRequest struct carries the value (MaxTokens) and the chosen key name
// (maxTokensField) as non-serialized fields, and this marshaller injects exactly
// one key — or none when the cap is unset.
//
// Byte-identity is preserved for the default case ("max_tokens"): every other
// field is marshalled through an alias of the same struct shape (identical tags,
// identical declaration order, identical omitempty), and the single token key is
// spliced in immediately after the always-present leading "model" field — exactly
// where the original `MaxTokens int `+"`json:\"max_tokens,omitempty\"`"+` placed it.
func (r oaiRequest) MarshalJSON() ([]byte, error) {
	// oaiRequestAlias shadows oaiRequest WITHOUT MarshalJSON (avoiding infinite
	// recursion) and without the two unexported/`json:"-"` token fields, so it
	// marshals every OTHER field byte-for-byte as before: model, messages, tools
	// (omitempty), stream (omitempty), stream_options (omitempty).
	type oaiRequestAlias oaiRequest
	rest, err := json.Marshal(oaiRequestAlias(r))
	if err != nil {
		return nil, err
	}

	// Omit the cap entirely when it is unset (<= 0) — mirrors the prior
	// omitempty on a zero int.
	if r.MaxTokens <= 0 {
		return rest, nil
	}

	// The key name defaults to "max_tokens" when unset, so a zero-value
	// oaiRequest (e.g. a test decode/re-encode) stays byte-identical to today.
	field := r.maxTokensField
	if field == "" {
		field = "max_tokens"
	}

	keyVal, err := json.Marshal(field)
	if err != nil {
		return nil, err
	}
	tokenPart := append(keyVal, ':')
	tokenPart = append(tokenPart, []byte(fmt.Sprintf("%d", r.MaxTokens))...)

	// "model" is always present and always first (no omitempty), so the body begins
	// with `{"model":<json-string>` and the model value is immediately followed by
	// either '}' (no other fields) or ','. Insert the single token key right after
	// that value, so it lands as the second field exactly where the original omitempty
	// int placed it. We locate the value's END by scanning the JSON string (honoring
	// escapes) — NOT by finding the first ',' byte, which was the bug: a model id
	// containing a comma (e.g. an OpenRouter route list) puts a comma INSIDE the model
	// string, and splicing there produces corrupt JSON.
	const modelPrefix = `{"model":`
	if !bytes.HasPrefix(rest, []byte(modelPrefix)) {
		return nil, fmt.Errorf("oaiRequest: marshalled body does not begin with the model field: %q", tail(string(rest), 120))
	}
	valEnd := endOfJSONString(rest, len(modelPrefix))
	if valEnd < 0 {
		return nil, fmt.Errorf("oaiRequest: could not locate the model value boundary in %q", tail(string(rest), 120))
	}
	// rest[valEnd:] is either "}" or ",<more fields>}"; inserting ",<token>" before it
	// yields `...<model>","max_tokens":N}` or `...<model>","max_tokens":N,<more>}`.
	out := make([]byte, 0, len(rest)+len(tokenPart)+1)
	out = append(out, rest[:valEnd]...)
	out = append(out, ',')
	out = append(out, tokenPart...)
	out = append(out, rest[valEnd:]...)
	return out, nil
}

// endOfJSONString returns the index in b just PAST the closing quote of the JSON
// string that begins at b[start] (which must be the opening '"'), honoring backslash
// escapes so a quote inside the string is not mistaken for the terminator. It returns
// -1 if b[start] is not a quote or the string is unterminated.
func endOfJSONString(b []byte, start int) int {
	if start >= len(b) || b[start] != '"' {
		return -1
	}
	for i := start + 1; i < len(b); i++ {
		switch b[i] {
		case '\\':
			i++ // skip the escaped byte (e.g. \" or \\)
		case '"':
			return i + 1
		}
	}
	return -1
}
