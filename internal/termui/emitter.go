package termui

import (
	"fmt"
	"strings"
	"sync"

	"nilcore/internal/emit"
	"nilcore/internal/verb"
)

// ConsoleEmitter renders an agent's live reasoning to a Console: it implements
// emit.Emitter (read-only over the loops' events — it never produces them) and
// owns the "thinking" spinner that fills the model-wait gaps. Streamed tokens
// flow inline (stopping the spinner); the per-step intent, the tool the loop
// reaches for, the verifier's verdict, and a steer acknowledgement each scroll as
// a coloured glyph line; and after a tool the spinner resumes to cover the
// execution + next-think gap.
//
// The Console owns all terminal state (the live line, the stream, styling) and is
// internally locked, so the emitter only guards its own small fields (the route
// flavour, the running token estimate, the spinner seed) and NEVER holds its lock
// across a Console call — keeping the two locks strictly ordered and race-clean.
type ConsoleEmitter struct {
	c   *Console
	mu  sync.Mutex
	cat verb.Category // route flavour for the spinner verbs
	chr int           // streamed characters this drive (≈4 per token)
	seq uint64        // spinner seed source, bumped per spin so verbs vary
}

// NewEmitter returns a ConsoleEmitter rendering to c, with cat the initial route
// flavour for the thinking spinner.
func NewEmitter(c *Console, cat verb.Category) *ConsoleEmitter {
	return &ConsoleEmitter{c: c, cat: cat}
}

// Begin starts the thinking spinner for a new drive of route flavour cat (called
// by the REPL when a drive goes Working). It resets the token estimate.
func (e *ConsoleEmitter) Begin(cat verb.Category) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.cat = cat
	e.chr = 0
	e.mu.Unlock()
	e.spin()
}

// End stops the live spinner when a drive settles (called by the REPL on Idle).
func (e *ConsoleEmitter) End() {
	if e != nil {
		e.c.StopSpin()
	}
}

// Emit renders one event. A nil receiver is a safe no-op (the gated default).
func (e *ConsoleEmitter) Emit(ev emit.Event) {
	if e == nil {
		return
	}
	st := e.c.Style()
	switch ev.Kind {
	case emit.KindToken:
		// A streamed token: flows inline (Console.Token stops the spinner).
		e.mu.Lock()
		e.chr += len(ev.Text)
		e.mu.Unlock()
		e.c.Token(ev.Text)
	case emit.KindIntent:
		// Per-step reasoning (the steer surface) when the reply did not stream: a
		// dim, rail-prefixed line.
		e.c.Line(st.Dim("  · " + ev.Text))
	case emit.KindTool:
		// The harness-authored action intent (e.g. "about to run: …") — never raw
		// tool output. A cyan ▸ marks it in the tool tree; the spinner then resumes
		// to cover the tool's execution and the next think.
		e.c.Line("  " + st.Info("▸") + " " + ev.Text)
		e.spin()
	case emit.KindVerify:
		glyph, paint := "✓", st.Success
		if isFailure(ev.Text) {
			glyph, paint = "✗", st.Danger
		}
		e.c.Line("  " + paint(glyph) + " " + ev.Text)
	case emit.KindSteerAck:
		// Acknowledge the steer, then resume the spinner — the loop is re-thinking.
		e.c.Line("  " + st.Warn("⤺ "+ev.Text))
		e.spin()
	case emit.KindAsk:
		// A question posed to the operator (ask_user): render a styled box from the
		// STRUCTURED payload when present (the native REPL widget), else fall back to the
		// plain "? "+Text line. The spinner is NOT resumed — the drive is parked waiting
		// on the human; the REPL settles and shows the prompt for the typed answer.
		if ev.Ask != nil {
			e.renderAskBox(st, ev.Ask)
		} else {
			e.c.Line("  " + st.Info("? ") + ev.Text)
		}
	default:
		e.c.Line("  " + ev.Text)
	}
}

// renderAskBox draws a styled bordered prompt for an ask_user question from the
// structured payload: a header with the batch position, the question, a numbered
// choice menu (label + dim detail), and a hint line. It is pure presentation over the
// same payload the answer grammar expects — the user still types a number (or "1,3",
// or free text), which the REPL's single stdin reader hands to Session.Turn. No raw
// terminal mode / arrow-keys (the line-REPL is not full-screen — that is the TUI's
// modal); the box just makes the question legible and distinct from reasoning.
func (e *ConsoleEmitter) renderAskBox(st Style, a *emit.AskPrompt) {
	head := "question"
	if a.Total > 1 {
		head = fmt.Sprintf("question %d/%d", a.Index, a.Total)
	}
	rule := 46 - len(head)
	if rule < 0 {
		rule = 0
	}
	e.c.Line("  " + st.Info("╭─ "+st.Bold(head)+" "+strings.Repeat("─", rule)))
	e.c.Line("  " + st.Info("│ ") + st.Bold(strings.TrimSpace(a.Question)))
	for i, c := range a.Choices {
		line := "  " + st.Info("│ ") + "  " + st.Warn(fmt.Sprintf("%d", i+1)) + "  " + c.Label
		if strings.TrimSpace(c.Detail) != "" {
			line += st.Dim("  · " + strings.TrimSpace(c.Detail))
		}
		e.c.Line(line)
	}
	var hint string
	switch {
	case len(a.Choices) == 0:
		hint = "type your answer"
	case a.MultiSelect:
		hint = "type the numbers (e.g. 1,3), add \"; note\" if you like — or type your own answer"
	default:
		hint = "type a number — or type your own answer"
	}
	e.c.Line("  " + st.Info("╰ ") + st.Dim(hint))
}

// spin (re)starts the thinking spinner with a fresh seed so the verb cycles, and
// the running token estimate as its live counter. It releases the emitter lock
// before calling the Console, so the two locks never nest.
func (e *ConsoleEmitter) spin() {
	e.mu.Lock()
	e.seq++
	seed := e.seq * 0x9e3779b97f4a7c15
	cat := e.cat
	e.mu.Unlock()
	e.c.Spin("", seed, cat, e.tokens)
}

// tokens is the live token estimate for the spinner meta (~4 chars/token). It is
// called by the Console ticker (under the Console lock), so it takes only the
// emitter lock and never calls back into the Console — no lock inversion.
func (e *ConsoleEmitter) tokens() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.chr / 4
}

// isFailure reports whether a verify line reads as a failure, so the glyph is ✗.
func isFailure(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "did not pass") || strings.Contains(l, "not verified") || strings.Contains(l, "failed")
}
