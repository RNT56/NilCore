package termui

import (
	"bytes"
	"strings"
	"testing"

	"nilcore/internal/emit"
	"nilcore/internal/verb"
)

// On a non-TTY the emitter renders each kind as a plain glyph line (no escapes),
// streamed tokens flow inline then close, and a verify line picks ✓/✗ by content.
func TestConsoleEmitterRendersKinds(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(New(&buf), verb.Native)

	e.Begin(verb.Native) // spinner is a no-op on a non-TTY
	e.Emit(emit.Event{Kind: emit.KindToken, Text: "Hel"})
	e.Emit(emit.Event{Kind: emit.KindToken, Text: "lo"})
	e.Emit(emit.Event{Kind: emit.KindTool, Text: "about to run: go test"})
	e.Emit(emit.Event{Kind: emit.KindVerify, Text: "checks passed"})
	e.Emit(emit.Event{Kind: emit.KindVerify, Text: "the checks did not pass"})
	e.Emit(emit.Event{Kind: emit.KindSteerAck, Text: "paused — folding your feedback"})
	e.Emit(emit.Event{Kind: emit.KindIntent, Text: "I'll add the limiter"})
	e.End()

	got := buf.String()
	if strings.Contains(got, "\033[") {
		t.Errorf("non-TTY output must carry no ANSI escapes:\n%q", got)
	}
	for _, want := range []string{
		"Hello\n",                            // tokens streamed inline, closed before the tool line
		"▸ about to run: go test\n",          // tool intent
		"✓ checks passed\n",                  // verify pass → ✓
		"✗ the checks did not pass\n",        // verify fail → ✗
		"⤺ paused — folding your feedback\n", // steer ack
		"· I'll add the limiter\n",           // reasoning
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%q", want, got)
		}
	}
}

// A KindAsk event carrying a structured AskPrompt renders the styled box: the batch
// header, the question, the numbered choice menu (label + detail), and a hint —
// while a KindAsk without a payload falls back to the plain "? " line (byte-identical).
func TestConsoleEmitterRendersAskBox(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(New(&buf), verb.Native)
	e.Emit(emit.Event{Kind: emit.KindAsk, Text: "fallback text", Ask: &emit.AskPrompt{
		Index: 2, Total: 3, Question: "Which database?", MultiSelect: false,
		Choices: []emit.AskChoice{{Label: "Postgres", Detail: "managed"}, {Label: "SQLite"}},
	}})
	got := buf.String()
	for _, want := range []string{"question 2/3", "Which database?", "1", "Postgres", "managed", "2", "SQLite", "type a number"} {
		if !strings.Contains(got, want) {
			t.Errorf("ask box missing %q in:\n%s", want, got)
		}
	}
	// Fallback: no payload ⇒ the plain "? "+Text line.
	var buf2 bytes.Buffer
	e2 := NewEmitter(New(&buf2), verb.Native)
	e2.Emit(emit.Event{Kind: emit.KindAsk, Text: "plain question"})
	if !strings.Contains(buf2.String(), "? plain question") {
		t.Errorf("no-payload KindAsk should render the plain line, got:\n%s", buf2.String())
	}
}

// On a forced-styled console, Begin starts an animated live line and End clears
// it — and a streamed token's char count feeds the spinner's token estimate.
func TestConsoleEmitterSpinnerLifecycle(t *testing.T) {
	var buf bytes.Buffer
	c := &Console{w: &buf, st: Style{on: true}}
	e := NewEmitter(c, verb.Chat)

	e.Begin(verb.Chat)
	for i := 0; i < 40; i++ { // ~160 chars ≈ 40 tokens
		e.Emit(emit.Event{Kind: emit.KindToken, Text: "word"})
	}
	if got := e.tokens(); got < 30 || got > 50 {
		t.Errorf("token estimate = %d, want ~40", got)
	}
	e.End() // must clear the live line without panicking
	if !strings.Contains(buf.String(), "\033[") {
		t.Error("a styled console should have emitted ANSI escapes")
	}
}

// A nil emitter is a safe no-op (the gated default) across the lifecycle.
func TestConsoleEmitterNilSafe(t *testing.T) {
	var e *ConsoleEmitter
	e.Begin(verb.General)
	e.Emit(emit.Event{Kind: emit.KindToken, Text: "x"})
	e.End() // none of these may panic
}
