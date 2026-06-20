package requeue

// plan.go — the planner and resolver (Pillar 4, P11-T21).
//
// WHY a leaf descriptor, not a spawn.Subtask. Plan emits requeue.Subtask, a
// flat-string struct that imports nothing from the orchestrator. The cmd wiring
// (P11-T23) translates each descriptor into the real spawn.Subtask + SubagentSpec
// it dispatches through the EXISTING spawn.DAGScheduler — Pillar 4 invents no
// loop. Keeping the descriptor leaf-typed is what lets this package stay a pure,
// hermetically testable transform (Worklist in, plan out) with no scheduler
// dependency, and it is asserted by `go list -deps` (no internal/spawn import).
//
// WHY one Subtask per artifact (grouped by owner). A run that produced ten
// artifacts with one red claim each should fan out ten focused fixes, not one
// monolithic "redo everything". Grouping by ArtifactID(+OwnerSubagent) yields the
// MINIMAL set: every red claim of an artifact rides in a single subtask whose
// Goal names exactly those claim ids, so the model re-derives only the broken
// cells and the base cut (ContinueFrom = the prior attempt's branch) preserves the
// claims that already passed.
//
// WHY Resolve decides termination here. Green is the verifier's after-worklist
// (I2): a Unit is resolved only when a FRESH ArtifactVerifier re-run dropped it
// from the worklist, never because a stored status said pass. Resolve classifies
// resolved / stillFailed / exhausted by diffing the before/after worklists and
// Bumping the Ledger, and reports whether the loop should run another round —
// bounded by MaxAttempts so a permanently-red cell converges rather than spins.
//
// Trust boundary (I7): a Subtask.Goal is harness-authored control text built from
// trusted claim ids and statuses; the model-authored ClaimID/Field are carried as
// data (a requeue key + label), never interpreted as instructions.

import (
	"fmt"
	"sort"
	"strings"
)

// Subtask is a LEAF re-dispatch descriptor — deliberately NOT a spawn.Subtask, so
// this package imports no orchestrator. The cmd wiring maps it onto the real
// scheduler types. ID identifies the focused fix, Goal is the harness-authored
// instruction naming only the red claim ids, DependsOn carries any ordering the
// caller wants, ContinueFrom is the prior attempt's base branch (so passing
// claims survive the re-run), and UnitKeys lists the keyed Units this subtask
// owns (used by Resolve and the retry ledger).
type Subtask struct {
	ID           string   `json:"id"`
	Goal         string   `json:"goal"`
	DependsOn    []string `json:"depends_on,omitempty"`
	ContinueFrom string   `json:"continue_from,omitempty"`
	UnitKeys     []string `json:"unit_keys,omitempty"`
}

// planGroup accumulates the red Units of a single (artifact, owner) group while
// Plan walks the worklist in deterministic order, preserving first-seen ordering
// for stable Goal text and UnitKeys.
type planGroup struct {
	artifactID string
	owner      string
	claimIDs   []string
	unitKeys   []string
}

// groupKey is the grouping identity: an artifact's red claims are fixed together,
// and a distinct OwnerSubagent splits the group so each owner re-runs its own
// cell. It is independent of key(Unit) (which is the per-Unit ledger identity).
func groupKey(u Unit) string { return u.ArtifactID + "\x00" + u.OwnerSubagent }

// Plan groups the worklist's eligible Units into the MINIMAL set of focused
// re-dispatch subtasks: one Subtask per (ArtifactID, OwnerSubagent) group, whose
// Goal names every failed ClaimID in that group and whose UnitKeys lists them.
// Exhausted Units (those that have spent their Ledger budget) are excluded — a
// permanently-red cell earns no further round. An empty or all-exhausted worklist
// yields zero Subtasks.
//
// priorAttempt is the base branch of the attempt that produced this worklist; it
// is set as each Subtask's ContinueFrom so the re-run cuts from the prior result
// and the claims that already passed are preserved rather than re-derived.
//
// Determinism: Units are visited in worklist order, groups are emitted in sorted
// group-key order, and claim ids within a group keep first-seen order — so the
// plan is stable across runs (meaningful for golden tests and reproducible
// dispatch).
func Plan(w Worklist, led *Ledger, priorAttempt string) []Subtask {
	groups := make(map[string]*planGroup)
	var order []string
	for _, u := range w.Units {
		// Exclude Units that have no budget left: an exhausted (or requeue-disabled,
		// MaxAttempts==0) Unit must not be re-dispatched.
		if led.Exhausted(u) {
			continue
		}
		gk := groupKey(u)
		g, ok := groups[gk]
		if !ok {
			g = &planGroup{artifactID: u.ArtifactID, owner: u.OwnerSubagent}
			groups[gk] = g
			order = append(order, gk)
		}
		g.claimIDs = append(g.claimIDs, u.ClaimID)
		g.unitKeys = append(g.unitKeys, key(u))
	}
	if len(order) == 0 {
		return nil
	}
	sort.Strings(order)

	subtasks := make([]Subtask, 0, len(order))
	for _, gk := range order {
		g := groups[gk]
		subtasks = append(subtasks, Subtask{
			ID:           requeueID(g.artifactID, g.owner),
			Goal:         goalFor(g),
			ContinueFrom: priorAttempt,
			UnitKeys:     g.unitKeys,
		})
	}
	return subtasks
}

// requeueID names a focused subtask stably from its group identity, so the same
// artifact/owner re-requeues under the same id across rounds.
func requeueID(artifactID, owner string) string {
	if owner == "" {
		return "requeue-" + artifactID
	}
	return "requeue-" + artifactID + "-" + owner
}

// goalFor builds the harness-authored re-dispatch instruction. It names only the
// trusted claim ids (control data, never the model's prose) so the worker
// re-derives exactly the red cells of one artifact and leaves the passing claims
// untouched.
func goalFor(g *planGroup) string {
	return fmt.Sprintf("Re-derive failed claims in artifact %q: %s",
		g.artifactID, strings.Join(g.claimIDs, ", "))
}

// Resolve diffs the before/after worklists of one requeue round to classify each
// previously-failing Unit and to decide whether the loop continues. Green is the
// verifier's after-worklist (I2): a Unit counts resolved ONLY because a fresh
// ArtifactVerifier re-run dropped it from `after`, never because a stored status
// said pass.
//
//   - resolved   = a Unit in `before` absent from `after` (the re-verify passed it).
//   - stillFailed = a Unit present in both; each is Bump'd against led (one more
//     attempt spent) and re-stamped with the new attempt count.
//   - exhausted  = the subset of stillFailed that has now hit MaxAttempts — these
//     earn no further round and converge red.
//
// The caller continues the loop iff len(stillFailed) > len(exhausted): there is at
// least one still-red Unit with budget remaining. When every still-red Unit is
// exhausted (or none remain), the loop stops and the run converges on whatever is
// left red.
func Resolve(before, after Worklist, led *Ledger) (resolved, stillFailed, exhausted []Unit) {
	// Index the after-worklist by per-Unit key for O(1) membership: presence means
	// the claim is STILL non-pass after the fresh verifier re-run.
	afterByKey := make(map[string]Unit, len(after.Units))
	for _, u := range after.Units {
		afterByKey[key(u)] = u
	}

	for _, b := range before.Units {
		cur, stillRed := afterByKey[key(b)]
		if !stillRed {
			// Dropped from the worklist by the verifier re-run ⇒ genuinely resolved.
			resolved = append(resolved, b)
			continue
		}
		// Still red: spend one attempt and re-stamp the post-bump count so the
		// caller (and the persisted Unit) reflect the new budget state.
		attempt := led.Bump(cur)
		cur.Attempt = attempt
		stillFailed = append(stillFailed, cur)
		if led.Exhausted(cur) {
			exhausted = append(exhausted, cur)
		}
	}
	return resolved, stillFailed, exhausted
}

// ShouldContinue reports whether the requeue loop runs another round given a
// round's Resolve output: true iff at least one still-red Unit retains budget
// (len(stillFailed) > len(exhausted)). It mirrors the termination rule inline in
// Resolve's contract, exposed so the cmd wiring reads the decision without
// recomputing it.
func ShouldContinue(stillFailed, exhausted []Unit) bool {
	return len(stillFailed) > len(exhausted)
}
