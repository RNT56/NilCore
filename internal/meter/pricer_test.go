package meter

import (
	"math"
	"testing"
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

		// OpenRouter — fusion is priced high (cumulative panel); the generic
		// openrouter prefix is the per-provider fallback.
		{"openrouter fusion 1k+1k", "openrouter/fusion", 1000, 1000, 0.020 + 0.150},
		{"openrouter provider/model", "openrouter/anthropic/claude-x", 1000, 1000, 0.015 + 0.120},

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
