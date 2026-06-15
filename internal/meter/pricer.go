// Package meter prices model token usage so the budget Ledger can be charged a
// real dollar amount per call (P0-T02 lays the pricing table; the charging
// decorator lands in P1-T01). The harness is small on purpose, so this is a
// flat, operator-auditable table — no network lookups, no provider SDKs, stdlib
// only (invariant I6). Prices are deliberately *conservative*: an unknown model
// id is priced at the most expensive known tier, never under-charged, so the
// budget ceiling stays a hard wall rather than a leaky estimate (invariant: the
// ceiling is the termination rail — see docs/MULTI-AGENT.md §7).
package meter

import "strings"

// Pricer turns a model id and a token split into a dollar cost. It is the seam
// the metering decorator (P1-T01) calls once per model response; the id it
// passes is exactly model.Provider.Model() (e.g. "claude-opus-4-8", "gpt-5.5",
// "openrouter/fusion").
type Pricer interface {
	// Price returns the dollar cost of a call that consumed in input tokens and
	// out output tokens on model modelID. Negative counts are clamped to zero so
	// a caller can never produce a negative (ceiling-relaxing) charge.
	Price(modelID string, in, out int) float64
}

// rate is the per-1,000-token price split for one model family. Input and output
// are billed separately because output is typically 5x input.
type rate struct {
	inPer1k  float64
	outPer1k float64
}

// knownRates maps a model-id prefix to its per-1k-token rate. Keys are prefixes
// (matched longest-first) because vendor ids carry suffixes the table should not
// have to enumerate — "claude-opus-4-8", "claude-opus-4-8-fast", and a dated
// snapshot all resolve to the opus tier. Prices are USD per 1,000 tokens,
// derived from published per-1M list prices (÷1000) as of 2026-06; they are a
// conservative default, intended to be operator-overridable, not a billing
// oracle. Rounding is toward the more expensive tier where a family spans a
// range, so the budget rail never under-estimates.
//
// Sources (per 1M input / output, list): Anthropic Fable 5 $10/$50, Opus
// $5/$25, Sonnet $3/$15, Haiku $1/$5; OpenAI GPT family priced at the Opus tier
// as a conservative stand-in (no public NilCore-side cost data); OpenRouter
// Fusion (a multi-model panel) priced at the most expensive Anthropic tier so a
// panel that may route to a premium model is never under-charged.
var knownRates = []struct {
	prefix string
	rate   rate
}{
	// Anthropic — longest prefixes first so a more specific family wins.
	{"claude-fable-5", rate{0.010, 0.050}},
	{"claude-opus", rate{0.005, 0.025}},
	{"claude-sonnet", rate{0.003, 0.015}},
	{"claude-haiku", rate{0.001, 0.005}},
	{"claude", rate{0.010, 0.050}}, // unrecognized claude-* → most expensive Anthropic tier
	// OpenAI — no first-party cost data here; price at the Opus tier conservatively.
	{"gpt", rate{0.005, 0.025}},
	// OpenRouter — Fusion routes to an opaque panel; price at the priciest tier.
	{"openrouter", rate{0.010, 0.050}},
}

// fallbackRate prices any model id that matches no known prefix. It is the most
// expensive tier in the table, so an unfamiliar provider is over-charged rather
// than under-charged — the budget ceiling stays a hard wall (the whole point of
// the table per docs/MULTI-AGENT.md §7).
var fallbackRate = rate{0.010, 0.050}

// Table is the default Pricer: the conservative built-in rate table. It holds no
// state, so a single value is safe to share across every metered provider.
type Table struct{}

// NewTable returns the default conservative pricing table.
func NewTable() Table { return Table{} }

// Price implements Pricer. It looks up modelID against the conservative prefix
// table (longest match wins) and falls back to the most expensive tier for any
// unknown id. Negative token counts are clamped to zero.
func (Table) Price(modelID string, in, out int) float64 {
	if in < 0 {
		in = 0
	}
	if out < 0 {
		out = 0
	}
	r := rateFor(modelID)
	return float64(in)/1000*r.inPer1k + float64(out)/1000*r.outPer1k
}

// rateFor resolves the rate for a model id: the longest matching known prefix,
// or the conservative fallback. Matching is case-insensitive on the id so a
// vendor that capitalizes differently still resolves to a known tier.
func rateFor(modelID string) rate {
	id := strings.ToLower(strings.TrimSpace(modelID))
	best := fallbackRate
	bestLen := -1 // -1 so even a zero-length-after-prefix match beats "no match"
	for _, kr := range knownRates {
		if strings.HasPrefix(id, kr.prefix) && len(kr.prefix) > bestLen {
			best = kr.rate
			bestLen = len(kr.prefix)
		}
	}
	return best
}
