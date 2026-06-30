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
	"sync"
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
//
// A single *Registry serializes Claim across goroutines (mu + claimed), so when two
// pollers share ONE registry — the serve waker and the autonomy wake feeder both poll
// the same instance (cmd/nilcore) — only one can win a given thread's wake. See Claim.
type Registry struct {
	store Store
	log   *eventlog.Log
	now   func() time.Time

	mu sync.Mutex // guards claimed; serializes Claim's disarm-and-return as one step
	// claimed records threadIDs already won via Claim, so a second concurrent Claim
	// loses even before the store's disarm is durably visible (two pollers polling the
	// SAME registry can otherwise both read a wake as Pending before either Disarm
	// lands). It only grows for wakes Claim actually fired; Arm clears a thread's entry
	// when a NEW wake is armed for it, so a re-armed wake is claimable again.
	claimed map[string]struct{}
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
	// A freshly-armed wake is a NEW timer for this thread: clear any prior single-fire
	// claim so the new wake is claimable again (the in-memory claimed set must not
	// permanently shadow a re-armed thread).
	r.mu.Lock()
	delete(r.claimed, threadID)
	r.mu.Unlock()
	r.audit("wake_armed", map[string]any{"thread": threadID, "after_s": int(after.Seconds())})
	return wakeAt, nil
}

// Claim is the single-fire primitive that makes "fire this wake exactly once" safe when
// more than one poller shares this Registry. It atomically (within this *Registry)
// records the thread as claimed AND disarms its wake, returning won=true to the FIRST
// caller and won=false to every concurrent loser — so a wake read as Pending by two
// pollers is delivered by exactly one of them, never both.
//
// The Store seam exposes no compare-and-disarm, so the atomicity is provided here: the
// mu-guarded claimed set is the source of truth for "already fired" and the loser
// observes the wake as already-gone WITHOUT a second store round-trip. The durable
// DisarmWake still runs for the winner so the wake does not re-fire after a restart.
// A loser does NOT touch the store. Safe with a nil store (no-op, always won=false so a
// caller never double-delivers an unstored wake). It honors ctx for the store write.
func (r *Registry) Claim(ctx context.Context, threadID string) (won bool, err error) {
	if r.store == nil {
		return false, nil
	}
	r.mu.Lock()
	if _, taken := r.claimed[threadID]; taken {
		r.mu.Unlock()
		return false, nil // a concurrent caller already won this thread's wake
	}
	if r.claimed == nil {
		r.claimed = make(map[string]struct{})
	}
	r.claimed[threadID] = struct{}{}
	r.mu.Unlock()

	// We hold the claim. Durably disarm so the wake never re-fires across a restart. If
	// the disarm fails, release the in-memory claim so a retry (or the other poller) can
	// pick it up rather than the wake being silently swallowed.
	if err := r.store.DisarmWake(ctx, threadID); err != nil {
		r.mu.Lock()
		delete(r.claimed, threadID)
		r.mu.Unlock()
		return false, err
	}
	r.audit("wake_claimed", map[string]any{"thread": threadID})
	return true, nil
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
