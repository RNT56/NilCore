package spawn

// Typed worker-result surface (Pillar 3, P11-T14).
//
// A typed-research subworker writes a spine `artifact.Artifact` JSON to its
// worktree; the host-side spawn func re-reads it AFTER the ArtifactVerifier has
// overwritten every claim's status, and projects the TRUSTED fields here. These
// types are deliberately FLAT (`string`/`bool`/slice only) and import nothing
// from `internal/artifact`, so `internal/spawn` stays a zero-artifact-import
// leaf and the projection can never smuggle a model-authored, unverified value
// into the supervisor's trusted control surface.

// ClaimStatus is the trusted, harness-set projection of one verified claim.
//
// It carries ONLY the identity (ID/Field) and the verifier-produced Status.
// Do NOT ever add Value or SourceURL (or any other model-authored field): those
// are UNTRUSTED, model-written data and must stay fenced as prose in the
// worker's Summary, never promoted into this trusted control struct. The whole
// point of the typed result is that what flows here is what the verifier
// asserted, not what the model claimed.
type ClaimStatus struct {
	ID     string // stable, run-spanning claim id (e.g. "company-041-revenue")
	Field  string // semantic label (e.g. "revenue_fy2024")
	Status string // verifier-set status: unverified/pass/fail/stale/unverifiable
}

// ArtifactSummary is the trusted projection of a verified evidence artifact.
//
// Green mirrors the ArtifactVerifier's pure projection (true iff every claim
// passed); it is harness-computed, never the worker's self-report. Claims
// carries one ClaimStatus per claim with the verifier-set status. All fields
// are flat by construction so this struct can ride on spawn.Result without
// pulling `internal/artifact` into the leaf.
type ArtifactSummary struct {
	ID     string
	Kind   string
	Green  bool
	Claims []ClaimStatus
}
