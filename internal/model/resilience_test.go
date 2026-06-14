package model

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeProvider is a controllable Provider for the resilience tests. It fails its
// first failUntil calls (returning errFail) and succeeds afterward. When
// alwaysFail is set it never succeeds. Call counting is mutex-guarded so the
// breaker's concurrency safety is exercised honestly.
type fakeProvider struct {
	model      string
	mu         sync.Mutex
	calls      int
	failUntil  int  // first N calls fail
	alwaysFail bool // never succeed
	blockOnCtx bool // block until ctx is done, then return ctx.Err()
}

var errFail = errors.New("provider down")

func (f *fakeProvider) Complete(ctx context.Context, _ string, _ []Message, _ []Tool, _ int) (Response, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()

	if f.blockOnCtx {
		<-ctx.Done()
		return Response{}, ctx.Err()
	}
	if f.alwaysFail || n <= f.failUntil {
		return Response{}, errFail
	}
	return Response{
		Content:    []Block{{Type: "text", Text: f.model + "-ok"}},
		StopReason: "end_turn",
		Usage:      Usage{InputTokens: 1, OutputTokens: 1},
	}, nil
}

func (f *fakeProvider) Model() string { return f.model }

func (f *fakeProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newTestResilient wires a Resilient whose sleep is a no-op so retries are
// instant. The breaker clock is a controllable fake.
func newTestResilient(t *testing.T, providers []Provider, opts Options, clock func() time.Time) *Resilient {
	t.Helper()
	r, err := NewResilient(providers, opts)
	if err != nil {
		t.Fatalf("NewResilient: %v", err)
	}
	r.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	if clock != nil {
		r.now = clock
	}
	return r
}

func TestNewResilient_Validation(t *testing.T) {
	if _, err := NewResilient(nil, Options{}); !errors.Is(err, ErrNoProviders) {
		t.Fatalf("want ErrNoProviders, got %v", err)
	}
	p := &fakeProvider{model: "primary"}
	r, err := NewResilient([]Provider{p}, Options{})
	if err != nil {
		t.Fatalf("NewResilient: %v", err)
	}
	if got := r.Model(); got != "primary" {
		t.Fatalf("Model() = %q, want primary", got)
	}
}

func TestComplete_RetrySucceeds(t *testing.T) {
	// Fails twice, then succeeds on the third call: a single provider with enough
	// retries must recover without failover.
	p := &fakeProvider{model: "p", failUntil: 2}
	r := newTestResilient(t, []Provider{p}, Options{
		MaxRetries:  3,
		BaseBackoff: time.Millisecond,
	}, nil)

	resp, err := r.Complete(context.Background(), "", nil, nil, 16)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.Content) == 0 || resp.Content[0].Text != "p-ok" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if p.callCount() != 3 {
		t.Fatalf("calls = %d, want 3", p.callCount())
	}
}

func TestComplete_RetryExhausted(t *testing.T) {
	// More failures than retries allow: the single provider gives up.
	p := &fakeProvider{model: "p", alwaysFail: true}
	r := newTestResilient(t, []Provider{p}, Options{
		MaxRetries:  2,
		BaseBackoff: time.Millisecond,
	}, nil)

	_, err := r.Complete(context.Background(), "", nil, nil, 16)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !errors.Is(err, errFail) {
		t.Fatalf("want wrapped errFail, got %v", err)
	}
	if p.callCount() != 3 { // initial + 2 retries
		t.Fatalf("calls = %d, want 3", p.callCount())
	}
}

func TestComplete_Failover(t *testing.T) {
	// Primary always fails; fallback works. The wrapper must fail over and the
	// fallback's response is returned.
	primary := &fakeProvider{model: "primary", alwaysFail: true}
	fallback := &fakeProvider{model: "fallback"}
	r := newTestResilient(t, []Provider{primary, fallback}, Options{
		MaxRetries:  1,
		BaseBackoff: time.Millisecond,
	}, nil)

	resp, err := r.Complete(context.Background(), "", nil, nil, 16)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content[0].Text != "fallback-ok" {
		t.Fatalf("got %q, want fallback-ok", resp.Content[0].Text)
	}
	if primary.callCount() != 2 { // initial + 1 retry, all failed
		t.Fatalf("primary calls = %d, want 2", primary.callCount())
	}
	if fallback.callCount() != 1 {
		t.Fatalf("fallback calls = %d, want 1", fallback.callCount())
	}
}

func TestComplete_AllProvidersFail(t *testing.T) {
	primary := &fakeProvider{model: "primary", alwaysFail: true}
	fallback := &fakeProvider{model: "fallback", alwaysFail: true}
	r := newTestResilient(t, []Provider{primary, fallback}, Options{
		MaxRetries:  0,
		BaseBackoff: time.Millisecond,
	}, nil)

	_, err := r.Complete(context.Background(), "", nil, nil, 16)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !errors.Is(err, errFail) {
		t.Fatalf("want wrapped errFail, got %v", err)
	}
}

func TestComplete_BreakerTripsAndSkips(t *testing.T) {
	// A single always-failing provider with a 2-failure threshold. Each Complete
	// uses MaxRetries=1, so one Complete == 2 consecutive failures == breaker
	// open. The next Complete must skip the provider entirely (no new calls).
	clock := &fakeClock{t: time.Unix(0, 0)}
	p := &fakeProvider{model: "p", alwaysFail: true}
	r := newTestResilient(t, []Provider{p}, Options{
		MaxRetries:       1,
		BaseBackoff:      time.Millisecond,
		BreakerThreshold: 2,
		BreakerCooldown:  time.Minute,
	}, clock.Now)

	// First Complete: 2 failing attempts -> breaker opens.
	if _, err := r.Complete(context.Background(), "", nil, nil, 16); err == nil {
		t.Fatal("want error on first Complete")
	}
	callsAfterFirst := p.callCount()
	if callsAfterFirst != 2 {
		t.Fatalf("calls after first Complete = %d, want 2", callsAfterFirst)
	}

	// Second Complete: breaker is open, provider must be skipped (no new calls).
	_, err := r.Complete(context.Background(), "", nil, nil, 16)
	if err == nil {
		t.Fatal("want error on second Complete")
	}
	if p.callCount() != callsAfterFirst {
		t.Fatalf("provider was called while breaker open: calls = %d, want %d", p.callCount(), callsAfterFirst)
	}
}

func TestComplete_BreakerRecoversAfterCooldown(t *testing.T) {
	// After the cooldown elapses, a half-open trial is allowed; if it succeeds
	// the breaker resets. failUntil=2 means the two opening failures fail, then
	// the trial call (3rd) succeeds.
	clock := &fakeClock{t: time.Unix(0, 0)}
	p := &fakeProvider{model: "p", failUntil: 2}
	r := newTestResilient(t, []Provider{p}, Options{
		MaxRetries:       1,
		BaseBackoff:      time.Millisecond,
		BreakerThreshold: 2,
		BreakerCooldown:  time.Minute,
	}, clock.Now)

	// Trip the breaker (2 failures).
	if _, err := r.Complete(context.Background(), "", nil, nil, 16); err == nil {
		t.Fatal("want error while tripping breaker")
	}

	// Still inside cooldown: skipped.
	if _, err := r.Complete(context.Background(), "", nil, nil, 16); err == nil {
		t.Fatal("want error while breaker open")
	}
	if p.callCount() != 2 {
		t.Fatalf("provider called during cooldown: calls = %d, want 2", p.callCount())
	}

	// Advance past cooldown: half-open trial should run and succeed.
	clock.advance(2 * time.Minute)
	resp, err := r.Complete(context.Background(), "", nil, nil, 16)
	if err != nil {
		t.Fatalf("Complete after cooldown: %v", err)
	}
	if resp.Content[0].Text != "p-ok" {
		t.Fatalf("got %q, want p-ok", resp.Content[0].Text)
	}
}

func TestComplete_BreakerSkipsToFallback(t *testing.T) {
	// Primary's breaker is pre-opened; the working fallback must still be reached.
	clock := &fakeClock{t: time.Unix(0, 0)}
	primary := &fakeProvider{model: "primary", alwaysFail: true}
	fallback := &fakeProvider{model: "fallback"}
	r := newTestResilient(t, []Provider{primary, fallback}, Options{
		MaxRetries:       0,
		BaseBackoff:      time.Millisecond,
		BreakerThreshold: 1,
		BreakerCooldown:  time.Minute,
	}, clock.Now)

	// First Complete trips primary's breaker (1 failure) and fails over.
	if resp, err := r.Complete(context.Background(), "", nil, nil, 16); err != nil {
		t.Fatalf("first Complete: %v", err)
	} else if resp.Content[0].Text != "fallback-ok" {
		t.Fatalf("got %q, want fallback-ok", resp.Content[0].Text)
	}
	primaryCalls := primary.callCount()

	// Second Complete: primary skipped (breaker open), fallback serves again.
	if resp, err := r.Complete(context.Background(), "", nil, nil, 16); err != nil {
		t.Fatalf("second Complete: %v", err)
	} else if resp.Content[0].Text != "fallback-ok" {
		t.Fatalf("got %q, want fallback-ok", resp.Content[0].Text)
	}
	if primary.callCount() != primaryCalls {
		t.Fatalf("primary called while breaker open: %d, want %d", primary.callCount(), primaryCalls)
	}
}

func TestComplete_ContextCancelledStopsRetries(t *testing.T) {
	// A provider that blocks until ctx is done. With a tiny CallTimeout the call
	// returns deadline-exceeded; a cancelled parent ctx must stop the walk.
	p := &fakeProvider{model: "p", blockOnCtx: true}
	r := newTestResilient(t, []Provider{p}, Options{
		MaxRetries:  5,
		BaseBackoff: time.Millisecond,
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, err := r.Complete(ctx, "", nil, nil, 16)
	if err == nil {
		t.Fatal("want error from cancelled context")
	}
	// Cancelled before any work: should not have looped many times.
	if p.callCount() > 1 {
		t.Fatalf("provider called %d times after cancel, want <= 1", p.callCount())
	}
}

func TestComplete_PerCallTimeout(t *testing.T) {
	// Provider blocks on ctx; CallTimeout must fire and surface deadline-exceeded.
	p := &fakeProvider{model: "p", blockOnCtx: true}
	r := newTestResilient(t, []Provider{p}, Options{
		MaxRetries:  0,
		BaseBackoff: time.Millisecond,
		CallTimeout: 5 * time.Millisecond,
	}, nil)

	_, err := r.Complete(context.Background(), "", nil, nil, 16)
	if err == nil {
		t.Fatal("want timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}

func TestBackoff_ExponentialAndCapped(t *testing.T) {
	r := newTestResilient(t, []Provider{&fakeProvider{model: "p"}}, Options{
		BaseBackoff: 10 * time.Millisecond,
		MaxBackoff:  50 * time.Millisecond,
	}, nil)

	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 10 * time.Millisecond},
		{2, 20 * time.Millisecond},
		{3, 40 * time.Millisecond},
		{4, 50 * time.Millisecond}, // capped
		{5, 50 * time.Millisecond}, // capped
	}
	for _, c := range cases {
		if got := r.backoff(c.attempt); got != c.want {
			t.Errorf("backoff(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}

func TestBackoff_JitterWithinBounds(t *testing.T) {
	r := newTestResilient(t, []Provider{&fakeProvider{model: "p"}}, Options{
		BaseBackoff: 10 * time.Millisecond,
		MaxBackoff:  10 * time.Millisecond,
		Jitter:      5 * time.Millisecond,
	}, nil)
	for i := 0; i < 100; i++ {
		got := r.backoff(1)
		if got < 10*time.Millisecond || got >= 15*time.Millisecond {
			t.Fatalf("backoff with jitter = %v, want [10ms, 15ms)", got)
		}
	}
}

// fakeClock is a deterministic, controllable clock for breaker timing.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
