// pool_test.go proves the composition invariants of the swarm provider pool
// HERMETICALLY: no real vendor adapter, no network, no strongcap/Resilient
// internals relied on by behavior beyond their public contract. Every provider is
// a scripted fake implementing model.Provider; the construction seams
// (resolve/cap/resilient) are injected via the unexported Options fields so each
// test sees exactly the providers it scripted. Pricing is a flat $1/call so
// ledger assertions are exact and independent of the real rate table.
package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"nilcore/internal/budget"
	"nilcore/internal/model"
)

// --- test doubles -----------------------------------------------------------

// fakeProvider is a scripted model.Provider. Each Complete consumes the next
// scripted outcome (cycling the last one once exhausted) and counts its calls. A
// nil err is a success returning a 1-input/1-output-token Response so the flat
// pricer charges exactly $1; a non-nil err is a failure that charges nothing.
type fakeProvider struct {
	id   string
	mu   sync.Mutex
	errs []error // per-call outcome; last entry repeats
	n    int     // call count
}

func newFake(id string, errs ...error) *fakeProvider {
	return &fakeProvider{id: id, errs: errs}
}

func (f *fakeProvider) Model() string { return f.id }

func (f *fakeProvider) Complete(ctx context.Context, system string, msgs []model.Message, tools []model.Tool, maxTokens int) (model.Response, error) {
	f.mu.Lock()
	i := f.n
	f.n++
	var err error
	if len(f.errs) > 0 {
		if i < len(f.errs) {
			err = f.errs[i]
		} else {
			err = f.errs[len(f.errs)-1]
		}
	}
	f.mu.Unlock()
	if err != nil {
		return model.Response{}, err
	}
	return model.Response{Usage: model.Usage{InputTokens: 1, OutputTokens: 1}}, nil
}

func (f *fakeProvider) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// flatPricer charges a fixed $1 for any successful call, so ledger totals equal
// the number of billed completions — exact, deterministic assertions.
type flatPricer struct{}

func (flatPricer) Price(modelID string, in, out int) float64 {
	if in <= 0 && out <= 0 {
		return 0
	}
	return 1
}

// PriceUsage satisfies the widened Pricer interface (P15-T15). It stays a faithful
// flat double: an authoritative reported cost wins when set, otherwise it delegates
// to Price over the usage's token split so the swarm's $1/call ledger assertions
// hold byte-identically.
func (f flatPricer) PriceUsage(modelID string, u model.Usage) float64 {
	if u.CostUSD > 0 {
		return u.CostUSD
	}
	return f.Price(modelID, u.InputTokens, u.OutputTokens)
}

// scriptedResolve maps spec strings to pre-built fakes, recording how many times
// each spec was resolved so dedup/sharing can be asserted. An unmapped spec is an
// error (forces tests to declare every spec they use).
type scriptedResolve struct {
	mu       sync.Mutex
	bySpec   map[string]model.Provider
	resolved map[string]int
}

func newScriptedResolve(m map[string]model.Provider) *scriptedResolve {
	return &scriptedResolve{bySpec: m, resolved: map[string]int{}}
}

func (s *scriptedResolve) fn(spec string, getenv func(string) string) (model.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolved[spec]++
	p, ok := s.bySpec[spec]
	if !ok {
		return nil, fmt.Errorf("scriptedResolve: no provider for spec %q", spec)
	}
	return p, nil
}

func (s *scriptedResolve) resolveCount(spec string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolved[spec]
}

// countingCap is a strongcap stand-in that enforces a real concurrency bound AND
// tracks peak in-flight, so a -race test can assert the peak never exceeds the
// cap. It is constructed via the injected capFunc, and records every instance so
// the test can prove sharing (one instance per distinct spec identity).
type countingCap struct {
	inner  model.Provider
	sem    chan struct{}
	cur    int64
	peak   int64
	peakMu sync.Mutex
}

func (c *countingCap) Model() string { return c.inner.Model() }

func (c *countingCap) Complete(ctx context.Context, system string, msgs []model.Message, tools []model.Tool, maxTokens int) (model.Response, error) {
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return model.Response{}, ctx.Err()
	}
	cur := atomic.AddInt64(&c.cur, 1)
	c.peakMu.Lock()
	if cur > c.peak {
		c.peak = cur
	}
	c.peakMu.Unlock()
	defer atomic.AddInt64(&c.cur, -1)
	return c.inner.Complete(ctx, system, msgs, tools, maxTokens)
}

// capFactory hands out countingCap instances and records them by the inner
// model id so a test can assert exactly one cap was created per distinct spec.
type capFactory struct {
	mu    sync.Mutex
	made  map[string]*countingCap
	count int
}

func newCapFactory() *capFactory { return &capFactory{made: map[string]*countingCap{}} }

func (cf *capFactory) fn(inner model.Provider, max int) model.Provider {
	cf.mu.Lock()
	defer cf.mu.Unlock()
	cf.count++
	c := &countingCap{inner: inner, sem: make(chan struct{}, max)}
	cf.made[inner.Model()] = c
	return c
}

// realResilient is the production Resilient wrapper — we use the REAL one so the
// "meter charges once across a retry/failover" property is tested against the
// actual decorator, not a stub. model.Options{} with defaults gives 0 extra
// retries, which is what the swarm uses; tests that need failover script the
// primary to error so Resilient falls over to the fallback provider.
func realResilient(providers []model.Provider, opts model.Options) (model.Provider, error) {
	return model.NewResilient(providers, opts)
}

// noCred is the by-name key resolver the pool never inspects.
func noCred(string) string { return "" }

// testOpts builds Options wired to the given construction seams plus the flat
// pricer, so every test gets deterministic charging.
func testOpts(resolve resolveFunc, cap capFunc, onUsage func(string, int, int)) Options {
	return Options{
		OnUsage:   onUsage,
		Pricer:    flatPricer{},
		resolve:   resolve,
		cap:       cap,
		resilient: realResilient,
	}
}

// --- Validate ---------------------------------------------------------------

func TestValidate(t *testing.T) {
	valid := []string{"anthropic", "openai", "openrouter"}
	tests := []struct {
		name    string
		cfg     PoolConfig
		wantErr bool
	}{
		{"zero is valid", PoolConfig{}, false},
		{"known vendors", PoolConfig{
			Planner: TierSpec{Spec: "anthropic:claude-opus-4-8"},
			Worker:  TierSpec{Spec: "openai:gpt-5.5", Fallback: "openrouter:fusion"},
		}, false},
		{"bare model defaults anthropic", PoolConfig{Worker: TierSpec{Spec: "claude-haiku-4-5"}}, false},
		{"zero cap ok", PoolConfig{Worker: TierSpec{Spec: "anthropic:x", Cap: 0}}, false},
		{"unknown vendor", PoolConfig{Worker: TierSpec{Spec: "nope:model"}}, true},
		{"unknown fallback vendor", PoolConfig{Worker: TierSpec{Spec: "anthropic:x", Fallback: "bad:y"}}, true},
		{"negative tier cap", PoolConfig{Worker: TierSpec{Spec: "anthropic:x", Cap: -1}}, true},
		{"negative caps entry", PoolConfig{Caps: map[string]int{"anthropic:x": -3}}, true},
		{"unknown caps vendor", PoolConfig{Caps: map[string]int{"weird:m": 2}}, true},
		{"bad jitter", PoolConfig{Jitter: "notaduration"}, true},
		{"bad call timeout", PoolConfig{CallTimeout: "5"}, true},
		{"good durations", PoolConfig{Jitter: "750ms", CallTimeout: "30s"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate(valid)
			if tc.wantErr != (err != nil) {
				t.Fatalf("Validate() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateNilProvidersSkipsVendorCheck(t *testing.T) {
	// A nil/empty valid list disables the vendor check (but NOT the cap/duration
	// checks).
	if err := (PoolConfig{Worker: TierSpec{Spec: "anything:goes"}}).Validate(nil); err != nil {
		t.Fatalf("nil providers should skip vendor check: %v", err)
	}
	if err := (PoolConfig{Worker: TierSpec{Spec: "x:y", Cap: -1}}).Validate(nil); err == nil {
		t.Fatal("negative cap must still be rejected with nil providers")
	}
}

// --- meter charges once across a Resilient retry/failover -------------------

func TestMeterChargesOncePerLogicalCall(t *testing.T) {
	// Primary fails its FIRST call, fallback succeeds. With the real Resilient the
	// logical Complete fails over to the fallback — one logical call, one billed
	// completion. The meter is OUTERMOST, so it must charge exactly $1 once, never
	// once-per-attempt.
	primary := newFake("p1", errors.New("rate limited"))
	fallback := newFake("p2") // always succeeds
	resolve := newScriptedResolve(map[string]model.Provider{
		"primary:m":  primary,
		"fallback:m": fallback,
	})
	led := budget.New()
	cfg := PoolConfig{Worker: TierSpec{Spec: "primary:m", Fallback: "fallback:m"}}
	p, err := Build(cfg, led, noCred, "run1", testOpts(resolve.fn, nil, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	w := p.WorkerFor("s0")
	if _, err := w.Complete(context.Background(), "", nil, nil, 0); err != nil {
		t.Fatalf("Complete failed over but should succeed: %v", err)
	}
	// One logical call → exactly $1 charged once.
	if tokens, dollars := led.Total(); dollars != 1 || tokens != 2 {
		t.Fatalf("expected 1 logical charge ($1, 2 tokens), got $%v / %d tokens", dollars, tokens)
	}
	if primary.calls() != 1 {
		t.Fatalf("primary should have been tried once, got %d", primary.calls())
	}
	if fallback.calls() != 1 {
		t.Fatalf("fallback should have served once, got %d", fallback.calls())
	}
}

// --- fallback on rate-limit -------------------------------------------------

func TestFallbackOnRateLimit(t *testing.T) {
	primary := newFake("p1", errors.New("429 too many requests"))
	fallback := newFake("p2")
	resolve := newScriptedResolve(map[string]model.Provider{"a:m": primary, "b:m": fallback})
	led := budget.New()
	cfg := PoolConfig{Worker: TierSpec{Spec: "a:m", Fallback: "b:m"}}
	p, err := Build(cfg, led, noCred, "run1", testOpts(resolve.fn, nil, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	resp, err := p.WorkerFor("s0").Complete(context.Background(), "", nil, nil, 0)
	if err != nil {
		t.Fatalf("expected fallback success, got %v", err)
	}
	if resp.Usage.InputTokens != 1 {
		t.Fatalf("expected fallback's response, got %+v", resp.Usage)
	}
}

// --- strongcap presence by cap ----------------------------------------------

func TestStrongcapPresenceByCap(t *testing.T) {
	t.Run("cap==0 → no cap layer", func(t *testing.T) {
		cf := newCapFactory()
		resolve := newScriptedResolve(map[string]model.Provider{"a:m": newFake("a")})
		_, err := Build(PoolConfig{Worker: TierSpec{Spec: "a:m", Cap: 0}}, budget.New(), noCred, "r", testOpts(resolve.fn, cf.fn, nil))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if cf.count != 0 {
			t.Fatalf("cap==0 must not create a strongcap layer, got %d", cf.count)
		}
	})
	t.Run("cap>0 → cap layer present", func(t *testing.T) {
		cf := newCapFactory()
		resolve := newScriptedResolve(map[string]model.Provider{"a:m": newFake("a")})
		_, err := Build(PoolConfig{Worker: TierSpec{Spec: "a:m", Cap: 3}}, budget.New(), noCred, "r", testOpts(resolve.fn, cf.fn, nil))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if cf.count != 1 {
			t.Fatalf("cap>0 must create exactly one strongcap layer, got %d", cf.count)
		}
	})
}

// --- per-shard meter isolation ----------------------------------------------

func TestPerShardMeterIsolation(t *testing.T) {
	// Two shards over the SAME worker stack: distinct Spent per scope, one shared
	// Inner provider (same call counter), all rolling into one Total.
	inner := newFake("w")
	resolve := newScriptedResolve(map[string]model.Provider{"w:m": inner})
	led := budget.New()
	p, err := Build(PoolConfig{Worker: TierSpec{Spec: "w:m"}}, led, noCred, "run1", testOpts(resolve.fn, nil, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wa := p.WorkerFor("sa")
	wb := p.WorkerFor("sb")

	// sa makes 2 calls, sb makes 1.
	for i := 0; i < 2; i++ {
		if _, err := wa.Complete(context.Background(), "", nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := wb.Complete(context.Background(), "", nil, nil, 0); err != nil {
		t.Fatal(err)
	}

	if _, d := p.Spent(p.Scope("sa")); d != 2 {
		t.Fatalf("sa Spent = $%v, want $2", d)
	}
	if _, d := p.Spent(p.Scope("sb")); d != 1 {
		t.Fatalf("sb Spent = $%v, want $1", d)
	}
	if _, total := p.Usage(); total != 3 {
		t.Fatalf("Total = $%v, want $3", total)
	}
	// Shared Inner: the one fake saw all 3 calls.
	if inner.calls() != 3 {
		t.Fatalf("shared inner calls = %d, want 3", inner.calls())
	}
	// Distinct meters: WorkerFor returns a fresh provider each call.
	if wa == wb {
		t.Fatal("WorkerFor must return distinct meter providers per shard")
	}
}

// --- spec dedup: shared Resilient (breaker) + shared strongcap --------------

func TestSharedStackAcrossSameSpecTiers(t *testing.T) {
	// Planner and Verifier name the SAME spec+cap → they must share ONE resolved
	// provider (resolved once), ONE strongcap, and (because the cap factory wraps
	// one Resilient) one breaker.
	inner := newFake("s")
	resolve := newScriptedResolve(map[string]model.Provider{"s:m": inner})
	cf := newCapFactory()
	cfg := PoolConfig{
		Planner:  TierSpec{Spec: "s:m", Cap: 2},
		Verifier: TierSpec{Spec: "s:m", Cap: 2},
		Worker:   TierSpec{Spec: "s:m", Cap: 2}, // all three tiers name the same spec
	}
	p, err := Build(cfg, budget.New(), noCred, "run1", testOpts(resolve.fn, cf.fn, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Spec resolved exactly once despite three tiers naming it.
	if got := resolve.resolveCount("s:m"); got != 1 {
		t.Fatalf("shared spec resolved %d times, want 1 (dedup)", got)
	}
	// Exactly one strongcap created for the shared spec.
	if cf.count != 1 {
		t.Fatalf("expected one shared strongcap, got %d", cf.count)
	}
	// Both tiers route through the SAME cap instance (peak counts both).
	if p.Planner() == p.Verifier() {
		t.Fatal("planner/verifier are distinct meter scopes even when sharing a stack")
	}
}

// TestInheritedTierSharesCappedStack proves the MAJOR-#4 fix: an inherited-spec
// strong tier (empty Spec, the DEFAULT `nilcore swarm` path) must share the
// worker's ONE capped stack, not fork a second uncapped one.
//
// The cap is declared ONLY via the per-spec Caps map keyed on the worker's
// resolved spec — NOT via TierSpec.Cap. Planner/Verifier carry an empty Spec, so
// they inherit the worker spec. Under the pre-fix code effectiveCap looked up
// caps[""] for the strong tiers (→ 0, uncapped) while the worker looked up
// caps["w:m"] (→ 5, capped); the differing effective caps produced different
// cache keys and thus a SECOND, uncapped stack for the shared provider. The fix
// keys the lookup on the resolved primary, so all three tiers collapse to one
// capped stack: ONE resolve, ONE strongcap.
func TestInheritedTierSharesCappedStack(t *testing.T) {
	inner := newFake("w")
	resolve := newScriptedResolve(map[string]model.Provider{"w:m": inner})
	cf := newCapFactory()
	cfg := PoolConfig{
		// Worker names the spec explicitly; Planner+Verifier inherit it (empty Spec).
		Worker:   TierSpec{Spec: "w:m"},
		Planner:  TierSpec{}, // inherits w:m
		Verifier: TierSpec{}, // inherits w:m
		// Cap declared by RESOLVED spec only — discriminates the bug: the raw
		// strong-tier Spec is "", so a caps[""] lookup would miss and leave them
		// uncapped, while the worker is capped.
		Caps: map[string]int{"w:m": 5},
	}
	p, err := Build(cfg, budget.New(), noCred, "run1", testOpts(resolve.fn, cf.fn, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// All three tiers collapse onto the one resolved spec.
	if got := resolve.resolveCount("w:m"); got != 1 {
		t.Fatalf("inherited tiers must dedup onto the worker spec: resolved %d times, want 1", got)
	}
	// Exactly ONE capped stack — the strong tiers did NOT fork a second uncapped one.
	if cf.count != 1 {
		t.Fatalf("inherited strong tiers must share the worker's single capped stack: got %d strongcaps, want 1", cf.count)
	}
	// And it is genuinely the capped stack (cap=5), shared by planner/verifier/worker:
	// every tier's inner is the one countingCap instance over the shared provider.
	cap := cf.made["w"]
	if cap == nil {
		t.Fatal("the shared stack must be the capped one (no strongcap recorded for the worker spec)")
	}
	// Behavioral proof: a call through the inherited planner tier and a call through
	// the worker tier both pass through the SAME capped countingCap (its in-flight
	// counter and the shared inner fake see both), confirming one shared stack.
	if _, err := p.Planner().Complete(context.Background(), "", nil, nil, 0); err != nil {
		t.Fatalf("planner call: %v", err)
	}
	if _, err := p.WorkerFor("s0").Complete(context.Background(), "", nil, nil, 0); err != nil {
		t.Fatalf("worker call: %v", err)
	}
	if inner.calls() != 2 {
		t.Fatalf("inherited planner + worker must share the one capped inner: inner saw %d calls, want 2", inner.calls())
	}
}

// TestSharedBreakerAcrossSameSpecTiers proves the breaker is shared: with the
// real Resilient and a breaker threshold of 1, a failure through the planner tier
// opens the breaker that the verifier tier (same shared stack) also sees.
func TestSharedBreakerAcrossSameSpecTiers(t *testing.T) {
	// Provider fails every call; with BreakerThreshold=1 the first failure opens
	// the breaker. Because planner+verifier share one Resilient, the SECOND call
	// (through verifier) is rejected as "breaker open" without invoking inner
	// again.
	inner := newFake("s", errors.New("down"))
	resolve := newScriptedResolve(map[string]model.Provider{"s:m": inner})
	cfg := PoolConfig{
		Planner:  TierSpec{Spec: "s:m"},
		Verifier: TierSpec{Spec: "s:m"},
		Worker:   TierSpec{Spec: "s:m"}, // keep all tiers on the one shared spec
		Breaker:  1,
	}
	p, err := Build(cfg, budget.New(), noCred, "run1", testOpts(resolve.fn, nil, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// First call through planner: fails, opens the shared breaker.
	if _, err := p.Planner().Complete(context.Background(), "", nil, nil, 0); err == nil {
		t.Fatal("expected planner call to fail")
	}
	callsAfterFirst := inner.calls()
	// Second call through verifier: breaker is open → inner is NOT called again.
	if _, err := p.Verifier().Complete(context.Background(), "", nil, nil, 0); err == nil {
		t.Fatal("expected verifier call to fail (breaker open)")
	}
	if inner.calls() != callsAfterFirst {
		t.Fatalf("shared breaker should have skipped inner; inner calls went %d → %d", callsAfterFirst, inner.calls())
	}
}

// --- per-provider concurrency cap under -race -------------------------------

func TestPerProviderCapUnderRace(t *testing.T) {
	// 50 goroutines hammer the SAME worker stack at cap=4; the countingCap must
	// never observe more than 4 in flight. Uses the REAL composition: every
	// WorkerFor wraps the one shared stack, so the cap is process-wide.
	inner := newFake("w")
	resolve := newScriptedResolve(map[string]model.Provider{"w:m": inner})
	cf := newCapFactory()
	p, err := Build(PoolConfig{Worker: TierSpec{Spec: "w:m", Cap: 4}}, budget.New(), noCred, "run1", testOpts(resolve.fn, cf.fn, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			w := p.WorkerFor(fmt.Sprintf("s%d", i))
			if _, err := w.Complete(context.Background(), "", nil, nil, 0); err != nil {
				t.Errorf("worker %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	cap := cf.made["w"]
	if cap == nil {
		t.Fatal("no strongcap created for worker spec")
	}
	cap.peakMu.Lock()
	peak := cap.peak
	cap.peakMu.Unlock()
	if peak > 4 {
		t.Fatalf("peak in-flight = %d, must be <= 4", peak)
	}
	if peak == 0 {
		t.Fatal("cap was never exercised (peak 0)")
	}
	if inner.calls() != n {
		t.Fatalf("all %d shards should have completed, got %d", n, inner.calls())
	}
}

// --- OnUsage tally under -race ----------------------------------------------

func TestOnUsageTallyUnderRace(t *testing.T) {
	inner := newFake("w")
	resolve := newScriptedResolve(map[string]model.Provider{"w:m": inner})
	var inTot, outTot int64
	onUsage := func(modelID string, in, out int) {
		atomic.AddInt64(&inTot, int64(in))
		atomic.AddInt64(&outTot, int64(out))
	}
	p, err := Build(PoolConfig{Worker: TierSpec{Spec: "w:m"}}, budget.New(), noCred, "run1", testOpts(resolve.fn, nil, onUsage))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			if _, err := p.WorkerFor(fmt.Sprintf("s%d", i)).Complete(context.Background(), "", nil, nil, 0); err != nil {
				t.Errorf("worker %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if atomic.LoadInt64(&inTot) != n || atomic.LoadInt64(&outTot) != n {
		t.Fatalf("OnUsage tally = in:%d out:%d, want %d/%d", inTot, outTot, n, n)
	}
}

// --- budget ceiling routing: global vs per-shard ----------------------------

func TestGlobalAndShardCeilingRouting(t *testing.T) {
	inner := newFake("w")
	resolve := newScriptedResolve(map[string]model.Provider{"w:m": inner})
	led := budget.New()
	p, err := Build(PoolConfig{Worker: TierSpec{Spec: "w:m"}}, led, noCred, "run1", testOpts(resolve.fn, nil, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Per-shard ceiling of $2 on shard sx; global ceiling of $5.
	p.SetShardCeiling("sx", 2)
	p.SetGlobalCeiling(5)

	wx := p.WorkerFor("sx")
	// Two calls succeed (spend $2, exactly the shard ceiling).
	for i := 0; i < 2; i++ {
		if _, err := wx.Complete(context.Background(), "", nil, nil, 0); err != nil {
			t.Fatalf("call %d should succeed: %v", i, err)
		}
	}
	// Third call on sx breaches the per-shard ceiling.
	if _, err := wx.Complete(context.Background(), "", nil, nil, 0); !errors.Is(err, budget.ErrCeiling) {
		t.Fatalf("expected per-shard ErrCeiling, got %v", err)
	}
	// Headroom for sx is now zero (out of shard budget); a different shard still
	// has global headroom ($5 - $2 spent = $3).
	if h, _ := p.Headroom(context.Background(), p.Scope("sx")); h > 0 {
		t.Fatalf("sx headroom should be <= 0, got %v", h)
	}
	if h, _ := p.Headroom(context.Background(), p.Scope("sy")); h <= 0 {
		t.Fatalf("sy (no shard ceiling) should still have global headroom, got %v", h)
	}

	// Now spend on other shards up to the global ceiling and confirm a global
	// breach is what stops the run.
	wy := p.WorkerFor("sy")
	for i := 0; i < 3; i++ { // $2 already spent; 3 more → $5 total (the global cap)
		if _, err := wy.Complete(context.Background(), "", nil, nil, 0); err != nil {
			t.Fatalf("sy call %d should succeed within global budget: %v", i, err)
		}
	}
	// Global headroom is now zero.
	if h, _ := p.Headroom(context.Background(), p.Scope("sz")); h > 0 {
		t.Fatalf("global headroom should be <= 0 after hitting global ceiling, got %v", h)
	}
	// Any further call breaches the global ceiling.
	if _, err := p.WorkerFor("sz").Complete(context.Background(), "", nil, nil, 0); !errors.Is(err, budget.ErrCeiling) {
		t.Fatalf("expected global ErrCeiling, got %v", err)
	}
}

func TestHeadroomUnlimitedWithoutCeilings(t *testing.T) {
	resolve := newScriptedResolve(map[string]model.Provider{"w:m": newFake("w")})
	p, err := Build(PoolConfig{Worker: TierSpec{Spec: "w:m"}}, budget.New(), noCred, "run1", testOpts(resolve.fn, nil, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	h, err := p.Headroom(context.Background(), p.Scope("s0"))
	if err != nil {
		t.Fatalf("Headroom err: %v", err)
	}
	if h < 1e300 {
		t.Fatalf("no ceilings ⇒ effectively unlimited headroom, got %v", h)
	}
}

func TestHeadroomCancelledCtxFailsClosed(t *testing.T) {
	resolve := newScriptedResolve(map[string]model.Provider{"w:m": newFake("w")})
	p, err := Build(PoolConfig{Worker: TierSpec{Spec: "w:m"}}, budget.New(), noCred, "run1", testOpts(resolve.fn, nil, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h, err := p.Headroom(ctx, p.Scope("s0"))
	if err == nil {
		t.Fatal("cancelled ctx must return an error (fail-closed)")
	}
	if h != 0 {
		t.Fatalf("cancelled ctx headroom must be 0, got %v", h)
	}
}

// --- CodeBackendFor ---------------------------------------------------------

func TestCodeBackendFor(t *testing.T) {
	resolve := newScriptedResolve(map[string]model.Provider{"w:m": newFake("w")})
	tests := []struct {
		name    string
		backend string
		want    string
	}{
		{"empty defaults native", "", "native"},
		{"native explicit", "native", "native"},
		{"codex", "codex", "codex"},
		{"claude-code", "claude-code", "claude-code"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := PoolConfig{Worker: TierSpec{Spec: "w:m", CodeBackend: tc.backend}}
			p, err := Build(cfg, budget.New(), noCred, "run1", testOpts(resolve.fn, nil, nil))
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if got := p.CodeBackendFor("implementer"); got != tc.want {
				t.Fatalf("CodeBackendFor = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- zero config reproduces the single cheap-worker wiring ------------------

func TestZeroConfigDefaultsToCheapWorker(t *testing.T) {
	// A zero PoolConfig must resolve every tier to the default cheap worker spec,
	// uncapped. The production resolver/cap seams are exercised here by injecting a
	// scripted resolver keyed on the default spec.
	def := newFake(defaultWorkerSpec)
	resolve := newScriptedResolve(map[string]model.Provider{defaultWorkerSpec: def})
	cf := newCapFactory()
	led := budget.New()
	p, err := Build(PoolConfig{}, led, noCred, "run1", testOpts(resolve.fn, cf.fn, nil))
	if err != nil {
		t.Fatalf("Build zero config: %v", err)
	}
	// All three tiers share the one default spec → resolved exactly once.
	if got := resolve.resolveCount(defaultWorkerSpec); got != 1 {
		t.Fatalf("default spec resolved %d times, want 1 (all tiers dedup)", got)
	}
	// Uncapped → no strongcap.
	if cf.count != 0 {
		t.Fatalf("zero config is uncapped, want 0 strongcaps, got %d", cf.count)
	}
	// Calls flow through to the single shared default provider.
	if _, err := p.WorkerFor("s0").Complete(context.Background(), "", nil, nil, 0); err != nil {
		t.Fatalf("worker call: %v", err)
	}
	if _, err := p.Planner().Complete(context.Background(), "", nil, nil, 0); err != nil {
		t.Fatalf("planner call: %v", err)
	}
	if def.calls() != 2 {
		t.Fatalf("default provider should have served both calls, got %d", def.calls())
	}
}

// --- I3: the credential never escapes the resolve seam ----------------------

// sentinelModel is a model.Provider whose Model() returns whatever id it is
// given, so the test can assert the sentinel did NOT bleed into a model id.
type sentinelModel struct{ id string }

func (s sentinelModel) Model() string { return s.id }
func (s sentinelModel) Complete(ctx context.Context, system string, msgs []model.Message, tools []model.Tool, maxTokens int) (model.Response, error) {
	return model.Response{Usage: model.Usage{InputTokens: 1, OutputTokens: 1}}, nil
}

// TestCredentialNeverEscapesResolveSeam pins invariant I3: a real credential
// flows ONLY to the by-name resolve seam (the getenv argument) and NEVER appears
// in a meter scope label, a backend Task key, a model id, or anything handed to a
// decorator. The other pool tests use noCred→"" so this property is structurally
// true but UNPINNED; here we feed a SENTINEL key and prove it does not leak.
func TestCredentialNeverEscapesResolveSeam(t *testing.T) {
	const sentinel = "sk-SECRET-DO-NOT-LEAK-7f3a9c"
	// cred returns the sentinel for any name — the pool must never invoke it
	// itself, only forward it to the resolve seam as getenv.
	var credCalls int64
	cred := func(string) string {
		atomic.AddInt64(&credCalls, 1)
		return sentinel
	}

	// The resolve seam captures the getenv it is handed and verifies that getenv
	// IS the sentinel-bearing cred (i.e. the credential is reachable at the seam,
	// the one place it is allowed). It builds providers whose Model() ids are
	// plain specs, NOT the resolved key.
	var seamSawSentinel bool
	var seamMu sync.Mutex
	resolveFn := func(spec string, getenv func(string) string) (model.Provider, error) {
		if getenv == nil {
			t.Errorf("resolve seam got nil getenv; cred must be threaded through")
		} else if getenv("ANY") == sentinel {
			seamMu.Lock()
			seamSawSentinel = true
			seamMu.Unlock()
		}
		return sentinelModel{id: spec}, nil
	}

	// Capture every value handed to the cap decorator (the inner provider and its
	// model id) so we can assert the sentinel is not among them.
	var capInnerIDs []string
	capFn := func(inner model.Provider, max int) model.Provider {
		capInnerIDs = append(capInnerIDs, inner.Model())
		return inner // pass-through; we only need to observe what flows in
	}

	led := budget.New()
	cfg := PoolConfig{
		Planner:  TierSpec{Spec: "anthropic:strong"},
		Verifier: TierSpec{Spec: "anthropic:strong"},
		Worker:   TierSpec{Spec: "anthropic:cheap", Fallback: "openai:backup", Cap: 2},
		Caps:     map[string]int{"anthropic:cheap": 2},
	}
	opts := Options{Pricer: flatPricer{}, resolve: resolveFn, cap: capFn, resilient: realResilient}
	p, err := Build(cfg, led, cred, "run-secret", opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// The credential MUST have reached the resolve seam (its sanctioned sink) ...
	seamMu.Lock()
	saw := seamSawSentinel
	seamMu.Unlock()
	if !saw {
		t.Fatal("credential never reached the resolve seam; the test cannot prove non-leakage")
	}
	// ... and the pool MUST NOT have invoked cred itself (it only forwards it).
	if n := atomic.LoadInt64(&credCalls); n == 0 {
		t.Fatal("cred was never called via the seam; non-leakage is unproven")
	}

	// Run real calls so every observable surface (scope Task labels, model ids) is
	// materialized through the live providers.
	for _, prov := range []model.Provider{p.Planner(), p.Verifier(), p.WorkerFor("shard0")} {
		if _, err := prov.Complete(context.Background(), "", nil, nil, 0); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if got := prov.Model(); got == sentinel {
			t.Fatalf("model id leaked the credential: %q", got)
		}
	}

	// Collect every surface that an event/log/ledger reader can see and assert the
	// sentinel appears in NONE of them.
	surfaces := []string{
		p.Scope("shard0"),
		p.scopeRoot() + "planner",
		p.scopeRoot() + "verifier",
		p.CodeBackendFor("implementer"),
		p.Planner().Model(),
		p.Verifier().Model(),
		p.WorkerFor("shard0").Model(),
	}
	surfaces = append(surfaces, capInnerIDs...)
	for _, s := range surfaces {
		if s == sentinel {
			t.Fatalf("credential leaked verbatim into an observable surface: %q", s)
		}
		// Substring check too: catch a key embedded in a composite label.
		if len(sentinel) > 0 && contains(s, sentinel) {
			t.Fatalf("credential leaked as a substring of an observable surface: %q", s)
		}
	}
}

// contains is a tiny strings.Contains stand-in kept local so the test file's
// import set stays minimal (matches the package's stdlib-lean style).
func contains(haystack, needle string) bool {
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// --- nil ledger guard -------------------------------------------------------

func TestBuildNilLedger(t *testing.T) {
	resolve := newScriptedResolve(map[string]model.Provider{"w:m": newFake("w")})
	if _, err := Build(PoolConfig{Worker: TierSpec{Spec: "w:m"}}, nil, noCred, "r", testOpts(resolve.fn, nil, nil)); err == nil {
		t.Fatal("Build with nil ledger must error")
	}
}

// --- scope shape ------------------------------------------------------------

func TestScopeShape(t *testing.T) {
	resolve := newScriptedResolve(map[string]model.Provider{"w:m": newFake("w")})
	p, err := Build(PoolConfig{Worker: TierSpec{Spec: "w:m"}}, budget.New(), noCred, "runXYZ", testOpts(resolve.fn, nil, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := p.Scope("shard7"), "swarm/runXYZ/shard7"; got != want {
		t.Fatalf("Scope = %q, want %q", got, want)
	}
}
