package model

import (
	"context"
	"errors"
	"net/http"
	"strings"
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

// streamingFakeProvider is a controllable Streamer (and Provider) for the
// streaming resilience tests. Like fakeProvider it fails its first failUntil
// calls; each call forwards a fixed sequence of deltas to onChunk BEFORE
// returning, so a test can prove that a failed attempt's chunks are discarded
// (no double-emit) while only the winning attempt's chunks reach the caller.
type streamingFakeProvider struct {
	model      string
	mu         sync.Mutex
	calls      int
	failUntil  int
	alwaysFail bool
	deltas     []string
}

func (f *streamingFakeProvider) Complete(ctx context.Context, _ string, _ []Message, _ []Tool, _ int) (Response, error) {
	// Streaming providers still implement Complete; route it through the same
	// failure model so a non-Stream caller behaves identically.
	return f.Stream(ctx, "", nil, nil, 0, nil)
}

func (f *streamingFakeProvider) Stream(ctx context.Context, _ string, _ []Message, _ []Tool, _ int, onChunk func(Chunk)) (Response, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()

	var b strings.Builder
	for _, d := range f.deltas {
		b.WriteString(d)
		if onChunk != nil {
			onChunk(Chunk{Text: d})
		}
	}
	if f.alwaysFail || n <= f.failUntil {
		// Even a failed attempt may have pushed deltas to its (buffering) callback;
		// the wrapper must NOT have committed them.
		return Response{}, errFail
	}
	return Response{
		Content:    []Block{{Type: "text", Text: b.String()}},
		StopReason: "end_turn",
		Usage:      Usage{InputTokens: 1, OutputTokens: 1},
	}, nil
}

func (f *streamingFakeProvider) Model() string { return f.model }

func (f *streamingFakeProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// compile-time assertion: Resilient is itself a Streamer, so the loop sees a
// streamer through the resilience wrapper (ST-T05).
var _ Streamer = (*Resilient)(nil)

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

// TestStream_RetrySucceedsNoDoubleEmit is the core streaming acceptance: a
// provider that fails its first two attempts then succeeds must (a) recover via
// retry and (b) emit ONLY the winning attempt's chunks — the discarded attempts'
// deltas never reach onChunk, so no double-emit.
func TestStream_RetrySucceedsNoDoubleEmit(t *testing.T) {
	p := &streamingFakeProvider{model: "p", failUntil: 2, deltas: []string{"Hel", "lo"}}
	r := newTestResilient(t, []Provider{p}, Options{
		MaxRetries:  3,
		BaseBackoff: time.Millisecond,
	}, nil)

	var got []string
	resp, err := r.Stream(context.Background(), "", nil, nil, 16, func(c Chunk) { got = append(got, c.Text) })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if resp.Content[0].Text != "Hello" {
		t.Fatalf("assembled = %q, want Hello", resp.Content[0].Text)
	}
	if p.callCount() != 3 {
		t.Fatalf("calls = %d, want 3", p.callCount())
	}
	// Two failed attempts each pushed 2 deltas to their buffers; only the winning
	// attempt's 2 deltas may have been committed.
	if strings.Join(got, "|") != "Hel|lo" {
		t.Fatalf("emitted %q, want exactly \"Hel|lo\" (no double-emit)", strings.Join(got, "|"))
	}
}

// TestStream_FailoverNoDoubleEmit asserts that on failover only the winning
// (fallback) provider's chunks are emitted — the failed primary's deltas are
// discarded.
func TestStream_FailoverNoDoubleEmit(t *testing.T) {
	primary := &streamingFakeProvider{model: "primary", alwaysFail: true, deltas: []string{"X", "Y"}}
	fallback := &streamingFakeProvider{model: "fallback", deltas: []string{"a", "b"}}
	r := newTestResilient(t, []Provider{primary, fallback}, Options{
		MaxRetries:  1,
		BaseBackoff: time.Millisecond,
	}, nil)

	var got []string
	resp, err := r.Stream(context.Background(), "", nil, nil, 16, func(c Chunk) { got = append(got, c.Text) })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if resp.Content[0].Text != "ab" {
		t.Fatalf("assembled = %q, want ab", resp.Content[0].Text)
	}
	if primary.callCount() != 2 { // initial + 1 retry, both failed
		t.Fatalf("primary calls = %d, want 2", primary.callCount())
	}
	// Only the fallback's deltas — none of the primary's failed "X","Y".
	if strings.Join(got, "|") != "a|b" {
		t.Fatalf("emitted %q, want exactly \"a|b\" (primary's discarded)", strings.Join(got, "|"))
	}
}

// TestStream_NonStreamerFallbackOneChunk asserts a provider that implements only
// Provider (no Streamer) is driven via Complete and its reply replayed as one
// chunk — so streaming inherits resilience even over a non-streaming provider.
func TestStream_NonStreamerFallbackOneChunk(t *testing.T) {
	// fakeProvider (the non-streaming one) returns Content[0].Text = "p-ok".
	p := &fakeProvider{model: "p"}
	r := newTestResilient(t, []Provider{p}, Options{
		MaxRetries:  0,
		BaseBackoff: time.Millisecond,
	}, nil)

	var got []Chunk
	resp, err := r.Stream(context.Background(), "", nil, nil, 16, func(c Chunk) { got = append(got, c) })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(got) != 1 || got[0].Text != "p-ok" {
		t.Fatalf("chunks = %+v, want one chunk \"p-ok\"", got)
	}
	if resp.Content[0].Text != "p-ok" {
		t.Fatalf("resp text = %q, want p-ok", resp.Content[0].Text)
	}
}

// TestStream_AllProvidersFail asserts an all-down streaming run surfaces the
// wrapped errFail and emits nothing.
func TestStream_AllProvidersFail(t *testing.T) {
	primary := &streamingFakeProvider{model: "primary", alwaysFail: true, deltas: []string{"x"}}
	fallback := &streamingFakeProvider{model: "fallback", alwaysFail: true, deltas: []string{"y"}}
	r := newTestResilient(t, []Provider{primary, fallback}, Options{
		MaxRetries:  0,
		BaseBackoff: time.Millisecond,
	}, nil)

	var emitted int
	_, err := r.Stream(context.Background(), "", nil, nil, 16, func(Chunk) { emitted++ })
	if err == nil || !errors.Is(err, errFail) {
		t.Fatalf("err = %v, want wrapped errFail", err)
	}
	if emitted != 0 {
		t.Fatalf("emitted %d chunks on total failure, want 0", emitted)
	}
}

// TestStream_BreakerSkipsToFallback asserts the breaker also governs streaming:
// once the primary's breaker is open it is skipped and the fallback streams.
func TestStream_BreakerSkipsToFallback(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	primary := &streamingFakeProvider{model: "primary", alwaysFail: true, deltas: []string{"x"}}
	fallback := &streamingFakeProvider{model: "fallback", deltas: []string{"a", "b"}}
	r := newTestResilient(t, []Provider{primary, fallback}, Options{
		MaxRetries:       0,
		BaseBackoff:      time.Millisecond,
		BreakerThreshold: 1,
		BreakerCooldown:  time.Minute,
	}, clock.Now)

	if resp, err := r.Stream(context.Background(), "", nil, nil, 16, nil); err != nil {
		t.Fatalf("first Stream: %v", err)
	} else if resp.Content[0].Text != "ab" {
		t.Fatalf("got %q, want ab", resp.Content[0].Text)
	}
	primaryCalls := primary.callCount()

	// Second Stream: primary skipped (breaker open), fallback serves again.
	if resp, err := r.Stream(context.Background(), "", nil, nil, 16, nil); err != nil {
		t.Fatalf("second Stream: %v", err)
	} else if resp.Content[0].Text != "ab" {
		t.Fatalf("got %q, want ab", resp.Content[0].Text)
	}
	if primary.callCount() != primaryCalls {
		t.Fatalf("primary streamed while breaker open: %d, want %d", primary.callCount(), primaryCalls)
	}
}

// TestStream_ContextCancelledStopsRetries asserts a cancelled parent ctx stops
// the streaming walk just like Complete's.
func TestStream_ContextCancelledStopsRetries(t *testing.T) {
	p := &streamingFakeProvider{model: "p", alwaysFail: true, deltas: []string{"x"}}
	r := newTestResilient(t, []Provider{p}, Options{
		MaxRetries:  5,
		BaseBackoff: time.Millisecond,
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := r.Stream(ctx, "", nil, nil, 16, nil); err == nil {
		t.Fatal("want error from cancelled context")
	}
	if p.callCount() > 1 {
		t.Fatalf("provider streamed %d times after cancel, want <= 1", p.callCount())
	}
}

// TestStream_ConcurrentRace exercises the streaming wrapper under -race: many
// goroutines streaming through one Resilient (shared breakers) must be race-free.
func TestStream_ConcurrentRace(t *testing.T) {
	p := &streamingFakeProvider{model: "p", deltas: []string{"a", "b"}}
	r := newTestResilient(t, []Provider{p}, Options{
		MaxRetries:  1,
		BaseBackoff: time.Millisecond,
	}, nil)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Stream(context.Background(), "", nil, nil, 16, func(Chunk) {}); err != nil {
				t.Errorf("Stream: %v", err)
			}
		}()
	}
	wg.Wait()
}

// errProvider is a Provider that always fails with a caller-supplied error. It
// counts calls so a test can assert how many attempts the wrapper made before
// stopping. Used by the typed-APIError proof gates.
type errProvider struct {
	model string
	mu    sync.Mutex
	calls int
	err   error
}

func (p *errProvider) Complete(_ context.Context, _ string, _ []Message, _ []Tool, _ int) (Response, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return Response{}, p.err
}

func (p *errProvider) Model() string { return p.model }

func (p *errProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// recordingResilient wires a Resilient that captures every backoff duration handed
// to sleep (instead of actually sleeping), so a test can assert the Retry-After
// floor was honored without real delays.
func recordingResilient(t *testing.T, providers []Provider, opts Options) (*Resilient, *[]time.Duration) {
	t.Helper()
	r, err := NewResilient(providers, opts)
	if err != nil {
		t.Fatalf("NewResilient: %v", err)
	}
	var slept []time.Duration
	r.sleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		return ctx.Err()
	}
	return r, &slept
}

// --- GATE 1: backward compatibility for plain (untyped) errors. ---------------

// TestGate1_UntypedErrorRetriesAndFailsOverExactlyAsBefore is the explicit
// backward-compat proof: a plain errors.New error (NOT an *APIError) must retry the
// primary the full budget (initial + MaxRetries) and THEN fail over to the
// fallback — byte-for-byte the pre-change behavior. This mirrors the existing
// TestComplete_Failover expectation and locks it against the new typed-error path.
func TestGate1_UntypedErrorRetriesAndFailsOverExactlyAsBefore(t *testing.T) {
	plain := errors.New("transient network blip") // untyped: NOT an *APIError
	primary := &errProvider{model: "primary", err: plain}
	fallback := &fakeProvider{model: "fallback"} // succeeds on first call
	r := newTestResilient(t, []Provider{primary, fallback}, Options{
		MaxRetries:  2,
		BaseBackoff: time.Millisecond,
	}, nil)

	resp, err := r.Complete(context.Background(), "", nil, nil, 16)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content[0].Text != "fallback-ok" {
		t.Fatalf("got %q, want fallback-ok (must fail over for untyped error)", resp.Content[0].Text)
	}
	// initial + 2 retries, all failed -> 3 calls, exactly as today.
	if primary.callCount() != 3 {
		t.Fatalf("primary calls = %d, want 3 (untyped error must use full retry budget)", primary.callCount())
	}
	if fallback.callCount() != 1 {
		t.Fatalf("fallback calls = %d, want 1 (must fail over)", fallback.callCount())
	}
}

// TestGate1_UntypedErrorExhaustsThenReturns proves a single-provider untyped-error
// run still exhausts the budget and returns the wrapped error — unchanged from
// TestComplete_RetryExhausted.
func TestGate1_UntypedErrorExhaustsThenReturns(t *testing.T) {
	plain := errors.New("still down")
	p := &errProvider{model: "p", err: plain}
	r := newTestResilient(t, []Provider{p}, Options{
		MaxRetries:  2,
		BaseBackoff: time.Millisecond,
	}, nil)

	_, err := r.Complete(context.Background(), "", nil, nil, 16)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !errors.Is(err, plain) {
		t.Fatalf("want wrapped plain error, got %v", err)
	}
	if p.callCount() != 3 { // initial + 2 retries
		t.Fatalf("calls = %d, want 3", p.callCount())
	}
}

// --- GATE 2: terminal APIError stops immediately; retryable honors Retry-After. -

// TestGate2_TerminalAPIErrorStopsImmediately proves a NON-retryable *APIError
// (401/403) stops on the FIRST attempt: no retry of the primary AND no failover to
// the fallback, even though the fallback would succeed. The typed error surfaces to
// the caller verbatim.
func TestGate2_TerminalAPIErrorStopsImmediately(t *testing.T) {
	for _, status := range []int{401, 403, 400} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			term := &APIError{StatusCode: status, Retryable: false, Type: "auth", Code: "bad"}
			primary := &errProvider{model: "primary", err: term}
			fallback := &fakeProvider{model: "fallback"} // would succeed if reached
			r := newTestResilient(t, []Provider{primary, fallback}, Options{
				MaxRetries:  5, // plenty of budget — must NOT be used
				BaseBackoff: time.Millisecond,
			}, nil)

			_, err := r.Complete(context.Background(), "", nil, nil, 16)
			if err == nil {
				t.Fatal("want terminal error, got nil")
			}
			// The typed error must surface (caller can inspect it).
			var got *APIError
			if !errors.As(err, &got) || got.StatusCode != status {
				t.Fatalf("err = %v, want *APIError status %d", err, status)
			}
			// Exactly one primary attempt: no retry.
			if primary.callCount() != 1 {
				t.Fatalf("primary calls = %d, want 1 (terminal must not retry)", primary.callCount())
			}
			// No failover.
			if fallback.callCount() != 0 {
				t.Fatalf("fallback calls = %d, want 0 (terminal must not fail over)", fallback.callCount())
			}
		})
	}
}

// TestGate2_RetryableAPIErrorRetriesAndFailsOver proves a RETRYABLE *APIError (429
// without a hint) behaves like an ordinary transient failure: it retries the full
// budget and then fails over — i.e. retryable typed errors do not regress the
// failover path.
func TestGate2_RetryableAPIErrorRetriesAndFailsOver(t *testing.T) {
	retry := &APIError{StatusCode: 429, Retryable: true, Type: "rate_limit"}
	primary := &errProvider{model: "primary", err: retry}
	fallback := &fakeProvider{model: "fallback"}
	r := newTestResilient(t, []Provider{primary, fallback}, Options{
		MaxRetries:  2,
		BaseBackoff: time.Millisecond,
	}, nil)

	resp, err := r.Complete(context.Background(), "", nil, nil, 16)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content[0].Text != "fallback-ok" {
		t.Fatalf("got %q, want fallback-ok", resp.Content[0].Text)
	}
	if primary.callCount() != 3 { // initial + 2 retries
		t.Fatalf("primary calls = %d, want 3 (retryable must use full budget)", primary.callCount())
	}
}

// TestGate2_RetryAfterIsBackoffFloor proves a retryable *APIError carrying a
// Retry-After LONGER than the computed backoff causes the wrapper to wait at least
// that long: the recorded sleep duration is the Retry-After, not the small
// exponential backoff.
func TestGate2_RetryAfterIsBackoffFloor(t *testing.T) {
	// Computed backoff for attempt 1 is BaseBackoff = 1ms; Retry-After = 3s should
	// dominate it as the floor.
	retry := &APIError{StatusCode: 429, Retryable: true, RetryAfter: 3 * time.Second}
	p := &errProvider{model: "p", err: retry}
	r, slept := recordingResilient(t, []Provider{p}, Options{
		MaxRetries:  1,
		BaseBackoff: time.Millisecond,
		MaxBackoff:  time.Millisecond, // ensures computed backoff stays tiny
	})

	if _, err := r.Complete(context.Background(), "", nil, nil, 16); err == nil {
		t.Fatal("want error, got nil")
	}
	if len(*slept) != 1 {
		t.Fatalf("recorded %d sleeps, want 1 (one retry)", len(*slept))
	}
	if (*slept)[0] != 3*time.Second {
		t.Fatalf("slept %v, want 3s (Retry-After must be the backoff floor)", (*slept)[0])
	}
}

// TestGate2_RetryAfterShorterThanBackoffUsesBackoff proves the floor is a floor,
// not a ceiling: when Retry-After is SHORTER than the computed backoff, the
// computed backoff wins — the timing of an ordinary retryable failure is unchanged.
func TestGate2_RetryAfterShorterThanBackoffUsesBackoff(t *testing.T) {
	retry := &APIError{StatusCode: 429, Retryable: true, RetryAfter: time.Millisecond}
	p := &errProvider{model: "p", err: retry}
	r, slept := recordingResilient(t, []Provider{p}, Options{
		MaxRetries:  1,
		BaseBackoff: 100 * time.Millisecond,
		MaxBackoff:  time.Second,
	})

	if _, err := r.Complete(context.Background(), "", nil, nil, 16); err == nil {
		t.Fatal("want error, got nil")
	}
	if len(*slept) != 1 {
		t.Fatalf("recorded %d sleeps, want 1", len(*slept))
	}
	// Computed backoff (100ms) exceeds the 1ms hint, so the backoff is used.
	if (*slept)[0] != 100*time.Millisecond {
		t.Fatalf("slept %v, want 100ms (computed backoff dominates a smaller hint)", (*slept)[0])
	}
}

// TestGate1_UntypedErrorBackoffUnchanged is a timing companion to GATE 1: an
// untyped error's retry delay is exactly the computed backoff (no Retry-After path
// reachable), proving untyped-error timing did not change.
func TestGate1_UntypedErrorBackoffUnchanged(t *testing.T) {
	p := &errProvider{model: "p", err: errors.New("blip")}
	r, slept := recordingResilient(t, []Provider{p}, Options{
		MaxRetries:  1,
		BaseBackoff: 50 * time.Millisecond,
		MaxBackoff:  time.Second,
	})
	if _, err := r.Complete(context.Background(), "", nil, nil, 16); err == nil {
		t.Fatal("want error, got nil")
	}
	if len(*slept) != 1 || (*slept)[0] != 50*time.Millisecond {
		t.Fatalf("slept %v, want exactly [50ms]", *slept)
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
