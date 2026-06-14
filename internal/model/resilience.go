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

// callWithRetry runs one provider with retry/backoff and updates its breaker on
// every attempt. Each failed attempt counts as one consecutive failure, so the
// breaker can open partway through a provider's own retry budget; when it does we
// stop retrying that provider immediately rather than burning the rest of the
// budget on a service we already know is down.
func (r *Resilient) callWithRetry(ctx context.Context, p Provider, b *breaker, system string, msgs []Message, tools []Tool, maxTokens int) (Response, error) {
	var lastErr error
	for attempt := 0; attempt <= r.opts.MaxRetries; attempt++ {
		if attempt > 0 {
			if err := r.sleep(ctx, r.backoff(attempt)); err != nil {
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
