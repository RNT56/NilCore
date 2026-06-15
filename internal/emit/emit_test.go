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
