package meter

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"

	"nilcore/internal/budget"
	"nilcore/internal/model"
)

// fakeProvider is a hermetic model.Provider stand-in (no network — invariant:
// unit tests are offline). It reports a fixed model id and a fixed token usage
// per Complete so a test can assert exactly what the decorator charges. An
// optional err lets a test exercise the failure path.
type fakeProvider struct {
	id    string
	usage model.Usage
	err   error
	calls int // observability: how many times Complete was forwarded
}

func (f *fakeProvider) Complete(ctx context.Context, system string, msgs []model.Message, tools []model.Tool, maxTokens int) (model.Response, error) {
	f.calls++
	if f.err != nil {
		return model.Response{}, f.err
	}
	return model.Response{Usage: f.usage}, nil
}

func (f *fakeProvider) Model() string { return f.id }

// streamingProvider is a hermetic model.Streamer (and model.Provider) stand-in.
// Its Stream forwards a fixed sequence of text deltas to onChunk and returns a
// fixed Usage, so a test can assert both the deltas the caller saw and the charge
// the decorator made. An optional err exercises the partial-on-cancel path: when
// err is non-nil Stream returns it alongside the (still non-zero) usage, modeling
// a stream cut short whose produced tokens are still billable.
type streamingProvider struct {
	id           string
	usage        model.Usage
	deltas       []string
	err          error
	streamCalls  int
	completeUsed bool // set if Complete (not Stream) was forwarded
}

func (s *streamingProvider) Complete(ctx context.Context, system string, msgs []model.Message, tools []model.Tool, maxTokens int) (model.Response, error) {
	s.completeUsed = true
	return model.Response{Usage: s.usage}, s.err
}

func (s *streamingProvider) Stream(ctx context.Context, system string, msgs []model.Message, tools []model.Tool, maxTokens int, onChunk func(model.Chunk)) (model.Response, error) {
	s.streamCalls++
	var b strings.Builder
	for _, d := range s.deltas {
		b.WriteString(d)
		if onChunk != nil {
			onChunk(model.Chunk{Text: d})
		}
	}
	resp := model.Response{
		Content: []model.Block{{Type: "text", Text: b.String()}},
		Usage:   s.usage,
	}
	return resp, s.err
}

func (s *streamingProvider) Model() string { return s.id }

// compile-time assertion: the decorator is itself a model.Provider, so it drops
// in wherever a provider is expected (the whole point — wrap every provider).
var _ model.Provider = (*Provider)(nil)

// compile-time assertion: the decorator is also a model.Streamer, so the loop
// always sees a streamer through the wrapper (ST-T05).
var _ model.Streamer = (*Provider)(nil)

func almostEqualDollars(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestCompleteChargesPerCall asserts the core acceptance criterion: a wrapped
// provider charges the ledger once per Complete, with the dollar amount the
// Pricer assigns to resp.Usage and the token count = input+output.
func TestCompleteChargesPerCall(t *testing.T) {
	inner := &fakeProvider{id: "claude-opus-4-8", usage: model.Usage{InputTokens: 1000, OutputTokens: 1000}}
	led := budget.New()
	p := &Provider{Inner: inner, Ledger: led, Task: "t1", Price: NewTable()}

	const calls = 3
	for i := 0; i < calls; i++ {
		if _, err := p.Complete(context.Background(), "sys", nil, nil, 100); err != nil {
			t.Fatalf("Complete %d: unexpected error %v", i, err)
		}
	}

	// Opus 1k in + 1k out = 0.005 + 0.025 = 0.030 per call; 2000 tokens per call.
	wantDollars := calls * (0.005 + 0.025)
	wantTokens := calls * 2000

	tokens, dollars := led.Spent("t1")
	if tokens != wantTokens {
		t.Errorf("task tokens = %d, want %d", tokens, wantTokens)
	}
	if !almostEqualDollars(dollars, wantDollars) {
		t.Errorf("task dollars = %v, want %v", dollars, wantDollars)
	}

	// The global total must mirror the single task's spend.
	gTokens, gDollars := led.Total()
	if gTokens != wantTokens || !almostEqualDollars(gDollars, wantDollars) {
		t.Errorf("global = (%d, %v), want (%d, %v)", gTokens, gDollars, wantTokens, wantDollars)
	}
	if inner.calls != calls {
		t.Errorf("inner.calls = %d, want %d", inner.calls, calls)
	}
}

// TestModelDelegates asserts Model() passes through unchanged so tier/pricer
// resolution sees the real model id.
func TestModelDelegates(t *testing.T) {
	inner := &fakeProvider{id: "claude-sonnet-4-6"}
	p := &Provider{Inner: inner, Ledger: budget.New(), Price: NewTable()}
	if got := p.Model(); got != "claude-sonnet-4-6" {
		t.Errorf("Model() = %q, want %q", got, "claude-sonnet-4-6")
	}
}

// TestCompleteCeilingAborts asserts a charge that breaches SetGlobalCeiling
// returns budget.ErrCeiling from Complete (the caller's stop signal) and that
// no spend past the wall is recorded.
func TestCompleteCeilingAborts(t *testing.T) {
	inner := &fakeProvider{id: "claude-opus-4-8", usage: model.Usage{InputTokens: 1000, OutputTokens: 1000}}
	led := budget.New()
	led.SetGlobalCeiling(0.05) // one 0.030 call fits; the second (→0.060) breaches.
	p := &Provider{Inner: inner, Ledger: led, Task: "t1", Price: NewTable()}

	if _, err := p.Complete(context.Background(), "sys", nil, nil, 100); err != nil {
		t.Fatalf("first call should fit under ceiling, got %v", err)
	}

	_, err := p.Complete(context.Background(), "sys", nil, nil, 100)
	if !errors.Is(err, budget.ErrCeiling) {
		t.Fatalf("second call: err = %v, want budget.ErrCeiling", err)
	}

	// The refused charge recorded nothing: spend stays at exactly one call.
	_, dollars := led.Total()
	if !almostEqualDollars(dollars, 0.030) {
		t.Errorf("after ceiling refusal, dollars = %v, want 0.030 (refused charge not recorded)", dollars)
	}
}

// TestCompleteInnerErrorChargesNothing asserts a failed inner call surfaces its
// error untouched and bills nothing — a non-completion is not billable.
func TestCompleteInnerErrorChargesNothing(t *testing.T) {
	wantErr := errors.New("provider boom")
	inner := &fakeProvider{id: "claude-opus-4-8", err: wantErr}
	led := budget.New()
	p := &Provider{Inner: inner, Ledger: led, Task: "t1", Price: NewTable()}

	_, err := p.Complete(context.Background(), "sys", nil, nil, 100)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if tokens, dollars := led.Total(); tokens != 0 || dollars != 0 {
		t.Errorf("failed call charged (%d, %v), want (0, 0)", tokens, dollars)
	}
}

// TestCompleteConcurrentSharedLedger is the -race acceptance criterion: many
// metered providers (distinct task keys) sharing ONE ledger must charge it
// concurrently without a data race, and the totals must add up exactly. Run the
// suite with -race to exercise it (make verify / go test -race).
func TestCompleteConcurrentSharedLedger(t *testing.T) {
	led := budget.New()
	const (
		workers      = 8
		callsPerWork = 50
	)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			// Each worker is its own subagent provider with its own Task key,
			// all charging the one shared ledger — the multi-agent wiring.
			inner := &fakeProvider{id: "claude-haiku-4-5", usage: model.Usage{InputTokens: 500, OutputTokens: 500}}
			p := &Provider{Inner: inner, Ledger: led, Task: string(rune('a' + w)), Price: NewTable()}
			for i := 0; i < callsPerWork; i++ {
				if _, err := p.Complete(context.Background(), "sys", nil, nil, 64); err != nil {
					t.Errorf("worker %d call %d: %v", w, i, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	// Haiku 500 in + 500 out = 0.0005 + 0.0025 = 0.003 per call; 1000 tokens.
	totalCalls := workers * callsPerWork
	wantTokens := totalCalls * 1000
	wantDollars := float64(totalCalls) * (0.0005 + 0.0025)

	gTokens, gDollars := led.Total()
	if gTokens != wantTokens {
		t.Errorf("concurrent global tokens = %d, want %d", gTokens, wantTokens)
	}
	if !almostEqualDollars(gDollars, wantDollars) {
		t.Errorf("concurrent global dollars = %v, want %v", gDollars, wantDollars)
	}
}

// TestStreamDelegatesAndCharges asserts that when Inner is a model.Streamer the
// decorator delegates (onChunk sees each delta) and charges resp.Usage exactly as
// Complete would — the budget wall holds on the streaming path.
func TestStreamDelegatesAndCharges(t *testing.T) {
	inner := &streamingProvider{
		id:     "claude-opus-4-8",
		usage:  model.Usage{InputTokens: 1000, OutputTokens: 1000},
		deltas: []string{"Hel", "lo"},
	}
	led := budget.New()
	p := &Provider{Inner: inner, Ledger: led, Task: "t1", Price: NewTable()}

	var got []string
	resp, err := p.Stream(context.Background(), "sys", nil, nil, 100,
		func(c model.Chunk) { got = append(got, c.Text) })
	if err != nil {
		t.Fatalf("Stream: unexpected error %v", err)
	}
	if inner.streamCalls != 1 || inner.completeUsed {
		t.Fatalf("delegation wrong: streamCalls=%d completeUsed=%v, want 1/false", inner.streamCalls, inner.completeUsed)
	}
	// onChunk saw each delta in order, passed through untouched.
	if strings.Join(got, "|") != "Hel|lo" {
		t.Errorf("deltas = %q, want \"Hel|lo\"", strings.Join(got, "|"))
	}
	if resp.Content[0].Text != "Hello" {
		t.Errorf("assembled text = %q, want \"Hello\"", resp.Content[0].Text)
	}

	// Charged the same as Complete: 1k in + 1k out Opus = 0.030, 2000 tokens.
	tokens, dollars := led.Total()
	if tokens != 2000 || !almostEqualDollars(dollars, 0.030) {
		t.Errorf("charged (%d, %v), want (2000, 0.030)", tokens, dollars)
	}
}

// TestStreamFallbackOneChunk asserts that a non-streaming Inner still satisfies
// the Streamer contract: Complete is called, the whole reply is replayed as ONE
// chunk, and the usage is charged.
func TestStreamFallbackOneChunk(t *testing.T) {
	inner := &fakeProvider{id: "claude-opus-4-8", usage: model.Usage{InputTokens: 1000, OutputTokens: 1000}}
	led := budget.New()
	p := &Provider{Inner: inner, Ledger: led, Task: "t1", Price: NewTable()}

	var chunks []model.Chunk
	if _, err := p.Stream(context.Background(), "sys", nil, nil, 100,
		func(c model.Chunk) { chunks = append(chunks, c) }); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if inner.calls != 1 {
		t.Errorf("inner.calls = %d, want 1 (Complete fallback)", inner.calls)
	}
	// Exactly one chunk for the whole (here empty-text) reply.
	if len(chunks) != 1 {
		t.Fatalf("forwarded %d chunks, want exactly 1 (one big chunk)", len(chunks))
	}
	// Still charged: 0.030, 2000 tokens.
	tokens, dollars := led.Total()
	if tokens != 2000 || !almostEqualDollars(dollars, 0.030) {
		t.Errorf("charged (%d, %v), want (2000, 0.030)", tokens, dollars)
	}
}

// TestStreamFallbackReplaysText asserts the one-chunk fallback replays the
// response's concatenated text (Streamer contract: forwarded chunks concatenate
// to the output text).
func TestStreamFallbackReplaysText(t *testing.T) {
	inner := &textProvider{id: "claude-opus-4-8", text: "hello world", usage: model.Usage{InputTokens: 1, OutputTokens: 1}}
	p := &Provider{Inner: inner, Ledger: budget.New(), Task: "t1", Price: NewTable()}

	var chunks []model.Chunk
	if _, err := p.Stream(context.Background(), "sys", nil, nil, 100,
		func(c model.Chunk) { chunks = append(chunks, c) }); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(chunks) != 1 || chunks[0].Text != "hello world" {
		t.Fatalf("chunks = %+v, want one chunk \"hello world\"", chunks)
	}
}

// TestStreamCeilingAborts asserts a streamed charge that breaches the ceiling
// returns budget.ErrCeiling — for BOTH the streaming path and the non-streamer
// one-chunk fallback.
func TestStreamCeilingAborts(t *testing.T) {
	t.Run("streaming", func(t *testing.T) {
		inner := &streamingProvider{id: "claude-opus-4-8", usage: model.Usage{InputTokens: 1000, OutputTokens: 1000}, deltas: []string{"x"}}
		led := budget.New()
		led.SetGlobalCeiling(0.05) // first 0.030 fits; second breaches.
		p := &Provider{Inner: inner, Ledger: led, Task: "t1", Price: NewTable()}

		if _, err := p.Stream(context.Background(), "sys", nil, nil, 100, nil); err != nil {
			t.Fatalf("first stream should fit, got %v", err)
		}
		if _, err := p.Stream(context.Background(), "sys", nil, nil, 100, nil); !errors.Is(err, budget.ErrCeiling) {
			t.Fatalf("second stream: err = %v, want budget.ErrCeiling", err)
		}
	})

	t.Run("fallback", func(t *testing.T) {
		inner := &fakeProvider{id: "claude-opus-4-8", usage: model.Usage{InputTokens: 1000, OutputTokens: 1000}}
		led := budget.New()
		led.SetGlobalCeiling(0.05)
		p := &Provider{Inner: inner, Ledger: led, Task: "t1", Price: NewTable()}

		if _, err := p.Stream(context.Background(), "sys", nil, nil, 100, nil); err != nil {
			t.Fatalf("first stream should fit, got %v", err)
		}
		if _, err := p.Stream(context.Background(), "sys", nil, nil, 100, nil); !errors.Is(err, budget.ErrCeiling) {
			t.Fatalf("second stream: err = %v, want budget.ErrCeiling", err)
		}
	})
}

// TestStreamPartialOnCancelStillCharges asserts the partial-on-cancel rule: when
// the inner stream returns a non-nil error together with usage (a cut-short
// stream), the decorator STILL charges the produced tokens and surfaces the inner
// error (the inner error takes precedence over a ceiling breach).
func TestStreamPartialOnCancelStillCharges(t *testing.T) {
	cancelErr := context.Canceled
	inner := &streamingProvider{
		id:     "claude-opus-4-8",
		usage:  model.Usage{InputTokens: 1000, OutputTokens: 1000},
		deltas: []string{"par", "tial"},
		err:    cancelErr,
	}
	led := budget.New()
	p := &Provider{Inner: inner, Ledger: led, Task: "t1", Price: NewTable()}

	_, err := p.Stream(context.Background(), "sys", nil, nil, 100, nil)
	if !errors.Is(err, cancelErr) {
		t.Fatalf("err = %v, want context.Canceled (inner error surfaced)", err)
	}
	// The partial's tokens were still charged.
	tokens, dollars := led.Total()
	if tokens != 2000 || !almostEqualDollars(dollars, 0.030) {
		t.Errorf("partial charged (%d, %v), want (2000, 0.030)", tokens, dollars)
	}
}

// TestStreamConcurrentSharedLedger is the -race acceptance criterion for the
// streaming path: many metered streamers (distinct task keys) sharing ONE ledger
// must charge it concurrently without a data race, with exact totals.
func TestStreamConcurrentSharedLedger(t *testing.T) {
	led := budget.New()
	const (
		workers      = 8
		callsPerWork = 50
	)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			inner := &streamingProvider{id: "claude-haiku-4-5", usage: model.Usage{InputTokens: 500, OutputTokens: 500}, deltas: []string{"a", "b"}}
			p := &Provider{Inner: inner, Ledger: led, Task: string(rune('a' + w)), Price: NewTable()}
			for i := 0; i < callsPerWork; i++ {
				if _, err := p.Stream(context.Background(), "sys", nil, nil, 64, func(model.Chunk) {}); err != nil {
					t.Errorf("worker %d call %d: %v", w, i, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	totalCalls := workers * callsPerWork
	wantTokens := totalCalls * 1000
	wantDollars := float64(totalCalls) * (0.0005 + 0.0025)
	gTokens, gDollars := led.Total()
	if gTokens != wantTokens || !almostEqualDollars(gDollars, wantDollars) {
		t.Errorf("concurrent stream global = (%d, %v), want (%d, %v)", gTokens, gDollars, wantTokens, wantDollars)
	}
}

// textProvider is a non-streaming model.Provider whose Complete returns a fixed
// text block, so the one-chunk fallback's text replay can be asserted.
type textProvider struct {
	id    string
	text  string
	usage model.Usage
}

func (t *textProvider) Complete(ctx context.Context, system string, msgs []model.Message, tools []model.Tool, maxTokens int) (model.Response, error) {
	return model.Response{Content: []model.Block{{Type: "text", Text: t.text}}, Usage: t.usage}, nil
}

func (t *textProvider) Model() string { return t.id }
