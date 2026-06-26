// Package emit is the live reasoning/intent sink for the conversational front
// door (C0-T02). As the agent loop runs it surfaces what it is about to do —
// the model's per-step intent, the tool it is reaching for, a verify verdict, a
// steer acknowledgement — so a watching user can read the agent's reasoning and
// steer mid-work.
//
// It is a stdlib-only leaf with no internal imports, so the frozen backend leaf
// (which must not import channel/session machinery) can hold an Emitter the same
// way it holds Advisor/Peer. A nil Emitter is the gated, byte-identical default:
// the loop holds a nil-able Emitter and calls Emit only when one is wired, so an
// absent sink costs nothing.
package emit

import (
	"fmt"
	"io"
	"sync"
)

// Event kinds. These are surfaced to the user, never executed: an Event is a
// description of intent, not an instruction the loop reads back.
const (
	KindIntent   = "intent"    // the model's per-step intent (its first text block)
	KindTool     = "tool"      // a tool the loop is about to run
	KindVerify   = "verify"    // the verifier's verdict
	KindSteerAck = "steer_ack" // a steer message was accepted/folded
	KindToken    = "token"     // an incremental output-text delta (a streamed token)
	KindAsk      = "ask"       // a question posed to the human operator (ask_user)
	KindGate     = "gate"      // an irreversible-action approval posed to the operator
)

// Event is one surfaced line of the agent's live reasoning. Step is the loop
// iteration it belongs to (0 for events outside a stepped loop). Keep it flat:
// the fields map directly onto one rendered line.
type Event struct {
	Kind string // one of the Kind* constants above
	Text string // the human-readable body
	Step int    // loop iteration this event belongs to
	// Ask, when non-nil (only on a KindAsk event), carries the STRUCTURED question
	// so a widget surface (the TUI modal, the styled REPL box, Telegram/Slack native
	// buttons) can render natively instead of parsing Text. nil on every other event,
	// so non-ask events stay byte-identical. These are emit-LOCAL types on purpose:
	// emit imports nothing internal (it must stay an import-leaf the frozen backend can
	// hold), so the question is mirrored from backend.AskQuestion at emit time, never
	// referenced. Text remains the authoritative PLAIN rendering — a surface that
	// ignores Ask renders exactly as before.
	Ask *AskPrompt
}

// AskPrompt is the structured form of one ask_user question (an emit-local mirror of
// backend.AskQuestion). Index/Total are 1-based position in the 1–5 batch.
type AskPrompt struct {
	Index, Total int
	Question     string
	Choices      []AskChoice
	MultiSelect  bool
}

// AskChoice is one labelled option (an emit-local mirror of backend.AskChoice).
type AskChoice struct {
	Label, Detail string
}

// Emitter receives live reasoning/intent events. Implementations must tolerate
// concurrent calls from the loop goroutine; they must never block the loop on a
// slow sink (a remote sink buffers and drains on its own goroutine). A nil
// Emitter is a valid no-op — callers gate on nil before emitting.
type Emitter interface {
	Emit(Event)
}

// NopEmitter discards every event. It exists so a non-nil zero value is always
// safe; the loop still prefers a nil check to skip the call entirely.
type NopEmitter struct{}

// Emit discards e.
func (NopEmitter) Emit(Event) {}

// WriterEmitter renders events as one human-readable line each to an io.Writer
// (the terminal's stdout for `nilcore chat`). It serializes writes with a mutex
// so concurrent emits never interleave on the underlying writer.
//
// A KindToken event is the exception: its Text is written raw and inline (no
// glyph/step framing, no trailing newline) so a run of tokens flows as one
// continuous line as the model thinks. midToken records whether the last write
// left such a line open; the next framed event flushes a newline first so it
// starts cleanly on its own line.
type WriterEmitter struct {
	mu       sync.Mutex
	w        io.Writer
	midToken bool // last write was an unterminated streamed token
}

// NewWriter returns a WriterEmitter that renders to w. A nil w yields a nil
// Emitter (the no-op gated default), so callers need no special-casing.
func NewWriter(w io.Writer) *WriterEmitter {
	if w == nil {
		return nil
	}
	return &WriterEmitter{w: w}
}

// Emit renders e. A non-token event renders as a single framed line: a
// kind-specific glyph, the step, and the text. A KindToken event renders its
// Text raw and inline so streamed tokens concatenate into one continuous line;
// the next framed event breaks that line with a newline first. A nil receiver
// is a safe no-op, so a WriterEmitter built from a nil writer behaves like an
// absent sink.
func (e *WriterEmitter) Emit(ev Event) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	if ev.Kind == KindToken {
		// Raw, inline, no trailing newline: tokens flow as one line.
		_, _ = io.WriteString(e.w, ev.Text)
		e.midToken = true
		return
	}

	// A framed event after an open token line starts on a fresh line.
	if e.midToken {
		_, _ = io.WriteString(e.w, "\n")
		e.midToken = false
	}
	line := fmt.Sprintf("%s [step %d] %s: %s\n", glyph(ev.Kind), ev.Step, ev.Kind, ev.Text)
	_, _ = io.WriteString(e.w, line)
}

// glyph maps a kind to a short prefix so the surface reads cleanly interleaved
// with the user's own typed input in the terminal.
func glyph(kind string) string {
	switch kind {
	case KindIntent:
		return "·"
	case KindTool:
		return "→"
	case KindVerify:
		return "✓"
	case KindSteerAck:
		return "!"
	case KindAsk:
		return "?"
	default:
		return "-"
	}
}
