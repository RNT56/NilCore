// Package graapprove is the graduated-auto-approval policy (Phase 16, Pillar 5 —
// docs/ROADMAP-CLOSED-LOOP.md §5). It is the headline of the closed-loop program:
// a policy.Approver that WRAPS the human approver and, per structured GateAction,
// auto-approves ONLY when the action-class+scope has EARNED trust — verifier-green
// N times, over a recent and unbroken hash chain — AND the action sits within the
// operator-authored envelope AND the shared blast-radius budget still admits it.
// Anything that does not clear every gate falls through to the human.
//
// WHY this shape:
//
//   - I2 (the verifier is the sole authority on "done"): every trust signal is
//     folded ONLY from dedicated, verifier-judged `boundary_outcome` events — never
//     a backend self-report, and never a prior `auto_approve` (no self-reinforcing
//     loop). The policy decides who-presses-the-button, never whether work shipped.
//   - I5 (append-only event log): the trust view is a READ-ONLY replay that runs
//     eventlog.Verify and FAILS CLOSED on a broken chain (empty tallies, ChainOK
//     false) — a tampered log can only remove trust, never forge it; a verify error
//     is surfaced so the caller denies explicitly.
//   - I3: the envelope, the trust tallies, and the blast state are operator-authored
//     host-side data; they never enter a prompt or any model tool. This leaf only
//     reads them.
//   - I7: GateAction.Branch / Detail (possibly PR-title-derived) are matched as
//     pure DATA via glob/equality — never interpreted as policy.
//
// Default-off discipline (three layers): MaybeWrap returns the human approver
// UNCHANGED when no envelope is configured (no GradedApprover is ever allocated);
// an empty/zero envelope auto-approves nothing; a broken chain ⇒ empty trust ⇒
// human-gated. The free-text Approve(string) path ALWAYS delegates to the human —
// free-text gates are never auto-approved.
//
// This package is a pure leaf (deps_test.go): stdlib + policy/trust(via eventlog)/
// eventlog/blastbudget only.
package graapprove

import (
	"errors"
	"fmt"
)

// classTypes is the CLOSED set of admissible ClassClause.Type strings, mirroring
// the closed policy.GateActionType String() set {promote-to-base, push, deploy,
// open-pr, bind-self-authored}. Anything outside it is rejected by Validate — a new
// boundary action is a deliberate addition, never inferred.
var classTypes = map[string]struct{}{
	"promote-to-base":    {},
	"push":               {},
	"deploy":             {},
	"open-pr":            {},
	"bind-self-authored": {},
}

// Envelope is the operator-authored auto-approval policy: one clause per action
// class the operator is willing to auto-approve. A nil or empty Envelope is the
// default-off state (auto-approves nothing). It is host-side data and never enters
// a prompt or a model tool (I3).
type Envelope struct {
	Classes []ClassClause `json:"classes,omitempty"`
}

// ClassClause bounds auto-approval for a single action class. Every numeric trust
// bar is REQUIRED and fail-closed: a blank/zero bar is rejected by Validate, never
// read as "unlimited".
type ClassClause struct {
	// Type is one of the closed GateActionType strings.
	Type string `json:"type"`
	// AllowBranches is a glob allowlist of admitted scopes; a scope that matches no
	// pattern is not admitted (falls through to the human).
	AllowBranches []string `json:"allow_branches,omitempty"`
	// DenyBranches is a glob denylist; a match ALWAYS wins over AllowBranches.
	DenyBranches []string `json:"deny_branches,omitempty"`
	// Environments is the Deploy-only target allowlist; prod* is always denied
	// structurally regardless of this list.
	Environments []string `json:"environments,omitempty"`
	// MinSuccesses is the minimum count of verifier-green boundary_outcomes for
	// this (Type,scope) before auto-approval is eligible. Must be >= 1.
	MinSuccesses int `json:"min_successes,omitempty"`
	// MinSample is the minimum total observations (guards a 1-of-1 fluke). Must be
	// >= MinSuccesses.
	MinSample int `json:"min_sample,omitempty"`
	// RecencyDays requires a green within this many days. Must be >= 1.
	RecencyDays int `json:"recency_days,omitempty"`
	// MaxPerDay is the per-UTC-day auto-approval rate cap for this class. Must be
	// >= 1.
	MaxPerDay int `json:"max_per_day,omitempty"`
	// MaxDollarsDay is the per-UTC-day dollar ceiling (Deploy), composed with the
	// shared blastbudget meter. Must be >= 0.
	MaxDollarsDay float64 `json:"max_dollars_day,omitempty"`
}

// Empty reports whether the envelope grants nothing (nil or no clauses). An empty
// envelope is the on-but-unproven default-off state: it auto-approves nothing.
func (e *Envelope) Empty() bool {
	return e == nil || len(e.Classes) == 0
}

// Validate fails closed on any malformed clause. A blank trust bar is REJECTED,
// never silently treated as unlimited — an operator who turns the feature on must
// state an explicit, non-trivial bar.
func (e *Envelope) Validate() error {
	if e == nil {
		return errors.New("graapprove: nil envelope")
	}
	if len(e.Classes) == 0 {
		return errors.New("graapprove: envelope has no class clauses")
	}
	for i := range e.Classes {
		if err := e.Classes[i].validate(); err != nil {
			return fmt.Errorf("graapprove: class clause %d (%q): %w", i, e.Classes[i].Type, err)
		}
	}
	return nil
}

func (c ClassClause) validate() error {
	if _, ok := classTypes[c.Type]; !ok {
		return fmt.Errorf("unknown type %q (must be one of promote-to-base|push|deploy|open-pr|bind-self-authored)", c.Type)
	}
	if c.MinSuccesses < 1 {
		return errors.New("min_successes must be >= 1 (a blank trust bar is rejected, never unlimited)")
	}
	if c.MinSample < c.MinSuccesses {
		return fmt.Errorf("min_sample (%d) must be >= min_successes (%d)", c.MinSample, c.MinSuccesses)
	}
	if c.RecencyDays < 1 {
		return errors.New("recency_days must be >= 1")
	}
	if c.MaxPerDay < 1 {
		return errors.New("max_per_day must be >= 1")
	}
	if c.MaxDollarsDay < 0 {
		return errors.New("max_dollars_day must be >= 0")
	}
	return nil
}
