# Concurrency — full multi-agent parallelism

**Read order:** `CLAUDE.md` → `docs/ARCHITECTURE.md` → `docs/MULTI-AGENT.md` → this file.

How NilCore goes from its shipped **serial** multi-agent path to **full concurrency** without breaking any of the seven invariants. The model is **dynamic-wave async dispatch**: within one supervisor turn, the independent subagents of a decomposition run concurrently through `spawn.DAGScheduler` over the race-tested `scheduler.Scheduler` pool; the supervisor reasons **serially between waves**; the integrator stays **strictly serial and verified** so the integration tip is always green; and a dependent is cut from its dependency's branch.

This design was produced and then **adversarially reviewed** (five lenses: deadlock, race, advisor-concurrency, invariants, determinism). The review caught real holes in the first draft — they are folded in below and called out in §4.

**The non-negotiable contract: `-concurrency 1` is byte-identical to today's serial path.** Concurrency is opt-in, bounded, fully logged. The verifier stays the only authority on done (I2); every model-emitted command stays sandboxed (I4); the event log stays append-only and hash-chained (I5); no new module dependency is added (I6).

---

## 1. As-is (grounded)

The shipped build path runs every subagent **synchronously and serially**, and several correctness properties *depend on that serialism*.

### 1.1 The live spawn path is serial
- `internal/super/dispatch.go:130` — `res := s.Spawn(ctx, spec)` is a **blocking synchronous** call inside the per-`tool_use`-block dispatch switch. One worker runs to completion before the next block is processed.
- `internal/super/super.go` — `runState` (`handles`/`findings`/`spawned`/`branch`) is **lock-free**, "touched only by the single supervisor goroutine." Sound *only* under serial dispatch.
- `dispatch.go:72` — the **ID-uniqueness check** (`id already spawned`) lives inside the serial `doSpawn`. Worktree/branch isolation keys entirely on `spec.ID` (`branch = "task/"+spec.ID`, build.go:442).
- `dispatch.go:153-164` (`depTip`) — the dependent re-base seam handles only the **single-dependency** case (cut from the dep's passing `task/<id>` branch); 0 / multi / not-yet-passed deps fall back to base `HEAD`.

### 1.2 The concurrent machinery exists but is unwired
- `internal/spawn/dag.go` — **`DAGScheduler`**: wave-based Kahn release; the only concurrency is inside `runWave` via a fresh `scheduler.New(MaxConcurrent)` per wave; `collectReady`, `OnReady`, and the result fold are **single-goroutine between waves**. Four terminal states (Passed/Failed/Skipped/Cycle), provable termination. **Fully unit-tested, instantiated in no production path.**
- `internal/scheduler/scheduler.go` — race-tested bounded pool; `Start` threads `ctx` to every worker; a queued-but-unstarted task is **skipped** on cancel so `Wait` always drains. Tested with `-race`.
- `internal/spawn/spawn.go` — the flat `Spawner` is a *sibling* (drops `DependsOn`), with a live bug: the semaphore acquire (`spawn.go:79`) is an **unconditional blocking send with no `select` on `ctx.Done()`**. Test-only-reachable today; fix in §7.

### 1.3 Already concurrency-safe (verified)
- **eventlog** — `Log.Append` serializes link/hash/write under one mutex; `prev`/`seq` advance only after a confirmed write. Safe for concurrent producers.
- **budget** — `Ledger.Charge` is one check-and-record critical section; no two concurrent charges slip past a ceiling.
- **meter** — `Provider` is a stateless decorator (safe given its `Inner`/`Ledger`/`Price`).
- **model providers** — immutable config + shared `*http.Client`; `model.Resilient` per-breaker mutexes. Concurrent `Complete` is safe.
- **agent bus** — RWMutex over boxes/waiters; **no lock held across a blocking channel send** (`deliverReply` releases `b.mu` before the cap-1 send). The decoupled reply path is deadlock-free.
- **supervisor reader** — a **dedicated goroutine** answers `ask_supervisor`/`request_review`, *not* the parked supervisor goroutine. This is the load-bearing deadlock-freedom property.
- **integrator** — `Integrate` is a sequential read-tip → `merge --no-commit` → commit → verify → keep-or-`reset --hard`. **Not internally synchronized; MUST stay serial.** The maximal green prefix survives; a red combination never poisons the tip.

### 1.4 The advisor-executor wiring (the heart of this design)
- **Per-worker Advisor.** `roster/worker.go:103-106` — each worker constructs a **fresh `*advisor.Advisor`** (own consult ceiling, `advisorMaxCalls=4`). The ceiling counter (`advisor.go:46 a.calls++`) is **non-atomic** — safe *only* because each worker owns its own Advisor.
- **EscalateAfter is per-worker** (`native.go:250,626-635`): auto-consult after K consecutive verify failures, reset to 0. A shared broken dependency can trip all N workers' auto-consult near-simultaneously — the **correlated herd**.
- **Two no-hang escalation channels:** (1) `ask_advisor` runs in the **worker's own goroutine** → cannot deadlock against a parked supervisor; on ceiling/error returns a "proceed with your best judgment" string. (2) `ask_supervisor` → bus → the **dedicated reader** → `Answer` hook, 30 s timeout + graceful fallback. A stuck executor always reaches advice.
- **Grounded answer (the `ask_supervisor` reply is plan-aware).** The reader's `Answer` path was a context-free one-shot; it now reads a **mutex-guarded `RunContext` snapshot** (goal + plan digest + live cohort state + integration tip) that the supervisor **publishes single-owner** at the points it already mutates `runState` (Run start, `doPlan`, every spawn/fold, `doIntegrate`) and the reader **loads by value** (`loadRunContext`). Same single-producer/single-consumer mutex hand-off as `reader.findings`, roles inverted — so the reply is grounded in the supervisor's own plan and what the cohort actually produced, with **no new race** (the reader never touches `runState`/`msgs`), **no new round-trip**, and **byte-identical** when nothing is published. The TRUSTED preamble carries only harness-derived state (goal, plan digest, cohort id·role·status·branch, tip, and a bounded **file-tree of the integrated tip**); each subagent's **untrusted work-report prose stays in a separate `guard.Wrap`-fenced block** (I7 — never laundered into the trusted preamble). **Live repo reads are now wired:** the supervisor holds a `ReadTools`/`ReadDir` registry over a long-lived **read worktree re-pointed at the integration tip** (`RefreshRead`, single-owner on the main loop at every tip change, under `gitMu`), so it reads/searches the *current integrated tree* — file CONTENTS, not just diffs — when it reasons, and the reader folds that tree's file-LIST into the grounded answer (the reader still never does live I/O — the file-tree is captured single-owner and read from the by-value snapshot). Honest boundary: the supervisor counterchecks the **integration tree** (merged+verified branches) and the plan — never the asking subagent's in-progress *private* worktree (its own sandbox).
- **The strong provider is shared** across the five roles and the `Answer` hook (build.go:258 metered). **But** the project-loop *reflect* advisor and the greenfield *bootstrap* advisor are built from **raw unmetered `d.strong`** (build.go:329/795) — a budget-escape, §7.

---

## 2. The design — dynamic-wave async dispatch

```
SUPERVISOR  (serial reasoning, between waves)
   │  one turn emits:  spawn(t1)  spawn(t2)  spawn(t3 depends_on:[t1])   ── a wave
   ▼
PRE-WAVE VALIDATE (single-goroutine): ID-uniqueness + role/depth/fanout rails   ← was inline in serial doSpawn
   ▼
spawn.DAGScheduler  +  scheduler pool (cap = -concurrency, process-global)
   │  t1 ─┐                         independent nodes run concurrently;
   │  t2 ─┘ parallel                a dependent is released only when its
   │  t3 released when t1 Passes ─► deps Passed; OnReady cuts it from the dep branch
   ▼
results folded into runState  (single-owner, between waves — never worker→runState)
   ▼
SUPERVISOR sees all tool_results (one per tool_use, ID-keyed), reasons, calls `integrate`
   ▼
INTEGRATOR  (serial, verified)  ──►  tip is always verifier-green
```

- **Within a turn, the wave runs concurrently; between turns the supervisor reasons serially.** Independent siblings parallelize (the common, valuable case); dependent chains stay serial (correct) via the DAG edges + branch-cut re-base.
- **`runState` stays single-owner:** the DAGScheduler folds per-node results between waves on one goroutine; a worker never mutates `runState`. Enforced **by test** (the keystone), not convention.
- **Integration stays serial + supervisor-orchestrated** (the `integrate` tool is unchanged) → I2 "tip always green" is untouched.
- **Pre-wave validation** moves the ID-uniqueness + role/depth/fanout rails out of the serial `doSpawn` into a single-goroutine pass that runs **before** the concurrent wave dispatches — so two workers can never collide on `task/<id>`.

---

## 3. Advisor-executor under concurrency (corrected)

The hard requirement: **a stuck executor must ALWAYS be able to reach the advisor.** The first draft proposed one semaphore on the shared `strong` provider — the review proved that **starves the coordination channel** (it would throttle the reader's `ask_supervisor` answers and the supervisor's own turns alongside the worker herd). Corrected model:

1. **Per-worker `Advisor`, never shared** (own ceiling; race-free because unshared). Unchanged.
2. **Every advisor path is metered.** Fix the budget-escape (§7): `advisorFor` must wrap the **metered** strong provider so reflect/bootstrap advisor spend cannot escape the dollar wall.
3. **The concurrency limiter goes on the WORKER `ask_advisor` path only** — the herd source — **never** the reader's `Answer` path or the supervisor's own `Model`. A ctx-honoring `model.Provider` limiter (small `sem chan struct{}`) wraps the provider **handed to roster workers**, leaving the reader + supervisor providers limiter-free (still metered, still budget-bounded). This smooths the `EscalateAfter` burst (provider rate-limit protection) **without** starving coordination.
4. **Sized below `MaxFanout`** (a small default, operator-configurable) — a cap equal to `MaxFanout` would admit the entire wave's herd and do nothing.
5. **Process-global, not per-wave** — peak concurrency is `driveGate × MaxFanout` across drives, so the worker limiter (and the sandbox cap) must be a process-level bound to actually cap host load and provider QPS.
6. **No-hang guarantee.** The limiter acquire is `select { case sem<-{}: case <-ctx.Done(): return ctx.Err() }`. On saturation it falls through to the **same graceful fallback** the ceiling path already produces ("proceed with your best judgment, or stop and ask the human"). With the limiter, the per-worker ceiling, and the dollar ledger all saturated, a stuck executor still gets guidance-to-self-judge — never blocks, never hangs.
7. **`ask_supervisor` stays independent** of the worker limiter, so the coordinator escalation channel is never head-of-line-blocked by an `ask_advisor` herd. (Optionally answer it from a small bounded pool rather than the single reader goroutine — §4.)
8. **No tree-wide call counter** (it would race `advisor.calls`). A global *cost* bound is the budget `Ledger`; a global *concurrency* bound is the limiter; a global *consult-count* bound, if ever wanted, is a distinct budget **scope** (never "supervisor", which would starve the supervisor's own turns).

---

## 4. Risk mitigations (from the adversarial review)

| Hazard | Mitigation |
|---|---|
| **Deadlock** — supervisor parked on a wave while a worker blocks on escalation | `ask_advisor` runs in the worker's own goroutine; `ask_supervisor` is answered by the dedicated reader, never the parked supervisor; bus never holds a lock across a send; all waits are ctx-bounded with graceful fallback. Worker limiter must **not** wrap the reader/supervisor path (§3). |
| **Advisor herd starves coordination** | limiter on worker `ask_advisor` only, sized `< MaxFanout`, process-global; reader/supervisor unthrottled. |
| **Multiplicative resource blow-up** (`driveGate × MaxFanout`) | one **process-global** sandbox + advisor limiter, not a per-wave cap. |
| **Branch/worktree collision** (ID keys isolation) | **pre-wave** ID-uniqueness + rails validation, single-goroutine, before dispatch. |
| **`runState` race** | single-owner fold between waves; worker→`runState` writes forbidden; enforced by a `-race` test. |
| **I2 / I7 by convention only** | property test: tip always verifier-green under N workers, red combo never poisons it; `guard.Wrap` on every worker report retained. |
| **Determinism / replay** | event *interleaving* is nondeterministic, but the hash chain is one linearizable order (audit holds); outcome is `f(passed branches, topo order)` not wall-clock. Documented carve-out for any byte-identical-log assumption in `inspect`/replay. |

---

## 5. Deployment phases

- **Flag-gated, default off:** `-concurrency N` (default `1` = serial = **byte-identical**; clamp ≥1). At 1 the existing serial `doSpawn` path is taken unchanged — no new code path.
- **Phase 1 (P8-T01..04) — ✅ SHIPPED.** pre-wave validation (`checkSpawnRails` + `runSpawnWave`); `DAGScheduler` wired into `dispatch()` (`dispatchConcurrent`) for in-turn concurrency, gated on `-concurrency`; the ctx-honoring process-global worker advisor limiter (`internal/strongcap`, `< MaxFanout`, worker `ask_advisor` path only); the unmetered-advisor budget-escape and the `Spawner` ctx bug fixed (P8-T01). Concurrent shared-repo git ops serialized (`gitMu`); integration unchanged. Proven by the §6 gates as `-race` property tests in `internal/super` / `internal/strongcap`. *Bulk of the value, lowest risk.*
- **Phase 2 (P8-T05) — ✅ SHIPPED.** Multi-dependency re-base: a dependent with ≥2 passed deps is cut from a **throwaway, unverified merged tip** of all its deps' verified branches, so it codes on the UNION of its deps' work (not just base HEAD). The supervisor resolves the dep branches single-owner (`resolveBaseRefs`, both serial + concurrent) into the harness-only `SubagentSpec.BaseRefs`; the wiring seam (`mergedBaseTip` in build.go) octopus-merges them under `gitMu` into a `rebase/<id>` branch via the hardened `worktree.Merge` primitive (I4), and cuts the worker from it. **Any conflict or git fault degrades to base HEAD — the spawn never fails.** The tip is re-base convenience ONLY; the serial Integrator stays the sole *verified* merge path (I2). `rebase/` branches are swept at run end. (Implemented at the SpawnFunc seam, not `OnReady` — it covers BOTH the serial and concurrent spawn paths in one place and touches no frozen `spawn/` primitive.)
- **Phase 3 — ❌ DROPPED (descoped).** Pipelined waves (plan wave N+1 while wave N runs) cannot actually run ahead: the supervisor plans each wave *from* the prior wave's folded results (runState is single-owner, folded between waves), so there is nothing to compute speculatively, and a red dependency forces a re-plan that would invalidate any speculative wave. The valuable parallelism — independent siblings of one decomposition running concurrently — already shipped in Phase 1. The between-wave re-plan-on-red path also already exists (a failed node Skips its dependents; the supervisor reasons over the fenced results next turn). Phase 3 would add large machinery (speculative planning, wave cancellation/rollback) for ~no marginal throughput while pressuring the single-owner runState invariant — not worth it unless a future, measured throughput need justifies reopening it.
- **Streaming integration — ❌ EVALUATED & DEFERRED.** The proposed sound reframe of Phase 3 — fold each green branch into the integration tip *the moment it passes*, concurrent with still-running wave workers, to hide the integrator's per-merge verify latency. A 3-angle design pass confirmed it is **mechanism-sound** (the integrator only ever merges completed+verified branches, stays serial-in-itself, advances only on green → I2 "tip always green" is genuinely preserved; the invariant-safe shape is a dedicated integrator goroutine owning a *private* tip, handed back to the supervisor by value at a per-wave barrier so `runState` stays single-owner) — but **deferred**, for three structural reasons grounded in the code: **(1)** that barrier reintroduces the exact serialization streaming meant to remove (the supervisor can't reason until the tip folds back single-owner), so the only latency hidden is integrator-verify-time that fits inside the *same* wave's tail — ~zero for the common balanced-sibling wave; **(2)** the "dependent re-bases off a more-integrated tip sooner" win is **structurally absent today** — dependents are cut from their deps' `task/<id>` verified branches (`resolveBaseRef`) or the `mergedBaseTip` union (Phase 2), **never** from `st.branch`/the integration tip, so advancing the tip earlier buys nothing without also re-pointing worker base-cuts at the streaming tip (a far larger new race surface); **(3)** `gitMu` contention flips from **zero** today (the integrator runs between waves when no worker holds `gitMu`) to live mid-wave contention against N workers' worktree ops + every `RefreshRead` checkout. Net: a performance-only change adding a third long-lived goroutine + a new `-race` property-test burden, for a near-zero win in the common case (integration is rarely the bottleneck — a worker's model loop dominates), removing the model's `integrate`-tool control for no correctness gain. Same trade we correctly rejected for pipelining. If a *measured* throughput need ever forces it, the invariant-safe shape + the required `-race` gates are recorded in the design transcript; the cheaper, ethos-aligned first move would be a serial *opportunistic* drain (the supervisor folds ready branches between its own model turns — single-owner, zero new `gitMu` contention) rather than a true concurrent integrator.

## 6. Verification gates (the proof)

- `go test -race` across `super`/`spawn`/`integrate`/`scheduler`/`advisor`/`meter`/`budget` (in CI).
- **Property test:** under N concurrent workers the integration tip is **always** verifier-green and a red combination never poisons it; the maximal-green prefix survives.
- **Advisor-under-concurrency test:** a correlated `EscalateAfter` herd never hangs and never starves `ask_supervisor`; the limiter degrades to the graceful fallback under saturation.
- **No-deadlock test:** a worker blocking on `ask_supervisor` *and* `ask_advisor` inside a concurrent wave still resolves.
- **Bound test:** peak concurrent sandboxes ≤ the process-global cap regardless of `driveGate × MaxFanout`.
- **Cancellation test:** a budget/deadline breach cancels all in-flight workers; `Wait` drains.
- **Byte-identical test:** `-concurrency 1` produces the same event log / branches / outcome as today.

## 7. Pre-existing bugs surfaced by the review (fix independent of concurrency)

- **Budget-escape (real, today):** the project-loop reflect advisor and greenfield-bootstrap advisor use raw unmetered `d.strong` (build.go:329/795 via `advisorFor`), bypassing the dollar wall. Route them through the metered provider.
- **`Spawner.Spawn` ctx-ignoring acquire** (spawn.go:79): make the semaphore acquire `select` on `ctx.Done()` and record a cancelled `Result` for the remainder. Test-only-reachable today, but a latent stall.

> Stale first-draft claims corrected: the `OnUsage` sink is already mutex-guarded and off the build fan-out path; the `Spawner` ctx bug is real but not on a wired path today.
