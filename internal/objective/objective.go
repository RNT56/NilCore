// Package objective is the standing-objectives backlog the autonomy daemon
// self-services when idle (Phase-16 Pillar 7, AUTO-T01/T02). A standing objective is
// a durable operator intent — "keep CI green", "keep deps current" — that the agent
// pulls from when it has no foreground work, executes *reversibly* through the verified
// orchestrator, and gates only at the irreversible edge (so it composes with Pillar 5's
// envelope without granting any new authority).
//
// WHY a leaf with a narrow Store seam: like internal/wake, this package owns only the
// scheduling policy (which objective is due, by priority + min-interval) and the typed
// shape; persistence lives behind a small Store interface that the concrete *store.Store
// satisfies later. That keeps the package pure (stdlib only, no nilcore import — see
// deps_test.go), inverts no dependency direction, and lets the selection logic be tested
// hermetically against an in-memory fake.
//
// OPERATOR-ONLY BY CONSTRUCTION (review I7-adjacent fix, XC-T06): this package exposes
// no model-facing surface. Its CRUD (Put/Get/List/Disable, plus MarkRun) is wired to an
// operator-only host verb (`nilcore objective`, AUTO-T07) — NEVER registered as a
// sandboxed model tool. A model must not be able to enqueue, edit, or re-prioritize its
// own standing objectives; it may only *do the work* one selected objective names, and
// every irreversible step still passes the gate. Objective.Goal is operator-authored
// text; this package treats every field as inert data (it never interprets Goal as
// instructions) and templates only structural fields, honoring I7.
package objective

import (
	"context"
	"errors"
	"sort"
	"time"
)

// Objective is one standing operator intent the agent self-services when idle.
//
//   - ID         stable operator-chosen key (used for Get/MarkRun/Disable).
//   - Goal       operator-authored intent text handed to the orchestrator as the
//     drive goal. Inert data here — never interpreted as instructions (I7).
//   - Priority   higher runs first among due, enabled objectives. Ties break by ID
//     (deterministic, so selection never depends on map/slice order).
//   - Enabled    disabled objectives are inert: never selected, but retained (an
//     operator can re-enable). A disabled objective is a paused intent, not a delete.
//   - MinPeriod  the minimum spacing between runs of THIS objective. An objective is
//     "due" only once MinPeriod has elapsed since LastRun (zero LastRun ⇒ never run ⇒
//     always due). A zero MinPeriod means "always due" once enabled.
//   - LastRun    when the objective last began a verified run (advanced by MarkRun).
type Objective struct {
	ID        string
	Goal      string
	Priority  int
	Enabled   bool
	MinPeriod time.Duration
	LastRun   time.Time
}

// due reports whether o may run at `now`: enabled and at least MinPeriod past LastRun.
// A zero LastRun is "never run", which is always due. A zero MinPeriod is always due.
func (o Objective) due(now time.Time) bool {
	if !o.Enabled {
		return false
	}
	if o.LastRun.IsZero() || o.MinPeriod <= 0 {
		return true
	}
	return !now.Before(o.LastRun.Add(o.MinPeriod))
}

// Store is the narrow durable seam this package needs, satisfied later by *store.Store
// (which is why this package does NOT import internal/store — it declares only what it
// uses, mirroring internal/wake.Store). Implementations persist Objective records
// keyed by ID; List returns every record (enabled or not), order unspecified — the
// Backlog re-sorts deterministically. Put inserts or replaces by ID. Get returns
// ErrNotFound (or any wrapping of it) when the ID is absent.
type Store interface {
	Put(ctx context.Context, o Objective) error
	Get(ctx context.Context, id string) (Objective, error)
	List(ctx context.Context) ([]Objective, error)
	Disable(ctx context.Context, id string) error
}

// ErrNotFound is returned by Backlog.Get (and expected from Store.Get) when no
// objective has the given ID. Callers test with errors.Is.
var ErrNotFound = errors.New("objective: not found")

// Backlog is the standing-objectives backlog over a Store. The zero value is unusable;
// construct with New. Now is injectable for deterministic tests (nil ⇒ time.Now).
//
// Backlog adds no authority: it only reads/writes the operator-owned backlog and tells
// the caller which objective is due. Running it — and gating its irreversible steps —
// is the orchestrator's job.
type Backlog struct {
	store Store
	now   func() time.Time
}

// New returns a Backlog over a durable Store. A nil store makes every method a nil-safe
// no-op (List/NextIdle yield nothing, Get yields ErrNotFound, writes succeed silently)
// so an unwired backlog is byte-identically inert — the default-off contract: the
// backlog source stays off unless objectives exist.
func New(s Store) *Backlog { return &Backlog{store: s} }

// WithClock returns a copy of b whose clock is f (for tests). A nil f restores time.Now.
// It does not mutate the receiver.
func (b *Backlog) WithClock(f func() time.Time) *Backlog {
	cp := *b
	cp.now = f
	return &cp
}

func (b *Backlog) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// Put inserts or replaces an objective by ID. Operator-only path (see package doc).
func (b *Backlog) Put(ctx context.Context, o Objective) error {
	if b.store == nil {
		return nil
	}
	return b.store.Put(ctx, o)
}

// Get returns the objective with the given ID, or ErrNotFound if absent (the store's
// own not-found error is normalized to ErrNotFound so callers test with errors.Is).
func (b *Backlog) Get(ctx context.Context, id string) (Objective, error) {
	if b.store == nil {
		return Objective{}, ErrNotFound
	}
	o, err := b.store.Get(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Objective{}, ErrNotFound
		}
		return Objective{}, err
	}
	return o, nil
}

// List returns every objective (enabled or not), deterministically ordered the same way
// selection considers them: highest Priority first, ties broken by ascending ID.
func (b *Backlog) List(ctx context.Context) ([]Objective, error) {
	if b.store == nil {
		return nil, nil
	}
	all, err := b.store.List(ctx)
	if err != nil {
		return nil, err
	}
	sortByPriority(all)
	return all, nil
}

// Disable marks an objective inert (paused, not deleted). Operator-only path.
func (b *Backlog) Disable(ctx context.Context, id string) error {
	if b.store == nil {
		return nil
	}
	return b.store.Disable(ctx, id)
}

// NextIdle returns the highest-priority enabled objective that is due to run at `now`
// (its MinPeriod has elapsed since LastRun), or ok=false if none is due. Selection is
// deterministic: among due objectives the highest Priority wins, ties broken by the
// smallest ID, so the same backlog state always yields the same choice. `now` is passed
// explicitly so the daemon and tests share one clock.
func (b *Backlog) NextIdle(ctx context.Context, now time.Time) (Objective, bool, error) {
	if b.store == nil {
		return Objective{}, false, nil
	}
	all, err := b.store.List(ctx)
	if err != nil {
		return Objective{}, false, err
	}
	sortByPriority(all)
	for _, o := range all {
		if o.due(now) {
			return o, true, nil
		}
	}
	return Objective{}, false, nil
}

// MarkRun records that the objective `id` began a run at `when`, advancing its LastRun
// so MinPeriod debounces the next selection. It is a read-modify-write through the
// narrow Store (the seam exposes no partial update), preserving every other field.
// ErrNotFound if the objective is absent. `when` is supplied by the caller (the daemon
// passes the verified-run timestamp) so the advance is deterministic, never wall-clock.
func (b *Backlog) MarkRun(ctx context.Context, id string, when time.Time) error {
	if b.store == nil {
		return nil
	}
	o, err := b.store.Get(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	o.LastRun = when.UTC()
	return b.store.Put(ctx, o)
}

// sortByPriority orders objectives by descending Priority, then ascending ID — the one
// deterministic order both List and NextIdle use, so neither depends on the Store's
// return order.
func sortByPriority(os []Objective) {
	sort.Slice(os, func(i, j int) bool {
		if os[i].Priority != os[j].Priority {
			return os[i].Priority > os[j].Priority
		}
		return os[i].ID < os[j].ID
	})
}
