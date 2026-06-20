package artifact

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Marshal renders an Artifact to canonical, byte-stable JSON: two-space indented,
// HTML-escaping off (so a SourceURL/Value with &, <, > round-trips verbatim rather
// than being mangled into \u00XX), and a trailing newline so the on-disk file is a
// well-formed text file. SchemaVersion is defaulted to the current SchemaVersion
// when the caller left it zero, so every persisted artifact carries a version.
//
// Determinism matters: the artifact file is written and re-written by the verifier
// (status overwrite) and read back by the supervisor; a stable serialization keeps
// diffs and golden tests meaningful.
func Marshal(a *Artifact) ([]byte, error) {
	if a == nil {
		return nil, fmt.Errorf("artifact: marshal nil artifact")
	}
	out := *a // copy so defaulting SchemaVersion never mutates the caller's value
	if out.SchemaVersion == 0 {
		out.SchemaVersion = SchemaVersion
	}
	// json.Encoder is the only stdlib path that turns off HTML escaping; it appends
	// a trailing newline, which is exactly the well-formed-text-file ending we want.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(&out); err != nil {
		return nil, fmt.Errorf("artifact: marshal: %w", err)
	}
	return buf.Bytes(), nil
}

// Unmarshal parses canonical artifact JSON back into an Artifact. A zero
// SchemaVersion on disk (a hand-written or legacy file) is defaulted to the
// current SchemaVersion so callers always see a versioned value; a parse failure
// is wrapped and returned (never a silent zero-value).
func Unmarshal(data []byte) (*Artifact, error) {
	var a Artifact
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("artifact: unmarshal: %w", err)
	}
	if a.SchemaVersion == 0 {
		a.SchemaVersion = SchemaVersion
	}
	return &a, nil
}
