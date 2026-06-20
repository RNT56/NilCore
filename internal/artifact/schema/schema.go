// Package schema is the stdlib-only, leaf STRUCTURE check that runs BEFORE any
// per-claim network verification in the artifact spine (Phase 12, swarm mode). It
// answers a cheaper, earlier question than evverify.ArtifactVerifier: not "is this
// claim's value true?" but "does this artifact even have the SHAPE its Kind
// demands — the required fields, citations/verifier-ids when the Kind requires
// them, a minimum number of claims, no duplicate claim ids, the right Kind?" A
// malformed or under-populated artifact fails closed HERE, cheaply, and is reported
// distinctly from a claim that resolved Fail/Unverifiable downstream.
//
// WHY a separate leaf (not a method on artifact, not a check in evverify):
//
//   - It is the cheapest-first gate. The assembler (SW-T05) wires SchemaVerifier as
//     Composite's Named[0], so a shape defect short-circuits the whole verdict before
//     a single sandbox curl runs. Keeping it a no-network, no-sandbox leaf is what
//     makes that short-circuit free.
//   - It is the SINGLE source of every built-in per-Kind shape. Default() holds the
//     report/matrix/spec/benchmark/research-dossier shapes; the assembler aggregates
//     it. The verify packs (audit/benchmark/code) contribute their own Kind shapes
//     via their Schemas() and do NOT import this package, so there is exactly one
//     declarative description of each built-in shape and no import cycle.
//   - verify must stay a leaf importing only sandbox, so SchemaVerifier (which adapts
//     this to verify.Verifier) lives HERE, in the artifact subtree — verify gains no
//     import of artifact/worktreefs and no cycle forms.
//
// No JSON-schema module (I6): the whole validator is a deterministic Go walk over the
// typed artifact.Artifact. Reasons are HARNESS-authored constants/templates bounded
// to 256 bytes; they NEVER echo a model-authored Value/SourceURL/Statement (I7), so a
// prompt-injection phrase smuggled into a claim value can never ride out in a Defect.
package schema

import (
	"fmt"
	"strings"

	"nilcore/internal/artifact"
)

// Code is the CLOSED enumeration of structural defects. It is closed on purpose: a
// downstream reader (the report projection SW-T06, the assembler) switches over a
// finite, harness-controlled set, and a new shape rule must add a named Code here
// rather than smuggle free-form prose into the stream. Every Code names a defect the
// HARNESS detected over trusted, typed fields — never a model assertion.
type Code string

const (
	// CodeMissingField marks a RequiredField the Kind demands that is absent on the
	// artifact (e.g. an empty Title) or, for a per-claim required field, absent on a
	// claim. The Reason names WHICH field, never its (model-authored) value.
	CodeMissingField Code = "MissingField"
	// CodeEmptyValue marks a claim whose Evidence.Value is blank — an assertion with
	// nothing to assert. It is distinct from MissingField (the field exists but is
	// empty) so the requeue router can tell "you forgot the value" from "you forgot
	// the field".
	CodeEmptyValue Code = "EmptyValue"
	// CodeMissingCitation marks a claim with no provenance (empty SourceURL) when the
	// Kind sets CitationRequired. A cite-required Kind whose claim carries no source
	// can never be trusted-green, so we reject it structurally before the network step.
	CodeMissingCitation Code = "MissingCitation"
	// CodeMissingVerifier marks a claim with no Evidence.Verifier id when the Kind sets
	// VerifierRequired. An unbound claim resolves Unverifiable downstream anyway; this
	// surfaces it cheaply and distinctly at the shape layer.
	CodeMissingVerifier Code = "MissingVerifier"
	// CodeDuplicateClaim marks a claim whose ID was already seen earlier in the
	// artifact. Claim ID is the stable, run-spanning requeue key (artifact.Claim.ID), so
	// a duplicate id makes the requeue routing ambiguous and is rejected.
	CodeDuplicateClaim Code = "DuplicateClaim"
	// CodeWrongKind marks an artifact whose Kind does not match the Schema it was
	// validated against, OR an artifact whose Kind has no registered Schema at all. It
	// is the fail-closed verdict: a nil *Schema (unschematized Kind) yields exactly one
	// CodeWrongKind defect and nothing else, because without a shape we cannot say the
	// artifact is well-formed and must refuse it rather than wave it through.
	CodeWrongKind Code = "WrongKind"
)

// Defect is one structural finding. Every field is HARNESS-authored: ClaimID and
// Field name a location in the typed artifact (trusted identifiers, not model prose),
// Code is from the closed enum above, and Reason is a bounded, harness-written
// explanation. Reason is capped at maxReason bytes and is built ONLY from constants
// and trusted identifiers (the Code, the field/claim NAME) — it NEVER interpolates a
// model-authored Value, SourceURL, or Statement (I7).
type Defect struct {
	ClaimID string // empty for an artifact-level defect; the claim's ID otherwise
	Field   string // the field name at fault ("title", "value", "source_url", "verifier", "claims", "kind"), or ""
	Code    Code
	Reason  string // harness-authored, <=256B, no model field echoed (I7)
}

// maxReason bounds the harness-authored Reason so a Defect can never flood the event
// stream or the report. 256 bytes is ample for "claim <id>: required field <name> is
// missing" built from trusted identifiers; we truncate defensively even though every
// Reason here is harness-built, so a future longer template can never breach the cap.
const maxReason = 256

// reason builds a bounded, harness-authored Defect.Reason. It accepts a fixed format
// and trusted identifier arguments ONLY — callers pass Codes, field names, and claim
// ids, never a model-authored Value/SourceURL/Statement. The result is trimmed to
// maxReason bytes so the cap holds regardless of caller.
func reason(format string, args ...any) string {
	s := fmt.Sprintf(format, args...)
	if len(s) > maxReason {
		return s[:maxReason]
	}
	return s
}

// Schema is the declarative shape one Kind must satisfy. It is data, not behavior:
//
//   - RequiredFields lists artifact/claim field NAMES that must be present and
//     non-empty. Recognized artifact-level names are "title"; everything else is a
//     per-claim required field name (currently "field", "statement"). Unknown names
//     are treated as per-claim fields so a pack can demand a claim carry a Statement
//     without this package enumerating every field.
//   - CitationRequired demands every claim carry a non-empty Evidence.SourceURL.
//   - VerifierRequired demands every claim name a non-empty Evidence.Verifier id.
//   - MinClaims is the floor on len(Claims); below it the artifact asserts too little
//     to be trusted-green. It is always >= 1 in Default() (an artifact that claims
//     nothing is never green — mirrors Artifact.Green()).
//
// A Schema carries its own Kind so Validate can assert the artifact's Kind matches.
type Schema struct {
	Kind             artifact.Kind
	RequiredFields   []string
	CitationRequired bool
	VerifierRequired bool
	MinClaims        int
}

// Validate is a PURE, deterministic walk producing the artifact's structural defects
// in a fixed order: artifact-level defects FIRST (wrong kind, too few claims, missing
// required artifact fields), THEN per-claim defects in CLAIM DECLARATION ORDER. The
// determinism is load-bearing: golden tests pin the order, and the report projection
// renders it stably.
//
// Fail-closed: a nil *Schema (the assembler reached an unschematized Kind) yields
// EXACTLY one defect, {Code: CodeWrongKind}, and nothing else — without a shape we
// cannot certify the artifact well-formed, so we refuse it rather than report it
// clean. A nil artifact is likewise a single CodeWrongKind (there is nothing to
// validate). Validate NEVER reads the network and NEVER echoes a model field.
func (s *Schema) Validate(a *artifact.Artifact) []Defect {
	if s == nil {
		// Unschematized Kind: no shape to check against ⇒ refuse, fail-closed.
		return []Defect{{Code: CodeWrongKind, Field: "kind", Reason: reason("no schema registered for this artifact kind")}}
	}
	if a == nil {
		return []Defect{{Code: CodeWrongKind, Field: "kind", Reason: reason("nil artifact")}}
	}

	var defects []Defect

	// (1) artifact-level: Kind mismatch first. A schema validated against the wrong
	// Kind is a categorical error — report only it (the per-Kind field/claim rules do
	// not meaningfully apply to a mis-Kinded artifact), still fail-closed.
	if a.Kind != s.Kind {
		return []Defect{{Code: CodeWrongKind, Field: "kind",
			Reason: reason("artifact kind %q does not match schema kind %q", string(a.Kind), string(s.Kind))}}
	}

	// (2) artifact-level: minimum claim count. Below the floor the artifact asserts too
	// little; an empty Claims set can never be trusted-green (mirrors Artifact.Green()).
	if len(a.Claims) < s.MinClaims {
		defects = append(defects, Defect{Code: CodeMissingField, Field: "claims",
			Reason: reason("artifact has %d claim(s); kind %q requires at least %d", len(a.Claims), string(s.Kind), s.MinClaims)})
	}

	// (3) artifact-level required fields (currently only "title").
	if s.requiresArtifactField("title") && strings.TrimSpace(a.Title) == "" {
		defects = append(defects, Defect{Code: CodeMissingField, Field: "title",
			Reason: reason("required artifact field %q is empty", "title")})
	}

	// (4) per-claim defects in declaration order. We also detect duplicate ids here, in
	// first-seen order: the SECOND (and later) occurrence of an id is the defect.
	seen := make(map[string]struct{}, len(a.Claims))
	for i := range a.Claims {
		c := a.Claims[i]
		id := strings.TrimSpace(c.ID)

		// Duplicate claim id (the run-spanning requeue key must be unique).
		if id != "" {
			if _, dup := seen[id]; dup {
				defects = append(defects, Defect{ClaimID: id, Field: "id", Code: CodeDuplicateClaim,
					Reason: reason("duplicate claim id %q", id)})
			} else {
				seen[id] = struct{}{}
			}
		} else {
			// An empty id cannot be a requeue key; surface it as a missing field.
			defects = append(defects, Defect{ClaimID: "", Field: "id", Code: CodeMissingField,
				Reason: reason("claim #%d has an empty id", i)})
		}

		// Per-claim required fields named in RequiredFields (e.g. "field", "statement").
		for _, f := range s.RequiredFields {
			if isArtifactField(f) {
				continue // handled at the artifact level above
			}
			if claimFieldEmpty(c, f) {
				defects = append(defects, Defect{ClaimID: id, Field: f, Code: CodeMissingField,
					Reason: reason("claim %q: required field %q is empty", id, f)})
			}
		}

		// Every claim must carry a non-empty asserted Value — an assertion needs
		// something to assert. (Distinct from MissingField: the field exists, it is
		// blank.) We never read the value's CONTENT, only whether it is blank.
		if strings.TrimSpace(c.Evidence.Value) == "" {
			defects = append(defects, Defect{ClaimID: id, Field: "value", Code: CodeEmptyValue,
				Reason: reason("claim %q: evidence value is empty", id)})
		}

		// Citation required by the Kind.
		if s.CitationRequired && strings.TrimSpace(c.Evidence.SourceURL) == "" {
			defects = append(defects, Defect{ClaimID: id, Field: "source_url", Code: CodeMissingCitation,
				Reason: reason("claim %q: source_url citation is required for kind %q", id, string(s.Kind))})
		}

		// Verifier id required by the Kind.
		if s.VerifierRequired && strings.TrimSpace(c.Evidence.Verifier) == "" {
			defects = append(defects, Defect{ClaimID: id, Field: "verifier", Code: CodeMissingVerifier,
				Reason: reason("claim %q: a verifier id is required for kind %q", id, string(s.Kind))})
		}
	}

	return defects
}

// requiresArtifactField reports whether name (an artifact-level field) is listed in
// RequiredFields. Only artifact-level names ("title") flow through here.
func (s *Schema) requiresArtifactField(name string) bool {
	for _, f := range s.RequiredFields {
		if f == name {
			return true
		}
	}
	return false
}

// isArtifactField reports whether a RequiredFields entry is an ARTIFACT-level field
// (handled once at the artifact level) rather than a per-claim field. Today only
// "title" lives on the artifact; "kind"/"claims" are validated unconditionally above.
func isArtifactField(name string) bool {
	switch name {
	case "title", "kind", "claims":
		return true
	default:
		return false
	}
}

// claimFieldEmpty reports whether the named per-claim field is blank on c. Unknown
// names map to known claim fields where possible and otherwise to "treated present"
// (we never invent a defect for a field name this package does not model, so a pack
// cannot accidentally fail every claim by naming a typo).
func claimFieldEmpty(c artifact.Claim, name string) bool {
	switch name {
	case "field":
		return strings.TrimSpace(c.Field) == ""
	case "statement":
		return strings.TrimSpace(c.Statement) == ""
	case "value":
		return strings.TrimSpace(c.Evidence.Value) == ""
	case "source_url":
		return strings.TrimSpace(c.Evidence.SourceURL) == ""
	case "verifier":
		return strings.TrimSpace(c.Evidence.Verifier) == ""
	default:
		return false
	}
}

// Registry maps a Kind to its Schema. It is the catalog the assembler queries to pick
// the Named[0] SchemaVerifier's shape for an artifact's Kind. It is not safe for
// concurrent Register — registration happens once at wiring time (Default plus each
// opted-in pack's Schemas()), before any validation runs; Lookup during a run is
// read-only over the then-frozen map.
type Registry struct {
	byKind map[artifact.Kind]*Schema
}

// NewRegistry returns an empty Registry. With nothing registered, Lookup returns
// (nil,false) for every Kind and the SchemaVerifier fails that Kind closed.
func NewRegistry() *Registry {
	return &Registry{byKind: make(map[artifact.Kind]*Schema)}
}

// Register adds (or replaces) the Schema for s.Kind. A nil schema, or one with an
// empty Kind, is ignored (it would otherwise shadow a real shape with a no-Kind
// entry). Last writer for a Kind wins — packs own disjoint Kinds, so a collision is a
// wiring bug the aggregator controls, not a runtime condition guarded here.
func (r *Registry) Register(s *Schema) {
	if s == nil || s.Kind == "" {
		return
	}
	r.byKind[s.Kind] = s
}

// Lookup resolves a Kind to its Schema. The second return is false for an
// unregistered Kind; the caller MUST treat that as fail-closed (the SchemaVerifier
// reports a CodeWrongKind / non-pass), never as "no defects".
func (r *Registry) Lookup(k artifact.Kind) (*Schema, bool) {
	s, ok := r.byKind[k]
	return s, ok
}

// Default returns the Registry preloaded with the built-in per-Kind shapes for the
// five canonical artifact Kinds. THIS is the single source of every built-in shape;
// the assembler (SW-T05) starts from Default() and overlays each opted-in pack's
// Schemas(). The shapes are deliberately conservative — they encode the structural
// minimum each Kind needs to be a candidate for trusted-green, not a style guide:
//
//   - report:           a titled narrative; every claim cited and bound to a verifier.
//   - matrix:           a comparison table; >=2 claims (a one-row matrix is degenerate),
//     every claim cited and bound.
//   - spec:             an executable specification; claims carry a verifier (the build
//     /test binding) but a cite is not mandatory (a spec claim is
//     often verified by running code, not by a URL).
//   - benchmark:        perf claims; every claim bound to a verifier (the re-run check),
//     citation optional (the source is the re-run, not a URL).
//   - research-dossier: the most citation-strict Kind; every claim cited and bound,
//     and the dossier is titled.
//
// Every shape sets MinClaims >= 1: an artifact that asserts nothing is never green.
func Default() *Registry {
	r := NewRegistry()
	r.Register(&Schema{
		Kind:             artifact.KindReport,
		RequiredFields:   []string{"title", "field"},
		CitationRequired: true,
		VerifierRequired: true,
		MinClaims:        1,
	})
	r.Register(&Schema{
		Kind:             artifact.KindMatrix,
		RequiredFields:   []string{"field"},
		CitationRequired: true,
		VerifierRequired: true,
		MinClaims:        2, // a comparison of one row is not a matrix
	})
	r.Register(&Schema{
		Kind:             artifact.KindSpec,
		RequiredFields:   []string{"field"},
		CitationRequired: false, // a spec claim is verified by running code, not a URL
		VerifierRequired: true,
		MinClaims:        1,
	})
	r.Register(&Schema{
		Kind:             artifact.KindBenchmark,
		RequiredFields:   []string{"field"},
		CitationRequired: false, // the "source" is the verifier's own re-run
		VerifierRequired: true,
		MinClaims:        1,
	})
	r.Register(&Schema{
		Kind:             artifact.KindDossier,
		RequiredFields:   []string{"title", "field"},
		CitationRequired: true, // a research dossier without provenance is worthless
		VerifierRequired: true,
		MinClaims:        1,
	})
	return r
}
