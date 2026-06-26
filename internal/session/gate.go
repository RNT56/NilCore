package session

// gate.go is the chat front door's session-backed irreversible-action approver. The
// terminal REPL has a SINGLE stdin reader; a ConsoleApprover that ALSO reads os.Stdin
// would race it (two bufio readers on one fd). Instead, this approver parks the session
// in AwaitingGate, surfaces the action through the reasoning sink, and blocks until a
// typed Turn answers y/n — exactly the AwaitingInput/ask pattern, applied to the gate.
// It is wired ONLY in the chat REPL (serve uses Channel.Ask; the TUI uses its own modal
// approver), so the REPL stays the sole stdin reader and AwaitingGate becomes real.
//
// Fail-closed (I3): a ctx-cancel (Cancel / shutdown) returns DENY, parity with
// ConsoleApprover's EOF→deny, so an irreversible action is never auto-approved.

import (
	"context"
	"strings"

	"nilcore/internal/emit"
	"nilcore/internal/eventlog"
	"nilcore/internal/policy"
)

// gateApprover is the session-backed policy.Approver bound to one drive's ctx.
type gateApprover struct {
	s   *Session
	ctx context.Context
}

// Approve parks the session and blocks for the typed y/n answer.
func (a *gateApprover) Approve(action string) bool { return a.s.approveViaTurn(a.ctx, action) }

// NewGateApprover returns a session-backed approver bound to ctx (the drive ctx). The
// chat REPL front door wires it in place of ConsoleApprover.
func (s *Session) NewGateApprover(ctx context.Context) policy.Approver {
	return &gateApprover{s: s, ctx: ctx}
}

// approveViaTurn flips Phase=AwaitingGate, surfaces the action, and blocks until a typed
// Turn delivers a y/n line (a non-y/n line re-prompts). ctx-cancel ⇒ DENY (fail-closed).
func (s *Session) approveViaTurn(ctx context.Context, action string) bool {
	s.mu.Lock()
	if s.Phase == Working {
		s.Phase = AwaitingGate
	}
	s.gatePending = true
	select { // drain any stale reply so this gate starts clean
	case <-s.gateReply:
	default:
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.gatePending = false
		if s.Phase == AwaitingGate {
			s.Phase = Working
		}
		s.mu.Unlock()
	}()

	if s.Out != nil {
		s.Out.Emit(emit.Event{Kind: emit.KindGate, Text: action})
	}
	s.Log.Append(eventlog.Event{Task: s.ID, Kind: "session_gate_ask"})
	for {
		select {
		case line := <-s.gateReply:
			ans, ok := parseYesNo(line)
			if !ok {
				if s.Out != nil {
					s.Out.Emit(emit.Event{Kind: emit.KindGate, Text: "please answer y (approve) or n (deny)"})
				}
				continue
			}
			s.Log.Append(eventlog.Event{Task: s.ID, Kind: "session_gate_answer",
				Detail: map[string]any{"approved": ans}})
			return ans
		case <-ctx.Done():
			s.Log.Append(eventlog.Event{Task: s.ID, Kind: "session_gate_answer",
				Detail: map[string]any{"approved": false, "cancelled": true}})
			return false // fail-closed: a cancelled/abandoned gate denies the irreversible action
		}
	}
}

// resolveGate delivers a typed line to a parked gate (non-blocking, single-flight). It
// returns false when no gate is outstanding (the caller falls back to the normal path).
func (s *Session) resolveGate(line string) bool {
	s.mu.Lock()
	pending := s.gatePending
	s.mu.Unlock()
	if !pending {
		return false
	}
	select {
	case s.gateReply <- line:
		return true
	default:
		return false
	}
}

// parseYesNo maps a gate answer line to (approved, recognized) — y/yes ⇒ approve,
// n/no ⇒ deny, anything else ⇒ not recognized (re-prompt). Matches ConsoleApprover's
// semantics (only an explicit yes approves).
func parseYesNo(line string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, true
	case "n", "no":
		return false, true
	}
	return false, false
}
