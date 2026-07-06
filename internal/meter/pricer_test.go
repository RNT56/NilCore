package meter

import (
	"math"
	"testing"

	"nilcore/internal/model"
)

// almostEqual compares dollar amounts within a sub-cent epsilon so binary
// float64 rounding never fails an otherwise-correct price.
func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestPrice exercises the table over known model families (each priced at its
// real per-token rate, with input ≠ output where the vendor differs them),
// unknown ids (which must fall back to the conservative tier), longest-prefix
// resolution, and the arithmetic for a mixed input/output split. Table-driven
// per CLAUDE.md §4.
func TestPrice(t *testing.T) {
	p := NewTable()

	cases := []struct {
		name    string
		modelID string
		in, out int
		want    float64
	}{
		// Known ids: cost = in/1000*inRate + out/1000*outRate. Realistic
		// per-model rates, not a blanket max.
		{"opus 1k+1k", "claude-opus-4-8", 1000, 1000, 0.005 + 0.025},
		{"opus split", "claude-opus-4-8", 2000, 500, 0.010 + 0.0125},
		{"sonnet 1k+1k", "claude-sonnet-4-6", 1000, 1000, 0.003 + 0.015},
		{"haiku 1k+1k", "claude-haiku-4-5", 1000, 1000, 0.001 + 0.005},
		{"fable 1k+1k", "claude-fable-5", 1000, 1000, 0.010 + 0.050},

		// GPT family — distinct per-tier rates; input ≠ output.
		{"gpt-5.5 1k+1k", "gpt-5.5", 1000, 1000, 0.00125 + 0.010},
		{"gpt-5.5-pro 1k+1k", "gpt-5.5-pro", 1000, 1000, 0.015 + 0.120},
		{"gpt-5.4-mini 1k+1k", "gpt-5.4-mini", 1000, 1000, 0.00025 + 0.002},

		// Newly reachable ids (P15): GPT-5.x standard snapshots and the o-series
		// reasoning models now land on a real tier instead of the fallback floor.
		{"gpt-5.4 standard", "gpt-5.4-2026-05", 1000, 1000, 0.00125 + 0.010},
		{"gpt-5 standard", "gpt-5-turbo", 1000, 1000, 0.00125 + 0.010},
		{"o3 reasoning", "o3-mini", 1000, 1000, 0.002 + 0.008},
		{"o4 reasoning", "o4-mini-high", 1000, 1000, 0.003 + 0.012},
		{"o1 reasoning", "o1-preview", 1000, 1000, 0.015 + 0.060},

		// OpenAI-compatible / self-hosted open-weight ids reachable via the P15
		// compat provider now resolve to a realistic hosted rate.
		{"llama compat", "llama-3.3-70b", 1000, 1000, 0.0009 + 0.0009},
		{"mistral compat", "mistral-large", 1000, 1000, 0.002 + 0.006},
		{"qwen compat", "qwen-2.5-72b", 1000, 1000, 0.0009 + 0.0009},
		{"deepseek compat", "deepseek-v3", 1000, 1000, 0.0014 + 0.0028},
		{"gemini-2 compat", "gemini-2.0-flash", 1000, 1000, 0.00125 + 0.010},

		// OpenRouter — fusion is priced high (cumulative panel); the generic
		// openrouter prefix is the per-provider fallback.
		{"openrouter fusion 1k+1k", "openrouter/fusion", 1000, 1000, 0.020 + 0.150},
		{"openrouter provider/model", "openrouter/anthropic/claude-x", 1000, 1000, 0.015 + 0.120},

		// A vendor-namespaced SERVED id (the form OpenRouter echoes in response.model)
		// resolves to its real tier via the post-'/' segment — NOT the conservative floor
		// ($0.020/$0.150). This is exactly the served-model-pricing case in meter.go.
		{"served vendor/model opus", "anthropic/claude-opus-4-8", 1000, 1000, 0.005 + 0.025},

		// Longest-prefix wins: opus is more specific than the generic claude tier.
		{"opus beats generic claude", "claude-opus-4-8-fast", 1000, 0, 0.005},
		// gpt-5.5-pro is more specific than gpt-5.5, which is more specific than gpt.
		{"pro beats plain gpt-5.5", "gpt-5.5-pro-2026", 1000, 0, 0.015},
		// Unrecognized claude-* / gpt-* still resolve to their priciest family tier,
		// not the generic fallback.
		{"unknown claude family", "claude-mystery-9", 1000, 1000, 0.010 + 0.050},
		{"unknown gpt family", "gpt-9-ultra", 1000, 1000, 0.015 + 0.120},

		// Case-insensitive id resolution.
		{"uppercase id", "CLAUDE-OPUS-4-8", 1000, 0, 0.005},

		// Zero tokens cost nothing regardless of model.
		{"zero tokens", "claude-opus-4-8", 0, 0, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := p.Price(c.modelID, c.in, c.out)
			if !almostEqual(got, c.want) {
				t.Errorf("Price(%q, %d, %d) = %v, want %v", c.modelID, c.in, c.out, got, c.want)
			}
		})
	}
}

// TestPriceKnownIdsAreRealistic asserts known ids are NOT all priced at the
// blanket-max fallback — the point of the table is per-model accuracy. At least
// one cheaper-than-fallback known tier must exist, and input must differ from
// output where the vendor prices them differently.
func TestPriceKnownIdsAreRealistic(t *testing.T) {
	p := NewTable()
	const in, out = 1000, 1000
	fallback := p.Price("totally-unknown-model", in, out)

	// Haiku is the cheapest tier; it must be well below the conservative fallback.
	if haiku := p.Price("claude-haiku-4-5", in, out); haiku >= fallback {
		t.Errorf("haiku %v not cheaper than fallback %v — table is blanket-max, not realistic", haiku, fallback)
	}

	// Input ≠ output on a representative model (sonnet: $3 in vs $15 out).
	inOnly := p.Price("claude-sonnet-4-6", 1000, 0)
	outOnly := p.Price("claude-sonnet-4-6", 0, 1000)
	if almostEqual(inOnly, outOnly) {
		t.Errorf("sonnet input %v == output %v — rates should differ", inOnly, outOnly)
	}
	if !(outOnly > inOnly) {
		t.Errorf("sonnet output rate %v should exceed input rate %v", outOnly, inOnly)
	}
}

// TestPriceFusionIsPricedHigh asserts openrouter/fusion (a cumulative-cost
// panel) is priced at or above the priciest single known model for the same
// usage, so a panel that may route to several premium models is never
// under-charged.
func TestPriceFusionIsPricedHigh(t *testing.T) {
	p := NewTable()
	const in, out = 1000, 1000
	fusion := p.Price("openrouter/fusion", in, out)

	knownSingles := []string{
		"claude-fable-5", "claude-opus-4-8", "gpt-5.5-pro", "gpt-5.5",
	}
	for _, id := range knownSingles {
		if single := p.Price(id, in, out); fusion < single-1e-9 {
			t.Errorf("fusion %v cheaper than single model %q %v — panel under-priced", fusion, id, single)
		}
	}
}

// TestPriceUnknownIsConservative asserts an unknown model id falls back to a
// tier at least as expensive as every known tier — never cheaper — so the
// budget ceiling can never be under-estimated by an unfamiliar provider (the
// table's reason for existing, docs/MULTI-AGENT.md §7).
func TestPriceUnknownIsConservative(t *testing.T) {
	p := NewTable()
	const in, out = 1000, 1000

	unknown := p.Price("some-brand-new-model", in, out)
	wantFloor := 0.020 + 0.150 // documented fallback FLOOR (>= every known tier)
	if !almostEqual(unknown, wantFloor) {
		t.Fatalf("unknown id priced %v, want conservative floor %v", unknown, wantFloor)
	}

	// The fallback must be >= every known tier for the same usage, or it would
	// under-charge an unfamiliar model relative to a known one.
	knownIDs := []string{
		"claude-opus-4-8", "claude-sonnet-4-6", "claude-haiku-4-5",
		"claude-fable-5", "gpt-5.5", "gpt-5.5-pro", "gpt-5.4-mini",
		"openrouter/fusion", "openrouter/x/y",
	}
	for _, id := range knownIDs {
		if known := p.Price(id, in, out); unknown < known-1e-9 {
			t.Errorf("fallback %v cheaper than known %q %v — not conservative", unknown, id, known)
		}
	}
}

// TestPriceNegativeClamped guards the ceiling: a negative token count must not
// produce a negative (ceiling-relaxing) charge.
func TestPriceNegativeClamped(t *testing.T) {
	p := NewTable()
	if got := p.Price("claude-opus-4-8", -1000, -1000); got != 0 {
		t.Errorf("Price with negative tokens = %v, want 0", got)
	}
	if got := p.Price("claude-opus-4-8", -1000, 1000); !almostEqual(got, 0.025) {
		t.Errorf("Price(-1000 in, 1000 out) = %v, want 0.025 (input clamped)", got)
	}
}

// TestTableSatisfiesPricer is a compile-time assertion that Table is a Pricer,
// matching the interface the metering decorator (P1-T01) depends on.
func TestTableSatisfiesPricer(t *testing.T) {
	var _ Pricer = NewTable()
}

// TestPriceUsageMatchesPriceForPlainSplit asserts the richer PriceUsage path is
// a strict superset of Price: with only InputTokens/OutputTokens set (no cached,
// reasoning, or cost), it returns the exact same number Price does, for known
// AND unknown ids. This locks the regression — adding the Usage-aware path never
// changes the dollar figure for the plain two-count case every existing caller
// produces.
func TestPriceUsageMatchesPriceForPlainSplit(t *testing.T) {
	p := NewTable()
	ids := []string{
		"claude-opus-4-8", "claude-sonnet-4-6", "claude-haiku-4-5", "claude-fable-5",
		"gpt-5.5", "gpt-5.5-pro", "gpt-5.4-mini", "openrouter/fusion",
		"totally-unknown-model", // unknown ⇒ both paths hit the same fallback floor
	}
	splits := [][2]int{{1000, 1000}, {2000, 500}, {0, 0}, {500, 0}, {0, 750}}
	for _, id := range ids {
		for _, s := range splits {
			plain := p.Price(id, s[0], s[1])
			usage := p.PriceUsage(id, model.Usage{InputTokens: s[0], OutputTokens: s[1]})
			if !almostEqual(plain, usage) {
				t.Errorf("PriceUsage(%q, in=%d out=%d)=%v != Price=%v", id, s[0], s[1], usage, plain)
			}
		}
	}
}

// TestPriceUsageCostOverridesEstimate asserts an authoritative Usage.CostUSD
// (what OpenRouter actually charged) wins over the local table estimate — the
// table can only drift from a real bill, so the reported figure is returned
// verbatim regardless of the token split or model id.
func TestPriceUsageCostOverridesEstimate(t *testing.T) {
	p := NewTable()

	// Huge token counts that would estimate far above the reported cost — the
	// reported cost must still win.
	got := p.PriceUsage("gpt-5.5-pro", model.Usage{
		InputTokens:  1_000_000,
		OutputTokens: 1_000_000,
		CostUSD:      0.0042,
	})
	if !almostEqual(got, 0.0042) {
		t.Errorf("PriceUsage with CostUSD=0.0042 = %v, want 0.0042 (authoritative override)", got)
	}

	// Tiny token counts that would estimate near-zero — a reported cost above
	// the estimate must also win (override is unconditional, both directions).
	got = p.PriceUsage("claude-haiku-4-5", model.Usage{
		InputTokens:  10,
		OutputTokens: 10,
		CostUSD:      0.99,
	})
	if !almostEqual(got, 0.99) {
		t.Errorf("PriceUsage with CostUSD=0.99 = %v, want 0.99 (authoritative override)", got)
	}

	// CostUSD == 0 means "not reported" → fall through to local estimation.
	est := p.PriceUsage("claude-opus-4-8", model.Usage{InputTokens: 1000, OutputTokens: 1000, CostUSD: 0})
	if !almostEqual(est, 0.005+0.025) {
		t.Errorf("PriceUsage with CostUSD=0 = %v, want local estimate %v", est, 0.005+0.025)
	}
}

// TestPriceUsageCachedTokensDiscounted asserts CachedTokens (input served from a
// prompt cache) bills at the reduced rate — strictly cheaper than charging the
// same input at the full rate, but never free.
func TestPriceUsageCachedTokensDiscounted(t *testing.T) {
	p := NewTable()
	// Opus: input rate 0.005/1k. 1000 input total, of which 800 are cached.
	// Fresh 200 @ full + cached 800 @ (full * cachedDiscount).
	const inRate = 0.005
	want := 200.0/1000*inRate + 800.0/1000*inRate*cachedDiscount
	got := p.PriceUsage("claude-opus-4-8", model.Usage{InputTokens: 1000, CachedTokens: 800})
	if !almostEqual(got, want) {
		t.Errorf("PriceUsage cached = %v, want %v", got, want)
	}

	// A cache hit must be strictly cheaper than billing all input at full rate,
	// and strictly more than free (the cached tokens still cost something).
	full := p.PriceUsage("claude-opus-4-8", model.Usage{InputTokens: 1000})
	if !(got < full-1e-12) {
		t.Errorf("cached charge %v not cheaper than full-rate %v", got, full)
	}
	if !(got > 0) {
		t.Errorf("cached charge %v should be > 0 (not free)", got)
	}

	// CachedTokens > InputTokens (mis-reported) must clamp, never go negative.
	clamped := p.PriceUsage("claude-opus-4-8", model.Usage{InputTokens: 1000, CachedTokens: 5000})
	allCached := 1000.0 / 1000 * inRate * cachedDiscount
	if !almostEqual(clamped, allCached) {
		t.Errorf("over-reported cached = %v, want all-cached %v (clamped)", clamped, allCached)
	}
	if clamped < 0 {
		t.Errorf("clamped cached charge %v must not be negative", clamped)
	}
}

// TestPriceUsageReasoningBilledAsOutput asserts ReasoningTokens (hidden thinking)
// are accounted at the output rate, added on top of the visible OutputTokens.
func TestPriceUsageReasoningBilledAsOutput(t *testing.T) {
	p := NewTable()
	// Sonnet: output rate 0.015/1k. 500 visible + 1500 reasoning = 2000 @ output.
	const outRate = 0.015
	want := 2000.0 / 1000 * outRate // input is 0
	got := p.PriceUsage("claude-sonnet-4-6", model.Usage{OutputTokens: 500, ReasoningTokens: 1500})
	if !almostEqual(got, want) {
		t.Errorf("PriceUsage reasoning = %v, want %v", got, want)
	}

	// Reasoning tokens are billed identically to the same count of output tokens.
	asOutput := p.PriceUsage("claude-sonnet-4-6", model.Usage{OutputTokens: 2000})
	if !almostEqual(got, asOutput) {
		t.Errorf("reasoning charge %v != equivalent output charge %v", got, asOutput)
	}
}

// TestPriceUsageUnknownStillConservative asserts the conservative floor survives
// the richer path: an unknown id priced through PriceUsage (with cached/reasoning
// splits) still bills at the fallback tier, never below it — the budget ceiling
// stays a hard wall (docs/MULTI-AGENT.md §7).
func TestPriceUsageUnknownStillConservative(t *testing.T) {
	p := NewTable()
	// No cached/reasoning: must equal the plain conservative floor for 1k+1k.
	got := p.PriceUsage("some-brand-new-model", model.Usage{InputTokens: 1000, OutputTokens: 1000})
	wantFloor := 0.020 + 0.150
	if !almostEqual(got, wantFloor) {
		t.Errorf("unknown id PriceUsage = %v, want conservative floor %v", got, wantFloor)
	}

	// With a cache hit the unknown id is still priced at the fallback INPUT rate
	// (discounted on the cached portion), never cheaper than the known tiers for
	// the same fresh/cached split.
	fresh, cached := 600, 400
	const flrIn, flrOut = 0.020, 0.150
	wantCached := float64(fresh)/1000*flrIn + float64(cached)/1000*flrIn*cachedDiscount + 1000.0/1000*flrOut
	gotCached := p.PriceUsage("some-brand-new-model", model.Usage{InputTokens: 1000, CachedTokens: 400, OutputTokens: 1000})
	if !almostEqual(gotCached, wantCached) {
		t.Errorf("unknown cached PriceUsage = %v, want %v", gotCached, wantCached)
	}
	for _, id := range []string{"claude-haiku-4-5", "gpt-5.4-mini", "llama-3.3-70b"} {
		known := p.PriceUsage(id, model.Usage{InputTokens: 1000, CachedTokens: 400, OutputTokens: 1000})
		if gotCached < known-1e-9 {
			t.Errorf("unknown cached %v cheaper than known %q %v — not conservative", gotCached, id, known)
		}
	}
}

// TestPriceUsageNegativeClamped guards the ceiling on the richer path: negative
// counts in any Usage field must not produce a negative (ceiling-relaxing) charge.
func TestPriceUsageNegativeClamped(t *testing.T) {
	p := NewTable()
	got := p.PriceUsage("claude-opus-4-8", model.Usage{
		InputTokens: -1000, OutputTokens: -1000, CachedTokens: -500, ReasoningTokens: -500,
	})
	if got != 0 {
		t.Errorf("PriceUsage all-negative = %v, want 0", got)
	}
}
