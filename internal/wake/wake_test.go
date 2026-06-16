package wake

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeStore is an in-memory wake.Store: armed wakes live in `armed`; Disarm removes
// (modelling the status-flip's effect — excluded from Pending).
type fakeStore struct{ armed map[string]string }

func newFakeStore() *fakeStore { return &fakeStore{armed: map[string]string{}} }

func (f *fakeStore) SaveWake(_ context.Context, threadID, detail string) error {
	f.armed[threadID] = detail
	return nil
}
func (f *fakeStore) LoadWakes(context.Context) (map[string]string, error) {
	out := make(map[string]string, len(f.armed))
	for k, v := range f.armed {
		out[k] = v
	}
	return out, nil
}
func (f *fakeStore) DisarmWake(_ context.Context, threadID string) error {
	delete(f.armed, threadID)
	return nil
}

func TestRegistryArmPendingDisarm(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	fixed := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	r := &Registry{store: fs, now: func() time.Time { return fixed }}

	wakeAt, err := r.Arm(ctx, "thread-1", "user-1", 30*time.Minute, "check CI run 42")
	if err != nil {
		t.Fatalf("Arm: %v", err)
	}
	if !wakeAt.Equal(fixed.Add(30 * time.Minute)) {
		t.Errorf("wakeAt = %v, want now+30m", wakeAt)
	}

	pend, err := r.Pending(ctx)
	if err != nil || len(pend) != 1 {
		t.Fatalf("Pending = %v (err %v), want 1", pend, err)
	}
	w := pend[0]
	if w.ThreadID != "thread-1" || w.Sender != "user-1" || w.Note != "check CI run 42" || !w.WakeAt.Equal(fixed.Add(30*time.Minute)) {
		t.Errorf("round-trip wrong: %+v", w)
	}

	// Re-arm replaces (one self-timer per thread).
	if _, err := r.Arm(ctx, "thread-1", "user-1", time.Hour, "newer"); err != nil {
		t.Fatal(err)
	}
	if pend, _ := r.Pending(ctx); len(pend) != 1 || pend[0].Note != "newer" {
		t.Errorf("re-arm should replace, got %v", pend)
	}

	// Disarm clears it (excluded from Pending).
	if err := r.Disarm(ctx, "thread-1"); err != nil {
		t.Fatal(err)
	}
	if pend, _ := r.Pending(ctx); len(pend) != 0 {
		t.Errorf("after Disarm Pending should be empty, got %v", pend)
	}
}

func TestRegistryClipsNote(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	r := &Registry{store: fs}
	if _, err := r.Arm(ctx, "t", "s", time.Minute, strings.Repeat("x", 5000)); err != nil {
		t.Fatal(err)
	}
	pend, _ := r.Pending(ctx)
	if len(pend) != 1 || len(pend[0].Note) > maxNoteBytes {
		t.Errorf("note not clipped to %d bytes: len=%d", maxNoteBytes, len(pend[0].Note))
	}
}

// A nil store makes Arm/Disarm/Pending safe no-ops (defensive — the caller guards,
// but this must never panic).
func TestRegistryNilStoreSafe(t *testing.T) {
	r := New(nil, nil)
	if _, err := r.Arm(context.Background(), "t", "s", time.Minute, "n"); err != nil {
		t.Errorf("nil-store Arm: %v", err)
	}
	if p, err := r.Pending(context.Background()); err != nil || p != nil {
		t.Errorf("nil-store Pending = %v, %v", p, err)
	}
}
