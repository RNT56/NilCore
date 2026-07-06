// Package ask is the outbound (loop→user) clarification seam — the mirror of
// internal/inbox. The drive goroutine, parked inside the native loop's ask_user
// dispatch, calls Box.Ask(ctx, questions) and blocks; the front-door reader feeds
// each typed principal line to Box.Resolve. Ask runs the per-question collection
// loop INTERNALLY: it renders each question through the emitter, waits for one
// reply, applies the resolution rules (with one re-prompt on an empty reply), and
// advances — so a "batch" of up to 5 questions is a presentation loop over a single
// one-line rendezvous, and the session Phase machine sees ONE park spanning the
// whole batch (no per-sub-question cursor leaks into it).
//
// It is a small leaf over internal/backend (the AskQuestion/AskAnswer value types
// and ErrAskTimeout, declared in the frozen-contract package — backend never imports
// this package, so its import graph stays clean) and internal/emit (to render the
// question). Single-flight: at most one batch outstanding (the loop blocks on it).
// The wall-clock backstop bounds operator ABSENCE, reset per question; on timeout the
// answers collected so far are returned with backend.ErrAskTimeout — never dropped.
package ask

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"nilcore/internal/backend"
	"nilcore/internal/emit"
)

// defaultBackstop bounds how long Ask waits for ANY one answer before proceeding on
// assumptions — it bounds operator absence, not deliberation (it resets per answer).
const defaultBackstop = 30 * time.Minute

// maxCustom clamps a free-form answer (the trusted principal turn) so a pathological
// paste cannot bloat the model context — the I7 length-clamp.
const maxCustom = 4000

// Box is the loop→user rendezvous. The drive goroutine calls Ask and blocks; the
// front-door reader calls Resolve once per shown question. All shared state is mutex-
// guarded and the reply channel is cap-1, so Box is race-free and owns no goroutine.
type Box struct {
	mu       sync.Mutex
	pending  bool // a batch is collecting (single-flight; also the Resolve gate)
	replies  chan string
	em       emit.Emitter
	backstop time.Duration
}

// New returns a ready Box rendering questions to em (the conversation's reasoning
// sink). A nil emitter still works (the question is not shown, but Resolve still
// drives collection) — though in practice the front door always wires one.
func New(em emit.Emitter) *Box {
	return &Box{replies: make(chan string, 1), em: em, backstop: defaultBackstop}
}

// Resolve delivers one principal line as the answer to the currently-shown question.
// It is non-blocking and returns true only when a batch is outstanding and the line
// was accepted; false otherwise (no batch in flight, or a reply already buffered), so
// the caller (Session.Turn) can fall back to the normal follow-up path. Single
// consumer (Ask), cap-1 channel — a Resolve racing the batch's end is simply false.
func (b *Box) Resolve(line string) bool {
	b.mu.Lock()
	pending := b.pending
	b.mu.Unlock()
	if !pending {
		return false
	}
	select {
	case b.replies <- line:
		return true
	default:
		return false
	}
}

// Pending reports whether a batch is currently collecting. It is a race-free read of
// the single-flight gate, used to observe the parked state (e.g. session tests
// synchronize on it as the box-level mirror of the AwaitingInput phase). Front-door
// surfaces render the question from the emitted AskPrompt event, not from this flag.
func (b *Box) Pending() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pending
}

// Ask poses the questions one after another and blocks until all are answered, the
// backstop fires (returning partial answers + backend.ErrAskTimeout), or ctx is
// cancelled (returning partial answers + ctx.Err()). It returns exactly one
// AskAnswer per answered question (a shorter slice on early exit).
func (b *Box) Ask(ctx context.Context, qs []backend.AskQuestion) ([]backend.AskAnswer, error) {
	b.mu.Lock()
	if b.pending {
		b.mu.Unlock()
		return nil, errors.New("an ask is already in progress")
	}
	b.pending = true
	// Drain any stale buffered reply so this batch starts clean.
	select {
	case <-b.replies:
	default:
	}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.pending = false
		// Drain any unconsumed reply so the next batch starts clean.
		select {
		case <-b.replies:
		default:
		}
		b.mu.Unlock()
	}()

	answers := make([]backend.AskAnswer, 0, len(qs))
	for i := range qs {
		line, timedOut, err := b.collectOne(ctx, qs[i], i, len(qs))
		if err != nil {
			return answers, err // ctx cancel: partial + err
		}
		if timedOut {
			return answers, backend.ErrAskTimeout // operator absent: partial, proceed
		}
		answers = append(answers, resolveReply(qs[i], line))
	}
	return answers, nil
}

// collectOne renders one question, waits for a reply (resetting the absence backstop
// per wait), and re-prompts once on a first empty reply. A second empty reply returns
// "" (the caller resolves it to "declined — you decide"), so a garbage/empty path
// always terminates and never wedges.
func (b *Box) collectOne(ctx context.Context, q backend.AskQuestion, idx, total int) (line string, timedOut bool, err error) {
	b.emitQuestion(q, idx, total)
	for attempt := 0; ; attempt++ {
		timer := time.NewTimer(b.backstop)
		select {
		case l := <-b.replies:
			timer.Stop()
			if strings.TrimSpace(l) == "" && attempt == 0 {
				b.emit("(no answer) — enter a choice number or type an answer, or press enter again to let me decide:")
				continue
			}
			return l, false, nil
		case <-timer.C:
			return "", true, nil
		case <-ctx.Done():
			timer.Stop()
			return "", false, ctx.Err()
		}
	}
}

func (b *Box) emit(text string) {
	if b.em != nil {
		b.em.Emit(emit.Event{Kind: emit.KindAsk, Text: text})
	}
}

// emitQuestion surfaces one question carrying BOTH the plain rendered Text (the
// authoritative menu every surface can fall back to) and the STRUCTURED *emit.AskPrompt
// a widget surface (TUI modal / styled REPL box / channel buttons) renders natively.
// The structured payload is an emit-local mirror of the backend question, so emit stays
// an import-leaf. A widget still answers by FORMATTING its selection into the line
// grammar resolveReply parses — Text and the payload never diverge.
func (b *Box) emitQuestion(q backend.AskQuestion, idx, total int) {
	if b.em == nil {
		return
	}
	choices := make([]emit.AskChoice, len(q.Choices))
	for i, c := range q.Choices {
		choices[i] = emit.AskChoice{Label: c.Label, Detail: strings.TrimSpace(c.Detail)}
	}
	b.em.Emit(emit.Event{
		Kind: emit.KindAsk,
		Text: renderQuestion(q, idx, total),
		Ask: &emit.AskPrompt{
			Index:       idx + 1,
			Total:       total,
			Question:    strings.TrimSpace(q.Prompt),
			Choices:     choices,
			MultiSelect: q.MultiSelect,
		},
	})
}

// renderQuestion formats one question, its numbered menu, and the free-form hint.
func renderQuestion(q backend.AskQuestion, idx, total int) string {
	var sb strings.Builder
	if total > 1 {
		fmt.Fprintf(&sb, "[%d/%d] %s", idx+1, total, strings.TrimSpace(q.Prompt))
	} else {
		sb.WriteString(strings.TrimSpace(q.Prompt))
	}
	for i, c := range q.Choices {
		fmt.Fprintf(&sb, "\n  [%d] %s", i+1, c.Label)
		if strings.TrimSpace(c.Detail) != "" {
			fmt.Fprintf(&sb, " — %s", strings.TrimSpace(c.Detail))
		}
	}
	switch {
	case len(q.Choices) == 0:
		sb.WriteString("\n  type your answer:")
	case q.MultiSelect:
		sb.WriteString("\n  select one or more by number (e.g. 1,3), then ; and any note, or type your own answer:")
	default:
		sb.WriteString("\n  enter a number, or type your own answer:")
	}
	return sb.String()
}

// resolveReply maps one operator line to an AskAnswer per the normative rules:
//
//   - empty                  → declined (both empty).
//   - no choices             → free-form (Custom, verbatim, clamped).
//   - single-select          → a bare in-range integer selects that label; anything
//     else is free-form verbatim (no trailing-custom splitting).
//   - multi-select           → split on the first ';'; if every token left of it is a
//     valid in-range index, those labels are Selected (deduped, menu order) and the
//     right side is Custom; otherwise the whole line is free-form.
//
// Selected entries are ALWAYS verbatim model-authored labels by index lookup — an
// unresolved index makes the line free-form (never a nudge, never typed-text matching).
func resolveReply(q backend.AskQuestion, line string) backend.AskAnswer {
	t := strings.TrimSpace(line)
	if t == "" {
		return backend.AskAnswer{}
	}
	if len(q.Choices) == 0 {
		return backend.AskAnswer{Custom: clamp(t)}
	}
	if !q.MultiSelect {
		if n, err := strconv.Atoi(t); err == nil && n >= 1 && n <= len(q.Choices) {
			return backend.AskAnswer{Selected: []string{q.Choices[n-1].Label}}
		}
		return backend.AskAnswer{Custom: clamp(t)}
	}
	idxPart, customPart := t, ""
	if k := strings.IndexByte(t, ';'); k >= 0 {
		idxPart, customPart = strings.TrimSpace(t[:k]), strings.TrimSpace(t[k+1:])
	}
	sel, ok := parseIndices(idxPart, q.Choices)
	if !ok {
		return backend.AskAnswer{Custom: clamp(t)}
	}
	return backend.AskAnswer{Selected: sel, Custom: clamp(customPart)}
}

// parseIndices parses a comma/space-separated index list against choices: every token
// must be a valid in-range integer (else ok=false → the caller treats the whole reply
// as free-form, never silently dropping a token). Returns the chosen labels deduped in
// MENU order.
func parseIndices(s string, choices []backend.AskChoice) ([]string, bool) {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	if len(fields) == 0 {
		return nil, false
	}
	picked := make([]bool, len(choices))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 1 || n > len(choices) {
			return nil, false
		}
		picked[n-1] = true
	}
	var out []string
	for i, p := range picked {
		if p {
			out = append(out, choices[i].Label)
		}
	}
	return out, true
}

// clamp bounds a free-form answer to maxCustom runes (I7 length-clamp), cutting on a
// rune boundary so the kept text is never invalid UTF-8.
func clamp(s string) string {
	r := []rune(s)
	if len(r) <= maxCustom {
		return s
	}
	return string(r[:maxCustom]) + "…"
}
