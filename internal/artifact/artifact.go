// Package artifact is the stdlib-only, leaf data contract for NilCore's
// verifier-backed artifact factory (Phase 11, the "spine"). An Artifact is a
// typed deliverable — a report, comparison matrix, spec, benchmark, or research
// dossier — whose every Claim rides with provenance Evidence and a verifier-set
// Status. The package holds NO behavior that reaches the network or the
// orchestrator: it is pure data so it can never pull sandbox/super/roster and
// can be imported by every pillar (evverify, packs, requeue, report) as a leaf.
//
// Trust boundary (I7): Value/SourceURL/RetrievedAt/ExtractionMethod/Statement are
// MODEL-AUTHORED — untrusted data, never instructions. Only Status and Detail are
// set by the harness verifier and are TRUSTED. Green() is a pure projection that
// MUST agree with the authoritative verify.Report the ArtifactVerifier produces.
package artifact

import "time"

// SchemaVersion is the current on-disk artifact schema. It is written into every
// marshaled Artifact (and defaulted on marshal) so a future reader can migrate.
const SchemaVersion = 1

// Status is the verifier-set verdict for one claim. It is set BY the verifier and
// is the only trusted field carrying a pass/fail meaning — a worker that
// self-writes a Status is overwritten by the real verdict (I2). The non-pass
// statuses are distinct because each routes a different requeue fix (Pillar 4):
// fail ⇒ re-derive the value, stale ⇒ re-fetch the source, unverifiable ⇒ fix the
// source or the verifier binding.
type Status string

const (
	// StatusUnverified is the initial state: the verifier has not run yet. It is
	// NOT green and never the basis to ship.
	StatusUnverified Status = "unverified"
	// StatusPass means the verifier ran and asserted the claim true. This is the
	// ONLY green status.
	StatusPass Status = "pass"
	// StatusFail means the verifier ran and asserted the claim false — the value is
	// WRONG. Requeue route: re-derive the value.
	StatusFail Status = "fail"
	// StatusStale means the source resolved but a freshness check failed. The value
	// may be right but is too old to trust. Distinct from Fail (the value is not
	// known wrong) and from Unverifiable (the source DID resolve). Requeue route:
	// re-fetch the source.
	StatusStale Status = "stale"
	// StatusUnverifiable means no decisive verdict was reached: a 404, no verifier
	// bound to the claim, or a denied/unreachable host. Distinct from Fail (nothing
	// was asserted false) — fail-closed, it is never treated as green. Requeue
	// route: fix the source or the verifier binding.
	StatusUnverifiable Status = "unverifiable"
)

// Kind names the artifact's shape. It is descriptive metadata only — it does not
// gate verification (every Kind is verified claim-by-claim the same way).
type Kind string

const (
	KindReport    Kind = "report"
	KindMatrix    Kind = "matrix"
	KindSpec      Kind = "spec"
	KindBenchmark Kind = "benchmark"
	KindDossier   Kind = "research-dossier"
)

// Evidence is the provenance + verdict carried by a single claim. Every field
// except Status and Detail is MODEL-AUTHORED and UNTRUSTED (I7); a verifier
// asserts over them and writes the trusted Status/Detail.
type Evidence struct {
	Value            string    `json:"value"`                       // asserted datum — UNTRUSTED (model-authored)
	SourceURL        string    `json:"source_url,omitempty"`        // provenance — UNTRUSTED; MUST be key-free (I3)
	RetrievedAt      time.Time `json:"retrieved_at,omitempty"`      // provenance; a HINT, never a basis to PASS (I2)
	ExtractionMethod string    `json:"extraction_method,omitempty"` // UNTRUSTED
	Verifier         string    `json:"verifier,omitempty"`          // verifier-id resolved via the Registry
	Status           Status    `json:"status"`                      // set BY the verifier — TRUSTED
	Detail           string    `json:"detail,omitempty"`            // verifier's bounded output tail — TRUSTED
}

// Claim is one verifiable assertion in an artifact. ID is the stable,
// run-spanning requeue key; Field is the semantic label; Statement is optional
// prose context — UNTRUSTED, never an instruction.
type Claim struct {
	ID        string   `json:"id"`
	Field     string   `json:"field"`
	Statement string   `json:"statement,omitempty"`
	Evidence  Evidence `json:"evidence"`
}

// Artifact is a typed deliverable carrying its own machine-verifiable acceptance
// criteria: it is GREEN only because every Claim passed a runnable check.
type Artifact struct {
	SchemaVersion int       `json:"schema_version"`
	ID            string    `json:"id"`
	Kind          Kind      `json:"kind"`
	Title         string    `json:"title,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	Claims        []Claim   `json:"claims"`
}

// Green is a PURE projection: true iff the artifact has at least one claim and
// every claim is StatusPass. It is fail-closed — an artifact that asserts nothing
// (empty Claims) is NOT green, because an artifact that claims nothing cannot be
// trusted-green. Authoritative green is the verify.Report the ArtifactVerifier
// returns; Green() must AGREE with it (it is the same predicate).
func (a *Artifact) Green() bool {
	if len(a.Claims) == 0 {
		return false
	}
	for i := range a.Claims {
		if a.Claims[i].Evidence.Status != StatusPass {
			return false
		}
	}
	return true
}
