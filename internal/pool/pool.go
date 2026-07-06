// Package pool composes the swarm's tiered, capped, failover-capable, metered
// model providers as ONE unit, out of seams that already ship. It introduces no
// new model dependency and adds no new behavior to any single decorator — it is
// pure composition (invariant I6). The whole reason it exists is that a 300-agent
// fan-out needs four properties on its model calls at once, and they must nest in
// a specific order to be correct:
//
//	meter.Provider        (OUTERMOST — charges the one shared ledger exactly ONCE
//	  └─ strongcap          per LOGICAL call, so a Resilient retry/failover under
//	       └─ Resilient      it is still a single billable call)
//	            └─ vendor   (strongcap caps concurrency BELOW the meter so the cap
//	                          governs real in-flight provider calls, not charges)
//
// The order is load-bearing and matches §6 of docs/SWARM.md. Getting it wrong
// (e.g. metering inside Resilient) would charge once per retry and make the
// budget ceiling lie. Getting strongcap above Resilient would let failover
// double-count a concurrency slot.
//
// Sharing is the second correctness property: every tier that names the SAME
// "provider:model" (primary+fallback) must share ONE Resilient (so they share one
// circuit breaker — a provider that is down is down for everyone) and ONE
// strongcap (so the concurrency bound is process-wide, not per-tier). The pool
// deduplicates on the spec identity to guarantee this.
//
// KEY DISCIPLINE (invariant I3): no key ever enters this package. Specs are
// "provider:model" strings only; the actual API key is resolved lazily, by NAME,
// through the injected `cred func(string) string` resolver handed to
// provider.ResolveWith. The pool never sees, stores, logs, or forwards a key.
//
// LEAF DISCIPLINE: pool imports only the model seams (model, provider, meter,
// budget, strongcap) — never the orchestrator (super/agent/project/swarm). It is
// imported BY cmd, swarm, and onboard (for the config type), never the reverse.
package pool

import (
	"context"
	"fmt"
	"sync"

	"nilcore/internal/budget"
	"nilcore/internal/meter"
	"nilcore/internal/model"
	"nilcore/internal/provider"
	"nilcore/internal/strongcap"
)

// defaultWorkerSpec is the "today" cheap-worker spec used when a PoolConfig (or
// just its worker tier) is zero. It keeps a zero-value config behaving exactly
// like the pre-swarm single-provider wiring.
const defaultWorkerSpec = "anthropic:claude-haiku-4-5"

// resolveFunc is the seam provider.ResolveWith satisfies: (spec, getenv) →
// provider. It is a package var so tests can inject scripted fake providers
// without a real vendor adapter or any network. Production wires it to
// provider.ResolveWith in Build's default (see newResolver).
type resolveFunc func(spec string, getenv func(string) string) (model.Provider, error)

// capFunc wraps an inner provider in a concurrency cap of `max`. It is a package
// var for the same reason as resolveFunc: tests substitute a counting limiter to
// assert the peak-in-flight bound without depending on strongcap's internals. In
// production it is strongcap.New.
type capFunc func(inner model.Provider, max int) model.Provider

// resilientFunc builds the failover/retry/breaker decorator over an ordered
// provider list. Injectable for the same test reason; production is
// model.NewResilient.
type resilientFunc func(providers []model.Provider, opts model.Options) (model.Provider, error)

// Options carries the cross-cutting hooks the pool threads into every metered
// call. It is distinct from model.Options (which tunes resilience) — these are
// the observability + pricing seams the scoreboard wires. The zero value is
// valid: a nil OnUsage is a no-op and a nil Pricer falls back to meter.NewTable.
type Options struct {
	// OnUsage, if set, is folded into EVERY tier's meter.Provider so the
	// scoreboard sees per-model (in,out) token counts on every model call across
	// the whole fleet. Must be safe for concurrent use (the fan-out calls it from
	// many goroutines).
	OnUsage func(modelID string, in, out int)
	// Pricer prices resp.Usage in dollars for the ledger's dollar ceiling. nil ⇒
	// meter.NewTable() (the conservative built-in table).
	Pricer meter.Pricer

	// resolve/cap/resilient are the injectable construction seams (nil ⇒
	// production defaults). They are unexported so the public surface stays the
	// three production seams; tests in this package set them directly.
	resolve   resolveFunc
	cap       capFunc
	resilient resilientFunc
}

// stack is the SHARED inner provider chain for one distinct spec identity:
// strongcap(Resilient([primary,fallback])) — everything BELOW the per-scope
// meter. Many tiers/shards point at the same *stack so they share one breaker
// and one concurrency cap. The meter is NOT part of stack: it is layered per
// scope on top (Planner/Verifier each get a fixed-scope meter; each WorkerFor
// gets a fresh per-shard meter), all wrapping this same inner.
type stack struct {
	inner model.Provider // strongcap(Resilient(...)) or just Resilient(...) when uncapped
}

// Pool is the composed, ready-to-call provider set for one swarm run. It is safe
// for concurrent use: every method either reads immutable construction state or
// delegates to the concurrency-safe ledger. Construct with Build.
type Pool struct {
	runID  string
	ledger *budget.Ledger
	pricer meter.Pricer
	onUse  func(modelID string, in, out int)

	planner  model.Provider // meter over the planner tier's shared stack (fixed scope)
	verifier model.Provider // meter over the verifier tier's shared stack (fixed scope)

	workerStack *stack // the SHARED cheap-worker inner stack WorkerFor wraps per shard

	// codeBackend records the worker tier's coding-backend selector so
	// CodeBackendFor can answer without re-deriving it.
	codeBackend string

	// ceilings mirrors the dollar ceilings the pool itself set (global under the
	// empty key, per-shard under the shard scope). The budget.Ledger does not
	// expose its ceilings, so the pool remembers what it declared in order to
	// compute Headroom as (ceiling - spent). Guarded by mu.
	mu         sync.RWMutex
	globalCeil float64
	shardCeil  map[string]float64
}

// scopeRoot is the run-scoped budget namespace prefix every scope shares, so one
// run's spend is isolated from another's in the shared ledger.
func (p *Pool) scopeRoot() string { return "swarm/" + p.runID + "/" }

// Scope returns the canonical ledger scope key for a shard id, the same key
// WorkerFor uses as the meter's Task and the same key SetShardCeiling /
// Spent / Headroom address. Centralizing it here keeps the "swarm/<runID>/<id>"
// shape in exactly one place.
func (p *Pool) Scope(shardID string) string { return p.scopeRoot() + shardID }

// Build composes the pool from a validated config. It resolves every distinct
// spec exactly once (sharing the resulting stack across tiers that name it),
// wires the strong tiers' fixed-scope meters, and stages the shared worker stack
// WorkerFor wraps per shard.
//
// cred is the by-NAME key resolver handed straight to provider.ResolveWith — the
// pool never inspects it (invariant I3). runID namespaces every budget scope.
// opts carries the OnUsage/Pricer hooks (and, in tests, the construction seams).
//
// A zero PoolConfig yields the "today" wiring: all three tiers resolve to the
// default cheap worker, uncapped, no breaker.
func Build(cfg PoolConfig, ledger *budget.Ledger, cred func(string) string, runID string, opts Options) (*Pool, error) {
	if ledger == nil {
		return nil, fmt.Errorf("pool: nil ledger")
	}
	// Resolve construction seams: production defaults unless a test injected one.
	resolve := opts.resolve
	if resolve == nil {
		resolve = newResolver()
	}
	capWith := opts.cap
	if capWith == nil {
		capWith = newCapper()
	}
	buildResilient := opts.resilient
	if buildResilient == nil {
		buildResilient = newResilientFunc()
	}
	pricer := opts.Pricer
	if pricer == nil {
		pricer = meter.NewTable()
	}

	jitter, err := parseDur(cfg.Jitter)
	if err != nil {
		return nil, fmt.Errorf("pool: %w", err)
	}
	callTimeout, err := parseDur(cfg.CallTimeout)
	if err != nil {
		return nil, fmt.Errorf("pool: %w", err)
	}
	mopts := model.Options{
		Jitter:           jitter,
		CallTimeout:      callTimeout,
		BreakerThreshold: cfg.Breaker,
		// MaxRetries is intentionally left at the Resilient default (0 additional
		// attempts) unless a future config field exposes it; the swarm leans on
		// FAILOVER + the requeue controller, not deep per-call retry, so the
		// large Jitter + fallback path is the primary resilience tactic.
	}

	p := &Pool{
		runID:     runID,
		ledger:    ledger,
		pricer:    pricer,
		onUse:     opts.OnUsage,
		shardCeil: map[string]float64{},
	}

	// builder shares one stack per distinct spec identity. The cache key is the
	// full spec identity (primary, fallback, effective cap) so two tiers that
	// name the SAME provider:model+fallback share ONE Resilient (one breaker) and
	// ONE strongcap, while a tier with a different fallback or cap gets its own.
	cache := map[string]*stack{}
	build := func(t TierSpec, fallbackDefault string) (*stack, error) {
		primary := t.Spec
		if primary == "" {
			primary = fallbackDefault
		}
		// Key the cap lookup on the RESOLVED primary — the SAME key the stack
		// cache uses below — not on the raw t.Spec. An inherited-spec strong tier
		// (empty t.Spec, the default `nilcore swarm` path) would otherwise look up
		// caps[""] (uncapped) while the worker tier looks up caps[workerSpec]
		// (capped); the two would compute different effective caps, land under
		// different cache keys, and the strong tier would get a SECOND, uncapped
		// stack — splitting the shared per-provider concurrency-cap/breaker.
		eff := effectiveCap(primary, t.Cap, cfg.Caps)
		key := primary + "\x00" + t.Fallback + "\x00" + fmt.Sprint(eff)
		if s, ok := cache[key]; ok {
			return s, nil
		}
		s, err := buildStack(primary, t.Fallback, eff, mopts, resolve, cred, capWith, buildResilient)
		if err != nil {
			return nil, err
		}
		cache[key] = s
		return s, nil
	}

	// The worker tier is built first because its EFFECTIVE primary spec is the
	// fallback default for an unset strong tier: a config that names only a worker
	// collapses all three tiers onto that one provider (the faithful "today /
	// single cheap-worker" wiring), and a fully zero config collapses onto
	// defaultWorkerSpec. Resolving the worker first lets planner/verifier inherit
	// it via the cache so the shared stack — and its breaker — is genuinely shared.
	workerSpec := cfg.Worker.Spec
	if workerSpec == "" {
		workerSpec = defaultWorkerSpec
	}
	workerStack, err := build(cfg.Worker, defaultWorkerSpec)
	if err != nil {
		return nil, fmt.Errorf("pool: worker tier: %w", err)
	}
	plannerStack, err := build(cfg.Planner, workerSpec)
	if err != nil {
		return nil, fmt.Errorf("pool: planner tier: %w", err)
	}
	verifierStack, err := build(cfg.Verifier, workerSpec)
	if err != nil {
		return nil, fmt.Errorf("pool: verifier tier: %w", err)
	}

	p.planner = p.meterFor(plannerStack, p.scopeRoot()+"planner")
	p.verifier = p.meterFor(verifierStack, p.scopeRoot()+"verifier")
	p.workerStack = workerStack
	p.codeBackend = cfg.Worker.CodeBackend
	return p, nil
}

// meterFor wraps a shared stack in a meter.Provider bound to a FIXED budget
// scope (used for the planner and verifier tiers, whose scope never changes).
// Per-shard workers do NOT go through here — they get a fresh meter each call so
// every shard's spend lands under its own Task key (see WorkerFor).
func (p *Pool) meterFor(s *stack, scope string) model.Provider {
	return &meter.Provider{
		Inner:   s.inner,
		Ledger:  p.ledger,
		Task:    scope,
		Price:   p.pricer,
		OnUsage: p.onUse,
	}
}

// Planner returns the strong-tier provider scoped to "swarm/<runID>/planner". It
// is the coordination channel's decomposition voice; its spend rolls into the
// planner scope and the global total.
func (p *Pool) Planner() model.Provider { return p.planner }

// Verifier returns the strong-tier provider scoped to "swarm/<runID>/verifier".
// This is the model side of evidence/judgement; it is NOT the I2 authority (the
// verify.Verifier is) — it is only the strong model used where a verifier needs
// to reason. Its spend rolls into the verifier scope and the global total.
func (p *Pool) Verifier() model.Provider { return p.verifier }

// WorkerFor returns a FRESH, stateless meter.Provider bound to this shard's
// scope, wrapping the SHARED cheap-worker stack. "Fresh" matters: each shard must
// charge its OWN ledger Task key (so a per-shard ceiling is enforceable and the
// scoreboard can attribute spend per shard), while "shared stack" matters: every
// shard's calls go through the SAME strongcap + Resilient, so the per-provider
// concurrency cap and circuit breaker are process-wide across the whole fan-out,
// not per shard. The two facts combine: independent Spent, one shared Inner, all
// rolling into the one global Total.
func (p *Pool) WorkerFor(shardID string) model.Provider {
	return &meter.Provider{
		Inner:   p.workerStack.inner,
		Ledger:  p.ledger,
		Task:    p.Scope(shardID),
		Price:   p.pricer,
		OnUsage: p.onUse,
	}
}

// CodeBackendFor selects the coding-shard delegation backend for a role. The
// pool only holds the worker tier's selector (the fan-out tier that actually runs
// coding shards); other roles default to native. A "" or "native" selector means
// the in-process native loop; "codex"/"claude-code" name a delegated CLI that
// cmd's buildBackend constructs IN-BOX (invariant I4) — the pool references it by
// NAME only and never holds the CLI.
func (p *Pool) CodeBackendFor(role string) string {
	if p.codeBackend == "" {
		return "native"
	}
	return p.codeBackend
}

// SetShardCeiling caps total dollars chargeable to a shard's scope, delegating to
// the ledger's per-task ceiling. A non-positive value removes the cap. The pool
// also records the ceiling so Headroom can report remaining dollars for the
// scope.
func (p *Pool) SetShardCeiling(shardID string, dollars float64) {
	scope := p.Scope(shardID)
	p.ledger.SetTaskCeiling(scope, dollars)
	p.mu.Lock()
	defer p.mu.Unlock()
	if dollars <= 0 {
		delete(p.shardCeil, scope)
		return
	}
	p.shardCeil[scope] = dollars
}

// SetGlobalCeiling caps total dollars across the whole run, delegating to the
// ledger's global ceiling, and records it for Headroom. A non-positive value
// removes the cap.
func (p *Pool) SetGlobalCeiling(dollars float64) {
	p.ledger.SetGlobalCeiling(dollars)
	p.mu.Lock()
	defer p.mu.Unlock()
	if dollars <= 0 {
		p.globalCeil = 0
		return
	}
	p.globalCeil = dollars
}

// Usage returns the run-wide token and dollar totals from the shared ledger — the
// read side of the metered pool, the natural pair of the SetGlobalCeiling write side.
func (p *Pool) Usage() (tokens int, dollars float64) { return p.ledger.Total() }

// Spent returns the tokens and dollars charged against a single scope key — the
// per-scope read side that mirrors SetShardCeiling's per-scope cap. Pass a scope as
// returned by Scope(shardID), or the literal planner/verifier scopes. An unknown
// scope reports zero.
func (p *Pool) Spent(scope string) (tokens int, dollars float64) { return p.ledger.Spent(scope) }

// Headroom reports the remaining dollars before `scope` (or, if smaller, the
// global total) hits a declared ceiling. It is the budget probe the controller
// runs before each pass: a non-positive shard headroom means that shard is out of
// budget; a non-positive global headroom means the whole run must stop.
//
// It honors ctx (a cancelled ctx yields a zero headroom + the ctx error — treat
// "stop" as the fail-closed default). When no ceiling was declared for the scope
// AND no global ceiling exists, headroom is +inf-equivalent (math.MaxFloat64),
// reported as "effectively unlimited" with a nil error so the caller does not
// gate. Headroom is computed from the ceilings the pool itself declared (the
// ledger does not expose its ceilings) minus current spend, so it is exact for
// every ceiling set through this pool.
func (p *Pool) Headroom(ctx context.Context, scope string) (float64, error) {
	if err := ctx.Err(); err != nil {
		// Fail-closed: a cancelled run has no headroom.
		return 0, err
	}
	p.mu.RLock()
	gceil := p.globalCeil
	sceil, hasShard := p.shardCeil[scope]
	p.mu.RUnlock()

	const unlimited = 1.797693134862315708145274237317043567981e+308 // math.MaxFloat64
	head := unlimited

	if hasShard {
		_, spent := p.ledger.Spent(scope)
		if h := sceil - spent; h < head {
			head = h
		}
	}
	if gceil > 0 {
		_, gspent := p.ledger.Total()
		if h := gceil - gspent; h < head {
			head = h
		}
	}
	return head, nil
}

// --- production construction seams -----------------------------------------
//
// These adapt the real provider/strongcap/model constructors to the injectable
// function types above. They live here (not as package vars) so the production
// wiring is explicit and testable: Build pulls them in only when a test did not
// inject a substitute.

// newResolver returns the production spec→provider resolver: provider.ResolveWith
// with the caller's by-NAME key lookup. The pool never inspects the key.
func newResolver() resolveFunc {
	return func(spec string, getenv func(string) string) (model.Provider, error) {
		return provider.ResolveWith(spec, getenv)
	}
}

// newCapper returns the production concurrency limiter: strongcap.New. It returns
// the *strongcap.Provider as a model.Provider (strongcap satisfies the seam).
func newCapper() capFunc {
	return func(inner model.Provider, max int) model.Provider {
		return strongcap.New(inner, max)
	}
}

// newResilientFunc returns the production failover decorator: model.NewResilient,
// adapted to return a model.Provider (the *Resilient satisfies it).
func newResilientFunc() resilientFunc {
	return func(providers []model.Provider, opts model.Options) (model.Provider, error) {
		return model.NewResilient(providers, opts)
	}
}

// effectiveCap is the concurrency cap that applies to a tier's stack: the larger
// of the tier's own Cap (tierCap) and any per-spec Caps entry keyed by the
// RESOLVED primary spec. The lookup MUST use the resolved primary (the same key
// the stack cache uses) — an inherited tier with an empty raw Spec resolves to
// the worker/default spec, and it must share that spec's single capped stack, not
// fork an uncapped one. "Larger" is deliberate — a per-spec Caps entry and a tier
// Cap are two operators expressing a bound; we honor the more permissive of the
// two declared bounds, so neither silently tightens the other. 0 ⇒ uncapped (no
// strongcap layer).
func effectiveCap(primary string, tierCap int, caps map[string]int) int {
	eff := tierCap
	if caps != nil {
		if c, ok := caps[primary]; ok && c > eff {
			eff = c
		}
	}
	if eff < 0 {
		eff = 0
	}
	return eff
}

// buildStack constructs the SHARED inner provider chain for one distinct spec
// identity, in the load-bearing nesting order described at the top of the file:
//
//	strongcap(  (only when cap > 0)
//	  Resilient(  (always — even a single provider gets the retry/breaker wrap)
//	    [ resolve(primary), resolve(fallback)? ] ))
//
// The meter is layered ABOVE this per scope by the caller, never here, so one
// shared stack can carry many independent meter scopes. resolve, cap, and
// buildResilient are the (possibly test-injected) construction seams.
func buildStack(
	primary, fallback string,
	cap int,
	mopts model.Options,
	resolve resolveFunc,
	cred func(string) string,
	capWith capFunc,
	buildResilient resilientFunc,
) (*stack, error) {
	prim, err := resolve(primary, cred)
	if err != nil {
		return nil, fmt.Errorf("resolve primary %q: %w", primary, err)
	}
	providers := []model.Provider{prim}
	if fallback != "" {
		fb, err := resolve(fallback, cred)
		if err != nil {
			return nil, fmt.Errorf("resolve fallback %q: %w", fallback, err)
		}
		providers = append(providers, fb)
	}
	res, err := buildResilient(providers, mopts)
	if err != nil {
		return nil, fmt.Errorf("build resilient: %w", err)
	}
	inner := res
	if cap > 0 {
		// One shared strongcap per distinct spec identity: every tier/shard
		// pointing at this stack shares the same semaphore, so the cap is
		// process-wide, not per-tier.
		inner = capWith(res, cap)
	}
	return &stack{inner: inner}, nil
}
