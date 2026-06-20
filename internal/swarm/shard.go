// Package swarm is the leaf data root for the verifier-backed agent swarm
// (Phase 12). A Shard is one unit of swarm work — a goal, the pack that proves
// it, and the typed Kind of artifact it must produce — that maps DOWN to the
// frozen backend/spawn units the existing orchestrator already drives. The
// package holds the small, testable guards that make the swarm's invariant
// claims executable as code (a ship gate that refuses a vacuous verifier, a
// budget classifier that distinguishes per-shard from global exhaustion, and a
// scoreboard projection that carries only verifier-trusted fields), so the
// proofs live next to the types rather than in prose.
//
// Leaf rule (enforced by deps_test.go): swarm imports the data contracts
// (artifact, verify, spawn, budget) but NEVER the orchestrator (super/agent/
// project) and NEVER a network/RPC/remote-DB transport. It is pure composition
// over shipped seams, so it can be imported by the swarm runner without pulling
// the orchestrator up into a leaf and without opening a standing service.
package swarm

import (
	"nilcore/internal/artifact"
	"nilcore/internal/spawn"
)

// ShardState is the closed terminal/lifecycle set for one shard. It is a closed
// enum on purpose: every shard is in exactly one of these, and a runner that
// branches on state can never encounter an unhandled value (no "and others"
// case). The non-terminal states (Queued/Running) gate dispatch; the terminal
// states (Passed/Failed/Exhausted/Skipped) are set ONLY from a verifier verdict
// or a scheduling decision, never from a worker's self-report (I2).
type ShardState string

const (
	// ShardQueued is the initial state: ready to dispatch once its Deps are met.
	ShardQueued ShardState = "queued"
	// ShardRunning means a worker is currently building this shard.
	ShardRunning ShardState = "running"
	// ShardPassed means the per-shard verifier asserted the artifact green. This is
	// the ONLY green terminal state and is set strictly from the verdict (I2).
	ShardPassed ShardState = "passed"
	// ShardFailed means the verifier ran and the artifact was not green — a
	// requeue-eligible terminal state (the shard may earn another attempt).
	ShardFailed ShardState = "failed"
	// ShardExhausted means the shard ran out of retry budget (or hit its per-shard
	// ceiling) and converges RED — distinct from Failed because no further attempt
	// is owed.
	ShardExhausted ShardState = "exhausted"
	// ShardSkipped means a dependency failed/was skipped (or the shard was pruned),
	// so it never ran. Distinct from Failed: nothing was asserted false.
	ShardSkipped ShardState = "skipped"
)

// Shard is one unit of swarm work. ID is the stable, run-spanning identity in the
// canonical form "swarm/<runID>/<n>" (the prefix is the run-isolation key the
// durable queue filters on, SW-T10). Input/Goal/Statement-like fields are the
// MODEL-FACING task description; Kind/Pack/Tier/Role are HARNESS routing metadata
// that selects the verifier and the provider stack but must never leak into the
// backend unit (see toSubtask). Deps names the shard IDs whose verified work this
// shard builds on; State/Attempt/Branch are the runner's bookkeeping.
type Shard struct {
	ID      string        // "swarm/<runID>/<n>"
	Input   string        // the source/seed material for the shard (model-facing)
	Goal    string        // the task the worker is asked to satisfy (model-facing)
	Kind    artifact.Kind // typed artifact shape the shard must produce (routing)
	Pack    string        // verifier pack name that proves the shard (routing)
	Role    string        // roster role the worker assumes (routing)
	Tier    string        // provider tier selector (routing)
	Deps    []string      // shard IDs this shard depends on (DAG ordering)
	State   ShardState    // current lifecycle state (set from verdict/scheduling)
	Attempt int           // 0-based retry counter for this shard
	Branch  string        // task branch the verified commit lives on (set on pass)
}

// toSubtask maps a Shard DOWN to the frozen spawn.Subtask the existing scheduler
// consumes. It carries ONLY ID, Goal, and Deps and DELIBERATELY DROPS the
// harness-routing fields (Kind/Pack/Tier/Role/Input/State/Attempt/Branch). This
// is the executable assertion of the I1 boundary: shard-extra data — which
// includes the choice of verifier pack and provider tier — never crosses into
// the backend unit, so a backend can neither read nor act on the swarm's routing
// decisions. The Summary is left zero here; the runner seeds it from a fresh
// ContextSummary (spawn owns that), keeping this mapping a pure projection.
func toSubtask(s Shard) spawn.Subtask {
	// Copy Deps so a later mutation of the shard's slice cannot reach into the
	// constructed subtask (the subtask is an independent value handed to spawn).
	var deps []string
	if len(s.Deps) > 0 {
		deps = append(deps, s.Deps...)
	}
	return spawn.Subtask{
		ID:        s.ID,
		Goal:      s.Goal,
		DependsOn: deps,
	}
}
