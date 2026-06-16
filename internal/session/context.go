package session

// context.go tracks how full the model's context window is and compacts the
// conversation before it overflows — the data behind the front door's context
// gauge, the /context read, and automatic summarization near the limit.
//
// The signal is the LAST model call's INPUT-token count (the model's own measure
// of the whole prompt it just saw), reported through the metered provider's OnUsage
// seam — far more accurate than estimating from message lengths. Divided by the
// model's context window (meter.CtxWindow, injected as CtxWindow so this leaf does
// not import meter), it yields a 0–100% fullness the gauge renders and the
// compactor watches.

import (
	"context"
	"strings"

	"nilcore/internal/eventlog"
	"nilcore/internal/model"
	"nilcore/internal/summarize"
)

// compactThreshold is the fullness at/above which the conversation is auto-compacted
// before the next drive (0.8 = 80% of the window). Below it, History is untouched.
const compactThreshold = 0.80

// usageState is the latest observed model usage (guarded by Session.mu via the
// accessors). lastInput is the authoritative live context size.
type usageState struct {
	model      string
	lastInput  int
	lastOutput int
}

// RecordUsage stores the latest model usage. It is wired to the metered provider's
// OnUsage callback (one shared holder per conversation), so it sees EVERY model
// call — the router classifier, the in-drive loop, chat replies, the summarize
// fold-back — and always reflects the most recent prompt size. Concurrency-safe.
func (s *Session) RecordUsage(modelID string, in, out int) {
	s.mu.Lock()
	s.usage = usageState{model: modelID, lastInput: in, lastOutput: out}
	s.mu.Unlock()
}

// ContextUsage reports the current context fullness: the percentage (0–100), the
// used input-token count, and the model's window. window is 0 (and pct 0) until the
// first model call lands or when no CtxWindow resolver is wired — the front door
// then shows nothing rather than a misleading 0%.
func (s *Session) ContextUsage() (pct, used, window int) {
	s.mu.Lock()
	u := s.usage
	s.mu.Unlock()
	if s.CtxWindow == nil || u.lastInput <= 0 {
		return 0, u.lastInput, 0
	}
	window = s.CtxWindow(u.model)
	if window <= 0 {
		return 0, u.lastInput, 0
	}
	pct = u.lastInput * 100 / window
	if pct > 100 {
		pct = 100
	}
	return pct, u.lastInput, window
}

// maybeCompact summarizes the prior conversation into a single seed turn when the
// context is near full (≥ compactThreshold), so the next drive continues from a
// compact summary rather than overrunning the window. It is BEST-EFFORT and lossy
// by construction: it keeps the latest turn verbatim and replaces everything before
// it with a summary, logging session_compact (I5 — the in-memory seed is rewritten;
// the append-only event log is never touched). A nil Summarizer, an unknown window,
// a below-threshold fullness, or a trivial history all return the history unchanged
// (byte-identical, so fake-driven tests never compact). It runs in route() with
// s.mu released (like the router's classifier call); the overwrite of s.History is
// taken under s.mu.
func (s *Session) maybeCompact(ctx context.Context, st WorkState, history []model.Message) []model.Message {
	if s.Summarizer == nil || len(history) <= 1 {
		return history
	}
	pct, _, window := s.ContextUsage()
	if window <= 0 || pct < int(compactThreshold*100) {
		return history
	}

	prior := history[:len(history)-1]
	last := history[len(history)-1]
	cs, err := summarize.Summarize(ctx, s.Summarizer, st.Summary.Goal, renderHistory(prior))
	if err != nil {
		return history // best-effort: a summarize failure leaves the conversation intact
	}
	seed := []model.Message{
		userTurn("[Earlier conversation, compacted to fit the context window]\n" + cs.String()),
		last,
	}
	s.mu.Lock()
	s.History = seed
	s.mu.Unlock()
	s.Log.Append(eventlog.Event{Task: s.ID, Kind: "session_compact",
		Detail: map[string]any{"pct": pct, "compacted_turns": len(prior)}})
	return seed
}

// renderHistory flattens a turn slice into a plain text transcript for the
// summarizer (role-prefixed text blocks; non-text blocks are skipped). It carries
// only the principal/assistant prose the summary needs, never tool internals.
func renderHistory(msgs []model.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		for _, blk := range m.Content {
			if blk.Type == "text" && strings.TrimSpace(blk.Text) != "" {
				b.WriteString(m.Role)
				b.WriteString(": ")
				b.WriteString(blk.Text)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}
