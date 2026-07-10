package schema

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/artifact"
)

// writeArtifact marshals a (or raw bytes) into a temp file and returns its path.
func writeArtifactFile(t *testing.T, a *artifact.Artifact) string {
	t.Helper()
	data, err := artifact.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return writeRaw(t, data)
}

func writeRaw(t *testing.T, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "artifact.json")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestSchemaVerifier_Clean(t *testing.T) {
	a := reportArtifact(claim("a"), claim("b"))
	v := &SchemaVerifier{Reg: Default(), RelPath: writeArtifactFile(t, a)}
	rep, err := v.Check(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rep.Passed {
		t.Fatalf("clean artifact should pass; output:\n%s", rep.Output)
	}
	if !strings.Contains(rep.Output, "schema OK") {
		t.Fatalf("clean output should say OK; got %q", rep.Output)
	}
}

func TestSchemaVerifier_StructuralDefectFailsClosed(t *testing.T) {
	a := reportArtifact() // no claims ⇒ min-claims defect
	v := &SchemaVerifier{Reg: Default(), RelPath: writeArtifactFile(t, a)}
	rep, err := v.Check(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rep.Passed {
		t.Fatal("artifact with no claims must fail closed")
	}
	if !strings.Contains(rep.Output, "schema FAIL") {
		t.Fatalf("defect output should say FAIL; got %q", rep.Output)
	}
	if !strings.Contains(rep.Output, string(CodeMissingField)) {
		t.Fatalf("output should name the Code; got %q", rep.Output)
	}
}

func TestSchemaVerifier_MissingFile(t *testing.T) {
	v := &SchemaVerifier{Reg: Default(), RelPath: filepath.Join(t.TempDir(), "nope.json")}
	rep, err := v.Check(context.Background())
	if err != nil {
		t.Fatalf("missing file should not be a Go error: %v", err)
	}
	if rep.Passed {
		t.Fatal("missing file must fail closed")
	}
	if !strings.Contains(rep.Output, "missing") {
		t.Fatalf("output should mention missing; got %q", rep.Output)
	}
}

func TestSchemaVerifier_CorruptFile(t *testing.T) {
	p := writeRaw(t, []byte("{not json"))
	v := &SchemaVerifier{Reg: Default(), RelPath: p}
	rep, err := v.Check(context.Background())
	if err != nil {
		t.Fatalf("corrupt file should not be a Go error: %v", err)
	}
	if rep.Passed {
		t.Fatal("corrupt file must fail closed")
	}
	if !strings.Contains(rep.Output, "parse error") {
		t.Fatalf("output should mention parse error; got %q", rep.Output)
	}
}

func TestSchemaVerifier_UnschematizedKind(t *testing.T) {
	a := reportArtifact(claim("a"))
	a.Kind = "made-up-kind"
	// Default has no schema for this kind ⇒ fail closed.
	v := &SchemaVerifier{Reg: Default(), RelPath: writeArtifactFile(t, a)}
	rep, err := v.Check(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rep.Passed {
		t.Fatal("unschematized kind must fail closed")
	}
	if !strings.Contains(rep.Output, string(CodeWrongKind)) {
		t.Fatalf("output should carry WrongKind; got %q", rep.Output)
	}
}

func TestSchemaVerifier_NilRegistryFailsClosed(t *testing.T) {
	a := reportArtifact(claim("a"))
	v := &SchemaVerifier{Reg: nil, RelPath: writeArtifactFile(t, a)}
	rep, err := v.Check(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rep.Passed {
		t.Fatal("nil registry must fail closed")
	}
}

// TestSchemaVerifier_OutputNeverEchoesModelField is the I7 guard at the verifier layer:
// the rendered Output must never contain a model-authored Value/SourceURL/Statement,
// even when those fields carry an injection phrase.
func TestSchemaVerifier_OutputNeverEchoesModelField(t *testing.T) {
	const inj = "INJECT-SECRET-PAYLOAD-12345"
	c := artifact.Claim{
		ID:        "c1",
		Field:     "", // defect
		Statement: inj,
		Evidence: artifact.Evidence{
			Value:     inj,
			SourceURL: "https://example.com/" + inj,
			Verifier:  "", // defect
		},
	}
	// Use a strict schema so defects (with rendered Reasons) are produced.
	reg := NewRegistry()
	reg.Register(&Schema{
		Kind:             artifact.KindReport,
		RequiredFields:   []string{"field"},
		CitationRequired: true,
		VerifierRequired: true,
		MinClaims:        1,
	})
	a := &artifact.Artifact{ID: "a", Kind: artifact.KindReport, Title: "T", Claims: []artifact.Claim{c}}
	v := &SchemaVerifier{Reg: reg, RelPath: writeArtifactFile(t, a)}
	rep, _ := v.Check(context.Background())
	if rep.Passed {
		t.Fatal("expected a failing report")
	}
	if strings.Contains(rep.Output, inj) {
		t.Fatalf("Output echoed a model-authored field (I7 violation):\n%s", rep.Output)
	}
}

// TestSchemaVerifier_EventEmitted asserts the metadata-only event fires with the right
// shape and that it, too, carries no model field.
func TestSchemaVerifier_EventEmitted(t *testing.T) {
	const inj = "EVENT-INJECT-PAYLOAD"
	c := artifact.Claim{
		ID:       "c1",
		Field:    "",
		Evidence: artifact.Evidence{Value: inj, SourceURL: "https://example.com/" + inj, Verifier: ""},
	}
	reg := NewRegistry()
	reg.Register(&Schema{Kind: artifact.KindReport, RequiredFields: []string{"field"}, CitationRequired: true, VerifierRequired: true, MinClaims: 1})
	a := &artifact.Artifact{ID: "art-x", Kind: artifact.KindReport, Title: "T", Claims: []artifact.Claim{c}}

	var got []any
	v := &SchemaVerifier{Reg: reg, RelPath: writeArtifactFile(t, a), EventSink: func(ev any) { got = append(got, ev) }}
	rep, _ := v.Check(context.Background())

	if len(got) != 1 {
		t.Fatalf("expected exactly one event, got %d", len(got))
	}
	ev, ok := got[0].(SchemaVerifyEvent)
	if !ok {
		t.Fatalf("event has wrong type: %T", got[0])
	}
	if ev.ArtifactID != "art-x" || ev.Kind != artifact.KindReport {
		t.Fatalf("event identity wrong: %+v", ev)
	}
	if ev.Passed != rep.Passed || ev.Passed {
		t.Fatalf("event Passed (%v) must match report (%v) and be false", ev.Passed, rep.Passed)
	}
	if len(ev.Defects) == 0 {
		t.Fatal("event should carry defect metadata")
	}
	// The event must not echo a model field anywhere in its DefectMeta fields.
	for _, d := range ev.Defects {
		if strings.Contains(d.Field, inj) {
			t.Fatalf("event DefectMeta echoed a model field (I7): %+v", d)
		}
	}
}

// TestSchemaVerifier_EventWireShape pins the on-wire contract (fix: the event was dead —
// no json tags, no claim_id/reason). A schema DEFECT must serialize to the shape the report
// decoder reads: a top-level {"id", "defects":[…]} whose defect entries carry lowercase
// {"code","field","claim_id","reason"}. We marshal the emitted event and decode it exactly
// as report.schemaDefectsFromEvent does, proving round-trip compatibility WITHOUT importing
// the report leaf. It also re-asserts I7: no model-authored field appears in the bytes.
func TestSchemaVerifier_EventWireShape(t *testing.T) {
	const inj = "WIRE-INJECT-PAYLOAD-98765"
	// A strict schema so the single claim yields defects that carry a ClaimID + Reason.
	reg := NewRegistry()
	reg.Register(&Schema{Kind: artifact.KindReport, RequiredFields: []string{"field"}, CitationRequired: true, VerifierRequired: true, MinClaims: 1})
	c := artifact.Claim{
		ID:        "claim-7",
		Field:     "", // ⇒ MissingField(claim-7)
		Statement: inj,
		Evidence:  artifact.Evidence{Value: inj, SourceURL: "", Verifier: ""}, // ⇒ MissingCitation + MissingVerifier
	}
	a := &artifact.Artifact{ID: "art-9", Kind: artifact.KindReport, Title: "T", Claims: []artifact.Claim{c}}

	var got []any
	v := &SchemaVerifier{Reg: reg, RelPath: writeArtifactFile(t, a), EventSink: func(ev any) { got = append(got, ev) }}
	if _, err := v.Check(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly one event, got %d", len(got))
	}

	// Serialize the event as the eventlog Detail would be, then decode it the way the report
	// projection does: keying off "id" and "defects" with {code,field,claim_id,reason}.
	data, err := json.Marshal(got[0])
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	if strings.Contains(string(data), inj) {
		t.Fatalf("serialized event echoed a model-authored field (I7 violation):\n%s", data)
	}
	var detail map[string]any
	if err := json.Unmarshal(data, &detail); err != nil {
		t.Fatalf("unmarshal event Detail: %v", err)
	}
	if detail["id"] != "art-9" {
		t.Fatalf("Detail[\"id\"] = %v, want \"art-9\" (the report decoder keys off this)", detail["id"])
	}
	raw, ok := detail["defects"].([]any)
	if !ok || len(raw) == 0 {
		t.Fatalf("Detail[\"defects\"] = %v, want a non-empty array (a dead pipeline yields none)", detail["defects"])
	}
	sawClaim, sawReason := false, false
	for _, item := range raw {
		d, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("defect entry is not an object: %T", item)
		}
		// Every key the decoder reads must be present (lowercase) and string-typed.
		for _, k := range []string{"code", "field", "claim_id", "reason"} {
			if _, present := d[k]; !present {
				t.Fatalf("defect entry missing %q key: %v", k, d)
			}
			if _, isStr := d[k].(string); !isStr {
				t.Fatalf("defect entry %q is not a string: %T", k, d[k])
			}
		}
		if d["claim_id"] == "claim-7" {
			sawClaim = true
		}
		if s, _ := d["reason"].(string); s != "" {
			sawReason = true
		}
	}
	if !sawClaim {
		t.Fatalf("no defect carried the trusted claim_id \"claim-7\": %v", raw)
	}
	if !sawReason {
		t.Fatalf("no defect carried a harness-authored reason: %v", raw)
	}
}

// TestSchemaVerifier_NoEventOnLoadFailure confirms a missing/corrupt file emits NO
// event (we have no trusted identity to put in one).
func TestSchemaVerifier_NoEventOnLoadFailure(t *testing.T) {
	var fired int
	v := &SchemaVerifier{
		Reg:       Default(),
		RelPath:   filepath.Join(t.TempDir(), "nope.json"),
		EventSink: func(ev any) { fired++ },
	}
	if _, err := v.Check(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fired != 0 {
		t.Fatalf("load failure must emit no event; fired %d", fired)
	}
}

// TestSchemaVerifier_SymlinkRefused confirms the O_NOFOLLOW open refuses a symlinked
// artifact path (worktree-confinement discipline). Skipped where symlinks are
// unavailable.
func TestSchemaVerifier_SymlinkRefused(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	v := &SchemaVerifier{Reg: Default(), RelPath: link}
	rep, err := v.Check(context.Background())
	if err != nil {
		t.Fatalf("symlink open should fail closed, not Go-error: %v", err)
	}
	if rep.Passed {
		t.Fatal("a symlinked artifact path must fail closed (O_NOFOLLOW refusal)")
	}
}

// compile-time: SchemaVerifier satisfies verify.Verifier is asserted in verifier.go;
// here we just confirm the event const is the documented value (SW-T06 depends on it).
func TestEventKindConst(t *testing.T) {
	if EventKind != "schema_verify" {
		t.Fatalf("EventKind drifted: %q", EventKind)
	}
}
