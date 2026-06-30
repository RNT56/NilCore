// Package preset is the plain-data catalog of NilCore's swarm bundles (Phase 12,
// SW-T15). A Preset names everything an operator would otherwise have to tune by hand:
// the artifact Kind the shards produce, the roster Role that produces it, that role's
// Profile, the verify packs whose checks make the artifact GREEN, the egress those packs
// reach, and the shape of the run (how shards fan out and how their results fold back).
//
// Why this is a LEAF, and why it must stay one:
//
//   - preset imports ONLY artifact (Kind), roster (Role/Profile), packs (HostsFor/
//     Select/Name*), policy (Egress), evverify (Registry) and the standard library.
//     It MUST NOT import internal/swarm — internal/swarm carries Kind/Pack/Role as
//     PLAIN string/typed fields precisely so the sharder never needs preset, and the
//     dependency runs one way only (the cmd layer, SW-T17, maps a Preset onto a
//     swarm.Sharder/Shard). Importing swarm here would create a cycle and is rejected.
//
//   - The enums below (FanIn/Shape/SharderKind) are preset-LOCAL strings, not aliases
//     of swarm types, for the same reason: a plain-data catalog must not pull a heavier
//     package just to name its own shape. The cmd wiring translates these locals into
//     the concrete runner/scheduler/integrator choices.
//
// Trust + verification discipline (I2): Resolve never returns an always-pass verifier.
// The registry it builds is evverify.Default() (which registers NO noop/always-pass
// check) plus exactly the selected packs' affirmative checks — so a claim is green only
// because a runnable check asserted it, never because a pack was missing. An unknown
// preset name fails closed (ErrUnknownPreset) before any work starts.
package preset

import (
	"nilcore/internal/artifact"
	"nilcore/internal/roster"
)

// FanIn is how a preset folds its shards' results back together.
type FanIn string

const (
	// FanInCollate gathers each shard's verified Artifact side-by-side into one
	// deliverable (report/matrix) WITHOUT merging worktrees — the non-code presets
	// (research/audit/benchmark/ui) all collate, because their shards produce parallel
	// evidence, not overlapping edits. The cmd layer passes a nil Integrator for these.
	FanInCollate FanIn = "collate"
	// FanInMerge integrates each shard's branch into a single tip via the Integrator —
	// the code preset, whose shards make overlapping edits that must land as one tree.
	FanInMerge FanIn = "merge"
)

// Shape is how a preset's shards relate to one another.
type Shape string

const (
	// ShapeFlat is an independent fan-out: shards have no inter-dependencies and run
	// through the bounded scheduler pool. The four collate presets are flat.
	ShapeFlat Shape = "flat"
	// ShapeDAG is a dependency wave: shards carry Deps and run through the DAGScheduler.
	// Only the code preset is a DAG (a spec decomposes into ordered tasks).
	ShapeDAG Shape = "dag"
)

// SharderKind is which Sharder the cmd layer instantiates for a preset (the preset
// names the KIND; the concrete swarm.Sharder is built by SW-T17, which keeps preset off
// swarm's import path).
type SharderKind string

const (
	// SharderList expands an operator-supplied static list into N namespaced shards with
	// no model call (deterministic) — the benchmark preset, where the inputs are given.
	SharderList SharderKind = "list"
	// SharderPlan asks the planner model to decompose the goal into a shard set
	// (research/code/audit/ui) — the JSON-only planner parse, error on an invalid plan.
	SharderPlan SharderKind = "plan"
	// SharderFailure derives one shard per detected red test — the failure-driven "fix the
	// red tests" flow. The "fix" preset selects it; the cmd layer builds a swarm.FailureSharder
	// over a box, runs verify.Detect once, and emits one fix shard per failing test.
	SharderFailure SharderKind = "failure"
)

// Preset is the plain-data binding an operator selects by name. Every field is a static
// fact about the bundle EXCEPT Profile and Egress, which Resolve fills in at lookup time
// (the Profile needs an advisor provider it cannot hold statically, and Egress is DERIVED
// from VerifyPacks, never hand-typed). The bare catalog entries below leave both zero;
// Lookup returns them as-is for inspection, and Resolve completes them.
type Preset struct {
	Name        string
	Kind        artifact.Kind
	Role        roster.Role
	Profile     roster.Profile // write roles MUST set ReadOnly:false (the Role.ReadOnly() gotcha); filled by Resolve
	VerifyPacks []string
	Egress      []string // = union of VerifyPacks' packs.HostsFor (derived by Resolve, not hand-typed)
	FanIn       FanIn
	Shape       Shape
	Sharder     SharderKind
	WorkerTier  string // provider-tier label the cmd layer maps to a worker model
	PlannerTier string // provider-tier label for the planner model (SharderPlan presets)
}

// Worker- and planner-tier labels. They are opaque strings the cmd layer (SW-T17) maps
// onto concrete providers via --worker-model/--planner-model; the catalog only records
// the INTENT (a strong planner, a cheaper worker) so a preset stays pure data.
const (
	tierWorker  = "worker"
	tierPlanner = "planner"
)

// catalog is the named bundles (SWARM.md §8.1). Each entry binds the static facts;
// VerifyPacks drives both the verifier registry (packs.Select) and the derived Egress
// (union of packs.HostsFor) at Resolve time. The roles split three ways:
//
//   - research reuses the EXISTING roster.RoleTypedResearch (write-capable, web fetch) —
//     SW-T15 adds NO new role for it; it already writes the spine Artifact.
//   - code/fix reuse roster.RoleImplementer (the only role whose Role.ReadOnly() is already
//     false — it writes a spec/code shard's edits, merged via the Integrator).
//   - audit/ui use the NEW roster.RoleAuditor/roster.RoleUI, whose Profiles
//     (roster.AuditorProfile/roster.UIProfile) set ReadOnly:false explicitly.
//
// FanIn/Shape/Sharder encode the run shape: research/audit/benchmark/ui collate flat; code
// and fix merge their branches (overlapping edits land as one tree). code plans a DAG;
// benchmark uses a static list; fix derives one shard per detected red test (flat, since
// failures are independent); the rest plan.
var catalog = map[string]Preset{
	"research": {
		Name:        "research",
		Kind:        artifact.KindDossier,
		Role:        roster.RoleTypedResearch,
		VerifyPacks: []string{"web", "finance"},
		FanIn:       FanInCollate,
		Shape:       ShapeFlat,
		Sharder:     SharderPlan,
		WorkerTier:  tierWorker,
		PlannerTier: tierPlanner,
	},
	"code": {
		Name:        "code",
		Kind:        artifact.KindSpec,
		Role:        roster.RoleImplementer,
		VerifyPacks: []string{"software", "code"},
		FanIn:       FanInMerge,
		Shape:       ShapeDAG,
		Sharder:     SharderPlan,
		WorkerTier:  tierWorker,
		PlannerTier: tierPlanner,
	},
	"fix": {
		// fix is the failure-driven flow: SharderFailure runs verify.Detect once over a box
		// and emits one shard per red test (flat — failures are independent units, no Deps).
		// It reuses RoleImplementer + the code/software packs and merges its branches like the
		// code preset, because a fix shard makes real edits that must land as one verified tree.
		Name:        "fix",
		Kind:        artifact.KindSpec,
		Role:        roster.RoleImplementer,
		VerifyPacks: []string{"software", "code"},
		FanIn:       FanInMerge,
		Shape:       ShapeFlat,
		Sharder:     SharderFailure,
		WorkerTier:  tierWorker,
		PlannerTier: tierPlanner,
	},
	"audit": {
		Name:        "audit",
		Kind:        artifact.KindReport,
		Role:        roster.RoleAuditor,
		VerifyPacks: []string{"audit", "web"},
		FanIn:       FanInCollate,
		Shape:       ShapeFlat,
		Sharder:     SharderPlan,
		WorkerTier:  tierWorker,
		PlannerTier: tierPlanner,
	},
	"benchmark": {
		Name:        "benchmark",
		Kind:        artifact.KindBenchmark,
		Role:        roster.RoleImplementer,
		VerifyPacks: []string{"benchmark"},
		FanIn:       FanInCollate,
		Shape:       ShapeFlat,
		Sharder:     SharderList,
		WorkerTier:  tierWorker,
		PlannerTier: tierPlanner,
	},
	"ui": {
		Name:        "ui",
		Kind:        artifact.KindReport,
		Role:        roster.RoleUI,
		VerifyPacks: []string{"ui"},
		FanIn:       FanInCollate,
		Shape:       ShapeFlat,
		Sharder:     SharderPlan,
		WorkerTier:  tierWorker,
		PlannerTier: tierPlanner,
	},
}

// Names lists the catalog's preset names (sorted) for usage strings and enumeration in
// tests. It never exposes the underlying map.
func Names() []string {
	out := make([]string, 0, len(catalog))
	for n := range catalog {
		out = append(out, n)
	}
	sortStrings(out)
	return out
}

// Lookup returns the bare catalog Preset for name (Profile/Egress still zero) and false
// if the name is unknown. Resolve is the constructor callers use to get a runnable preset
// (Profile + derived Egress + verifier registry); Lookup is for inspection only.
func Lookup(name string) (Preset, bool) {
	p, ok := catalog[normalize(name)]
	return p, ok
}
