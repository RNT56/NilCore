// Package strongcap is the process-global, ctx-honoring concurrency limiter on the
// WORKER ask_advisor path (docs/CONCURRENCY.md §3). Under full multi-agent
// concurrency a correlated EscalateAfter burst can send every running worker to
// the strong advisor tier at once (the "herd"); this decorator caps how many of
// those advisor Complete/Stream calls run concurrently, smoothing the burst so it
// cannot overrun the provider's rate limit.
//
// It wraps ONLY the strong provider handed to roster workers' advisors — never the
// supervisor's own Model nor the reader's Answer hook — so the coordination
// channel (the planner's turns and ask_supervisor replies) is NEVER throttled by
// the worker herd. That separation is the whole point: the first design draft put
// one semaphore on the shared provider and the adversarial review proved it would
// starve coordination.
//
// No-hang guarantee (the load-bearing property): the acquire is a select on the
// gate OR ctx.Done(). On a full gate it blocks until a slot frees or ctx fires; a
// done ctx returns ctx.Err() WITHOUT calling Inner, so the caller's ask_advisor
// handler falls through to its existing graceful "proceed with your best judgment"
// fallback. A stuck executor always gets guidance-to-self-judge — never a deadlock.
//
// Stdlib only (I6): a buffered channel as the semaphore. The Inner provider stays
// metered/budget-bounded — this decorator adds concurrency control, nothing else,
// so every advisor call still charges the one dollar ledger.
package strongcap

import (
	"context"

	"nilcore/internal/model"
)

// Provider caps concurrent model calls through Inner at the semaphore's capacity.
// The zero value is unusable; construct with New. It is safe for concurrent use as
// long as Inner is (the strong providers are — immutable config + shared client).
type Provider struct {
	inner model.Provider
	sem   chan struct{} // buffered to the concurrency cap; a token per in-flight call
}

// New returns a limiter over inner that admits at most max concurrent calls. A max
// below 1 is clamped to 1 (a limiter that admits nothing would be a deadlock, not a
// cap). Process-global by intent: construct ONE and share it across every worker so
// the bound holds tree-wide, not per-wave (peak load is driveGate × MaxFanout).
func New(inner model.Provider, max int) *Provider {
	if max < 1 {
		max = 1
	}
	return &Provider{inner: inner, sem: make(chan struct{}, max)}
}

// Complete gates entry on the semaphore, honoring ctx. On a full gate it blocks
// until a slot frees or ctx is done; a done ctx returns ctx.Err() and never touches
// Inner (so the caller falls through to its graceful fallback — never hangs). On
// admission it releases the slot on return, including on an Inner error or panic.
func (p *Provider) Complete(ctx context.Context, system string, msgs []model.Message, tools []model.Tool, maxTokens int) (model.Response, error) {
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
		return p.inner.Complete(ctx, system, msgs, tools, maxTokens)
	case <-ctx.Done():
		return model.Response{}, ctx.Err()
	}
}

// Stream gates entry the same way and forwards to Inner's Stream when Inner is a
// model.Streamer; otherwise it falls back to a gated Complete and replays the whole
// Response as one chunk (the same degradation meter.Provider applies). The advisor
// path uses Complete, but a proper model.Provider must not silently drop streaming
// for any path that type-asserts the wrapped provider to a Streamer.
func (p *Provider) Stream(ctx context.Context, system string, msgs []model.Message, tools []model.Tool, maxTokens int, onChunk func(model.Chunk)) (model.Response, error) {
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
		if s, ok := p.inner.(model.Streamer); ok {
			return s.Stream(ctx, system, msgs, tools, maxTokens, onChunk)
		}
		resp, err := p.inner.Complete(ctx, system, msgs, tools, maxTokens)
		if err != nil {
			return resp, err
		}
		for _, b := range resp.Content {
			if b.Type == "text" && b.Text != "" && onChunk != nil {
				onChunk(model.Chunk{Text: b.Text})
			}
		}
		return resp, nil
	case <-ctx.Done():
		return model.Response{}, ctx.Err()
	}
}

// Model reports the wrapped provider's configured provider:model string unchanged —
// the limiter is transparent to identity, pricing, and metering.
func (p *Provider) Model() string { return p.inner.Model() }
