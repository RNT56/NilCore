package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"nilcore/internal/autosrc"
	"nilcore/internal/wake"
)

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
// and a due wake fires exactly once across many ticks (deliver-then-disarm + the
// in-flight guard prevent a re-fire) while the future wake stays armed.
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
