package meter

import (
	"context"
	"errors"
	"math"
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

// compile-time assertion: the decorator is itself a model.Provider, so it drops
// in wherever a provider is expected (the whole point — wrap every provider).
var _ model.Provider = (*Provider)(nil)

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
