package eventlog

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactionSliceAndRawMessageTypes covers the broadened redactValue type switch:
// a []string (args/env/hosts) and a json.RawMessage (a raw tool input) carried in an
// event Detail are now redacted with the SAME rules as a plain string. Before the fix
// the switch only handled string/map/[]any, so these two shapes fell through to the
// default (passed through UNREDACTED) and a secret could reach the append-only log (I3).
//
// This is discriminating by construction: the secrets live ONLY inside a []string and a
// json.RawMessage value. If either case is dropped from redactValue, the corresponding
// secret survives json.Marshal below and the leak assertion fails.
func TestRedactionSliceAndRawMessageTypes(t *testing.T) {
	// []string element forms the redactor recognizes: a bare prefixed provider token
	// (secretRe) and an inline `--token <value>` flag (flagSecretRe) — plus a plain,
	// non-secret element that must survive untouched.
	skToken := "sk-abc123def456ghi789jkl"
	d := map[string]any{
		"args": []string{"run --token s3cr3tflagvalue now", skToken, "plainkeep"},
		// A raw-json tool input with an inline credential assigned to a named field
		// (inlineSecretRe): the key name is kept, only the value is masked.
		"raw": json.RawMessage(`{"api_key":"live_secretrawvalue"}`),
	}
	redact(d)

	blob, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal redacted detail: %v", err)
	}
	s := string(blob)

	// The secrets must be gone — this is what proves the []string and json.RawMessage
	// cases actually ran (each secret lives only in one of those two value shapes).
	for _, leak := range []string{"s3cr3tflagvalue", skToken, "live_secretrawvalue"} {
		if strings.Contains(s, leak) {
			t.Errorf("secret leaked through redaction: %q present in %s", leak, s)
		}
	}
	if !strings.Contains(s, "[redacted]") {
		t.Error("expected a redaction marker in the output")
	}
	// A non-secret []string element must be preserved (no over-redaction).
	if !strings.Contains(s, "plainkeep") {
		t.Error("a plain non-secret []string element was destroyed")
	}
	// Structure (field/flag names) must survive so the audit line stays meaningful.
	for _, keep := range []string{"--token", "api_key"} {
		if !strings.Contains(s, keep) {
			t.Errorf("redaction destroyed structure: %q missing from %s", keep, s)
		}
	}
	// The redacted json.RawMessage must remain valid JSON (it is masked in place, then
	// re-embedded), so the marshal above must have kept the object shape.
	if !strings.Contains(s, `{"api_key":"[redacted]"}`) {
		t.Errorf("json.RawMessage value was not masked in place: %s", s)
	}
}
