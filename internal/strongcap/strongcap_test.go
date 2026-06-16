package strongcap

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nilcore/internal/model"
)

// gateProv is a fake provider whose Complete blocks until released, so a test can
// observe how many calls the limiter admits into Inner at once. Each entry sends to
// entered (so the test can count admissions) and then waits on release.
type gateProv struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (g *gateProv) Model() string { return "fake-strong" }
func (g *gateProv) Complete(ctx context.Context, _ string, _ []model.Message, _ []model.Tool, _ int) (model.Response, error) {
	g.calls.Add(1)
	g.entered <- struct{}{}
	select {
	case <-g.release:
		return model.Response{StopReason: "end_turn"}, nil
	case <-ctx.Done():
		return model.Response{}, ctx.Err()
	}
}

// At most `max` calls reach Inner concurrently; the rest block on the gate until a
// slot frees. We admit exactly max, prove a further call cannot enter, then release.
func TestCapRespected(t *testing.T) {
	g := &gateProv{entered: make(chan struct{}, 8), release: make(chan struct{})}
	const max = 2
	lim := New(g, max)

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = lim.Complete(context.Background(), "", nil, nil, 0) }()
	}

	// Exactly `max` admissions should arrive; a (max+1)th must NOT within the window.
	for i := 0; i < max; i++ {
		select {
		case <-g.entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d calls were admitted", i, max)
		}
	}
	select {
	case <-g.entered:
		t.Fatalf("limiter admitted more than the cap of %d concurrent calls", max)
	case <-time.After(100 * time.Millisecond):
		// good: the gate is holding the rest back
	}

	close(g.release) // let everyone drain
	wg.Wait()
	if got := g.calls.Load(); got != 5 {
		t.Errorf("all 5 calls should eventually run; got %d", got)
	}
}

// A full gate + a done ctx returns ctx.Err() WITHOUT touching Inner — the no-hang
// fallback: the caller's ask_advisor handler then degrades to "proceed", never blocks.
func TestCtxHonoredOnFullGate(t *testing.T) {
	g := &gateProv{entered: make(chan struct{}, 1), release: make(chan struct{})}
	lim := New(g, 1)

	// Hold the only slot.
	go func() { _, _ = lim.Complete(context.Background(), "", nil, nil, 0) }()
	<-g.entered // the holder is now inside Inner

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := lim.Complete(cancelled, "", nil, nil, 0)
	if err != context.Canceled {
		t.Fatalf("want context.Canceled on a full gate with a done ctx, got %v", err)
	}
	if g.calls.Load() != 1 {
		t.Errorf("a ctx-cancelled acquire must NOT enter Inner; Inner calls = %d, want 1", g.calls.Load())
	}
	close(g.release)
}

// Model() is transparent; a non-positive cap clamps to 1 (never zero — that would
// deadlock the first caller).
func TestModelPassthroughAndClamp(t *testing.T) {
	g := &gateProv{entered: make(chan struct{}, 1), release: make(chan struct{})}
	if New(g, 0).Model() != "fake-strong" {
		t.Error("Model() must pass through to Inner")
	}
	lim := New(g, 0) // clamped to 1
	close(g.release)
	if _, err := lim.Complete(context.Background(), "", nil, nil, 0); err != nil {
		t.Fatalf("a clamped (cap-1) limiter must still admit a call: %v", err)
	}
}
