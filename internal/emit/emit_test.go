package emit

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

// nilSafe asserts that every nil/no-op form of an Emitter is safe to call.
func TestNilAndNopAreSafe(t *testing.T) {
	// A nil interface value gated by the caller is the documented default; the
	// loop checks `if e != nil` before emitting, but a nil *WriterEmitter must
	// itself be a safe no-op so a writer built from a nil io.Writer is harmless.
	var nilWriter *WriterEmitter
	nilWriter.Emit(Event{Kind: KindIntent, Text: "should not panic", Step: 1})

	if got := NewWriter(nil); got != nil {
		t.Fatalf("NewWriter(nil) = %v, want nil emitter", got)
	}

	// NopEmitter discards without touching anything.
	var e Emitter = NopEmitter{}
	e.Emit(Event{Kind: KindTool, Text: "noop", Step: 2})
}

// TestWriterRendersKindStepText asserts every kind renders Kind, Step, and Text
// on exactly one line.
func TestWriterRendersKindStepText(t *testing.T) {
	tests := []struct {
		name  string
		ev    Event
		glyph string
	}{
		{"intent", Event{Kind: KindIntent, Text: "scaffold the handler", Step: 0}, "·"},
		{"tool", Event{Kind: KindTool, Text: "about to run: go test ./...", Step: 3}, "→"},
		{"verify", Event{Kind: KindVerify, Text: "verify passed", Step: 7}, "✓"},
		{"steer_ack", Event{Kind: KindSteerAck, Text: "folding steer", Step: 4}, "!"},
		{"ask", Event{Kind: KindAsk, Text: "which framework?", Step: 2}, "?"},
		{"gate", Event{Kind: KindGate, Text: "merge to main", Step: 5}, "⛔"},
		{"unknown kind", Event{Kind: "mystery", Text: "x", Step: 1}, "-"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			em := NewWriter(&buf)
			if em == nil {
				t.Fatal("NewWriter(&buf) returned nil")
			}
			em.Emit(tt.ev)

			out := buf.String()
			if strings.Count(out, "\n") != 1 {
				t.Fatalf("want exactly one line, got %q", out)
			}
			if !strings.HasPrefix(out, tt.glyph) {
				t.Errorf("line %q does not start with glyph %q", out, tt.glyph)
			}
			// Kind, Step, and Text must all be present in the rendered line.
			if !strings.Contains(out, tt.ev.Kind) {
				t.Errorf("line %q missing kind %q", out, tt.ev.Kind)
			}
			if !strings.Contains(out, "step "+itoa(tt.ev.Step)) {
				t.Errorf("line %q missing step %d", out, tt.ev.Step)
			}
			if !strings.Contains(out, tt.ev.Text) {
				t.Errorf("line %q missing text %q", out, tt.ev.Text)
			}
		})
	}
}

// TestNilSafeToken asserts a nil *WriterEmitter tolerates a KindToken event.
func TestNilSafeToken(t *testing.T) {
	var nilWriter *WriterEmitter
	nilWriter.Emit(Event{Kind: KindToken, Text: "tok", Step: 1})
}

// TestTokensRenderRawInline asserts a run of KindToken events concatenates into
// one continuous line with no per-token glyph/step framing and no trailing
// newlines between tokens.
func TestTokensRenderRawInline(t *testing.T) {
	var buf bytes.Buffer
	em := NewWriter(&buf)

	for _, tok := range []string{"Hel", "lo, ", "wor", "ld"} {
		em.Emit(Event{Kind: KindToken, Text: tok, Step: 0})
	}

	out := buf.String()
	if out != "Hello, world" {
		t.Fatalf("tokens did not render raw+inline: got %q, want %q", out, "Hello, world")
	}
	if strings.Contains(out, "\n") {
		t.Errorf("streamed tokens must not emit a newline: %q", out)
	}
	if strings.ContainsAny(out, "·→✓!-") {
		t.Errorf("streamed tokens must not be framed with a glyph: %q", out)
	}
	if strings.Contains(out, "step ") || strings.Contains(out, KindToken) {
		t.Errorf("streamed tokens must not carry step/kind framing: %q", out)
	}
}

// TestFramedEventBreaksTokenLine asserts a framed event following a run of
// tokens starts on a fresh line, so the streamed line is closed before the next
// glyph-framed line.
func TestFramedEventBreaksTokenLine(t *testing.T) {
	var buf bytes.Buffer
	em := NewWriter(&buf)

	em.Emit(Event{Kind: KindToken, Text: "thinking", Step: 0})
	em.Emit(Event{Kind: KindToken, Text: "...", Step: 0})
	em.Emit(Event{Kind: KindTool, Text: "go test ./...", Step: 3})

	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines (token run + framed), got %d: %q", len(lines), out)
	}
	if lines[0] != "thinking..." {
		t.Errorf("first line should be the raw token run, got %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "→") || !strings.Contains(lines[1], "go test ./...") {
		t.Errorf("framed event did not start cleanly on its own line: %q", lines[1])
	}
}

// TestFramedEventWithoutPriorTokenIsUnchanged asserts the newline-break only
// fires after an open token line: a framed event with no preceding token emits
// exactly one line, preserving the pre-existing rendering.
func TestFramedEventWithoutPriorTokenIsUnchanged(t *testing.T) {
	var buf bytes.Buffer
	em := NewWriter(&buf)

	em.Emit(Event{Kind: KindIntent, Text: "start", Step: 1})

	out := buf.String()
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("want exactly one line with no leading break, got %q", out)
	}
	if strings.HasPrefix(out, "\n") {
		t.Errorf("framed event must not be preceded by a stray newline: %q", out)
	}
}

// TestWriterConcurrentEmit asserts concurrent emits do not interleave on the
// underlying writer (one whole line per Emit) and are race-free under -race.
func TestWriterConcurrentEmit(t *testing.T) {
	var buf bytes.Buffer
	em := NewWriter(&buf)

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(step int) {
			defer wg.Done()
			em.Emit(Event{Kind: KindIntent, Text: "concurrent", Step: step})
		}(i)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != n {
		t.Fatalf("got %d lines, want %d (interleaved write)", len(lines), n)
	}
	for _, l := range lines {
		if !strings.Contains(l, "concurrent") || !strings.HasPrefix(l, "·") {
			t.Fatalf("torn line: %q", l)
		}
	}
}

// itoa is a tiny stdlib-free int formatter for the assertions above so the test
// does not depend on the production formatting verbatim.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// TestWriterRendersGateEvidence: a KindGate event carrying a GatePrompt renders a
// delimited evidence block under the framed line — with every empty section
// skipped — while a payload-less gate stays the single framed line (pinned by
// TestWriterRendersKindStepText's exactly-one-line assertion).
func TestWriterRendersGateEvidence(t *testing.T) {
	var buf bytes.Buffer
	em := NewWriter(&buf)
	em.Emit(Event{Kind: KindGate, Text: "promote-to-base main", Gate: &GatePrompt{
		Action:     "promote-to-base main",
		Diffstat:   "2 file(s) changed, +10 −3",
		VerifyTail: "all checks passed",
		SpentUSD:   1.5,
	}})
	out := buf.String()
	for _, want := range []string{
		"⛔",                               // the framed gate line still leads
		"DATA under review, not commands", // I7: delimited as data
		"│ diffstat:",
		"2 file(s) changed, +10 −3",
		"│ last verify (tail):",
		"all checks passed",
		"spend so far: $1.5000",
		"└─ end gate evidence",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// The empty diff-excerpt section is skipped, not rendered as a bare header.
	if strings.Contains(out, "diff excerpt") {
		t.Errorf("empty section must be skipped:\n%s", out)
	}
	// Every evidence line is quote-railed so it reads as data, never as a prompt.
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.Contains(line, "all checks passed") && !strings.HasPrefix(line, "│") {
			t.Errorf("evidence line not railed: %q", line)
		}
	}
}
