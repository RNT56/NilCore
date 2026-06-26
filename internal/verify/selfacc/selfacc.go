// Package selfacc proposes self-generated acceptance criteria for an
// under-specified goal and authors *candidate* verifiers for them — the
// "contract-first, but the agent writes the contract" half of Pillar 3
// (LRN-T06). It exists because the loop only earns autonomy by raising its own
// bar: when a goal arrives with no acceptance pack, the agent should be able to
// PROPOSE how "done" would be checked rather than make the operator hand-write
// every criterion. Crucially, proposing is all this package does — it never
// marks work done and never runs a verifier of record.
//
// Two invariants shape every line here:
//
//   - I2 (the verifier is the sole authority on "done"). A criterion this
//     package proposes is inert until a real verifier binds to it. A candidate
//     verifier is UNTRUSTED: until it is admitted by the meta-check AND
//     registered in the evverify Registry, resolving a claim against it yields
//     artifact.StatusUnverifiable — NEVER StatusPass. Absence of proof is never
//     read as a pass (fail-closed). Nothing here can fold a self-report into a
//     verdict.
//
//   - I4 (model-emitted execution is sandboxed). A candidate verifier may ONLY
//     ever run as a sandboxed command/artifact verifier — an evverify.CheckFunc
//     that execs inside the box. It can NEVER be admitted as an in-process,
//     host-side Go func. The meta-check (Admit) is the gate that enforces this:
//     it rejects any candidate that is not a bounded sandbox command, so a
//     model-authored "verifier" can never become arbitrary host code.
//
// The package is a LEAF: it imports only artifact, evverify, planner, sandbox,
// and the standard library — never the orchestrator (agent/super/project). The
// deps_test.go guard enforces that closure.
//
// Default-off / additive: constructing a Proposal or a Candidate runs nothing.
// The result is data the caller may surface, store, or discard; a candidate
// only ever reaches the sandbox once an operator-controlled wiring layer chooses
// to Register its admitted CheckFunc. I7: every model-authored field
// (goal text, criterion statements, the candidate command) is treated as data —
// it is bounded and validated structurally, never interpreted as instructions.
package selfacc

import (
	"fmt"
	"strings"

	"nilcore/internal/artifact"
	"nilcore/internal/planner"
)

// AcceptanceCriterion is one proposed, machine-checkable acceptance statement
// for a goal. It is the contract-first unit: a Field naming what is asserted, a
// human-readable Statement (UNTRUSTED model-authored prose, never an
// instruction), and an optional Verifier id naming the check that WOULD decide
// it. A criterion with an empty Verifier is a bare proposal — nothing can pass
// it until a verifier is bound and admitted.
type AcceptanceCriterion struct {
	// Field is the stable semantic label for what this criterion asserts
	// (e.g. "build_passes", "url_resolves"). It maps to a claim's Field.
	Field string
	// Statement is the prose form of the criterion. UNTRUSTED (model-authored):
	// it is carried as data and surfaced for review, never executed or templated
	// into a command.
	Statement string
	// Verifier is the evverify verifier-id that WOULD decide this criterion. It
	// is only a binding hint at proposal time: until the id is both admitted
	// (Admit) and registered, a claim using it resolves to Unverifiable.
	Verifier string
}

// Proposal is the structured output of proposing acceptance criteria for an
// under-specified goal. It is pure data — building it runs nothing and asserts
// nothing about the goal being met.
type Proposal struct {
	// Goal is the (possibly under-specified) goal text the criteria were
	// proposed for. UNTRUSTED model-authored data.
	Goal string
	// Criteria are the proposed, contract-first acceptance statements. May be
	// empty when the goal yields no extractable criterion — an empty proposal is
	// honest (nothing was proposed), never a silent pass.
	Criteria []AcceptanceCriterion
}

// Propose derives candidate acceptance criteria from an under-specified goal and
// any task tree a planner already produced for it. It is deterministic and makes
// no model call: it lifts each plan task's contract-first Acceptance field into a
// criterion (the planner already required every task to state one), and de-dupes.
// When no tree is available, the proposal carries the goal with no criteria — an
// honest "nothing proposed yet", never a fabricated pass.
//
// The returned Proposal is INERT: no Verifier ids are bound here, because binding
// a criterion to a checkable verifier is a separate, meta-checked step (Author +
// Admit). Propose only frames WHAT should be checked, never asserts it was.
func Propose(goal string, tree *planner.Tree) Proposal {
	p := Proposal{Goal: strings.TrimSpace(goal)}
	if tree == nil {
		return p
	}
	seen := make(map[string]bool)
	for _, task := range tree.Tasks {
		stmt := strings.TrimSpace(task.Acceptance)
		if stmt == "" {
			continue // a task without acceptance contributes no criterion
		}
		// Field is the task id (a stable, structural handle), never the
		// untrusted prose — the prose rides only in Statement as data.
		field := strings.TrimSpace(task.ID)
		if field == "" {
			field = fmt.Sprintf("criterion-%d", len(p.Criteria)+1)
		}
		key := field + "\x00" + stmt
		if seen[key] {
			continue
		}
		seen[key] = true
		p.Criteria = append(p.Criteria, AcceptanceCriterion{
			Field:     field,
			Statement: stmt,
		})
	}
	return p
}

// Claims projects the proposal's criteria into artifact.Claims so they can ride
// the existing Phase-11 verification spine. Each claim carries the criterion's
// Statement as UNTRUSTED model-authored prose and the (possibly empty) Verifier
// binding. A claim whose Verifier is unbound — or bound to an id no admitted
// candidate registered — resolves to artifact.StatusUnverifiable when the
// ArtifactVerifier runs it (never StatusPass): that is the fail-closed I2 rule
// this package never weakens.
func (p Proposal) Claims() []artifact.Claim {
	claims := make([]artifact.Claim, 0, len(p.Criteria))
	for i, c := range p.Criteria {
		claims = append(claims, artifact.Claim{
			ID:        fmt.Sprintf("acc-%d", i+1),
			Field:     c.Field,
			Statement: c.Statement,
			Evidence: artifact.Evidence{
				// Status starts Unverified: nothing has run. It only becomes
				// Pass via an affirmative, admitted, sandboxed check.
				Status:   artifact.StatusUnverified,
				Verifier: c.Verifier,
			},
		})
	}
	return claims
}
