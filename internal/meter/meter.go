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
	"strings"

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

// Stream makes the metering decorator transparent to the streaming loop: whether
// the wrapped provider can stream or not, the loop always sees a model.Streamer
// through the wrapper, and every assembled Response is charged to the ledger
// exactly as Complete charges — so the budget ceiling stays a real wall on the
// streaming path too.
//
// Two cases, one charging rule:
//
//   - Inner is a model.Streamer: delegate straight to Inner.Stream, passing
//     onChunk through untouched so every output-text delta still reaches the
//     caller live. The returned (assembled) Response is then charged.
//
//   - Inner is NOT a Streamer: fall back to Inner.Complete and replay the whole
//     assembled reply as ONE Chunk — the response's concatenated text in a single
//     onChunk(model.Chunk{Text}) — so a non-streaming provider still satisfies the
//     Streamer contract (the concatenation of forwarded chunks equals the output
//     text, just delivered as one big chunk). The Response is then charged.
//
// Charging mirrors Complete: tokens = input+output and the priced dollar cost of
// resp.Usage, charged under p.Task with ctx threaded into Charge. ErrCeiling (or
// a ctx error from Charge) propagates so the orchestrating loop aborts.
//
// Partial-on-cancel: if the inner stream is cut short (ctx cancelled mid-stream),
// Inner.Stream returns the partial Response together with ctx.Err(); we STILL
// charge whatever Usage came back — the tokens already produced are billable —
// and then surface the inner error. The inner transport/cancel error takes
// precedence over a ceiling breach in that case; a ceiling breach only surfaces
// when the inner call itself succeeded.
func (p *Provider) Stream(ctx context.Context, system string, msgs []model.Message, tools []model.Tool, maxTokens int, onChunk func(model.Chunk)) (model.Response, error) {
	var (
		resp    model.Response
		callErr error
	)
	if s, ok := p.Inner.(model.Streamer); ok {
		// Streaming provider: deltas flow through onChunk as they arrive.
		resp, callErr = s.Stream(ctx, system, msgs, tools, maxTokens, onChunk)
	} else {
		// Non-streaming provider: complete, then replay the whole reply as one
		// chunk so the contract (forwarded chunks concatenate to output text) holds.
		resp, callErr = p.Inner.Complete(ctx, system, msgs, tools, maxTokens)
		if callErr == nil && onChunk != nil {
			onChunk(model.Chunk{Text: responseText(resp)})
		}
	}

	// Charge whatever usage came back — including a partial-on-cancel response —
	// because the tokens it reports were genuinely produced and are billable.
	tokens := resp.Usage.InputTokens + resp.Usage.OutputTokens
	dollars := p.Price.Price(p.Inner.Model(), resp.Usage.InputTokens, resp.Usage.OutputTokens)
	cerr := p.Ledger.Charge(ctx, p.Task, tokens, dollars)

	if callErr != nil {
		// The inner call already failed (transport fault or partial-on-cancel);
		// that is the authoritative reason — surface it, charge recorded above.
		return resp, callErr
	}
	if cerr != nil {
		// Inner call succeeded but the charge breached a ceiling (or ctx is done):
		// propagate so the caller aborts; the response must not be used.
		return resp, cerr
	}
	return resp, nil
}

// responseText concatenates the text of a response's text blocks — the output
// prose a front end would paint — so the non-streaming fallback can replay it as
// a single Chunk.
func responseText(resp model.Response) string {
	var b strings.Builder
	for _, blk := range resp.Content {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// Model delegates to the inner provider so a metered provider is a drop-in for
// the one it wraps — role/tier resolution and the pricer both see the real
// model id.
func (p *Provider) Model() string { return p.Inner.Model() }
