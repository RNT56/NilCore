package autosrc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"nilcore/internal/objective"
)

// fakeStore is an in-memory objective.Store for hermetic tests. It is the narrow seam
// the objective leaf declares — we never import internal/store. Safe for the single
// pump goroutine that drives one Source (a small mutex makes -race clean regardless).
type fakeStore struct {
	mu sync.Mutex
	m  map[string]objective.Objective
}

func newFakeStore(objs ...objective.Objective) *fakeStore {
	fs := &fakeStore{m: make(map[string]objective.Objective)}
	for _, o := range objs {
		fs.m[o.ID] = o
	}
	return fs
}

func (f *fakeStore) Put(_ context.Context, o objective.Objective) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[o.ID] = o
	return nil
}

func (f *fakeStore) Get(_ context.Context, id string) (objective.Objective, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.m[id]
	if !ok {
		return objective.Objective{}, objective.ErrNotFound
	}
	return o, nil
}

func (f *fakeStore) List(_ context.Context) ([]objective.Objective, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]objective.Objective, 0, len(f.m))
	for _, o := range f.m {
		out = append(out, o)
	}
	return out, nil
}

func (f *fakeStore) Disable(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if o, ok := f.m[id]; ok {
		o.Enabled = false
		f.m[id] = o
	}
	return nil
}

func (f *fakeStore) lastRun(t *testing.T, id string) time.Time {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.m[id]
	if !ok {
		t.Fatalf("objective %q absent from store", id)
	}
	return o.LastRun
}

// immediateWait is a Wait that never sleeps; it only honors ctx cancellation. With it,
// the poll loop spins through Next deterministically without consuming real time.
func immediateWait(ctx context.Context, _ time.Duration) error { return ctx.Err() }

// TestBacklogSourceEmitsHighestPriorityDue proves the source picks by priority+due-ness,
// emits a low-priority QueuedSignal labelled "backlog", and advances LastRun (MarkRun).
func TestBacklogSourceEmitsHighestPriorityDue(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	fs := newFakeStore(
		objective.Objective{ID: "low", Goal: "keep deps current", Priority: 1, Enabled: true},
		objective.Objective{ID: "high", Goal: "keep CI green", Priority: 9, Enabled: true},
		objective.Objective{ID: "off", Goal: "disabled intent", Priority: 99, Enabled: false},
	)
	bl := objective.New(fs)

	src := NewBacklogSource(bl, BacklogConfig{
		Now:  func() time.Time { return now },
		Wait: immediateWait,
	})

	sig, ok, err := src.Next(context.Background())
	if err != nil || !ok {
		t.Fatalf("Next: ok=%v err=%v, want a signal", ok, err)
	}
	if sig.Signal.Goal != "keep CI green" {
		t.Fatalf("emitted goal %q, want the highest-priority due objective %q", sig.Signal.Goal, "keep CI green")
	}
	if sig.Signal.Source != "backlog" {
		t.Fatalf("emitted source %q, want %q", sig.Signal.Source, "backlog")
	}
	if sig.Priority != DefaultBacklogPriority {
		t.Fatalf("emitted priority %d, want the low default %d", sig.Priority, DefaultBacklogPriority)
	}

	// MarkRun advanced the selected objective's debounce clock to `now`.
	if got := fs.lastRun(t, "high"); !got.Equal(now) {
		t.Fatalf("MarkRun did not advance LastRun: got %v, want %v", got, now)
	}
	// The unselected objective is untouched.
	if got := fs.lastRun(t, "low"); !got.IsZero() {
		t.Fatalf("unselected objective LastRun mutated: %v", got)
	}
}

// TestBacklogPriorityBelowReactive proves the emitted band is low enough that a
// reactive signal preempts it in the shared queue (the whole point of the idle tier).
func TestBacklogPriorityBelowReactive(t *testing.T) {
	now := time.Now()
	fs := newFakeStore(objective.Objective{ID: "o", Goal: "idle work", Priority: 1000, Enabled: true})
	src := NewBacklogSource(objective.New(fs), BacklogConfig{
		Now:  func() time.Time { return now },
		Wait: immediateWait,
	})

	idleSig, ok, err := src.Next(context.Background())
	if err != nil || !ok {
		t.Fatalf("Next: ok=%v err=%v", ok, err)
	}

	const reactivePriority = 100 // any reactive source uses a band above the backlog default
	q := newBoundedQueue(0)
	if err := q.enqueue(idleSig); err != nil {
		t.Fatalf("enqueue idle: %v", err)
	}
	if err := q.enqueue(sig("reactive", reactivePriority)); err != nil {
		t.Fatalf("enqueue reactive: %v", err)
	}
	first, _, err := q.dequeue(context.Background())
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if first.Signal.Goal != "reactive" {
		t.Fatalf("backlog work was not preempted: drained %q first, want the reactive signal", first.Signal.Goal)
	}
}

// TestBacklogNothingDuePolls proves nothing-due ⇒ no signal: the source keeps polling
// (it never emits and never reports done), and ctx cancellation is the only exit.
func TestBacklogNothingDuePolls(t *testing.T) {
	// One objective, but it ran recently and its MinPeriod has not elapsed ⇒ not due.
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	fs := newFakeStore(objective.Objective{
		ID: "recent", Goal: "g", Priority: 5, Enabled: true,
		MinPeriod: time.Hour, LastRun: now.Add(-time.Minute),
	})

	var polls int
	src := NewBacklogSource(objective.New(fs), BacklogConfig{
		Now: func() time.Time { return now },
		Wait: func(ctx context.Context, _ time.Duration) error {
			polls++
			if polls >= 3 {
				return context.Canceled // end the loop after a few confirmed empty polls
			}
			return ctx.Err()
		},
	})

	_, ok, err := src.Next(context.Background())
	if ok {
		t.Fatal("source emitted a signal though nothing was due")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Next ended with %v, want context.Canceled", err)
	}
	if polls < 3 {
		t.Fatalf("source did not keep polling: polls=%d", polls)
	}
	// Nothing due ⇒ LastRun untouched (no MarkRun).
	if got := fs.lastRun(t, "recent"); !got.Equal(now.Add(-time.Minute)) {
		t.Fatalf("LastRun mutated despite nothing due: %v", got)
	}
}

// TestBacklogWaitsWhileBusy proves the idle gate: while the daemon is busy the source
// emits nothing even though an objective is due; once idle, it emits.
func TestBacklogWaitsWhileBusy(t *testing.T) {
	now := time.Now()
	fs := newFakeStore(objective.Objective{ID: "o", Goal: "due work", Priority: 5, Enabled: true})

	var busy = true
	var polls int
	src := NewBacklogSource(objective.New(fs), BacklogConfig{
		Now:  func() time.Time { return now },
		Idle: func() bool { return !busy },
		Wait: func(ctx context.Context, _ time.Duration) error {
			polls++
			busy = false // become idle after the first busy poll
			return ctx.Err()
		},
	})

	sig, ok, err := src.Next(context.Background())
	if err != nil || !ok {
		t.Fatalf("Next: ok=%v err=%v, want a signal once idle", ok, err)
	}
	if sig.Signal.Goal != "due work" {
		t.Fatalf("emitted %q, want %q", sig.Signal.Goal, "due work")
	}
	if polls != 1 {
		t.Fatalf("expected exactly one busy poll before idle, got %d", polls)
	}
}

// TestBacklogMarkRunAdvancesDebounce proves MarkRun advances: a second Next at the same
// `now` no longer re-selects the just-run objective (its MinPeriod debounces it), so a
// lower-priority still-due objective is chosen instead.
func TestBacklogMarkRunAdvancesDebounce(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	fs := newFakeStore(
		objective.Objective{ID: "a", Goal: "high then debounced", Priority: 9, Enabled: true, MinPeriod: time.Hour},
		objective.Objective{ID: "b", Goal: "lower still due", Priority: 1, Enabled: true},
	)
	src := NewBacklogSource(objective.New(fs), BacklogConfig{
		Now:  func() time.Time { return now },
		Wait: immediateWait,
	})

	first, ok, err := src.Next(context.Background())
	if err != nil || !ok || first.Signal.Goal != "high then debounced" {
		t.Fatalf("first Next: goal=%q ok=%v err=%v", first.Signal.Goal, ok, err)
	}

	// At the SAME now, "a" is now debounced (LastRun=now, MinPeriod=1h) ⇒ "b" wins.
	second, ok, err := src.Next(context.Background())
	if err != nil || !ok {
		t.Fatalf("second Next: ok=%v err=%v", ok, err)
	}
	if second.Signal.Goal != "lower still due" {
		t.Fatalf("second Next picked %q, want the still-due lower objective %q", second.Signal.Goal, "lower still due")
	}
	if got := fs.lastRun(t, "a"); !got.Equal(now) {
		t.Fatalf("objective a LastRun = %v, want %v", got, now)
	}
}

// TestBacklogContextCancelExits proves Next unblocks promptly on cancellation while it
// is waiting (busy / nothing-due), returning the context error and never a panic.
func TestBacklogContextCancelExits(t *testing.T) {
	now := time.Now()
	fs := newFakeStore(objective.Objective{ID: "o", Goal: "g", Priority: 5, Enabled: true})
	src := NewBacklogSource(objective.New(fs), BacklogConfig{
		Now:      func() time.Time { return now },
		Idle:     func() bool { return false }, // never idle ⇒ always waits
		Interval: time.Millisecond,             // real timer; cancel must beat it
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := src.Next(ctx)
		done <- err
	}()
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Next returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Next did not unblock on cancel")
	}
}

// TestBacklogNilBacklogInert is the default-off golden: a source with a nil backlog (the
// unwired posture) emits nothing — it just polls — and exits only on cancel. With the
// feature unwired the source is byte-identically a no-op.
func TestBacklogNilBacklogInert(t *testing.T) {
	var polls int
	src := NewBacklogSource(nil, BacklogConfig{
		Wait: func(ctx context.Context, _ time.Duration) error {
			polls++
			if polls >= 2 {
				return context.Canceled
			}
			return ctx.Err()
		},
	})
	_, ok, err := src.Next(context.Background())
	if ok {
		t.Fatal("nil-backlog source emitted a signal")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("nil-backlog Next ended with %v, want context.Canceled", err)
	}
}

// TestBacklogDefaultsApplied proves the zero-config constructor fills safe defaults:
// the low priority band and a positive poll interval (so an idle daemon never
// hot-spins). This is the additive default path the wiring layer relies on.
func TestBacklogDefaultsApplied(t *testing.T) {
	src := NewBacklogSource(objective.New(newFakeStore()), BacklogConfig{})
	if src.priority != DefaultBacklogPriority {
		t.Fatalf("default priority = %d, want %d", src.priority, DefaultBacklogPriority)
	}
	if src.interval != DefaultBacklogInterval {
		t.Fatalf("default interval = %v, want %v", src.interval, DefaultBacklogInterval)
	}
	if src.isIdle() != true {
		t.Fatal("nil Idle predicate must default to always-idle")
	}
}

// TestBacklogSourceSatisfiesInterface proves BacklogSource is a usable autosrc.Source —
// the daemon can pump it like any other source.
func TestBacklogSourceSatisfiesInterface(t *testing.T) {
	var _ Source = (*BacklogSource)(nil)
}
