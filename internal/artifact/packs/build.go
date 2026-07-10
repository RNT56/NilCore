package packs

// build.go is the verify-pack ASSEMBLER (Phase 12, swarm mode, SW-T05). It is the one
// seam the swarm wiring (SW-T17) calls to turn a single `--verify-pack <name>` into the
// composed, cheapest-first verifier a shard is judged by, plus the documented egress
// host-set that pack reaches. It is pure composition over the shipped spine — it adds no
// mechanism and holds no state.
//
// The composed verifier (a verify.Composite) is ordered cheapest-first so a malformed
// artifact fails fast, BEFORE any sandbox curl runs:
//
//	Named[0] "schema"   schema.SchemaVerifier  — structural shape for the artifact's Kind
//	                                              (required fields, citations, min claims,
//	                                              no dup ids, right Kind). No network, no box.
//	Named[1] "evidence" evverify.ArtifactVerifier — the I2 per-claim gate: re-runs each
//	                                              claim's check IN-BOX and OVERWRITES the
//	                                              worker's self-claimed Status. Green iff
//	                                              every claim is StatusPass.
//	Named[2] (optional) a raw build/browser child — code ⇒ verify.New (autodetected
//	                                              build/test in-box); ui ⇒ verify.NewBrowser.
//	                                              benchmark / research (web+finance) / audit
//	                                              add NO extra child: their per-claim checks
//	                                              already ARE the in-box re-run.
//
// Fail-closed (I2): an UNKNOWN pack name is an ERROR, never a verify.Pass{} and never a
// make-verify "unknown ⇒ true" default — Build INVERTS verify.Detect's permissiveness, so
// a typo can never let a shard ship on a vacuous gate. The schema verifier is always
// Named[0], so even a nil box still runs the structural check; with a nil box the per-claim
// network checks resolve Unverifiable (the ArtifactVerifier fails them closed) rather than
// reaching for a host that is not there.
//
// Trust boundary (I7): every verifier the assembler composes reports only harness-set
// Status/Detail. Build never reads or echoes a model-authored Value/SourceURL/Statement; it
// only routes by the trusted pack NAME and the artifact's Kind.

import (
	"fmt"

	"nilcore/internal/artifact/evverify"
	"nilcore/internal/artifact/schema"
	"nilcore/internal/sandbox"
	"nilcore/internal/verify"
)

// PackPlan is what the assembler hands back to the wiring: the composed verifier a shard
// is judged by, and the documented egress host-set the pack reaches (so the caller can
// build the shard's egress allowlist from the same single source of truth HostsFor reads,
// cross-checked by P11-T35). Hosts is nil for the local/in-box packs.
type PackPlan struct {
	Verifier verify.Verifier
	Hosts    []string
}

// Build assembles the verify-pack named by name into a PackPlan, wiring NO event sink. It
// is the backward-compatible entry point; it delegates to BuildWithSink(…, nil). To record
// the append-only evidence events (schema_verify / claim_verify / artifact_verify), a caller
// uses BuildWithSink and supplies the eventlog-backed sink.
func Build(name string, box sandbox.Sandbox, relPath string, schemaReg *schema.Registry) (PackPlan, error) {
	return BuildWithSink(name, box, relPath, schemaReg, nil)
}

// BuildWithSink assembles the verify-pack named by name into a PackPlan. It:
//
//  1. starts from evverify.Default() and registers EXACTLY the named pack into it via
//     Select — so an unknown name aborts here (Select returns an error) BEFORE composing
//     anything, and the registry is never left half-populated;
//  2. composes a cheapest-first verify.Composite: the schema check (Named[0], over
//     schemaReg), then the per-claim ArtifactVerifier (Named[1], the I2 gate over the
//     just-registered pack registry), then — for code/ui only — a raw build/browser child;
//  3. returns the pack's documented egress host-set (HostsFor(name)).
//
// box may be nil: the composite still forms and the schema check still runs; the per-claim
// checks then resolve Unverifiable (never Pass) because the ArtifactVerifier fails a nil
// box closed. relPath is the worktree-relative artifact path both the schema and evidence
// verifiers read (the same path, so the two layers judge the SAME artifact). schemaReg is
// the per-Kind shape catalog (normally DefaultSchemas()); a nil schemaReg makes the schema
// verifier fail every Kind closed, which is the correct fail-closed behavior, not a panic.
//
// sink is threaded into BOTH the schema (Named[0]) and evidence (Named[1]) verifiers, so a
// caller can record the metadata-only schema_verify event and the per-claim/artifact
// evidence events. The leaves never import eventlog — the orchestrator supplies the backed
// sink (the cmd-side adapter that serializes each event's Detail is owned separately). A nil
// sink ⇒ no events emit and the plan behaves byte-identically to a sink-less build.
func BuildWithSink(name string, box sandbox.Sandbox, relPath string, schemaReg *schema.Registry, sink func(ev any)) (PackPlan, error) {
	// Register exactly the named pack into a fresh default registry. Select is ATOMIC and
	// fail-closed: an unknown name returns an error and registers nothing, so Build can
	// never compose a verifier over a pack that does not exist. This is the inversion of
	// verify.Detect's "unknown ⇒ true": here unknown ⇒ refuse.
	reg := evverify.Default()
	if err := Select([]string{name}, reg); err != nil {
		return PackPlan{}, fmt.Errorf("packs: build %q: %w", name, err)
	}

	// The worktree root relPath lives under bounds the evidence verifier's no-follow
	// write-back check (see evverify.ArtifactVerifier.Root). When a box is present its
	// Workdir IS that root; with a nil box we leave it empty and evverify falls back to
	// relPath's own directory — still a correct at/below-root boundary.
	var root string
	if box != nil {
		root = box.Workdir()
	}

	// Named[0] schema (structural, no network, no box) and Named[1] evidence (the I2
	// per-claim gate). Both read the SAME relPath, so a shape defect short-circuits the
	// whole verdict before any claim check runs. Both receive the sink (nil ⇒ no events).
	named := []verify.NamedVerifier{
		{Name: "schema", V: &schema.SchemaVerifier{Reg: schemaReg, RelPath: relPath, EventSink: sink}},
		{Name: "evidence", V: &evverify.ArtifactVerifier{Box: box, Reg: reg, RelPath: relPath, Root: root, EventSink: sink}},
	}

	// Named[2] optional raw child. Only code and ui add one; benchmark, audit, and the
	// research bundle (web+finance) already verify in-box through their per-claim checks,
	// so adding a build/browser child would be redundant (and, for those Kinds, wrong).
	// We guard on a non-nil box: with no box there is nothing for a build/browser child to
	// run against, so we omit it and let the schema + (Unverifiable) evidence layers stand.
	if box != nil {
		switch normalize(name) {
		case NameCode:
			// Re-run the autodetected build/test in-box and AND it with the typed `code`
			// claims, so BOTH the typed artifact and the raw build gate the shard.
			named = append(named, verify.NamedVerifier{
				Name: "build",
				V:    verify.New(box, verify.Detect(box.Workdir())),
			})
		case NameUI:
			// A browser-flow shard: drive the headless browser in-box. The driver command
			// is the autodetected verify command for now (the ui pack's own per-claim
			// checks carry the flow/screenshot/console assertions); a misconfigured/empty
			// command fails the BrowserVerifier closed rather than passing.
			named = append(named, verify.NamedVerifier{
				Name: "browser",
				V:    verify.NewBrowser(box, verify.Detect(box.Workdir())),
			})
		}
	}

	return PackPlan{
		Verifier: verify.Composite{Named: named},
		Hosts:    HostsFor(name),
	}, nil
}

// DefaultSchemas returns the built-in per-Kind shape catalog the assembler wires as the
// SchemaVerifier's Named[0] registry. It is exactly schema.Default(): the SINGLE source of
// every built-in per-Kind shape (report/matrix/spec/benchmark/research-dossier). The packs
// deliberately do NOT expose a Schemas() of their own here — schema.Default() owns the
// shapes so there is one declarative description per Kind and no import cycle (the packs
// never import the schema leaf). Callers that need pack-contributed Kinds overlay them onto
// this registry themselves; Build takes the registry as a parameter precisely so that
// choice stays with the wiring, not baked in here.
func DefaultSchemas() *schema.Registry { return schema.Default() }
