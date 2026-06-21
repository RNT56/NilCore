package trust

import (
	"context"

	"nilcore/internal/backend"
)

// Router is the strength-routing seam: it consults the Trust Ledger to choose
// WHICH wired backend to try first for a task, biasing toward the one with the
// strongest earned (verifier-judged) track record. It STRUCTURALLY satisfies
// agent.Router (same Route signature) without importing agent, exactly like
// route.SingleRouter — wiring is additive and the leaf rule holds.
//
// I2 boundary: Route only ORDERS/SELECTS a candidate backend. It never runs the
// task, never judges it, and never decides "done". The orchestrator still runs
// the chosen backend in a fresh worktree and the verifier still re-runs as the
// final gate; this router merely picks who gets the first attempt.
type Router struct {
	ledger   *Ledger
	backends map[string]backend.CodingBackend // wired backends, keyed by Name()
	def      backend.CodingBackend            // the configured default
}

// agentRouter is a LOCAL restatement of the agent.Router interface. Asserting
// *Router against it at compile time proves the structural match WITHOUT importing
// agent (which would invert the leaf dependency direction). If agent.Router's
// signature ever drifts, this assertion stops compiling — a deliberate tripwire.
type agentRouter interface {
	Route(ctx context.Context, t backend.Task, def backend.CodingBackend) backend.CodingBackend
}

var _ agentRouter = (*Router)(nil)

// NewRouter wires a Router over a ledger, a set of available backends, and the
// configured default. The backends map is keyed by Name() (the same identity the
// ledger records under), so a ranked name can be resolved back to a runnable
// backend. A nil ledger is permitted and degrades to always returning the
// fallback (byte-identical default behaviour — the safe no-history path).
func NewRouter(ledger *Ledger, backends map[string]backend.CodingBackend, def backend.CodingBackend) *Router {
	return &Router{ledger: ledger, backends: backends, def: def}
}

// Route returns the highest-ranked backend that is actually WIRED (present in
// backends), letting earned strength pick the first attempt. It falls back to the
// caller-supplied fallback when there is no earned signal to act on: a nil ledger,
// an empty ledger, or a ranking whose every name is unknown to this router. The
// fallback is returned BYTE-IDENTICALLY (the same value the caller passed), so the
// no-history path is indistinguishable from having no router at all — which is
// what keeps this strictly a bias, never an override (I2).
func (r *Router) Route(_ context.Context, _ backend.Task, fallback backend.CodingBackend) backend.CodingBackend {
	if r == nil || r.ledger == nil {
		return fallback
	}
	for _, name := range r.ledger.Rank() {
		if be, ok := r.backends[name]; ok {
			return be
		}
	}
	// No ranked backend is wired here (empty ledger, or every strong name is
	// missing from this router's set): defer to the configured default.
	return fallback
}
