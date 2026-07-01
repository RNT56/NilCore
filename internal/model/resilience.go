// resilience.go adds provider robustness without leaking the concern into the
// loop or into any single vendor adapter. A model call over the network is the
// flakiest thing the agent does: a vendor can throttle, time out, or have a bad
// minute. Resilient wraps an ordered list of Providers and turns those transient
// failures into recoverable ones via three layered tactics — per-call retry with
// exponential backoff + jitter, failover to the next provider, and a circuit
// breaker that stops hammering a provider that is consistently down. Because
// Resilient itself satisfies Provider, the loop calls it exactly like any other
// model and never learns that retries or failover happened.
package model

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// Options tunes the resilience behavior. All durations and counts are fields so
// tests can drive tiny values (e.g. BaseBackoff of 1ms) and stay fast and
// deterministic. Zero values fall back to sane production defaults.
type Options struct {
	// MaxRetries is the number of *additional* attempts after the first, per
	// provider, before giving up on that provider and failing over.
	MaxRetries int
	// BaseBackoff is the first backoff delay; it doubles each retry.
	BaseBackoff time.Duration
	// MaxBackoff caps the exponential growth.
	MaxBackoff time.Duration
	// Jitter, when > 0, adds a random delay in [0, Jitter) to each backoff so a
	// fleet of agents does not retry in lockstep.
	Jitter time.Duration
	// CallTimeout bounds a single Complete call; <= 0 means no per-call timeout.
	CallTimeout time.Duration
	// BreakerThreshold is the number of consecutive failures after which a
	// provider's breaker opens and the provider is skipped. <= 0 disables it.
	BreakerThreshold int
	// BreakerCooldown is how long a breaker stays open before allowing a single
	// trial call to test recovery.
	BreakerCooldown time.Duration
}

func (o Options) withDefaults() Options {
	if o.MaxRetries < 0 {
		o.MaxRetries = 0
	}
	if o.BaseBackoff <= 0 {
		o.BaseBackoff = 200 * time.Millisecond
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = 5 * time.Second
	}
	if o.CallTimeout < 0 {
		o.CallTimeout = 0
	}
	if o.BreakerCooldown <= 0 {
		o.BreakerCooldown = 30 * time.Second
	}
	return o
}

// breaker is the per-provider circuit-breaker state. It is its own small type so
// the locking stays local and the failure-counting logic is easy to read.
type breaker struct {
	mu        sync.Mutex
	failures  int
	openUntil time.Time
}

// allow reports whether a call to this provider should be attempted now. When the
// breaker is open it stays skipped until the cooldown elapses, after which one
// trial call is permitted (half-open).
func (b *breaker) allow(now time.Time, threshold int) bool {
	if threshold <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failures < threshold {
		return true
	}
	// Breaker is open: only allow a single trial once the cooldown has passed.
	return now.After(b.openUntil)
}

func (b *breaker) recordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.openUntil = time.Time{}
}

func (b *breaker) recordFailure(now time.Time, threshold int, cooldown time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if threshold > 0 && b.failures >= threshold {
		b.openUntil = now.Add(cooldown)
	}
}

// Resilient wraps an ordered list of providers. The first is the primary and
// supplies Model(); the rest are failover targets tried in order.
type Resilient struct {
	providers []Provider
	breakers  []*breaker
	opts      Options

	// now and sleep are injectable so tests stay hermetic; they default to the
	// real clock.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// ErrNoProviders is returned by NewResilient when the provider list is empty.
var ErrNoProviders = errors.New("model: resilient requires at least one provider")

// NewResilient builds a Resilient over providers (primary first) with opts. The
// list is defensively copied. It returns ErrNoProviders if the list is empty;
// it never panics.
func NewResilient(providers []Provider, opts Options) (*Resilient, error) {
	if len(providers) == 0 {
		return nil, ErrNoProviders
	}
	ps := make([]Provider, len(providers))
	copy(ps, providers)
	bs := make([]*breaker, len(providers))
	for i := range bs {
		bs[i] = &breaker{}
	}
	return &Resilient{
		providers: ps,
		breakers:  bs,
		opts:      opts.withDefaults(),
		now:       time.Now,
		sleep:     sleepCtx,
	}, nil
}

// Model returns the primary provider's model string.
func (r *Resilient) Model() string { return r.providers[0].Model() }

// Complete tries each provider in order. For each provider it retries up to
// MaxRetries times with exponential backoff + jitter, honoring ctx throughout.
// A provider whose breaker is open is skipped. It returns the first success;
// if every provider is exhausted it returns the last error seen (joined with a
// sentinel so callers can detect the all-down case).
func (r *Resilient) Complete(ctx context.Context, system string, msgs []Message, tools []Tool, maxTokens int) (Response, error) {
	var errs []error
	skipped := 0
	for i, p := range r.providers {
		if !r.breakers[i].allow(r.now(), r.opts.BreakerThreshold) {
			skipped++
			errs = append(errs, fmt.Errorf("provider %d (%s): breaker open", i, p.Model()))
			continue
		}
		resp, err := r.callWithRetry(ctx, p, r.breakers[i], system, msgs, tools, maxTokens)
		if err == nil {
			return resp, nil
		}
		// A terminal *APIError (e.g. 401/403/400) cannot be helped by a different
		// provider either — the request itself is bad. Return it immediately,
		// unwrapped, so the caller sees the typed error and no failover happens.
		if apiErr := terminalAPIError(err); apiErr != nil {
			return Response{}, apiErr
		}
		errs = append(errs, fmt.Errorf("provider %d (%s): %w", i, p.Model(), err))
		// If the context is done, stop walking the list — no provider will help.
		if ctx.Err() != nil {
			break
		}
	}
	if skipped == len(r.providers) {
		return Response{}, fmt.Errorf("all providers skipped (breakers open): %w", errors.Join(errs...))
	}
	return Response{}, fmt.Errorf("all providers failed: %w", errors.Join(errs...))
}

// Stream is the streaming counterpart to Complete: it applies the exact same
// retry/backoff/failover/breaker logic, but around each provider's Stream (or a
// non-streaming provider's Complete replayed as one chunk), so streaming inherits
// resilience and the loop sees a model.Streamer through the wrapper regardless of
// what the underlying providers support.
//
// Live on attempt 0, no double-emit on retry: the first attempt (the ~99%
// single-provider / no-retry case) forwards each delta to onChunk AS IT ARRIVES,
// so the operator sees real incremental token streaming rather than one
// end-of-stream burst. Only once a RETRY or failover begins (attempt > 0) does
// the wrapper switch to buffer-and-discard — a retried attempt's deltas are
// staged and never re-emitted, since attempt 0 already forwarded its own chunks
// live. The net effect is exactly one committed sequence: live for the common
// single-attempt path, and never a double-emit when an attempt is thrown away.
// (The contract still holds: the concatenation of the forwarded chunks equals the
// returned Response's output text.)
func (r *Resilient) Stream(ctx context.Context, system string, msgs []Message, tools []Tool, maxTokens int, onChunk func(Chunk)) (Response, error) {
	var errs []error
	skipped := 0
	// liveForward is the single, walk-wide "may still stream live" gate. It is true
	// only for the very first attempt of the whole walk (attempt 0 of the first
	// provider actually tried). The instant a second attempt begins — a retry OR a
	// failover — attempt 0's chunks are already out on the wire, so we flip it off
	// and every subsequent attempt buffers-and-discards. This gives live streaming
	// on the ~99% single-attempt path while never re-emitting a thrown-away
	// attempt's chunks. A nil onChunk stays nil (nothing to forward).
	liveForward := true
	live := func(c Chunk) {
		if onChunk != nil {
			onChunk(c)
		}
	}
	for i, p := range r.providers {
		if !r.breakers[i].allow(r.now(), r.opts.BreakerThreshold) {
			skipped++
			errs = append(errs, fmt.Errorf("provider %d (%s): breaker open", i, p.Model()))
			continue
		}
		resp, err := r.streamWithRetry(ctx, p, r.breakers[i], &liveForward, live, system, msgs, tools, maxTokens)
		if err == nil {
			return resp, nil
		}
		// Terminal *APIError: fail fast, no failover (same as Complete).
		if apiErr := terminalAPIError(err); apiErr != nil {
			return Response{}, apiErr
		}
		errs = append(errs, fmt.Errorf("provider %d (%s): %w", i, p.Model(), err))
		// If the context is done, stop walking the list — no provider will help.
		// Cancellation is terminal (not retried, not failed over), so return the
		// PARTIAL resp the underlying provider assembled alongside the ctx error:
		// a mid-stream steer preserves the partial reasoning instead of losing it.
		if ctx.Err() != nil {
			return resp, err
		}
	}
	if skipped == len(r.providers) {
		return Response{}, fmt.Errorf("all providers skipped (breakers open): %w", errors.Join(errs...))
	}
	return Response{}, fmt.Errorf("all providers failed: %w", errors.Join(errs...))
}

// streamWithRetry is the streaming twin of callWithRetry: same retry/backoff and
// breaker bookkeeping. liveForward is the walk-wide gate owned by Stream: the very
// first attempt of the whole walk forwards chunks live through onChunk; the instant
// a second attempt (retry OR failover) begins it is flipped off and every later
// attempt buffers-and-discards, so a thrown-away attempt's chunks are never
// re-emitted.
func (r *Resilient) streamWithRetry(ctx context.Context, p Provider, b *breaker, liveForward *bool, onChunk func(Chunk), system string, msgs []Message, tools []Tool, maxTokens int) (Response, error) {
	var lastErr error
	for attempt := 0; attempt <= r.opts.MaxRetries; attempt++ {
		if attempt > 0 {
			if err := r.sleep(ctx, r.retryDelay(attempt, lastErr)); err != nil {
				return Response{}, fmt.Errorf("backoff interrupted: %w", err)
			}
		}
		// Only the walk's very first attempt forwards live; capture that decision
		// then close the gate so every later attempt (this provider's retry or a
		// failover) buffers-and-discards and can never re-emit attempt 0's chunks.
		forwardLive := *liveForward
		*liveForward = false
		resp, err := r.streamOnce(ctx, p, forwardLive, onChunk, system, msgs, tools, maxTokens)
		if err == nil {
			b.recordSuccess()
			return resp, nil
		}
		lastErr = err
		b.recordFailure(r.now(), r.opts.BreakerThreshold, r.opts.BreakerCooldown)
		// A terminal *APIError cannot succeed on a retry — return it as-is so the
		// caller's terminal check fires and stops the walk (no retry, no failover).
		if isTerminalAPIError(err) {
			return Response{}, err
		}
		// A cancelled/expired parent context is terminal — do not keep retrying.
		// streamOnce already returned the PARTIAL resp alongside the ctx error, so
		// surface it verbatim (partial + wrapped ctx err) to preserve mid-stream
		// steer state; do NOT wrap it in a fresh Response{}.
		if ctx.Err() != nil {
			return resp, fmt.Errorf("attempt %d: %w", attempt+1, err)
		}
		// If this provider's breaker just opened, stop spending its retry budget.
		if !b.allow(r.now(), r.opts.BreakerThreshold) {
			return Response{}, fmt.Errorf("attempt %d (breaker opened): %w", attempt+1, err)
		}
	}
	return Response{}, fmt.Errorf("exhausted %d attempts: %w", r.opts.MaxRetries+1, lastErr)
}

// streamOnce runs a single streaming attempt with the optional per-call timeout.
// When forwardLive is true (the walk's very first attempt) it forwards each delta
// to onChunk LIVE, so the operator sees incremental tokens in the common
// single-provider / no-retry case. When forwardLive is false (any retry or
// failover) it buffers-and-discards: the deltas are staged but never emitted,
// because the walk's first attempt already forwarded its own chunks — this
// preserves the no-double-emit guarantee when a prior attempt is thrown away. A
// non-streaming provider is driven via Complete and its assembled reply replayed
// as a single chunk, so it satisfies the streaming contract too. On a context
// cancellation the underlying provider returns the PARTIAL assembled Response
// alongside ctx.Err(); streamOnce passes that partial resp back (not a zeroed one)
// so a mid-stream steer preserves it.
func (r *Resilient) streamOnce(ctx context.Context, p Provider, forwardLive bool, onChunk func(Chunk), system string, msgs []Message, tools []Tool, maxTokens int) (Response, error) {
	cctx := ctx
	if r.opts.CallTimeout > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, r.opts.CallTimeout)
		defer cancel()
	}

	// First attempt forwards live; a retry/failover buffers-and-discards (the first
	// attempt already emitted its own chunks, so re-emitting here would double-emit).
	sink := func(Chunk) {} // discard by default
	if forwardLive && onChunk != nil {
		sink = onChunk
	}

	var (
		resp Response
		err  error
	)
	if s, ok := p.(Streamer); ok {
		resp, err = s.Stream(cctx, system, msgs, tools, maxTokens, sink)
	} else {
		// Non-streaming provider: complete, then replay the whole reply as one chunk.
		resp, err = p.Complete(cctx, system, msgs, tools, maxTokens)
		if err == nil {
			sink(Chunk{Text: responseText(resp)})
		}
	}
	if err != nil {
		// Return the (possibly partial) resp verbatim: on a ctx cancellation the
		// provider assembled real reasoning we must preserve; on an ordinary
		// failure resp is the provider's zero value anyway. Chunks were already
		// forwarded live (first attempt) or discarded (retry/failover) via sink —
		// either way there is nothing to flush here.
		return resp, err
	}
	return resp, nil
}

// responseText concatenates a response's text blocks — the output prose a front
// end paints — so a non-streaming provider's reply can be replayed as one Chunk.
func responseText(resp Response) string {
	var b strings.Builder
	for _, blk := range resp.Content {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// callWithRetry runs one provider with retry/backoff and updates its breaker on
// every attempt. Each failed attempt counts as one consecutive failure, so the
// breaker can open partway through a provider's own retry budget; when it does we
// stop retrying that provider immediately rather than burning the rest of the
// budget on a service we already know is down.
func (r *Resilient) callWithRetry(ctx context.Context, p Provider, b *breaker, system string, msgs []Message, tools []Tool, maxTokens int) (Response, error) {
	var lastErr error
	for attempt := 0; attempt <= r.opts.MaxRetries; attempt++ {
		if attempt > 0 {
			if err := r.sleep(ctx, r.retryDelay(attempt, lastErr)); err != nil {
				return Response{}, fmt.Errorf("backoff interrupted: %w", err)
			}
		}
		resp, err := r.callOnce(ctx, p, system, msgs, tools, maxTokens)
		if err == nil {
			b.recordSuccess()
			return resp, nil
		}
		lastErr = err
		b.recordFailure(r.now(), r.opts.BreakerThreshold, r.opts.BreakerCooldown)
		// A terminal *APIError (non-retryable, e.g. 401/403) cannot succeed on a
		// retry — return it as-is so the caller's terminal check fires and stops
		// the whole walk (no further retry, no failover).
		if isTerminalAPIError(err) {
			return Response{}, err
		}
		// A cancelled/expired parent context is terminal — do not keep retrying.
		if ctx.Err() != nil {
			return Response{}, fmt.Errorf("attempt %d: %w", attempt+1, err)
		}
		// If this provider's breaker just opened, stop spending its retry budget.
		if !b.allow(r.now(), r.opts.BreakerThreshold) {
			return Response{}, fmt.Errorf("attempt %d (breaker opened): %w", attempt+1, err)
		}
	}
	return Response{}, fmt.Errorf("exhausted %d attempts: %w", r.opts.MaxRetries+1, lastErr)
}

// callOnce wraps a single Complete with the optional per-call timeout.
func (r *Resilient) callOnce(ctx context.Context, p Provider, system string, msgs []Message, tools []Tool, maxTokens int) (Response, error) {
	if r.opts.CallTimeout <= 0 {
		return p.Complete(ctx, system, msgs, tools, maxTokens)
	}
	cctx, cancel := context.WithTimeout(ctx, r.opts.CallTimeout)
	defer cancel()
	return p.Complete(cctx, system, msgs, tools, maxTokens)
}

// isTerminalAPIError reports whether err is (or wraps) an *APIError that is NOT
// retryable. A terminal error stops the per-provider retry loop immediately and,
// once it surfaces to Complete/Stream, stops the provider walk entirely (no
// failover). A plain (untyped) error is never terminal here, so untyped errors
// retry and fail over exactly as before — the backward-compatibility guarantee.
func isTerminalAPIError(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return !apiErr.Retryable
	}
	return false
}

// terminalAPIError returns the wrapped *APIError when err is terminal (a
// non-retryable *APIError), else nil. Complete/Stream use it to short-circuit the
// provider walk and return the typed error to the caller verbatim.
func terminalAPIError(err error) *APIError {
	var apiErr *APIError
	if errors.As(err, &apiErr) && !apiErr.Retryable {
		return apiErr
	}
	return nil
}

// retryDelay is the wait before retry attempt n. It is the computed exponential
// backoff EXCEPT when the previous failure was a retryable *APIError carrying a
// Retry-After hint that exceeds the computed backoff — then the server's hint is
// the FLOOR (we wait at least that long). For any non-APIError, or a hint that is
// shorter than the backoff, this returns exactly backoff(n) — so untyped-error
// timing is unchanged.
func (r *Resilient) retryDelay(attempt int, lastErr error) time.Duration {
	d := r.backoff(attempt)
	var apiErr *APIError
	if errors.As(lastErr, &apiErr) && apiErr.RetryAfter > d {
		return apiErr.RetryAfter
	}
	return d
}

// backoff computes the delay before retry attempt n (n >= 1): an exponential
// base*2^(n-1) capped at MaxBackoff, plus a random jitter in [0, Jitter).
func (r *Resilient) backoff(attempt int) time.Duration {
	d := r.opts.BaseBackoff
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= r.opts.MaxBackoff {
			d = r.opts.MaxBackoff
			break
		}
	}
	if d > r.opts.MaxBackoff {
		d = r.opts.MaxBackoff
	}
	if r.opts.Jitter > 0 {
		d += time.Duration(rand.Int63n(int64(r.opts.Jitter)))
	}
	return d
}

// sleepCtx sleeps for d but returns early with ctx.Err() if ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
