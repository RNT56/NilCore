package trust

import (
	"strings"
	"testing"

	"nilcore/internal/termui"
)

// offStyle is the zero-value termui.Style: styling disabled, every method returns
// its input unchanged. This is the non-terminal (pipe / CI / SSH-to-dumb) path —
// the one whose output must carry ZERO ANSI escapes. termui does not export a way
// to construct an ENABLED Style from outside the package, so render tests cover
// the off path (the I6 guarantee + deterministic layout); the on path differs only
// by the wrap() escapes that termui itself unit-tests.
var offStyle termui.Style

// sampleSnapshot returns a populated snapshot for rendering tests. Built by hand
// (not via Snapshot) so the row order is explicit and the test does not depend on
// the ledger's sort — Render must render whatever order it is given, verbatim.
func sampleSnapshot() Snapshot {
	return Snapshot{
		Backends: []Stat{
			{Backend: "codex", Races: 10, Wins: 9, PassRate: 0.9},
			{Backend: "native", Races: 12, Wins: 6, PassRate: 0.5},
		},
		Configs: []ConfigStat{
			{Config: "native+opus", PassRate: 0.8, TotalCost: 1.25, Cases: 5},
		},
	}
}

// TestRenderZeroANSIOffStyle: off Style ⇒ no escape sequences anywhere in the
// output (the SSH/CI/pipe guarantee, invariant I6).
func TestRenderZeroANSIOffStyle(t *testing.T) {
	out := Render(sampleSnapshot(), offStyle)
	if strings.Contains(out, "\033") || strings.Contains(out, "\x1b") {
		t.Errorf("off-Style render contains ANSI escapes:\n%q", out)
	}
	// And the substance is present.
	for _, want := range []string{"codex", "native", "pass-rate", "native+opus", "eval configs"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
}

// TestRenderHeaderCarriesI2Reminder: the header must state, in plain words, that
// the ledger ranks while the verifier decides — so no reader mistakes a scoreboard
// for a verdict (the I2 boundary).
func TestRenderHeaderCarriesI2Reminder(t *testing.T) {
	out := Render(sampleSnapshot(), offStyle)
	if !strings.Contains(out, "the ledger ranks, the verifier still decides") {
		t.Errorf("render header is missing the I2 reminder:\n%s", out)
	}
	if !strings.Contains(out, "verifier-judged outcomes") {
		t.Errorf("render header is missing the verifier-judged note:\n%s", out)
	}
}

// TestRenderDeterministic: the same snapshot renders byte-identically every time.
func TestRenderDeterministic(t *testing.T) {
	snap := sampleSnapshot()
	a := Render(snap, offStyle)
	b := Render(snap, offStyle)
	if a != b {
		t.Errorf("Render is non-deterministic:\n--a--\n%s\n--b--\n%s", a, b)
	}
}

// TestRenderRowOrderPreserved: Render emits rows in the order the snapshot gives
// them (the snapshot owns the sort; Render must not reorder). codex precedes
// native in the sample, so codex's line must come first.
func TestRenderRowOrderPreserved(t *testing.T) {
	out := Render(sampleSnapshot(), offStyle)
	ci := strings.Index(out, "codex")
	ni := strings.Index(out, "native ") // trailing space avoids matching "native+opus"
	if ci < 0 || ni < 0 || ci > ni {
		t.Errorf("row order not preserved (codex@%d, native@%d):\n%s", ci, ni, out)
	}
}

// TestRenderEmptyLedger: a snapshot with no backends renders the "no earned
// outcomes" line and still carries the I2 header, with zero ANSI off-Style.
func TestRenderEmptyLedger(t *testing.T) {
	out := Render(Snapshot{}, offStyle)
	if strings.Contains(out, "\033") {
		t.Errorf("empty render contains ANSI escapes:\n%q", out)
	}
	if !strings.Contains(out, "no earned outcomes yet") {
		t.Errorf("empty render missing the default-defers line:\n%s", out)
	}
	if !strings.Contains(out, "the ledger ranks, the verifier still decides") {
		t.Errorf("empty render missing the I2 reminder:\n%s", out)
	}
}

// TestRenderNoConfigsOmitsConfigSection: with no eval configs, the config table
// (and its header) is omitted entirely.
func TestRenderNoConfigsOmitsConfigSection(t *testing.T) {
	snap := Snapshot{Backends: []Stat{{Backend: "native", Races: 1, Wins: 1, PassRate: 1.0}}}
	out := Render(snap, offStyle)
	if strings.Contains(out, "eval configs") {
		t.Errorf("config section rendered with no configs:\n%s", out)
	}
}
