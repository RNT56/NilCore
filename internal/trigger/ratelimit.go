package trigger

// ratelimit.go — the bounded self-start fence (denial-of-wallet).
//
// WHY a rate bound at all. The webhook intake serializes runs on a mutex, but a
// mutex only forces them one-at-a-time — it does not cap how MANY there are. An
// HMAC only proves a forge relayed the delivery, not that an authorized human asked
// for a run, so on a public repo an attacker who can post signed deliveries (e.g. by
// opening/labeling issues) could otherwise queue an unbounded stream of agent runs,
// each burning tokens and ingesting attacker-controlled content. This bounds the
// COUNT (irreversible authority stays contained by the hardcoded headless deny).

import (
	"sync"
	"time"
)

// rateWindow is the trailing span the per-day self-start cap is measured over. A
// rolling 24h window (rather than a calendar day) needs no midnight reset and cannot
// be gamed across a timezone boundary.
const rateWindow = 24 * time.Hour

// RateLimiter bounds how OFTEN self-starts may fire. Both bounds are opt-in — a
// non-positive value disables that bound, and a nil *RateLimiter or a zero value
// allows everything (so an unwired trigger is unchanged):
//
//   - MaxPerDay:   at most N allowed self-starts within any trailing rateWindow.
//   - MinInterval: a cooldown — consecutive self-starts must be at least this apart.
//
// It is safe for concurrent use (webhook deliveries arrive on many HTTP goroutines).
type RateLimiter struct {
	MaxPerDay   int           // cap per trailing 24h window; <= 0 disables the cap
	MinInterval time.Duration // min gap between self-starts; <= 0 disables the cooldown
	// Now is the clock, injectable for tests. Defaults to time.Now.
	Now func() time.Time

	mu     sync.Mutex
	starts []time.Time // times of recent allowed self-starts, pruned to the trailing window
}

// Allow reports whether a self-start may proceed NOW and, when it may not, a short
// machine reason for the audit trail ("daily-cap" | "cooldown"). A permitted start is
// recorded so it counts against both bounds; a rejected one records nothing. A nil
// receiver, or one with neither bound set, always allows (the opt-in default).
func (r *RateLimiter) Allow() (ok bool, reason string) {
	if r == nil || (r.MaxPerDay <= 0 && r.MinInterval <= 0) {
		return true, ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	// Prune starts that have aged out of the window (in place, order-preserving) so
	// both the cap count and the "most recent start" cooldown read only live entries.
	cutoff := now.Add(-rateWindow)
	kept := r.starts[:0]
	for _, t := range r.starts {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	r.starts = kept

	// Cooldown: the last recorded start must be at least MinInterval in the past.
	if r.MinInterval > 0 && len(r.starts) > 0 {
		if last := r.starts[len(r.starts)-1]; now.Sub(last) < r.MinInterval {
			return false, "cooldown"
		}
	}
	// Daily cap: the trailing window must not already hold MaxPerDay starts.
	if r.MaxPerDay > 0 && len(r.starts) >= r.MaxPerDay {
		return false, "daily-cap"
	}
	r.starts = append(r.starts, now)
	return true, ""
}

func (r *RateLimiter) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
