package trust

import (
	"context"

	"nilcore/internal/backend"
)

// Selector is the multi-backend strength-routing seam (Phase 13): it orders a set
// of candidate backend NAMES best-first by their earned, verifier-judged track
// record, so the orchestrator's multi-backend path tries the historically-strongest
// backend first and races the distinct backends in that order. It STRUCTURALLY
// satisfies agent.Selector (same Select signature) WITHOUT importing agent — the
// leaf rule holds (the orchestrator wires the leaf, never the reverse).
//
// I2 boundary: Select only ORDERS which backend gets the first attempt. It never
// runs a task, never judges one, and never decides "done" or the race winner. The
// orchestrator still runs each chosen backend in a fresh worktree and the verifier
// still judges every race (route.Race) and re-runs as the final gate. This Selector
// is a bias on attempt order, nothing more.
type Selector struct {
	ledger *Ledger
}

// agentSelector is a LOCAL restatement of the agent.Selector interface. Asserting
// *Selector against it at compile time proves the structural match WITHOUT importing
// agent (which would invert the leaf dependency direction). If agent.Selector's
// signature ever drifts, this assertion stops compiling — a deliberate tripwire.
type agentSelector interface {
	Select(ctx context.Context, t backend.Task, names []string) []string
}

var _ agentSelector = (*Selector)(nil)

// NewSelector wires a Selector over a ledger. A nil ledger is permitted and degrades
// to returning the candidate names unchanged (byte-identical "no earned signal ⇒
// keep configured order" — the safe no-history path).
func NewSelector(l *Ledger) *Selector {
	return &Selector{ledger: l}
}

// Select orders the candidate backend names best-first by the ledger's smoothed,
// verifier-judged score (Ledger.Order: known backends ahead of unknown ones, ties
// broken deterministically, input order preserved among unknowns). A nil receiver,
// a nil ledger, or an empty ledger returns names UNCHANGED — there is no earned
// signal to act on, so the configured order is kept verbatim (byte-identical). The
// input slice is not mutated. This only ORDERS; the verifier still decides "done"
// (I2).
func (s *Selector) Select(_ context.Context, _ backend.Task, names []string) []string {
	if s == nil || s.ledger == nil {
		return names
	}
	return s.ledger.Order(names)
}
