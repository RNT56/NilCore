package tools

import (
	"context"
	"encoding/json"
	"testing"
)

// nameTool is a minimal Tool with a configurable name, for exercising the Defs()
// tool-name guard without depending on any real tool's naming.
type nameTool struct{ name string }

func (t nameTool) Name() string            { return t.name }
func (t nameTool) Description() string     { return "d" }
func (t nameTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t nameTool) Run(context.Context, string, json.RawMessage) (string, error) {
	return "", nil
}

func TestValidToolName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"read", true},
		{"web_search", true},
		{"rename_symbol", true},
		{"skill_greet", true},
		{"A-b_9", true},
		{"", false},               // empty
		{"skill_my greet", false}, // space
		{"skill_héllo", false},    // non-ASCII
		{"has/slash", false},      // path separator
		{"has.dot", false},        // dot
	}
	for _, c := range cases {
		if got := validToolName(c.name); got != c.ok {
			t.Errorf("validToolName(%q) = %v, want %v", c.name, got, c.ok)
		}
	}

	// A name of exactly 64 chars is valid; 65 is not.
	if !validToolName(string(bytesOf('a', 64))) {
		t.Error("64-char name should be valid")
	}
	if validToolName(string(bytesOf('a', 65))) {
		t.Error("65-char name should be invalid")
	}
}

func bytesOf(c byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return b
}

// TestDefsSkipsInvalidToolName proves one tool with an out-of-spec name is
// dropped from Defs() while every valid tool still ships — so a single malformed
// installed skill can never invalidate the whole Messages request.
func TestDefsSkipsInvalidToolName(t *testing.T) {
	r := NewRegistry(
		nameTool{"read"},
		nameTool{"skill_bad name"}, // space => invalid, must be skipped
		nameTool{"web_search"},
	)
	defs := r.Defs()

	got := map[string]bool{}
	for _, d := range defs {
		got[d.Name] = true
	}
	if !got["read"] || !got["web_search"] {
		t.Errorf("valid tools dropped from Defs(): %+v", defs)
	}
	if got["skill_bad name"] {
		t.Error("invalid tool name should have been skipped from Defs()")
	}
	if len(defs) != 2 {
		t.Errorf("Defs() = %d entries, want 2 (invalid one skipped)", len(defs))
	}

	// The invalid tool is still registered and dispatchable — the guard only
	// affects what is advertised to the provider, not local dispatch/replay.
	if !r.Has("skill_bad name") {
		t.Error("invalid tool should remain registered (guard is advertise-only)")
	}
}
