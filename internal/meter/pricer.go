// Package meter prices model token usage so the budget Ledger can be charged a
// real dollar amount per call (P0-T02 lays the pricing table; the charging
// decorator lands in P1-T01). The harness is small on purpose, so this is a
// flat, operator-auditable table — no network lookups, no provider SDKs, stdlib
// only (invariant I6). Per-model rates are *real* published list prices (see the
// per-entry citations on knownRates), so the budget ledger meters accurately;
// the *fallback* for an unknown id stays deliberately conservative (most
// expensive known tier) so an unfamiliar model is over-charged, never
// under-charged, and the budget ceiling stays a hard wall rather than a leaky
// estimate (invariant: the ceiling is the termination rail — see
// docs/MULTI-AGENT.md §7).
package meter

import (
	"strings"

	"nilcore/internal/model"
)

// Pricer turns a model id and a token usage into a dollar cost. It is the seam
// the metering decorator (P1-T01) calls once per model response; the id it
// passes is exactly model.Provider.Model() (e.g. "claude-opus-4-8", "gpt-5.5",
// "openrouter/fusion").
//
// It carries two methods. Price is the original two-count (input/output) form.
// PriceUsage (P15-T15) is the Usage-aware path the metering decorator now charges
// through: it consults the authoritative vendor cost (Usage.CostUSD), the
// cached-input discount, and reasoning tokens. For a plain split (no
// CostUSD/cached/reasoning) PriceUsage equals Price(in,out), so existing charging
// stays byte-identical (Table proved this in P15-T11).
type Pricer interface {
	// Price returns the dollar cost of a call that consumed in input tokens and
	// out output tokens on model modelID. Negative counts are clamped to zero so
	// a caller can never produce a negative (ceiling-relaxing) charge.
	Price(modelID string, in, out int) float64

	// PriceUsage returns the dollar cost of a full model.Usage on model modelID —
	// preferring an authoritative reported cost (Usage.CostUSD), discounting cached
	// input, and billing reasoning tokens at the output rate. For a usage that
	// carries only InputTokens/OutputTokens it equals Price(modelID, in, out).
	PriceUsage(modelID string, u model.Usage) float64
}

// rate is the per-1,000-token price split for one model family. Input and output
// are billed separately because output is typically ~5x input.
type rate struct {
	inPer1k  float64
	outPer1k float64
}

// knownRates maps a model-id prefix to its per-1k-token rate. Keys are prefixes
// (matched longest-first) because vendor ids carry suffixes the table should not
// have to enumerate — "claude-opus-4-8", "claude-opus-4-8-fast", and a dated
// snapshot all resolve to the opus tier. Prices are USD per 1,000 tokens,
// derived from published per-1M list prices (÷1000).
//
// PRICES ARE ESTIMATES AND DRIFT. Each entry below cites the per-1M list price
// and the as-of date it was captured; re-verify against the vendor's current
// pricing page before relying on these for billing. Where a family spans a
// range or a price is uncertain (the GPT and OpenRouter entries), we round
// toward the more expensive tier so the budget rail never under-estimates.
var knownRates = []struct {
	prefix string
	rate   rate
}{
	// ── Anthropic (Claude) — list prices per 1M input / output, as of 2026-06.
	// Source: platform.claude.com pricing (claude-api skill, cached 2026-05-26).
	{"claude-fable-5", rate{0.010, 0.050}}, // Fable 5: $10 / $50 per 1M  (est. 2026-06)
	{"claude-opus", rate{0.005, 0.025}},    // Opus 4.x: $5 / $25 per 1M  (est. 2026-06)
	{"claude-sonnet", rate{0.003, 0.015}},  // Sonnet 4.x: $3 / $15 per 1M (est. 2026-06)
	{"claude-haiku", rate{0.001, 0.005}},   // Haiku 4.x: $1 / $5 per 1M  (est. 2026-06)
	{"claude", rate{0.010, 0.050}},         // unrecognized claude-* → priciest Claude tier (Fable), conservative

	// ── OpenAI (GPT) — list prices per 1M input / output, as of 2026-06.
	// First-party cost figures vary by snapshot; values below are best-known
	// published estimates and round high where uncertain. Re-verify on the
	// OpenAI pricing page.
	//
	// P15 widened the provider so OpenAI's reasoning models and a few more
	// snapshots are now actually reachable; their prefixes are listed here so a
	// real call lands on a real tier instead of the conservative fallback floor.
	// Longest-prefix-wins keeps the existing gpt-5.5-pro / gpt-5.5 / gpt-5.4-mini
	// resolutions byte-identical — the new entries only catch ids that the older
	// table sent to fallback.
	{"gpt-5.5-pro", rate{0.015, 0.120}},    // GPT-5.5 Pro: ~$15 / $120 per 1M (est. 2026-06)
	{"gpt-5.5", rate{0.00125, 0.010}},      // GPT-5.5: ~$1.25 / $10 per 1M    (est. 2026-06)
	{"gpt-5.4-mini", rate{0.00025, 0.002}}, // GPT-5.4 mini: ~$0.25 / $2 per 1M (est. 2026-06)
	// GPT-5.x reasoning / general snapshots now reachable via P15. gpt-5.x (no
	// -pro, no -mini) is the standard tier; round toward the standard $1.25/$10
	// so a known-family snapshot is metered accurately rather than at the Pro max.
	{"gpt-5.4", rate{0.00125, 0.010}}, // GPT-5.4 (standard): ~$1.25 / $10 per 1M (est. 2026-06)
	{"gpt-5", rate{0.00125, 0.010}},   // GPT-5.x (standard snapshots): ~$1.25 / $10 per 1M (est. 2026-06)
	// o-series reasoning models (o3 / o4-…): reasoning-heavy, priced above the
	// standard chat tier. Round high where the published figure is uncertain.
	{"o4", rate{0.003, 0.012}},  // o4-class reasoning: ~$3 / $12 per 1M (est. 2026-06, rounded high)
	{"o3", rate{0.002, 0.008}},  // o3-class reasoning: ~$2 / $8 per 1M  (est. 2026-06, rounded high)
	{"o1", rate{0.015, 0.060}},  // o1-class reasoning: ~$15 / $60 per 1M (est. 2026-06)
	{"gpt", rate{0.015, 0.120}}, // unrecognized gpt-* → priciest GPT tier (Pro), conservative

	// ── OpenRouter — a routing layer, not a single model.
	//   openrouter/fusion bills the *cumulative* cost of every model on its
	//   panel, so a single call can be far pricier than any one model. Price it
	//   deliberately HIGH so the panel is never under-charged.
	//   openrouter/<provider>/<model> falls back to this generic prefix when no
	//   more specific Claude/GPT prefix above matches the tail of the id.
	// Estimates as of 2026-06 — OpenRouter passes through upstream prices, which
	// drift; treat these as a conservative ceiling, not a quote. NOTE: OpenRouter
	// reports its own authoritative charged amount in Usage.CostUSD (decoded by
	// P15-T05); PriceUsage prefers that over these estimates when present, so
	// these rates only apply when a routed call returns no cost field.
	{"openrouter/fusion", rate{0.020, 0.150}}, // Fusion panel (cumulative): priced high  (est. 2026-06)
	{"openrouter", rate{0.015, 0.120}},        // other openrouter/* → priciest GPT-tier estimate (est. 2026-06)

	// ── OpenAI-compatible / self-hosted vendors reachable via the P15 "compat"
	// provider. These ids carry no canonical vendor prefix, so without an entry
	// they would hit the conservative fallback floor. A common deployment is a
	// Llama-class open weight behind an OpenAI-compatible endpoint; price the
	// known open-weight families at their typical hosted list rate (cheaper than
	// the frontier tiers) so a compat call is metered realistically, while any id
	// NOT matched here still falls through to the conservative floor.
	{"llama", rate{0.0009, 0.0009}},    // Llama-class hosted: ~$0.90 / $0.90 per 1M (est. 2026-06)
	{"mistral", rate{0.002, 0.006}},    // Mistral-class hosted: ~$2 / $6 per 1M     (est. 2026-06)
	{"qwen", rate{0.0009, 0.0009}},     // Qwen-class hosted: ~$0.90 / $0.90 per 1M  (est. 2026-06)
	{"deepseek", rate{0.0014, 0.0028}}, // DeepSeek-class hosted: ~$1.4 / $2.8 per 1M (est. 2026-06)
	{"gemini-2", rate{0.00125, 0.010}}, // Gemini 2.x (compat): ~$1.25 / $10 per 1M  (est. 2026-06)
	{"gemini", rate{0.00125, 0.010}},   // other gemini-* (compat): standard tier    (est. 2026-06)
}

// cachedDiscount is the fraction of the full input rate charged for input tokens
// served from a prompt cache (Usage.CachedTokens). Cached input is materially
// cheaper than fresh input at every vendor that surfaces it (Anthropic ~0.1x,
// OpenAI ~0.25–0.5x on the cached portion); we use a single conservative 0.25
// factor — high enough never to under-charge, low enough that a cache hit is
// genuinely cheaper than a full-price read. It only applies on the local-
// estimation path (PriceUsage); when a vendor reports an authoritative
// Usage.CostUSD that already reflects its real cache discount, PriceUsage uses
// that figure directly and this factor is not consulted.
const cachedDiscount = 0.25

// fallbackRate prices any model id that matches no known prefix. It is at least
// as expensive as every known tier in the table, so an unfamiliar provider is
// over-charged rather than under-charged — the budget ceiling stays a hard wall
// (the whole point of the table per docs/MULTI-AGENT.md §7). This is the
// documented FLOOR for unknown ids: a new/unknown model is never billed below
// these rates. Estimate as of 2026-06; revisit if a pricier tier is ever added
// above so this stays the maximum.
var fallbackRate = rate{0.020, 0.150}

// Table is the default Pricer: the built-in rate table. It holds no state, so a
// single value is safe to share across every metered provider.
type Table struct{}

// NewTable returns the default pricing table.
func NewTable() Table { return Table{} }

// Price implements Pricer. It looks up modelID against the prefix table (longest
// match wins) and falls back to the most expensive tier for any unknown id.
// Negative token counts are clamped to zero.
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

// PriceUsage prices a full model.Usage — the richer path the metering decorator
// uses now that providers report cached/reasoning splits and (on OpenRouter) an
// authoritative charged amount (P15-T03/T05). It is a concrete Table method, not
// a Pricer interface method, so it adds capability without touching the frozen
// Pricer contract or any other implementation.
//
// Rules, in order:
//
//  1. AUTHORITATIVE COST WINS. If u.CostUSD > 0, the vendor (OpenRouter) already
//     charged that exact amount — return it verbatim. Local estimation can only
//     drift from a real bill, so a reported cost always overrides the table. A
//     non-positive CostUSD means "not reported" and falls through to estimation.
//
//  2. CACHED INPUT IS DISCOUNTED. CachedTokens is the subset of InputTokens
//     served from a prompt cache; it bills at cachedDiscount × the input rate,
//     and only the remaining (fresh) input bills at the full input rate. Cached
//     tokens are clamped to the input total so a mis-reported cache count can
//     never drive the fresh portion (or the charge) negative.
//
//  3. REASONING IS OUTPUT. ReasoningTokens (hidden thinking) is billed at the
//     output rate, added to the visible OutputTokens — vendors meter reasoning
//     as completion tokens.
//
// Negative counts are clamped to zero (the ceiling can never be relaxed), and an
// unknown model id still resolves to the conservative fallback floor via rateFor.
func (Table) PriceUsage(modelID string, u model.Usage) float64 {
	// Authoritative vendor cost short-circuits all local estimation.
	if u.CostUSD > 0 {
		return u.CostUSD
	}

	in := u.InputTokens
	out := u.OutputTokens
	cached := u.CachedTokens
	reasoning := u.ReasoningTokens
	if in < 0 {
		in = 0
	}
	if out < 0 {
		out = 0
	}
	if reasoning < 0 {
		reasoning = 0
	}
	if cached < 0 {
		cached = 0
	}
	// Cached input is a subset of total input; never let it exceed (which would
	// otherwise produce a negative fresh-input count and relax the charge).
	if cached > in {
		cached = in
	}
	fresh := in - cached

	r := rateFor(modelID)
	// Fresh input at full rate, cached input at the reduced rate, and visible +
	// reasoning output at the output rate.
	cost := float64(fresh)/1000*r.inPer1k +
		float64(cached)/1000*r.inPer1k*cachedDiscount +
		float64(out+reasoning)/1000*r.outPer1k
	return cost
}

// knownWindows maps a model-id prefix to its context-window size in tokens
// (longest-prefix match, same discipline as knownRates). It powers the context-
// usage gauge and auto-compaction (the front door divides the last call's input
// tokens by this). Unlike pricing — which rounds UP for safety — the window
// FALLBACK rounds DOWN (a small conservative window) so an unknown model compacts
// EARLY rather than overruns its real limit. Values are approximate published
// context limits; re-verify against the vendor before relying on them.
var knownWindows = []struct {
	prefix string
	window int
}{
	{"claude-fable", 200_000},
	{"claude-opus", 200_000},
	{"claude-sonnet", 200_000},
	{"claude-haiku", 200_000},
	{"claude", 200_000},
	{"gpt-5.5", 400_000},
	{"gpt-5.4", 400_000},
	{"gpt", 400_000},
	{"openrouter", 128_000},
}

// fallbackWindow is the conservative window for an unknown id — small, so an
// unfamiliar model triggers compaction early rather than silently overrunning.
const fallbackWindow = 128_000

// CtxWindow returns the context-window size (in tokens) for a model id. The 1M-
// context variants advertise it in the id ("[1m]" / "-1m" suffix), so those win
// outright; otherwise it is the longest matching prefix, or the conservative
// fallback. Stdlib-only arithmetic (I6).
func CtxWindow(modelID string) int {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if strings.Contains(id, "[1m]") || strings.Contains(id, "-1m") {
		return 1_000_000
	}
	best, bestLen := fallbackWindow, -1
	for _, kw := range knownWindows {
		if strings.HasPrefix(id, kw.prefix) && len(kw.prefix) > bestLen {
			best, bestLen = kw.window, len(kw.prefix)
		}
	}
	return best
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
