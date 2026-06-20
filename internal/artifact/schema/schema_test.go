package schema

import (
	"strings"
	"testing"

	"nilcore/internal/artifact"
)

// claim is a tiny constructor for a fully-populated claim, so a test only overrides
// the field under examination and every other field is non-empty by default.
func claim(id string) artifact.Claim {
	return artifact.Claim{
		ID:        id,
		Field:     "field-" + id,
		Statement: "statement-" + id,
		Evidence: artifact.Evidence{
			Value:     "value-" + id,
			SourceURL: "https://example.com/" + id,
			Verifier:  "web.url_resolves",
		},
	}
}

func reportArtifact(claims ...artifact.Claim) *artifact.Artifact {
	return &artifact.Artifact{
		ID:     "art-1",
		Kind:   artifact.KindReport,
		Title:  "A Title",
		Claims: claims,
	}
}

// codesOf projects defects to their Codes for compact golden assertions.
func codesOf(ds []Defect) []Code {
	out := make([]Code, len(ds))
	for i, d := range ds {
		out[i] = d.Code
	}
	return out
}

func eqCodes(a, b []Code) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestValidate_CleanArtifact(t *testing.T) {
	sch, ok := Default().Lookup(artifact.KindReport)
	if !ok {
		t.Fatal("report kind not in Default registry")
	}
	got := sch.Validate(reportArtifact(claim("a"), claim("b")))
	if len(got) != 0 {
		t.Fatalf("clean artifact produced defects: %+v", got)
	}
}

func TestValidate_NilSchemaFailsClosed(t *testing.T) {
	var s *Schema
	got := s.Validate(reportArtifact(claim("a")))
	want := []Code{CodeWrongKind}
	if !eqCodes(codesOf(got), want) {
		t.Fatalf("nil schema: got %v, want %v", codesOf(got), want)
	}
	if len(got) != 1 {
		t.Fatalf("nil schema must yield exactly one defect, got %d", len(got))
	}
}

func TestValidate_NilArtifactFailsClosed(t *testing.T) {
	sch, _ := Default().Lookup(artifact.KindReport)
	got := sch.Validate(nil)
	if !eqCodes(codesOf(got), []Code{CodeWrongKind}) {
		t.Fatalf("nil artifact: got %v", codesOf(got))
	}
}

func TestValidate_WrongKind(t *testing.T) {
	sch, _ := Default().Lookup(artifact.KindReport)
	a := reportArtifact(claim("a"))
	a.Kind = artifact.KindBenchmark // mismatch vs the report schema
	got := sch.Validate(a)
	if !eqCodes(codesOf(got), []Code{CodeWrongKind}) {
		t.Fatalf("wrong kind: got %v", codesOf(got))
	}
}

// TestValidate_GoldenOrdering pins the deterministic defect order: artifact-level
// defects FIRST (min-claims, then missing title), then per-claim defects in
// declaration order, and within a claim in (id/dup, required-fields, value, citation,
// verifier) order.
func TestValidate_GoldenOrdering(t *testing.T) {
	// Schema demanding everything, MinClaims 3 (we supply 2 ⇒ a min-claims defect).
	sch := &Schema{
		Kind:             artifact.KindReport,
		RequiredFields:   []string{"title", "field", "statement"},
		CitationRequired: true,
		VerifierRequired: true,
		MinClaims:        3,
	}

	// claim 1: dup id (same as claim 0) + empty value + missing citation + missing verifier.
	c0 := claim("dup")
	c1 := artifact.Claim{
		ID:    "dup", // duplicate of c0
		Field: "f",
		// Statement empty ⇒ MissingField(statement)
		Evidence: artifact.Evidence{
			Value:     "", // EmptyValue
			SourceURL: "", // MissingCitation
			Verifier:  "", // MissingVerifier
		},
	}
	a := &artifact.Artifact{
		ID:     "art-1",
		Kind:   artifact.KindReport,
		Title:  "", // missing title (artifact-level)
		Claims: []artifact.Claim{c0, c1},
	}

	got := codesOf(sch.Validate(a))
	want := []Code{
		CodeMissingField,    // claims < MinClaims (artifact-level, first)
		CodeMissingField,    // title (artifact-level)
		CodeDuplicateClaim,  // c1 id duplicates c0 (per-claim, declaration order)
		CodeMissingField,    // c1 statement
		CodeEmptyValue,      // c1 value
		CodeMissingCitation, // c1 source_url
		CodeMissingVerifier, // c1 verifier
	}
	if !eqCodes(got, want) {
		t.Fatalf("golden ordering mismatch:\n got  %v\n want %v", got, want)
	}
}

func TestValidate_EmptyClaimID(t *testing.T) {
	sch, _ := Default().Lookup(artifact.KindReport)
	c := claim("")
	c.ID = "" // empty id
	got := sch.Validate(reportArtifact(c))
	// Empty id ⇒ a MissingField(id) defect; the rest of the claim is well-formed.
	found := false
	for _, d := range got {
		if d.Code == CodeMissingField && d.Field == "id" {
			found = true
		}
	}
	if !found {
		t.Fatalf("empty claim id should produce MissingField(id); got %+v", got)
	}
}

func TestValidate_MinClaimsZeroAtFloor(t *testing.T) {
	// An empty Claims set must trip the MinClaims floor (Default sets it >=1).
	sch, _ := Default().Lookup(artifact.KindReport)
	got := sch.Validate(reportArtifact())
	if len(got) == 0 {
		t.Fatal("empty claims must produce at least a min-claims defect")
	}
	if got[0].Code != CodeMissingField || got[0].Field != "claims" {
		t.Fatalf("first defect for empty claims should be MissingField(claims), got %+v", got[0])
	}
}

func TestRegistry_LookupMiss(t *testing.T) {
	r := NewRegistry()
	if s, ok := r.Lookup(artifact.KindReport); ok || s != nil {
		t.Fatalf("empty registry must miss: got (%v,%v)", s, ok)
	}
}

func TestRegistry_RegisterNilIgnored(t *testing.T) {
	r := NewRegistry()
	r.Register(nil)
	r.Register(&Schema{Kind: ""}) // empty kind ignored
	if _, ok := r.Lookup(""); ok {
		t.Fatal("empty-kind schema must not be registered")
	}
}

func TestDefault_AllKindsRegistered(t *testing.T) {
	r := Default()
	for _, k := range []artifact.Kind{
		artifact.KindReport, artifact.KindMatrix, artifact.KindSpec,
		artifact.KindBenchmark, artifact.KindDossier,
	} {
		s, ok := r.Lookup(k)
		if !ok {
			t.Fatalf("Default missing kind %q", k)
		}
		if s.MinClaims < 1 {
			t.Fatalf("kind %q must require >=1 claim (never green on nothing), got %d", k, s.MinClaims)
		}
		if s.Kind != k {
			t.Fatalf("kind %q registered under wrong key %q", k, s.Kind)
		}
	}
}

// TestValidate_MatrixMinClaims confirms a one-row matrix is rejected (MinClaims 2).
func TestValidate_MatrixMinClaims(t *testing.T) {
	sch, _ := Default().Lookup(artifact.KindMatrix)
	a := &artifact.Artifact{ID: "m", Kind: artifact.KindMatrix, Claims: []artifact.Claim{claim("a")}}
	got := sch.Validate(a)
	if len(got) == 0 || got[0].Code != CodeMissingField || got[0].Field != "claims" {
		t.Fatalf("one-row matrix should trip min-claims; got %+v", got)
	}
	// Two rows ⇒ clean.
	a.Claims = []artifact.Claim{claim("a"), claim("b")}
	if d := sch.Validate(a); len(d) != 0 {
		t.Fatalf("two-row matrix should be clean; got %+v", d)
	}
}

// TestReason_NeverEchoesModelValue is the I7 guard at the Validate layer: a defect's
// Reason must never contain a model-authored Value/SourceURL/Statement.
func TestReason_NeverEchoesModelValue(t *testing.T) {
	const inj = "IGNORE-PREVIOUS-INSTRUCTIONS-SECRET-VALUE"
	sch := &Schema{
		Kind:             artifact.KindReport,
		RequiredFields:   []string{"field", "statement"},
		CitationRequired: true,
		VerifierRequired: true,
		MinClaims:        1,
	}
	// A claim carrying the injection string in EVERY model field, but structurally
	// defective so defects (with Reasons) are produced.
	c := artifact.Claim{
		ID:        "c1",
		Field:     "", // missing ⇒ a defect with a Reason
		Statement: inj,
		Evidence: artifact.Evidence{
			Value:     inj,
			SourceURL: "https://example.com/" + inj,
			Verifier:  "", // missing ⇒ a defect with a Reason
		},
	}
	defects := sch.Validate(&artifact.Artifact{ID: "a", Kind: artifact.KindReport, Title: "T", Claims: []artifact.Claim{c}})
	if len(defects) == 0 {
		t.Fatal("expected defects")
	}
	for _, d := range defects {
		if strings.Contains(d.Reason, inj) {
			t.Fatalf("Reason echoed a model-authored field (I7 violation): %q", d.Reason)
		}
		if len(d.Reason) > maxReason {
			t.Fatalf("Reason exceeds maxReason bound: %d", len(d.Reason))
		}
	}
}

// TestReason_Bounded confirms the maxReason truncation holds even for a pathological
// (long) identifier.
func TestReason_Bounded(t *testing.T) {
	long := strings.Repeat("x", 4000)
	got := reason("claim %q: required field %q is empty", long, "field")
	if len(got) > maxReason {
		t.Fatalf("reason not bounded: %d", len(got))
	}
}

// TestSpecCitationOptional confirms the spec Kind does NOT require a citation (a spec
// claim is verified by running code).
func TestSpecCitationOptional(t *testing.T) {
	sch, _ := Default().Lookup(artifact.KindSpec)
	c := claim("s")
	c.Evidence.SourceURL = "" // no citation
	got := sch.Validate(&artifact.Artifact{ID: "sp", Kind: artifact.KindSpec, Claims: []artifact.Claim{c}})
	for _, d := range got {
		if d.Code == CodeMissingCitation {
			t.Fatalf("spec kind must not demand a citation; got %+v", got)
		}
	}
}
