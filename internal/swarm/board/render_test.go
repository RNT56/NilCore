package board

// render_test.go pins RenderScoreboard's two hard guarantees: ZERO ANSI on an off Style
// (the I6 pipe/CI degrade path) and that the clean-pass headline appears ONLY when the
// snapshot's FinalCleanPass is set (a green banner never over an unverified run, I2).

import (
	"os"
	"strings"
	"testing"
	"time"

	"nilcore/internal/termui"
)

// offStyle is the zero-value Style: no ANSI, the pipe/CI degrade path.
func offStyle() termui.Style { return termui.New(nil).Style() }

// onStyle returns a colour-ON Style, built hermetically by handing termui a pty master
// (a char device) so detectStyle reports a terminal without the test process being
// attached to one. Mirrors internal/report/render's helper.
func onStyle(t *testing.T) termui.Style {
	t.Helper()
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm")
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pty available for on-Style test: %v", err)
	}
	t.Cleanup(func() { _ = ptmx.Close() })
	st := termui.New(ptmx).Style()
	if !termui.New(ptmx).Styled() {
		t.Skip("pty did not detect as a styled terminal on this host")
	}
	return st
}

// sampleSnapshot is a representative mid-run snapshot: a clean two-shard pass with a
// retry, one model's tokens, and per-shard rows carrying trusted fields only.
func sampleSnapshot(clean bool) Snapshot {
	return Snapshot{
		Pass: 2, Checked: 1, Passed: 1, Failed: 0, RetryPass: 1, Remaining: 0, Total: 2,
		FinalCleanPass: clean,
		Cost:           1.2345,
		Tokens:         150,
		Models:         []ModelTokens{{Model: "claude-opus-4-8", In: 100, Out: 50, Dollars: 0.0017}},
		RunElapsed:     90 * time.Second,
		Shards: []ShardRow{
			{ID: "a", Pass: 1, Passed: true, Status: "pass", Verifier: "vk", SourceURL: "https://example.com/a", Elapsed: 3 * time.Second},
			{ID: "c", Pass: 2, Passed: true, Status: "pass", Verifier: "vk", SourceURL: "https://example.com/c", Elapsed: 5 * time.Second},
		},
	}
}

// TestRenderOffStyleZeroANSI is the I6 guarantee: an off Style yields not one escape
// sequence, regardless of how colourful the content would be on a terminal.
func TestRenderOffStyleZeroANSI(t *testing.T) {
	for _, clean := range []bool{true, false} {
		out := RenderScoreboard(sampleSnapshot(clean), offStyle())
		if strings.Contains(out, "\033[") {
			t.Fatalf("off-Style scoreboard (clean=%v) contains an ANSI escape:\n%s", clean, out)
		}
		if out == "" {
			t.Fatalf("off-Style scoreboard (clean=%v) is empty", clean)
		}
	}
}

// TestRenderCleanHeadlineOnlyWhenFinal asserts the green "swarm clean" headline appears
// EXACTLY when FinalCleanPass is set, and the in-progress headline otherwise (I2).
func TestRenderCleanHeadlineOnlyWhenFinal(t *testing.T) {
	clean := RenderScoreboard(sampleSnapshot(true), offStyle())
	if !strings.Contains(clean, "swarm clean") {
		t.Fatalf("clean snapshot missing the clean headline:\n%s", clean)
	}

	dirty := RenderScoreboard(sampleSnapshot(false), offStyle())
	if strings.Contains(dirty, "swarm clean") {
		t.Fatalf("non-clean snapshot showed the clean headline:\n%s", dirty)
	}
	if !strings.Contains(dirty, "swarm pass") {
		t.Fatalf("non-clean snapshot missing the in-progress headline:\n%s", dirty)
	}
}

// TestRenderContent asserts the counts banner, the cost/time/token line, and the
// per-shard rows (with their trusted SourceURL) all appear.
func TestRenderContent(t *testing.T) {
	out := RenderScoreboard(sampleSnapshot(true), offStyle())
	for _, want := range []string{
		"checked 1", "passed 1", "retry-pass 1", "remaining 0",
		"cost $1.2345", "time 1m30s", "tokens 150",
		"claude-opus-4-8",
		"a", "https://example.com/a",
		"c", "https://example.com/c",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scoreboard missing %q:\n%s", want, out)
		}
	}
}

// TestRenderOnStylePaints asserts that with colour ON a clean board emits at least one
// ANSI escape (the headline/tints fire) — the inverse of the off-Style guarantee.
func TestRenderOnStylePaints(t *testing.T) {
	st := onStyle(t)
	out := RenderScoreboard(sampleSnapshot(true), st)
	if !strings.Contains(out, "\033[") {
		t.Fatalf("on-Style scoreboard emitted no ANSI:\n%q", out)
	}
}

// TestRenderEmptySnapshot asserts a zero-value snapshot renders without panicking and
// shows the in-progress (not clean) headline.
func TestRenderEmptySnapshot(t *testing.T) {
	out := RenderScoreboard(Snapshot{}, offStyle())
	if strings.Contains(out, "swarm clean") {
		t.Fatalf("empty snapshot showed clean headline:\n%s", out)
	}
}
