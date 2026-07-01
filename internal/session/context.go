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
// s.mu released (like the router's classifier call); the SPLICE into s.History is
// taken under s.mu.
//
// The summarize call is a seconds-long model round-trip made with s.mu RELEASED, so a
// concurrent follow-up Turn can append to s.History while it runs (session.go: Turn's
// in-flight branch does s.History = append(s.History, userMsg)). We therefore SPLICE
// rather than overwrite: under the lock we replace only the summarized PREFIX — the
// prior turns that went into the summary — while preserving `history`'s last turn and
// every turn appended after the pre-lock snapshot, so a racing follow-up survives in
// the in-memory projection. If s.History was reset out from under us (a concurrent
// Clear, or it shrank below the summarized prefix), we skip the splice and return the
// computed seed without touching s.History — best-effort, never corrupting the live slice.
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
	summaryTurn := userTurn("[Earlier conversation, compacted to fit the context window]\n" + cs.String())

	// The seed returned to the caller (this drive's seed) is [summary, last] — the
	// same compact two-turn shape as before, computed purely from the snapshot.
	seed := []model.Message{summaryTurn, last}

	// Splice the SAME summary prefix into the LIVE s.History under the lock. Everything
	// from index len(prior) onward (the snapshot's last turn plus any follow-up appended
	// during the summarize window) is preserved verbatim. If a concurrent reset shrank
	// s.History below the summarized prefix, leave it untouched (best-effort).
	s.mu.Lock()
	if len(s.History) >= len(prior) {
		tail := s.History[len(prior):]
		spliced := make([]model.Message, 0, 1+len(tail))
		spliced = append(spliced, summaryTurn)
		spliced = append(spliced, tail...)
		s.History = spliced
	}
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
