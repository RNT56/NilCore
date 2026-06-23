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
	// "model" is always present and always first (no omitempty), so the body
	// begins with `{"model":<...>` and the next byte is either '}' (no other
	// fields) or ','. Splice the single token key right after the model value,
	// preserving the original field position.
	insertAt := bytes.IndexByte(rest, ',')
	tokenPart := append(keyVal, ':')
	tokenPart = append(tokenPart, []byte(fmt.Sprintf("%d", r.MaxTokens))...)

	var out []byte
	if insertAt < 0 {
		// Only "model" was emitted: `{"model":...}` -> insert before the '}'.
		closeAt := bytes.LastIndexByte(rest, '}')
		if closeAt < 0 {
			return nil, fmt.Errorf("oaiRequest: malformed marshalled body %q", rest)
		}
		out = make([]byte, 0, len(rest)+len(tokenPart)+1)
		out = append(out, rest[:closeAt]...)
		out = append(out, ',')
		out = append(out, tokenPart...)
		out = append(out, rest[closeAt:]...)
		return out, nil
	}
	// At least one more field follows "model": splice the token key + a comma
	// in at the first comma boundary, keeping `max_tokens` as the second field.
	out = make([]byte, 0, len(rest)+len(tokenPart)+1)
	out = append(out, rest[:insertAt+1]...) // up to and including the first comma
	out = append(out, tokenPart...)
	out = append(out, ',')
	out = append(out, rest[insertAt+1:]...)
	return out, nil
}
