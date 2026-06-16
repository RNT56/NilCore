// Package wake is the durable self-scheduled timer behind serve's `sleep` tool: an
// agent suspends its drive and asks to be re-engaged after a delay it chooses, then a
// serve waker re-engages the conversation when the timer elapses. A Wake is
// {ThreadID, Sender, WakeAt, Note}; the Registry persists ONE pending wake per thread
// through a narrow store seam (so a wake survives a process restart — re-fired on the
// next poll) and the serve waker drives Pending → fire.
//
// It schedules only; it never runs anything and grants no authority. The re-engage
// itself is an ordinary session Turn (the woken drive still gates any irreversible
// action, I3). Stdlib + eventlog + the narrow store seam only (I6); the store seam
// keeps wake a leaf (the concrete *agent.Checkpoint satisfies it, mirroring
// session.Store), so wake never imports agent.
package wake

import (
	"context"
	"encoding/json"
	"time"

	"nilcore/internal/eventlog"
)

// Wake is one armed timer: re-engage ThreadID (owned by Sender) at WakeAt with Note
// (the agent's bounded message to its future self — what it is waiting on / to check).
type Wake struct {
	ThreadID string
	Sender   string
	WakeAt   time.Time
	Note     string
}

// Store is the narrow durable seam (satisfied by *agent.Checkpoint). It carries OPAQUE
// JSON detail keyed by threadID; the store never parses it. SaveWake replaces any
// existing pending wake for the thread (one self-timer per conversation — debounce).
// LoadWakes returns only currently-armed wakes (a fired/disarmed one is excluded).
type Store interface {
	SaveWake(ctx context.Context, threadID, detail string) error
	LoadWakes(ctx context.Context) (map[string]string, error) // threadID -> detail JSON
	DisarmWake(ctx context.Context, threadID string) error
}

// detail is the JSON the Registry owns inside the store's opaque blob.
type detail struct {
	Sender string    `json:"sender"`
	Note   string    `json:"note"`
	WakeAt time.Time `json:"wake_at"`
}

// Registry arms/disarms/lists durable wakes. The zero value is unusable; construct
// with New. Now is injectable for tests (nil ⇒ time.Now).
type Registry struct {
	store Store
	log   *eventlog.Log
	now   func() time.Time
}

// New returns a Registry over a durable Store. A nil log disables audit (the wakes
// still persist); a nil store makes every method a no-op error-free caller's guard.
func New(s Store, log *eventlog.Log) *Registry { return &Registry{store: s, log: log} }

func (r *Registry) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// maxNoteBytes bounds the agent's self-note so a sprawling note can never bloat the
// store row or the re-engage prompt.
const maxNoteBytes = 600

// Arm records (or replaces) the single pending wake for threadID, due `after` from
// now, carrying the bounded note. It returns the absolute wake time. A non-positive
// `after` is treated as "due now" (fires on the next poll).
func (r *Registry) Arm(ctx context.Context, threadID, sender string, after time.Duration, note string) (time.Time, error) {
	if after < 0 {
		after = 0
	}
	wakeAt := r.clock().Add(after).UTC()
	d, err := json.Marshal(detail{Sender: sender, Note: clip(note, maxNoteBytes), WakeAt: wakeAt})
	if err != nil {
		return time.Time{}, err
	}
	if r.store == nil {
		return wakeAt, nil
	}
	if err := r.store.SaveWake(ctx, threadID, string(d)); err != nil {
		return time.Time{}, err
	}
	r.audit("wake_armed", map[string]any{"thread": threadID, "after_s": int(after.Seconds())})
	return wakeAt, nil
}

// Disarm clears the pending wake for threadID (a status flip in the store, not a
// delete — it keeps the audit trail and excludes it from Pending). Safe when none is
// armed.
func (r *Registry) Disarm(ctx context.Context, threadID string) error {
	if r.store == nil {
		return nil
	}
	if err := r.store.DisarmWake(ctx, threadID); err != nil {
		return err
	}
	r.audit("wake_disarmed", map[string]any{"thread": threadID})
	return nil
}

// Pending returns every currently-armed wake (across restarts — the store survives),
// so the waker can fire the due ones. Order is unspecified.
func (r *Registry) Pending(ctx context.Context) ([]Wake, error) {
	if r.store == nil {
		return nil, nil
	}
	m, err := r.store.LoadWakes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Wake, 0, len(m))
	for threadID, raw := range m {
		var d detail
		if json.Unmarshal([]byte(raw), &d) != nil {
			continue // a malformed row is skipped, never fatal
		}
		out = append(out, Wake{ThreadID: threadID, Sender: d.Sender, WakeAt: d.WakeAt, Note: d.Note})
	}
	return out, nil
}

func (r *Registry) audit(kind string, detail map[string]any) {
	if r.log != nil {
		r.log.Append(eventlog.Event{Kind: kind, Detail: detail})
	}
}

// clip truncates s to at most n bytes on a rune boundary (valid UTF-8).
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut]
}
