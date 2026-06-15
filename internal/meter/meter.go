// This file adds the metering decorator that finally makes the budget Ledger a
// real wall. Today the Ledger is dead code — nothing calls Charge — so the
// "budget ceiling" that every multi-agent facet leans on as the termination
// rail does not actually exist (docs/MULTI-AGENT.md §7, blocker #1). Provider
// closes that hole: it wraps any model.Provider, and after each Complete it
// prices the response's token usage (via the Pricer table in pricer.go) and
// charges one shared *budget.Ledger. When a charge would breach a ceiling the
// Ledger returns budget.ErrCeiling, which Provider propagates verbatim so the
// supervisor or project loop treats it as a hard stop and aborts the run.
//
// WHY post-charge (not pre-reserve): the true token count is only known after
// the call returns in resp.Usage, and the loop above us already bounds work by
// MaxRounds/MaxSteps/deadline, so charging on the way out is sufficient to make
// the ceiling a wall — the *next* Complete is refused once the wall is hit. WHY
// stdlib-only: invariant I6 — this is pure arithmetic over existing types.

package meter

import (
	"context"

	"nilcore/internal/budget"
	"nilcore/internal/model"
)

// Provider is a model.Provider decorator that charges a shared budget.Ledger for
// every Complete it forwards. It is the single seam that makes the budget
// ceiling enforceable: wrap every provider handed to the supervisor and each
// subagent (each with its own Task key) and the global ceiling becomes a real
// termination rail (docs/MULTI-AGENT.md §7).
//
// The zero value is not usable: Inner, Ledger, and Price must be set. Task is
// the budget scope key — the subagent/task id whose per-task ceiling (if any)
// this provider's spend counts against; an empty Task simply charges only the
// global total. Provider holds no mutable state of its own, so one value is
// safe to call concurrently as long as Inner, Ledger, and Price are (Ledger is;
// Table is stateless; a real provider must be).
type Provider struct {
	Inner  model.Provider // the underlying vendor provider this decorates
	Ledger *budget.Ledger // shared ledger charged once per Complete
	Task   string         // budget scope key for this provider's charges
	Price  Pricer         // dollar pricer for resp.Usage (e.g. meter.Table)
}

// Complete forwards to the inner provider and, on success, charges the ledger
// for the call's token usage at the priced dollar cost. If the inner call fails
// it returns that error and charges nothing (a failed call consumed no billable
// completion). If the charge would breach a per-task or global ceiling the
// Ledger returns budget.ErrCeiling; Complete returns it (alongside the response,
// which the caller must not use) so the orchestrating loop aborts — the ceiling
// is the wall, not an after-the-fact report.
//
// Charging is post-call because resp.Usage carries the authoritative token
// counts only after the response returns. The total charged is input+output
// tokens (for the ledger's token meter) priced in dollars by Price over the
// inner provider's model id (for the dollar ceiling). ctx is threaded into
// Charge so a cancelled context refuses the charge rather than recording it.
func (p *Provider) Complete(ctx context.Context, system string, msgs []model.Message, tools []model.Tool, maxTokens int) (model.Response, error) {
	resp, err := p.Inner.Complete(ctx, system, msgs, tools, maxTokens)
	if err != nil {
		// The inner call did not produce a billable completion; surface its
		// error untouched and charge nothing.
		return resp, err
	}

	tokens := resp.Usage.InputTokens + resp.Usage.OutputTokens
	dollars := p.Price.Price(p.Inner.Model(), resp.Usage.InputTokens, resp.Usage.OutputTokens)
	if cerr := p.Ledger.Charge(ctx, p.Task, tokens, dollars); cerr != nil {
		// ErrCeiling (and any ctx error from Charge) propagates so the caller
		// aborts. The response is returned for completeness but the non-nil
		// error tells the caller not to proceed.
		return resp, cerr
	}
	return resp, nil
}

// Model delegates to the inner provider so a metered provider is a drop-in for
// the one it wraps — role/tier resolution and the pricer both see the real
// model id.
func (p *Provider) Model() string { return p.Inner.Model() }
