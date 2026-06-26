package objective

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeStore is an in-memory Store for hermetic tests. It is concurrency-safe so the
// -race detector exercises a realistic seam (the real *store.Store is goroutine-safe).
type fakeStore struct {
	mu  sync.Mutex
	m   map[string]Objective
	err error // if non-nil, every method returns it (to test error propagation)
}

func newFakeStore(objs ...Objective) *fakeStore {
	f := &fakeStore{m: map[string]Objective{}}
	for _, o := range objs {
		f.m[o.ID] = o
	}
	return f
}

func (f *fakeStore) Put(_ context.Context, o Objective) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.m[o.ID] = o
	return nil
}

func (f *fakeStore) Get(_ context.Context, id string) (Objective, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return Objective{}, f.err
	}
	o, ok := f.m[id]
	if !ok {
		return Objective{}, ErrNotFound
	}
	return o, nil
}

func (f *fakeStore) List(_ context.Context) ([]Objective, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	// Return in an intentionally non-sorted order so the Backlog's own ordering is
	// what the tests observe, not the store's.
	out := make([]Objective, 0, len(f.m))
	for _, o := range f.m {
		out = append(out, o)
	}
	return out, nil
}

func (f *fakeStore) Disable(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	o, ok := f.m[id]
	if !ok {
		return ErrNotFound
	}
	o.Enabled = false
	f.m[id] = o
	return nil
}

// base is a fixed instant so every test is deterministic.
var base = time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)

func clockAt(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestNextIdlePicksByPriority(t *testing.T) {
	st := newFakeStore(
		Objective{ID: "deps", Goal: "keep deps current", Priority: 1, Enabled: true},
		Objective{ID: "ci", Goal: "keep CI green", Priority: 5, Enabled: true},
		Objective{ID: "docs", Goal: "keep docs fresh", Priority: 3, Enabled: true},
	)
	b := New(st)

	got, ok, err := b.NextIdle(context.Background(), base)
	if err != nil {
		t.Fatalf("NextIdle: %v", err)
	}
	if !ok {
		t.Fatal("expected an objective to be due")
	}
	if got.ID != "ci" {
		t.Fatalf("expected highest-priority %q, got %q", "ci", got.ID)
	}
}

func TestNextIdleTieBreaksByID(t *testing.T) {
	st := newFakeStore(
		Objective{ID: "zeta", Priority: 7, Enabled: true},
		Objective{ID: "alpha", Priority: 7, Enabled: true},
		Objective{ID: "mid", Priority: 7, Enabled: true},
	)
	b := New(st)
	got, ok, err := b.NextIdle(context.Background(), base)
	if err != nil || !ok {
		t.Fatalf("NextIdle ok=%v err=%v", ok, err)
	}
	if got.ID != "alpha" {
		t.Fatalf("equal priority should tie-break on smallest ID, want %q got %q", "alpha", got.ID)
	}
}

func TestNextIdleRespectsMinPeriod(t *testing.T) {
	hour := time.Hour
	st := newFakeStore(
		// High priority but ran 30m ago with a 1h period ⇒ NOT due.
		Objective{ID: "hot", Priority: 9, Enabled: true, MinPeriod: hour, LastRun: base.Add(-30 * time.Minute)},
		// Lower priority but ran 2h ago with a 1h period ⇒ due, so it wins.
		Objective{ID: "cool", Priority: 1, Enabled: true, MinPeriod: hour, LastRun: base.Add(-2 * hour)},
	)
	b := New(st)

	got, ok, err := b.NextIdle(context.Background(), base)
	if err != nil || !ok {
		t.Fatalf("NextIdle ok=%v err=%v", ok, err)
	}
	if got.ID != "cool" {
		t.Fatalf("a not-yet-due higher-priority objective must be skipped: want %q got %q", "cool", got.ID)
	}

	// Advance past the high-priority objective's period; now it should win.
	later := base.Add(31 * time.Minute)
	got, ok, err = b.NextIdle(context.Background(), later)
	if err != nil || !ok {
		t.Fatalf("NextIdle later ok=%v err=%v", ok, err)
	}
	if got.ID != "hot" {
		t.Fatalf("once due, the higher-priority objective wins: want %q got %q", "hot", got.ID)
	}
}

func TestNextIdleZeroPeriodAlwaysDue(t *testing.T) {
	st := newFakeStore(
		Objective{ID: "x", Priority: 2, Enabled: true, LastRun: base.Add(-time.Second)}, // zero MinPeriod
	)
	b := New(st)
	_, ok, err := b.NextIdle(context.Background(), base)
	if err != nil {
		t.Fatalf("NextIdle: %v", err)
	}
	if !ok {
		t.Fatal("a zero-MinPeriod enabled objective is always due")
	}
}

func TestNextIdleSkipsDisabled(t *testing.T) {
	st := newFakeStore(
		Objective{ID: "off", Goal: "paused", Priority: 99, Enabled: false},
		Objective{ID: "on", Goal: "live", Priority: 1, Enabled: true},
	)
	b := New(st)
	got, ok, err := b.NextIdle(context.Background(), base)
	if err != nil || !ok {
		t.Fatalf("NextIdle ok=%v err=%v", ok, err)
	}
	if got.ID != "on" {
		t.Fatalf("disabled objective must be skipped even at higher priority: got %q", got.ID)
	}
}

func TestNextIdleNoneDue(t *testing.T) {
	st := newFakeStore(
		Objective{ID: "a", Priority: 1, Enabled: false},
		Objective{ID: "b", Priority: 2, Enabled: true, MinPeriod: time.Hour, LastRun: base},
	)
	b := New(st)
	_, ok, err := b.NextIdle(context.Background(), base)
	if err != nil {
		t.Fatalf("NextIdle: %v", err)
	}
	if ok {
		t.Fatal("expected no objective due (all disabled or within period)")
	}
}

func TestNextIdleEmpty(t *testing.T) {
	b := New(newFakeStore())
	_, ok, err := b.NextIdle(context.Background(), base)
	if err != nil {
		t.Fatalf("NextIdle: %v", err)
	}
	if ok {
		t.Fatal("empty backlog yields no objective")
	}
}

func TestMarkRunAdvancesLastRun(t *testing.T) {
	st := newFakeStore(
		Objective{ID: "ci", Goal: "keep CI green", Priority: 5, Enabled: true, MinPeriod: time.Hour},
	)
	b := New(st)
	ctx := context.Background()

	// Initially due (never run).
	if _, ok, _ := b.NextIdle(ctx, base); !ok {
		t.Fatal("never-run objective should be due")
	}

	if err := b.MarkRun(ctx, "ci", base); err != nil {
		t.Fatalf("MarkRun: %v", err)
	}

	got, err := b.Get(ctx, "ci")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.LastRun.Equal(base) {
		t.Fatalf("LastRun not advanced: want %v got %v", base, got.LastRun)
	}
	// Other fields preserved through the read-modify-write.
	if got.Goal != "keep CI green" || got.Priority != 5 || !got.Enabled {
		t.Fatalf("MarkRun clobbered other fields: %+v", got)
	}

	// Now within the period ⇒ no longer due.
	if _, ok, _ := b.NextIdle(ctx, base.Add(10*time.Minute)); ok {
		t.Fatal("objective should be debounced within MinPeriod after MarkRun")
	}
	// Past the period ⇒ due again.
	if _, ok, _ := b.NextIdle(ctx, base.Add(2*time.Hour)); !ok {
		t.Fatal("objective should be due again after MinPeriod elapses")
	}
}

func TestMarkRunNotFound(t *testing.T) {
	b := New(newFakeStore())
	err := b.MarkRun(context.Background(), "ghost", base)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestMarkRunNormalizesToUTC(t *testing.T) {
	st := newFakeStore(Objective{ID: "x", Enabled: true})
	b := New(st)
	ctx := context.Background()
	// A non-UTC instant must be stored as UTC for deterministic comparisons.
	loc := time.FixedZone("X+5", 5*3600)
	when := time.Date(2026, 6, 26, 17, 0, 0, 0, loc)
	if err := b.MarkRun(ctx, "x", when); err != nil {
		t.Fatalf("MarkRun: %v", err)
	}
	got, _ := b.Get(ctx, "x")
	if got.LastRun.Location() != time.UTC {
		t.Fatalf("LastRun should be UTC, got %v", got.LastRun.Location())
	}
	if !got.LastRun.Equal(when) {
		t.Fatalf("instant changed by normalization: want %v got %v", when.UTC(), got.LastRun)
	}
}

func TestListIsDeterministicallyOrdered(t *testing.T) {
	st := newFakeStore(
		Objective{ID: "b", Priority: 1, Enabled: true},
		Objective{ID: "a", Priority: 5, Enabled: false},
		Objective{ID: "c", Priority: 5, Enabled: true},
	)
	b := New(st)
	got, err := b.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// Highest priority first, ties by ascending ID; disabled still listed.
	wantOrder := []string{"a", "c", "b"}
	if len(got) != len(wantOrder) {
		t.Fatalf("List len = %d, want %d", len(got), len(wantOrder))
	}
	for i, id := range wantOrder {
		if got[i].ID != id {
			t.Fatalf("List order[%d] = %q, want %q (full: %v)", i, got[i].ID, id, ids(got))
		}
	}
}

func TestPutThenGet(t *testing.T) {
	b := New(newFakeStore())
	ctx := context.Background()
	o := Objective{ID: "new", Goal: "do the thing", Priority: 3, Enabled: true, MinPeriod: time.Minute}
	if err := b.Put(ctx, o); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := b.Get(ctx, "new")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != o {
		t.Fatalf("round-trip mismatch: want %+v got %+v", o, got)
	}
}

func TestGetNotFound(t *testing.T) {
	b := New(newFakeStore())
	_, err := b.Get(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestDisableMakesInert(t *testing.T) {
	st := newFakeStore(Objective{ID: "x", Priority: 9, Enabled: true})
	b := New(st)
	ctx := context.Background()
	if err := b.Disable(ctx, "x"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, ok, _ := b.NextIdle(ctx, base); ok {
		t.Fatal("a disabled objective must never be selected")
	}
	// Still retained (paused, not deleted).
	got, err := b.Get(ctx, "x")
	if err != nil {
		t.Fatalf("Get after Disable: %v", err)
	}
	if got.Enabled {
		t.Fatal("Disable should clear Enabled")
	}
}

func TestErrorsPropagate(t *testing.T) {
	boom := errors.New("store unavailable")
	st := &fakeStore{m: map[string]Objective{}, err: boom}
	b := New(st)
	ctx := context.Background()

	if _, _, err := b.NextIdle(ctx, base); !errors.Is(err, boom) {
		t.Fatalf("NextIdle should propagate store error, got %v", err)
	}
	if _, err := b.List(ctx); !errors.Is(err, boom) {
		t.Fatalf("List should propagate store error, got %v", err)
	}
	if err := b.MarkRun(ctx, "x", base); !errors.Is(err, boom) {
		t.Fatalf("MarkRun should propagate store error, got %v", err)
	}
}

// TestNilStoreIsInert proves the default-off contract: an unwired backlog is a nil-safe
// no-op so the autonomy source stays byte-identically off until objectives exist.
func TestNilStoreIsInert(t *testing.T) {
	b := New(nil)
	ctx := context.Background()

	if _, ok, err := b.NextIdle(ctx, base); ok || err != nil {
		t.Fatalf("nil-store NextIdle should be (false,nil), got ok=%v err=%v", ok, err)
	}
	if got, err := b.List(ctx); got != nil || err != nil {
		t.Fatalf("nil-store List should be (nil,nil), got %v %v", got, err)
	}
	if err := b.Put(ctx, Objective{ID: "x"}); err != nil {
		t.Fatalf("nil-store Put should be nil, got %v", err)
	}
	if err := b.Disable(ctx, "x"); err != nil {
		t.Fatalf("nil-store Disable should be nil, got %v", err)
	}
	if err := b.MarkRun(ctx, "x", base); err != nil {
		t.Fatalf("nil-store MarkRun should be nil, got %v", err)
	}
	if _, err := b.Get(ctx, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nil-store Get should be ErrNotFound, got %v", err)
	}
}

// TestWithClockIsDeterministic shows the injectable clock drives "now" when a caller
// uses the backlog's own clock helper rather than passing now explicitly.
func TestWithClockIsDeterministic(t *testing.T) {
	st := newFakeStore(Objective{ID: "x", Enabled: true, MinPeriod: time.Hour, LastRun: base})
	b := New(st).WithClock(clockAt(base.Add(90 * time.Minute)))
	// Use the injected clock as `now` to confirm it is wired and pure.
	_, ok, err := b.NextIdle(context.Background(), b.clock())
	if err != nil || !ok {
		t.Fatalf("with injected clock past the period, expected due: ok=%v err=%v", ok, err)
	}
}

func ids(os []Objective) []string {
	out := make([]string, len(os))
	for i, o := range os {
		out[i] = o.ID
	}
	return out
}
