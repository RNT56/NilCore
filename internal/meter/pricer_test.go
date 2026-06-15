package meter

import (
	"math"
	"testing"
)

// almostEqual compares dollar amounts within a sub-cent epsilon so binary
// float64 rounding never fails an otherwise-correct price.
func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestPrice exercises the table over known model families, unknown ids (which
// must fall back to the conservative tier), longest-prefix resolution, and the
// arithmetic for a mixed input/output split. Table-driven per CLAUDE.md §4.
func TestPrice(t *testing.T) {
	p := NewTable()

	cases := []struct {
		name    string
		modelID string
		in, out int
		want    float64
	}{
		// Known ids: cost = in/1000*inRate + out/1000*outRate.
		{"opus 1k+1k", "claude-opus-4-8", 1000, 1000, 0.005 + 0.025},
		{"opus split", "claude-opus-4-8", 2000, 500, 0.010 + 0.0125},
		{"sonnet 1k+1k", "claude-sonnet-4-6", 1000, 1000, 0.003 + 0.015},
		{"haiku 1k+1k", "claude-haiku-4-5", 1000, 1000, 0.001 + 0.005},
		{"fable 1k+1k", "claude-fable-5", 1000, 1000, 0.010 + 0.050},
		{"gpt 1k+1k", "gpt-5.5", 1000, 1000, 0.005 + 0.025},
		{"openrouter fusion 1k+1k", "openrouter/fusion", 1000, 1000, 0.010 + 0.050},

		// Longest-prefix wins: opus is more specific than the generic claude tier.
		{"opus beats generic claude", "claude-opus-4-8-fast", 1000, 0, 0.005},
		// Unrecognized claude-* still resolves to the (most expensive) Anthropic tier,
		// not the generic fallback — both happen to be equal here, asserted explicitly.
		{"unknown claude family", "claude-mystery-9", 1000, 1000, 0.010 + 0.050},

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

// TestPriceUnknownIsConservative asserts an unknown model id falls back to the
// most expensive known tier — never cheaper — so the budget ceiling can never be
// under-estimated by an unfamiliar provider (the table's reason for existing,
// docs/MULTI-AGENT.md §7).
func TestPriceUnknownIsConservative(t *testing.T) {
	p := NewTable()
	const in, out = 1000, 1000

	unknown := p.Price("some-brand-new-model", in, out)
	want := 0.010 + 0.050 // fallback == most expensive (Fable) tier
	if !almostEqual(unknown, want) {
		t.Fatalf("unknown id priced %v, want conservative %v", unknown, want)
	}

	// The fallback must be >= every known tier for the same usage, or it would
	// under-charge an unfamiliar model relative to a known one.
	knownIDs := []string{
		"claude-opus-4-8", "claude-sonnet-4-6", "claude-haiku-4-5",
		"claude-fable-5", "gpt-5.5", "openrouter/fusion",
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
