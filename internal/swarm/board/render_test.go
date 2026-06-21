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

// TestRenderRedactsAndEscapes is the MINOR #12 regression: a per-shard row's
// Status/Verifier/SourceURL is routed through the SAME redactor + control/markup
// sanitizer the matrix renderer uses, so a secret-looking token never reaches output and
// a control byte cannot repaint the terminal. The fields are TRUSTED (verifier-set,
// key-free by I3), but the board still defends in depth — exactly as the matrix does.
func TestRenderRedactsAndEscapes(t *testing.T) {
	// A SourceURL carrying an api_key= query param (the keyed-source leak shape) plus a
	// provider-token-shaped param; a Verifier with a raw control byte (ESC) that would
	// otherwise inject an ANSI sequence; a Status with markup metacharacters.
	const leakKey = "AKIAIOSFODNN7EXAMPLE"      // an AWS-key-shaped secret
	const ghToken = "ghp_abcdefghijklmnop12345" // a GitHub-token-shaped secret
	snap := Snapshot{
		Pass: 1, Total: 1,
		Shards: []ShardRow{{
			ID:        "s0",
			Pass:      1,
			Passed:    false,
			Status:    "fail<script>",
			Verifier:  "v\x1b[31mk", // embedded ESC — a control byte
			SourceURL: "https://api.example.com/x?api_key=" + ghToken + "&q=ok#frag-" + leakKey,
		}},
	}
	out := RenderScoreboard(snap, offStyle())

	// 1. No secret shape survives anywhere in the rendered board.
	for _, secret := range []string{ghToken, leakKey, "api_key=" + ghToken} {
		if strings.Contains(out, secret) {
			t.Errorf("rendered board leaked a secret-looking token %q:\n%q", secret, out)
		}
	}
	// 2. The api_key param value is gone (stripped by name) and a redaction marker is shown.
	if strings.Contains(out, "ghp_") || strings.Contains(out, "AKIA") {
		t.Errorf("a secret prefix survived redaction:\n%q", out)
	}
	// 3. No raw ESC / C0 control byte survives (it would have come from the Verifier).
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("a raw ESC control byte survived sanitization:\n%q", out)
	}
	// 4. Markup metacharacters in Status are escaped to inert entities (no live '<').
	if strings.Contains(out, "<script>") {
		t.Errorf("a raw <script> survived markup escaping:\n%q", out)
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Errorf("expected the escaped markup form &lt;script&gt; in:\n%q", out)
	}
	// 5. The benign part of the URL (host/path/non-key query) still renders — redaction
	// strips the key, it does not blank the whole locator.
	if !strings.Contains(out, "api.example.com") {
		t.Errorf("redaction blanked the benign part of the SourceURL:\n%q", out)
	}
}
