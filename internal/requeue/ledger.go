package requeue

// ledger.go — the bounded per-Unit retry budget (Pillar 4).
//
// WHY a budget at all. Granular requeue re-dispatches exactly the red claims, but
// a permanently-red cell (a 404 source that never comes back, a value the model
// cannot fix) must not spin forever. The Ledger caps how many focused re-runs any
// single Unit (keyed ArtifactID/ClaimID) earns: once a Unit hits MaxAttempts it is
// Exhausted and the loop converges RED on it rather than looping. This is what
// turns "try again" into a terminating, addressable retry.
//
// WHY MaxAttempts==0 disables requeue. The whole pillar is additive and opt-in:
// with NILCORE_REQUEUE unset the cmd wiring builds a zero Ledger, and a zero
// Ledger reports every Unit Exhausted at attempt 0 — so no requeue round ever
// runs and the default binary stays byte-identical. The disabled-by-default path
// is therefore the same code path as "budget consumed", not a special case.
//
// WHY it round-trips through JSON. The Ledger is the one mutable piece of requeue
// state; it persists beside agent.RunState in store.Task.Detail as an additive
// sibling field, so a resumed run remembers how many attempts each Unit already
// spent. An absent/empty blob unmarshals to a zero Ledger (resumes disabled),
// which keeps an old, pre-requeue snapshot loadable without error.

import (
	"encoding/json"
	"fmt"
)

// Bump records one more attempt against u's budget and returns the new count.
// It lazily allocates the Attempts map so a freshly-constructed Ledger (or one
// resumed from an absent blob) is usable without explicit initialization. The key
// is the stable ArtifactID/ClaimID identity, so two artifacts that share a claim
// id keep independent counters and one Unit's retries never charge another's.
func (l *Ledger) Bump(u Unit) int {
	if l.Attempts == nil {
		l.Attempts = make(map[string]int)
	}
	l.Attempts[key(u)]++
	return l.Attempts[key(u)]
}

// Exhausted reports whether u has spent its retry budget: true iff its recorded
// attempt count has reached MaxAttempts. With MaxAttempts==0 (requeue disabled)
// every Unit is Exhausted even at attempt 0, so the disabled path and the
// budget-consumed path collapse into one — no Unit is ever eligible for a round.
// The comparison is >= (not ==) so a Ledger loaded with an attempt count already
// at or beyond the ceiling still reads as exhausted rather than looping past it.
func (l *Ledger) Exhausted(u Unit) bool {
	return l.attemptFor(u) >= l.MaxAttempts
}

// Marshal serializes the Ledger to JSON for persistence as an additive sibling of
// agent.RunState inside store.Task.Detail. A nil receiver marshals to a zero
// Ledger blob (rather than the literal null) so the on-disk shape is always a
// well-formed Ledger object that UnmarshalLedger round-trips back to disabled.
func (l *Ledger) Marshal() ([]byte, error) {
	src := l
	if src == nil {
		src = &Ledger{}
	}
	data, err := json.Marshal(src)
	if err != nil {
		return nil, fmt.Errorf("requeue: marshal ledger: %w", err)
	}
	return data, nil
}

// UnmarshalLedger parses a Ledger blob produced by Marshal. An empty or absent
// blob (len 0) is NOT an error: it yields a zero Ledger (MaxAttempts 0, nil
// Attempts), so an old snapshot taken before requeue existed resumes with requeue
// simply disabled. A present-but-corrupt blob is a hard error, never a silent
// zero — a parse failure must not be mistaken for "requeue off".
func UnmarshalLedger(data []byte) (*Ledger, error) {
	if len(data) == 0 {
		return &Ledger{}, nil
	}
	var l Ledger
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("requeue: unmarshal ledger: %w", err)
	}
	return &l, nil
}
