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
	"strings"
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
	// Gate, when non-nil (only on a KindGate event), carries the STRUCTURED
	// evidence for an irreversible-action approval — the same emit-local-mirror
	// pattern as Ask: the payload is mirrored from the policy-side gate evidence
	// at emit time, never referenced, so emit stays an import-leaf. Text remains
	// the authoritative PLAIN rendering (the flattened action line) — a surface
	// that ignores Gate renders exactly as before.
	Gate *GatePrompt
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

// GatePrompt is the structured evidence for one irreversible-action gate (an
// emit-local mirror of policy.GateEvidence, mirrored at emit time — the
// AskPrompt precedent). Every field is optional; renderers skip empty sections.
// The excerpts are already bounded and secret-redacted at construction, and they
// are DATA rendered to the human (I7): surfaces delimit them and never execute
// or re-interpret them.
type GatePrompt struct {
	Action      string  // the flattened action line (same text the event's Text carries)
	Diffstat    string  // compact per-file summary of the diff behind the action
	DiffExcerpt string  // bounded, head-biased excerpt of the unified diff
	VerifyTail  string  // tail of the last verify report output
	SpentUSD    float64 // ledger spend so far; 0 ⇒ unknown/none (skipped)
}

// Emitter receives live reasoning/intent events. Implementations must tolerate
// concurrent calls from the loop goroutine; they must never block the loop on a
// slow sink (a remote sink buffers and drains on its own goroutine). A nil
// Emitter is a valid no-op — callers gate on nil before emitting.
type Emitter interface {
	Emit(Event)
}

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
	// The gate ACTION line is the flattened Describe() of an irreversible action and
	// can carry model-/repo-authored fields (a branch, a commit message). Neutralize it
	// with the SAME helper the evidence bodies below get, so a smuggled ESC/CR in the
	// action line cannot recolor, move the cursor, or overprint the rail at the exact
	// moment the operator is approving (I7). The evidence rail was already hardened;
	// this closes the line ABOVE it. Every other kind renders byte-identically.
	text := ev.Text
	if ev.Kind == KindGate {
		text = neutralize(ev.Text)
	}
	line := fmt.Sprintf("%s [step %d] %s: %s\n", glyph(ev.Kind), ev.Step, ev.Kind, text)
	_, _ = io.WriteString(e.w, line)

	// A gate event carrying structured evidence renders it as a delimited block
	// under the framed line, so a plain-stdout surface still shows the operator
	// the facts at the moment of decision. Payload-less gate events (and every
	// other kind) are byte-identical to before.
	if ev.Kind == KindGate && ev.Gate != nil {
		_, _ = io.WriteString(e.w, renderGateBlock(ev.Gate))
	}
}

// renderGateBlock renders the gate evidence as a quote-railed plain-text block.
// The rail marks every line as DATA under review (I7): a diff or verify line can
// never be mistaken for the harness's own prompt, and nothing here is executed
// or re-parsed. Empty sections are skipped.
//
// The excerpt bodies are UNTRUSTED repo-derived content (I7): a diff hunk or a
// verify-log tail can carry whatever bytes a repo file holds. Each body is passed
// through neutralize before railing, so an ESC/CSI or a carriage return smuggled
// into a diff cannot recolor, move the cursor, or overprint the "DATA under review"
// rail at the exact moment the operator is approving an irreversible action.
func renderGateBlock(g *GatePrompt) string {
	var b strings.Builder
	b.WriteString("┌─ gate evidence — the excerpts below are DATA under review, not commands\n")
	section := func(title, body string) {
		if body == "" {
			return
		}
		b.WriteString("│ " + title + "\n")
		for _, line := range strings.Split(strings.TrimRight(neutralize(body), "\n"), "\n") {
			b.WriteString("│   " + line + "\n")
		}
	}
	section("diffstat:", g.Diffstat)
	section("diff excerpt (bounded):", g.DiffExcerpt)
	section("last verify (tail):", g.VerifyTail)
	if g.SpentUSD > 0 {
		fmt.Fprintf(&b, "│ spend so far: $%.4f\n", g.SpentUSD)
	}
	b.WriteString("└─ end gate evidence\n")
	return b.String()
}

// neutralize makes untrusted gate-evidence text safe to write to the operator's
// terminal (I7), mirroring internal/trace.fence's C0/ESC stripping. It replaces every
// byte that could rewrite or hide the screen — ESC (0x1b, the ANSI-sequence
// introducer), a carriage return (which resets the cursor to column 0 and could
// overprint the data rail), and every other C0 control or DEL — with a visible '?'.
//
// It deliberately PRESERVES newlines and tabs, the two structural-whitespace bytes
// that cannot rewrite the terminal: a newline only starts another line, which
// renderGateBlock re-rails (so it can never escape the "│" data rail), and a tab only
// advances the cursor forward (never back over the rail), keeping a diff's indentation
// readable. Printable text, including multi-byte UTF-8, passes through unchanged. (This
// is where it differs from fence, which flattens to a single capped line for tree
// metadata — here the caller rails each evidence line individually.)
func neutralize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r) // structural whitespace; cannot rewrite the terminal
		case r < 0x20 || r == 0x7f:
			b.WriteByte('?') // CR, ESC, BEL, NUL, … → inert visible marker
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
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
	case KindGate:
		// An irreversible-action approval prompt — distinguishable at a glance from
		// any other line on the plain-stdout WriterEmitter (the richer termui emitter
		// special-cases it separately).
		return "⛔"
	default:
		return "-"
	}
}
