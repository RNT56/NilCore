package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"nilcore/internal/autosrc"
	"nilcore/internal/objective"
	"nilcore/internal/wake"
)

// fakeObjectiveStore is a tiny in-memory objective.Store for exercising the autonomy
// handler's MarkSuccess wiring without standing up SQLite.
type fakeObjectiveStore struct {
	m map[string]objective.Objective
}

func newFakeObjectiveStore(objs ...objective.Objective) *fakeObjectiveStore {
	m := make(map[string]objective.Objective, len(objs))
	for _, o := range objs {
		m[o.ID] = o
	}
	return &fakeObjectiveStore{m: m}
}

func (f *fakeObjectiveStore) Put(_ context.Context, o objective.Objective) error {
	f.m[o.ID] = o
	return nil
}
func (f *fakeObjectiveStore) Get(_ context.Context, id string) (objective.Objective, error) {
	o, ok := f.m[id]
	if !ok {
		return objective.Objective{}, objective.ErrNotFound
	}
	return o, nil
}
func (f *fakeObjectiveStore) List(_ context.Context) ([]objective.Objective, error) {
	out := make([]objective.Objective, 0, len(f.m))
	for _, o := range f.m {
		out = append(out, o)
	}
	return out, nil
}
func (f *fakeObjectiveStore) Disable(_ context.Context, id string) error {
	o, ok := f.m[id]
	if !ok {
		return objective.ErrNotFound
	}
	o.Enabled = false
	f.m[id] = o
	return nil
}

// TestMarkObjectiveSuccessAdvancesLastSuccess proves the handler's verified-outcome
// wiring: markObjectiveSuccess resolves the enabled objective by its goal and advances
// LastSuccess (and LastRun), so the success-aware cadence (MinPeriod, not the shorter
// RetryPeriod) gates the next pull.
func TestMarkObjectiveSuccessAdvancesLastSuccess(t *testing.T) {
	ctx := context.Background()
	fs := newFakeObjectiveStore(
		objective.Objective{ID: "ci", Goal: "keep CI green", Enabled: true,
			MinPeriod: time.Hour, RetryPeriod: time.Minute},
	)
	backlog := objective.New(fs)

	markObjectiveSuccess(ctx, backlog, "keep CI green")

	got, err := backlog.Get(ctx, "ci")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LastSuccess.IsZero() || got.LastRun.IsZero() {
		t.Fatalf("MarkSuccess must advance LastRun and LastSuccess: %+v", got)
	}
	if !got.LastSuccess.Equal(got.LastRun) {
		t.Fatalf("MarkSuccess should set LastRun == LastSuccess: %+v", got)
	}
}

// TestMarkObjectiveSuccessSkipsAmbiguous proves the handler credits nothing when the
// goal does not resolve to exactly one enabled objective — an unmatched goal (a
// file/wake signal that happened to reach here) or a duplicate-goal backlog is left to
// re-arm after RetryPeriod rather than crediting the wrong record.
func TestMarkObjectiveSuccessSkipsAmbiguous(t *testing.T) {
	ctx := context.Background()
	fs := newFakeObjectiveStore(
		objective.Objective{ID: "a", Goal: "dup goal", Enabled: true},
		objective.Objective{ID: "b", Goal: "dup goal", Enabled: true},
	)
	backlog := objective.New(fs)

	markObjectiveSuccess(ctx, backlog, "dup goal")     // ambiguous ⇒ no-op
	markObjectiveSuccess(ctx, backlog, "no such goal") // unmatched ⇒ no-op

	for _, id := range []string{"a", "b"} {
		got, err := backlog.Get(ctx, id)
		if err != nil {
			t.Fatalf("get %q: %v", id, err)
		}
		if !got.LastSuccess.IsZero() {
			t.Fatalf("ambiguous/unmatched goal must not credit %q: %+v", id, got)
		}
	}
}

// TestMarkObjectiveSuccessListErrorIsNonFatal proves a store error during the lookup is
// swallowed (best-effort): the verified work already shipped, so a missed success
// timestamp must never panic or propagate — it only re-services the objective sooner.
func TestMarkObjectiveSuccessListErrorIsNonFatal(t *testing.T) {
	backlog := objective.New(errObjectiveStore{})
	// Must not panic; nothing to assert beyond a clean return.
	markObjectiveSuccess(context.Background(), backlog, "anything")
}

type errObjectiveStore struct{}

func (errObjectiveStore) Put(context.Context, objective.Objective) error { return errObjBoom }
func (errObjectiveStore) Get(context.Context, string) (objective.Objective, error) {
	return objective.Objective{}, errObjBoom
}
func (errObjectiveStore) List(context.Context) ([]objective.Objective, error) {
	return nil, errObjBoom
}
func (errObjectiveStore) Disable(context.Context, string) error { return errObjBoom }

var errObjBoom = errors.New("objective store unavailable")

// TestFileSignalFeeder proves the file funnel: a dropped goal file is read into a
// FileSignal (name + trimmed contents), emitted on the channel the FileSource drains,
// and removed (processed once).
func TestFileSignalFeeder(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "task1"), []byte("  fix the flaky test \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan autosrc.FileSignal)
	go fileSignalFeeder(ctx, dir, ch, 2*time.Millisecond)

	select {
	case fs := <-ch:
		if fs.Name != "task1" || fs.Goal != "fix the flaky test" {
			t.Fatalf("FileSignal = %+v, want {task1, fix the flaky test}", fs)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the file signal")
	}
	// The file must be consumed (removed) so it never re-fires.
	if _, err := os.Stat(filepath.Join(dir, "task1")); !os.IsNotExist(err) {
		t.Error("signal file should have been removed after processing")
	}
}

// fakeWakeStore is a tiny in-memory wake.Store for exercising the wake feeder.
type fakeWakeStore struct {
	mu sync.Mutex
	m  map[string]string
}

func (f *fakeWakeStore) SaveWake(_ context.Context, id, detail string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil {
		f.m = map[string]string{}
	}
	f.m[id] = detail
	return nil
}
func (f *fakeWakeStore) LoadWakes(_ context.Context) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.m))
	for k, v := range f.m {
		out[k] = v
	}
	return out, nil
}
func (f *fakeWakeStore) DisarmWake(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, id)
	return nil
}
func (f *fakeWakeStore) armed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.m)
}

// TestWakeFeederFiresAndDisarms proves the wake funnel — the gap this fills: a due
// durable wake is fired (its note emitted for the WakeSource) and disarmed so it never
// re-fires. (Before this wiring, serve armed wakes but never fired them.)
func TestWakeFeederFiresAndDisarms(t *testing.T) {
	store := &fakeWakeStore{}
	reg := wake.New(store, nil)
	// after=0 ⇒ WakeAt == now ⇒ immediately due.
	if _, err := reg.Arm(context.Background(), "thread-7", "sender", 0, "resume the migration"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan autosrc.Wake)
	go wakeFeeder(ctx, reg, ch, 2*time.Millisecond)

	select {
	case w := <-ch:
		if w.ThreadID != "thread-7" || w.Note != "resume the migration" {
			t.Fatalf("Wake = %+v, want {thread-7, resume the migration}", w)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the wake to fire")
	}
	// The fired wake must be disarmed so it never re-fires.
	deadline := time.After(2 * time.Second)
	for store.armed() != 0 {
		select {
		case <-deadline:
			t.Fatal("fired wake was not disarmed")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestWakeFeederSkipsNotDueAndFiresAtMostOnce: a not-yet-due wake is never delivered,
// and a due wake fires exactly once across many ticks (Registry.Claim durably disarms
// the wake on the winning fire, so no later tick re-claims it) while the future wake
// stays armed.
func TestWakeFeederSkipsNotDueAndFiresAtMostOnce(t *testing.T) {
	store := &fakeWakeStore{}
	reg := wake.New(store, nil)
	if _, err := reg.Arm(context.Background(), "due", "s", 0, "now"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Arm(context.Background(), "future", "s", time.Hour, "later"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan autosrc.Wake)
	go wakeFeeder(ctx, reg, ch, 2*time.Millisecond)

	select {
	case w := <-ch:
		if w.ThreadID != "due" {
			t.Fatalf("only the due wake should fire, got %q", w.ThreadID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the due wake never fired")
	}
	// No second delivery across many ticks: the due wake is at-most-once, the future
	// wake is not due.
	select {
	case w := <-ch:
		t.Fatalf("unexpected second delivery: %+v", w)
	case <-time.After(80 * time.Millisecond):
	}
	if store.armed() != 1 {
		t.Errorf("the future (not-due) wake must stay armed, armed=%d", store.armed())
	}
}

// TestWakeFeederSingleFireAcrossPollers is the finding's guarantee: two feeders over
// the SAME registry (as two serve pollers, or serve's own runWaker + this feeder,
// would be) must fire a due wake AT MOST ONCE — Registry.Claim is the durable,
// atomic single-fire gate, not a per-process in-memory guard. Exactly one delivery
// lands across both channels, and the wake is durably disarmed.
func TestWakeFeederSingleFireAcrossPollers(t *testing.T) {
	store := &fakeWakeStore{}
	reg := wake.New(store, nil)
	if _, err := reg.Arm(context.Background(), "shared", "s", 0, "once"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chA := make(chan autosrc.Wake, 4)
	chB := make(chan autosrc.Wake, 4)
	go wakeFeeder(ctx, reg, chA, time.Millisecond)
	go wakeFeeder(ctx, reg, chB, time.Millisecond)

	var got []autosrc.Wake
	deadline := time.After(2 * time.Second)
	// Collect for a window long enough for many ticks on both feeders.
	drain := time.After(200 * time.Millisecond)
loop:
	for {
		select {
		case w := <-chA:
			got = append(got, w)
		case w := <-chB:
			got = append(got, w)
		case <-drain:
			break loop
		case <-deadline:
			break loop
		}
	}
	if len(got) != 1 {
		t.Fatalf("wake fired %d times across two pollers, want exactly 1 (durable at-most-once): %+v", len(got), got)
	}
	if store.armed() != 0 {
		t.Errorf("the fired wake must be durably disarmed, armed=%d", store.armed())
	}
}
