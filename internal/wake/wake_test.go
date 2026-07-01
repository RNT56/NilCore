package wake

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStore is an in-memory wake.Store: armed wakes live in `armed`; Disarm removes
// (modelling the status-flip's effect — excluded from Pending). It is mutex-guarded so
// the -race detector exercises a realistic, goroutine-safe seam (like the real store).
type fakeStore struct {
	mu        sync.Mutex
	armed     map[string]string
	disarmCnt map[string]int // how many times DisarmWake landed per thread (single-fire proof)
}

func newFakeStore() *fakeStore {
	return &fakeStore{armed: map[string]string{}, disarmCnt: map[string]int{}}
}

func (f *fakeStore) SaveWake(_ context.Context, threadID, detail string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed[threadID] = detail
	return nil
}
func (f *fakeStore) LoadWakes(context.Context) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.armed))
	for k, v := range f.armed {
		out[k] = v
	}
	return out, nil
}
func (f *fakeStore) DisarmWake(_ context.Context, threadID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.armed, threadID)
	f.disarmCnt[threadID]++
	return nil
}

func (f *fakeStore) disarms(threadID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.disarmCnt[threadID]
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
	if won, err := r.Claim(context.Background(), "t"); won || err != nil {
		t.Errorf("nil-store Claim = (%v,%v), want (false,nil)", won, err)
	}
}

// TestClaimSingleFire proves the single-fire primitive (B5-autonomy.2): the FIRST
// Claim of an armed wake wins and durably disarms it; a SECOND Claim of the same wake
// loses (won=false) and does not re-disarm — so two pollers sharing one registry deliver
// the wake exactly once.
func TestClaimSingleFire(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	r := New(fs, nil)
	if _, err := r.Arm(ctx, "thread-1", "user-1", time.Minute, "n"); err != nil {
		t.Fatalf("Arm: %v", err)
	}

	won1, err := r.Claim(ctx, "thread-1")
	if err != nil || !won1 {
		t.Fatalf("first Claim should win: won=%v err=%v", won1, err)
	}
	won2, err := r.Claim(ctx, "thread-1")
	if err != nil || won2 {
		t.Fatalf("second Claim of the same wake must lose: won=%v err=%v", won2, err)
	}
	// Exactly one durable disarm landed.
	if got := fs.disarms("thread-1"); got != 1 {
		t.Fatalf("a claimed wake must be disarmed exactly once, got %d", got)
	}
	// The wake is gone from Pending.
	if pend, _ := r.Pending(ctx); len(pend) != 0 {
		t.Fatalf("a claimed wake must not remain Pending, got %v", pend)
	}
}

// TestClaimConcurrentExactlyOnce is the race-detector proof: many goroutines racing to
// Claim ONE armed wake yield exactly one winner, modelling the serve waker and the
// autonomy feeder both polling the SAME shared registry.
func TestClaimConcurrentExactlyOnce(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	r := New(fs, nil)
	if _, err := r.Arm(ctx, "t", "s", time.Minute, "n"); err != nil {
		t.Fatalf("Arm: %v", err)
	}

	const racers = 16
	var wg sync.WaitGroup
	wins := make([]bool, racers)
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func(i int) {
			defer wg.Done()
			won, err := r.Claim(ctx, "t")
			if err != nil {
				t.Errorf("Claim error: %v", err)
			}
			wins[i] = won
		}(i)
	}
	wg.Wait()

	winners := 0
	for _, w := range wins {
		if w {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("exactly one racer must win the single-fire claim, got %d winners", winners)
	}
	if got := fs.disarms("t"); got != 1 {
		t.Fatalf("a concurrently-claimed wake must disarm exactly once, got %d", got)
	}
}

// TestReArmIsClaimableAgain proves a re-armed wake clears the prior claim, so a thread
// that fired once can fire again after the agent arms a fresh self-timer.
func TestReArmIsClaimableAgain(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	r := New(fs, nil)
	if _, err := r.Arm(ctx, "t", "s", time.Minute, "first"); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	if won, _ := r.Claim(ctx, "t"); !won {
		t.Fatal("first Claim should win")
	}
	// Re-arm a fresh wake for the same thread.
	if _, err := r.Arm(ctx, "t", "s", time.Minute, "second"); err != nil {
		t.Fatalf("re-Arm: %v", err)
	}
	if won, err := r.Claim(ctx, "t"); err != nil || !won {
		t.Fatalf("a re-armed wake must be claimable again: won=%v err=%v", won, err)
	}
}
