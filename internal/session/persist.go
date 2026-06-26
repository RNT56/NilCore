package session

import (
	"context"
	"encoding/json"
	"fmt"

	"nilcore/internal/eventlog"
)

// ---------------------------------------------------------------------------
// Session persistence (C4-T01) — continue across restart.
//
// A conversation survives a process restart by persisting its BOUNDED WorkState
// (summarize.ContextSummary + the active-driver pointer + branch/last-outcome),
// NEVER a raw transcript. On Session creation Restore rehydrates the prior state
// for that conversation ID; on each terminal drive persist writes the updated
// state back. Both are BEST-EFFORT: a nil Store is in-memory only and never
// blocks, and a store error is logged (metadata-only) but never fails a Turn —
// durability is a backstop, not a rail (the verifier and the event log remain the
// authorities). The full History tail is NOT persisted: it is reconstructable
// from the append-only event log if ever needed, and WorkState is the bounded
// handoff a follow-up re-enters from (continue, not restart).
//
// Store is a narrow interface satisfied by *agent.Checkpoint's
// SaveConversation/LoadConversation pair. Declaring it here (rather than importing
// internal/agent) keeps session a leaf: it pulls in no store/backend machinery,
// only the two-method seam — the same injection discipline Router/Driver use.
// ---------------------------------------------------------------------------

// Store is the minimal persistence seam a Session needs: a single crash-atomic
// write of the bounded work-state under the conversation ID, and a read-back that
// reports a never-seen conversation as not-found (not an error). *agent.Checkpoint
// satisfies it. A nil Store makes the Session purely in-memory.
type Store interface {
	// SaveConversation durably records detail (bounded-state JSON, never a raw
	// transcript) under the conversation id, with goal as a short human label.
	SaveConversation(ctx context.Context, id, goal, detail string) error
	// LoadConversation reads back the bounded-state JSON for id. found is false
	// (nil error) for a never-seen conversation.
	LoadConversation(ctx context.Context, id string) (detail string, found bool, err error)
}

// persistedState is the on-disk shape of a Session's bounded carry-over. It is
// EXACTLY WorkState minus nothing structural — but deliberately NOT the History:
// the wire format carries only the summary, the active-driver pointer, the
// integration branch, and the data-only last outcome. The route is stored by its
// stable string name (not the int enum) so a reordered enum across versions never
// silently remaps a restored driver; an unknown name restores as RouteContinue's
// zero and the next route re-decides.
type persistedState struct {
	Goal        string   `json:"goal"`
	Constraints []string `json:"constraints,omitempty"`
	Decisions   []string `json:"decisions,omitempty"`
	Remaining   string   `json:"remaining,omitempty"`
	Active      string   `json:"active,omitempty"`
	Branch      string   `json:"branch,omitempty"`
	LastOutcome string   `json:"last_outcome,omitempty"`
	// Mode is the user-set behavioral policy, stored by its stable string name. It
	// MUST round-trip: a mode is a SAFETY posture (e.g. /plan = "do not write my
	// repo"), so a conversation that resumes after a restart must come back in the
	// same mode rather than silently defaulting to full-capability ModeAuto. An
	// unknown name decodes to ModeAuto (router decides), never an unexpected pin.
	Mode string `json:"mode,omitempty"`
	// AskLevel is the user-set ask-aggressiveness (1..6); like Mode it is a posture the
	// operator dialed, so it must round-trip a restart. Zero (an old snapshot) is
	// normalized to the default ("normal") at use, never silently off.
	AskLevel int `json:"ask_level,omitempty"`
}

// encodeState maps the bounded WorkState onto its wire shape. It is total and
// never errors: every field is plain data, and the route is rendered by its
// stable string name.
func encodeState(st WorkState) persistedState {
	return persistedState{
		Goal:        st.Summary.Goal,
		Constraints: st.Summary.Constraints,
		Decisions:   st.Summary.Decisions,
		Remaining:   st.Summary.Remaining,
		Active:      st.Active.String(),
		Branch:      st.Branch,
		LastOutcome: st.LastOutcome,
		Mode:        st.Mode.String(),
		AskLevel:    st.AskLevel,
	}
}

// decodeState rebuilds a WorkState from its wire shape, mapping the active-driver
// name back to a Route. An unrecognized name (a forward-compat snapshot or a
// renamed route) decodes to RouteContinue's zero, so a stale pointer never
// dispatches to the wrong machine — the next route re-decides instead.
func (p persistedState) decode() WorkState {
	st := WorkState{
		Active:      routeFromString(p.Active),
		Branch:      p.Branch,
		LastOutcome: p.LastOutcome,
		Mode:        ModeFromString(p.Mode),
		AskLevel:    p.AskLevel,
	}
	st.Summary.Goal = p.Goal
	st.Summary.Constraints = p.Constraints
	st.Summary.Decisions = p.Decisions
	st.Summary.Remaining = p.Remaining
	return st
}

// routeFromString maps a stored route name back to its Route. It is the inverse
// of Route.String for the dispatchable routes; an unknown name yields the zero
// RouteContinue so a restored pointer is never silently mis-dispatched.
func routeFromString(s string) Route {
	switch s {
	case "native":
		return RouteNative
	case "supervise":
		return RouteSupervise
	case "project":
		return RouteProject
	case "chat":
		return RouteChat
	default:
		return RouteContinue
	}
}

// Restore rehydrates this Session's WorkState from the store for its conversation
// ID, so a follow-up after a restart re-enters the prior driver rather than
// restarting. It is BEST-EFFORT: a nil Store is a no-op (returns false); a store
// error or an unparseable blob is logged (metadata-only) and leaves the Session
// at its zero state rather than failing — a torn or absent snapshot just means a
// fresh conversation. It reports whether prior state was restored.
//
// Restore is intended to be called once, right after New and before the first
// Turn, while no drive is running; it takes s.mu so a (degenerate) concurrent
// reader never sees a half-applied State.
func (s *Session) Restore(ctx context.Context) (restored bool) {
	if s.Store == nil {
		return false
	}
	detail, found, err := s.Store.LoadConversation(ctx, s.ID)
	if err != nil {
		s.logPersist("session_restore", map[string]any{"error": true})
		return false
	}
	if !found || detail == "" {
		return false
	}
	var p persistedState
	if err := json.Unmarshal([]byte(detail), &p); err != nil {
		// A corrupt snapshot is not fatal: start fresh, record that we did.
		s.logPersist("session_restore", map[string]any{"error": true, "corrupt": true})
		return false
	}
	st := p.decode()

	s.mu.Lock()
	s.State = st
	s.mu.Unlock()

	s.logPersist("session_restore", map[string]any{
		"active":      st.Active.String(),
		"has_goal":    st.Summary.Goal != "",
		"has_outcome": st.LastOutcome != "",
	})
	return true
}

// persist writes the given bounded WorkState back to the store. It is called by
// the drive goroutine after each terminal fold (and may be called on a clean
// shutdown checkpoint). BEST-EFFORT: a nil Store is a no-op; a marshal or store
// error is logged (metadata-only) and swallowed so a persistence fault never
// fails a drive — the verifier and the event log remain the authorities. Only the
// bounded state is written; History never touches disk.
func (s *Session) persist(ctx context.Context, st WorkState) {
	if s.Store == nil {
		return
	}
	blob, err := json.Marshal(encodeState(st))
	if err != nil {
		s.logPersist("session_persist", map[string]any{"error": true})
		return
	}
	if err := s.Store.SaveConversation(ctx, s.ID, st.Summary.Goal, string(blob)); err != nil {
		s.logPersist("session_persist", map[string]any{"error": true})
		return
	}
	s.logPersist("session_persist", map[string]any{
		"active":   st.Active.String(),
		"len_blob": len(blob),
	})
}

// logPersist appends a metadata-only persistence audit event. The detail carries
// only sizes/flags/route names — never the work-state body or any transcript
// (I5/I7). Log.Append is nil-safe, so this is safe with no event log wired.
func (s *Session) logPersist(kind string, detail map[string]any) {
	s.Log.Append(eventlog.Event{Task: s.ID, Kind: kind, Detail: detail})
}

// snapshotState returns a copy of the bounded WorkState under s.mu so the persist
// write operates on a stable value rather than the live field a Turn might mutate.
// WorkState's slices are shared by reference; persist only reads them, so a shallow
// copy is sufficient for the marshal.
func (s *Session) snapshotState() WorkState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.State
}

// Checkpoint persists the Session's CURRENT bounded WorkState — the public entry
// the front doors call on a clean shutdown (SIGTERM/Ctrl-C) so the conversation
// resumes rather than restarts. It snapshots State under s.mu and writes it
// best-effort. A nil Store makes it a no-op. The returned error is the store
// error if any, surfaced so a shutdown path may report it; the drive-time persist
// swallows errors because a drive must not fail on a durability fault, but an
// explicit checkpoint caller may want to know.
func (s *Session) Checkpoint(ctx context.Context) error {
	if s.Store == nil {
		return nil
	}
	st := s.snapshotState()
	blob, err := json.Marshal(encodeState(st))
	if err != nil {
		return fmt.Errorf("session checkpoint marshal: %w", err)
	}
	if err := s.Store.SaveConversation(ctx, s.ID, st.Summary.Goal, string(blob)); err != nil {
		return fmt.Errorf("session checkpoint save: %w", err)
	}
	s.logPersist("session_persist", map[string]any{
		"active":   st.Active.String(),
		"len_blob": len(blob),
		"manual":   true,
	})
	return nil
}
