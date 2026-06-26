package session

// ask.go is the session half of the attended ask_user seam: it owns the outbound
// ask box, the Phase=AwaitingInput park wrapper, and the ask-level dial. The native
// loop sees only backend.AskHandle (satisfied by *askAdapter); the per-question
// collection lives in internal/ask. The park is ONE AwaitingInput spanning the whole
// batch — the adapter flips Phase once on entry and once on the single Ask return, so
// no per-sub-question state leaks into the session phase machine.

import (
	"context"

	"nilcore/internal/ask"
	"nilcore/internal/backend"
	"nilcore/internal/emit"
	"nilcore/internal/eventlog"
)

// EnableAskUser turns on the attended ask_user capability, rendering questions
// through em (the conversation's reasoning sink). It is called ONLY by an interactive
// front door (the chat REPL, or a live serve thread) — never on a headless path — so a
// parked ask can always reach a present human (I3/I4). Idempotent; it also seeds the
// default ask level if unset. Enabling it is what makes a native drive advertise the
// ask_user / set_ask_level tools.
func (s *Session) EnableAskUser(em emit.Emitter) {
	s.mu.Lock()
	if s.askBox == nil {
		s.askBox = ask.New(em)
	}
	if s.State.AskLevel == 0 {
		s.State.AskLevel = askLevelNormal
	}
	s.mu.Unlock()
}

// askAdapter is the session-owned bridge satisfying backend.AskHandle: it flips
// Phase=AwaitingInput around the parked ask (making the deep park visible to the phase
// machine without leaking per-question state into it), delegates the collection to the
// ask box, and dials the per-drive budget + level from live conversation state.
type askAdapter struct {
	s   *Session
	box *ask.Box
}

// Ask parks the drive on the batch: flip into AwaitingInput, collect every answer
// through the box, then restore Working. The drive goroutine is the caller, so the
// phase flip and the block happen on it; a concurrent Turn resolves each answer.
func (a *askAdapter) Ask(ctx context.Context, qs []backend.AskQuestion) ([]backend.AskAnswer, error) {
	a.s.enterAwaitingInput()
	defer a.s.leaveAwaitingInput()
	return a.box.Ask(ctx, qs)
}

// MaxAsks is the live per-drive ask ceiling (0 ⇒ asking off).
func (a *askAdapter) MaxAsks() int { return a.s.askMaxAsks() }

// SetLevel dials the conversation's ask level on the operator's request.
func (a *askAdapter) SetLevel(spec string) (string, error) { return a.s.SetAskLevelSpec(spec) }

// enterAwaitingInput / leaveAwaitingInput flip the parked phase under s.mu, guarded so
// a concurrent Cancel (which drives Phase to Idle via the unwinding drive goroutine) is
// never clobbered: each transition fires only from its expected neighbor phase.
func (s *Session) enterAwaitingInput() {
	s.mu.Lock()
	if s.Phase == Working {
		s.Phase = AwaitingInput
	}
	s.mu.Unlock()
}

func (s *Session) leaveAwaitingInput() {
	s.mu.Lock()
	if s.Phase == AwaitingInput {
		s.Phase = Working
	}
	s.mu.Unlock()
}

// askMaxAsks is the per-drive ask_user ceiling derived from the conversation's ask
// level (0 ⇒ off). Read under s.mu so a mid-drive level change is reflected at once.
func (s *Session) askMaxAsks() int {
	s.mu.Lock()
	lvl := s.State.AskLevel
	s.mu.Unlock()
	return askBudgetFor(lvl)
}

// SetAskLevelSpec moves the conversation's ask level per spec ("less"/"more"/"off"/
// "normal"/a number) and returns a one-line ack. It is the single mutator used by BOTH
// the set_ask_level tool (the operator talking to the agent) and the /questions control
// verb — both principal-authorized paths. The level is sticky and persisted with
// WorkState (it survives a restart). An empty spec is a no-op that reports the current
// level.
func (s *Session) SetAskLevelSpec(spec string) (string, error) {
	s.mu.Lock()
	next, err := applyAskSpec(s.State.AskLevel, spec)
	if err != nil {
		s.mu.Unlock()
		return "", err
	}
	changed := next != normalizeAskLevel(s.State.AskLevel)
	s.State.AskLevel = next
	s.mu.Unlock()
	if changed {
		s.Log.Append(eventlog.Event{Task: s.ID, Kind: "session_ask_level",
			Detail: map[string]any{"level": askLevelName(next), "budget": askBudgetFor(next)}})
	}
	return askLevelAck(next), nil
}

// AskLevelName reports the current ask level name (for /status and acks).
func (s *Session) AskLevelName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return askLevelName(s.State.AskLevel)
}
