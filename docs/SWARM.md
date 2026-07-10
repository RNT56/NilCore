# Verified swarm mode — Phase 12 (self-hostable, default-off)

**Read order:** `CLAUDE.md` → `docs/ARCHITECTURE.md` → `docs/CONCURRENCY.md` → `docs/MULTI-AGENT.md` → this file.

The implementation-ready plan for **verified swarm mode**: a first-class high-throughput
surface that fans **N units of work into a bounded in-process pool on one host**, where every
unit produces a **typed, checkable artifact** judged by a **verifier per unit**, requeues
**only failed shards** until clean (or a budget / pass limit), folds verifier-green work
through the **serial Integrator** (which never lands to base), and renders a **verification
scoreboard**.

> **The product line.** NilCore does not make swarms trustworthy by asking more agents. It
> makes them trustworthy by **refusing to merge anything that cannot be verified.** Massive
> fan-out is allowed only because every unit has a typed output and a verifier — no majority
> vote, no "the model says it looks right." A worker wins **only** by producing a checkable
> artifact that passes its verify-pack.

This document is a **plan**, not yet shipped. It is structured so a fleet of parallel agents
can execute it under the `CLAUDE.md` §5 work-selection rule with zero collision. Promoting its
task specs into `docs/TASKS.md` (and the layer-map rows into `docs/ARCHITECTURE.md`) is itself
the final, serialized contract task (SW-T18).

---

## Table of contents

- [§0 Where this sits](#0-where-this-sits)
- [§1 The product surface](#1-the-product-surface)
- [§2 As-is: what already ships (the Phase-11 spine + the concurrency machinery)](#2-as-is-what-already-ships)
- [§3 The architecture (artifact-first)](#3-the-architecture-artifact-first)
- [§4 The five product properties → mechanism](#4-the-five-product-properties--mechanism)
- [§5 The artifact-verification layer (verify-packs)](#5-the-artifact-verification-layer-verify-packs)
- [§6 The provider pool](#6-the-provider-pool)
- [§7 Scoreboard, report & source–claim trace](#7-scoreboard-report--sourceclaim-trace)
- [§8 Presets & the CLI surface](#8-presets--the-cli-surface)
- [§9 Invariant & thesis ledger](#9-invariant--thesis-ledger)
- [§10 The task DAG (SW-T01 … SW-T18)](#10-the-task-dag)
- [§11 Pipelines & parallel-execution map](#11-pipelines--parallel-execution-map)
- [§12 Contract-file changes & docs impacts](#12-contract-file-changes--docs-impacts)
- [§13 Honest caveats & the future-EXT boundary](#13-honest-caveats--the-future-ext-boundary)
- [§14 Verification gates (the proof obligations)](#14-verification-gates)

---

## §0 Where this sits

**Upgrade #2 of 2.** Phase 11 (the *verifier-backed artifact factory*, `docs/ROADMAP-EVIDENCE-ARTIFACTS.md`)
built the **artifact-verification layer** — typed artifacts + a verifier per unit. This phase
(**#2**) builds **verified swarm mode** *on top of* that foundation. The prompt's explicit
ordering — "build the swarm only **after** the artifact-verification layer" — is satisfied by
construction: the foundation is ~80% already in the tree, and the remaining foundation tasks
(SW-T01…SW-T06) land **before** the first swarm-orchestration task (SW-T09).

**In-process, single-host, bounded.** `--agents 300 --concurrency 40` is a **bounded pool on one
host**, over the already-shipped `internal/scheduler` (race-tested bounded pool) and
`internal/spawn` (wave-DAG). It reuses the existing budget wall, sandbox, event log, and the
serial verified Integrator. **This is not** the gated `EXT-01` managed cloud fleet, **nor**
`EXT-05` enterprise control plane. It stays a self-hostable, stdlib-first, **default-off**
upgrade that reinforces the thesis — exactly like Phases 8 (concurrency), 9 (behavioral verify),
and 10 (steering/distribution). The precise line it does **not** cross is documented in §13.

**Default-off, byte-identical.** `swarm` is one new subcommand and `report` gains swarm-aware
extensions. No existing dispatch arm or shared helper changes behavior; no existing package
imports a new swarm leaf. `--concurrency 1` drives the same single-flight path. The default
`nilcore` binary is **byte-identical** when `swarm`/`report` are never invoked — proven, not
asserted (§14).

**Zero frozen-contract code change.** Every piece is a **new leaf package** or an **additive
seam**. `backend.CodingBackend` (I1), `go.mod` (I6), `Makefile`, and `internal/channel/channel.go`
are **untouched**. The only serialized surfaces are docs (SW-T18), the config schema (SW-T08),
the pack registry (SW-T05), and the cmd dispatch (SW-T17) — each held by exactly one task.

---

## §1 The product surface

```
nilcore swarm \
  --goal "research 100 EV companies" \
  --preset research \
  --agents 300 \
  --concurrency 40 \
  --artifact report+matrix \
  --verify-pack finance \
  --passes until-clean \
  --budget 500
```

Six **named presets** so an operator does not have to tune everything:

| Preset | What it swarms | Artifact kind | Verify-pack(s) | Fan-in |
|---|---|---|---|---|
| `research` | verifiable reports & matrices, source-backed claims | `research-dossier` | `web` + `finance` | collate |
| `code` | many implementation attempts; verifier picks green branches | `spec` | `software` + `code` | merge (Integrator) |
| `fix` | one shard per detected red test (`FailureSharder` runs `verify.Detect` once); merges the green fixes | `spec` | `software` + `code` | merge (Integrator) |
| `audit` | security / codebase / docs / API review, evidence-backed findings | `report` | `audit` + `web` | collate |
| `benchmark` | repeated runs, variance checks, perf claims verified by scripts | `benchmark` | `benchmark` | collate |
| `ui` | browser-flow shards: screenshots, console checks, visual assertions | `report` | `ui` | collate |

The **verification scoreboard** (live during the run; replayable afterwards with
`nilcore report <run>`):

```
swarm research · 100 EV companies · preset=research · pass 3
─────────────────────────────────────────────────────────────
  checked          100        passed            91
  failed             9        retry pass         7   (was red, now green)
  remaining          2        final clean      ✗  (2 unverifiable sources)
─────────────────────────────────────────────────────────────
  cost  $182.40 / $500.00     time  14m 22s     tokens  41.2M in · 3.8M out
  models  worker=anthropic:claude-haiku-4-5  verify=anthropic:claude-opus-4-8
─────────────────────────────────────────────────────────────
  source–claim trace: 488/490 claims source-resolved · 2 dead links → shards 37, 81
```

`--artifact report+matrix` requests **two run-level deliverables**: a rendered report
**and** a cross-shard **matrix** (rows = companies, columns = the claim/field union, cells =
`status[value]` + a numbered source footnote). The matrix is the verifiable, source-traced
table the research swarm exists to produce.

---

## §2 As-is: what already ships

The single most important fact for this plan: **the artifact-verification layer is not net-new.**
It is **Phase 11 "the spine," already in the working tree** (`feat/p11-verified-artifacts`). This
phase **reuses and extends** it — it never rebuilds it.

### 2.1 The shipped Phase-11 spine (reuse, do not rebuild)

| Package | What it gives the swarm |
|---|---|
| `internal/artifact` | the typed `Artifact{Kind, Claims[]}`, `Claim`, `Evidence{Value, SourceURL, RetrievedAt, ExtractionMethod, Verifier, Status}`, `Status` lifecycle (`pass`/`fail`/`stale`/`unverifiable`), `Green()` pure all-pass projection, canonical `Marshal`, and an on-disk `store` (`.nilcore/artifacts/<id>.json`). |
| `internal/artifact/evverify` | `ArtifactVerifier` — **already implements `verify.Verifier`** (`var _ verify.Verifier`). It re-runs each claim's check, **overwrites the worker's self-claimed `Status`**, atomically writes back, emits metadata-only events, and reports `Passed` iff every claim is `StatusPass`. **This is the per-shard I2 gate.** Plus `Registry`, `Default()`, `Select(names, reg)`. |
| `internal/artifact/packs/{web,software,finance,ui}` | four **shipped verify-packs** — real `RegisterAll(*Registry)` verifier-ids over sandboxed `box.Exec`. Finance has SEC-fact / FRED / market-quote checks; keyed checks inject `$NILCORE_FRED_KEY` / `$NILCORE_MARKET_KEY` **by name** via `box.ExecWithEnv` (no SDK, no key to the model). |
| `internal/requeue` | `Unit`/`Worklist`/`Scan(worktree, ledger)` over `.nilcore/artifacts/*.json` + a retry `Ledger` (`MaxAttempts`/`Bump`/`Exhausted`, `Marshal`/`UnmarshalLedger`) + `Resolve`/`ShouldContinue`. **This is requeue-only-failed-claims and the until-clean convergence test** (empty `Worklist` == clean). |
| `internal/report` | `ReplayReport(logPath)` → `ReportModel`/`CheckResult`/`ClaimRow`/`RetryAttempt`, an `eventlog.Verify` chain check, and `render` (text/md/html) + `WriteReport`. **This is the scoreboard projection over the append-only log.** `cmd/nilcore/report.go` already wires `nilcore report` (`main.go` dispatches `case "report"`). |
| `internal/spawn/artifact.go` | `spawn.Result` already carries a typed `ArtifactSummary` + `ClaimStatus`; `super` already renders artifact/claim control lines (`dispatch.go`). |

### 2.2 The shipped concurrency & routing machinery (reuse)

| Package | What the swarm reuses |
|---|---|
| `internal/spawn` (`dag.go`) | `DAGScheduler{MaxConcurrent, RunSub, OnReady}.Run(ctx, []Subtask) → map[string]Result` — the wave-DAG used for the `code` preset. `Subtask`, `Result{ID,Summary,Branch,Passed,State,Err}`, `RunFunc`. |
| `internal/scheduler` | the race-tested bounded pool (`New(n)`, `Submit`/`Start`/`Wait`-always-drains, ctx cancel skips queued) — the flat fan-out for non-code presets. |
| `internal/integrate` | `Integrator{BaseRepo, BaseRef, …}.Integrate(ctx, []MergeItem) → (tip, []MergeResult)` — serial read-tip → `merge --no-commit` → commit → **re-verify** → keep-or-`reset`. The maximal-green prefix survives; a red combo never poisons the tip. **Never lands to base.** |
| `internal/roster` | role workers as "configuration over the one loop"; `RoleResearcher`, `RoleTypedResearch`, `RoleImplementer`, … (the keyspace is open), `Profile{System, ReadOnly, …}`, `NewWorker`, `NewDefault(provider)`. **Note the gotcha:** `Role.ReadOnly()` is hardcoded `role != RoleImplementer`, so a *new* write-capable role must rely on `Profile.ReadOnly:false`, not the helper. |
| `internal/route` | `RaceN`/`Race` (best-of-N, the **verifier** selects the winner) and `Review` (cross-model review before the gate). |
| `internal/provider` + `internal/model` | `Provider{Complete, (Streamer.Stream)}`; `model.Resilient([]Provider, Options{Jitter, BreakerThreshold, CallTimeout, MaxRetries})` — an **ordered** failover list passed a single element today; `provider.ResolveWith(spec, cred)` resolves a key by name from the SecretStore. |
| `internal/meter` + `internal/budget` + `internal/strongcap` | `meter.Provider` (charges usage to a `budget.Ledger`, `OnUsage` telemetry, conservative `Pricer`); `budget.Ledger` (`SetGlobalCeiling`, `SetTaskCeiling`, `Total`, `ErrCeiling`); `strongcap` (ctx-honoring per-provider concurrency limiter). |
| `internal/store` | local pure-Go SQLite (`SetMaxOpenConns(1)` — one writer, the serialization point), `UpsertTask`, `TasksByStatus`. |
| `internal/sandbox`, `internal/worktree`, `internal/worktreefs` | per-shard isolation: a box + a disposable worktree; `worktreefs` host-side artifact I/O (`O_NOFOLLOW`, atomic temp+rename). |

**What is genuinely new** (the only code this phase writes): a stdlib schema/shape leaf, three
missing packs (`audit`/`benchmark`/`code`), a verify-pack assembler, a report source–claim-trace
projection, a provider **pool** leaf, the **swarm** runner (shard type + durable queue +
requeue-until-clean controller + scoreboard), preset bundles, and the cmd wiring. Every one is a
new leaf or additive seam.

---

## §3 The architecture (artifact-first)

The organizing principle: **a typed Artifact + a Verifier-per-artifact (a verify-pack) is the
foundational contract; shards, presets, the scoreboard, and the multi-pass loop are all *derived*
from "what artifact + which verify-pack."** This is the architecture the codebase already
committed to in Phase 11; the swarm is the high-throughput driver over it.

```
                          nilcore swarm --goal … --preset … --agents N --concurrency K --passes until-clean --budget D
                                                   │
                              ┌────────────────────┴───────────────────────┐
                              │  buildSwarm (cmd/nilcore/swarm.go)          │  composes the leaves; ONE budget.Ledger wall
                              └────────────────────┬───────────────────────┘
                                                   ▼
   preset.Resolve(name) ──► {Sharder, roster role+Profile, artifact.Kind, verify-pack names, tier labels, fan-in, flat|dag}
                                                   ▼
   Sharder (once)  ──►  N swarm.Shard  ──►  Queue.Enqueue  (store, swarm-* Status namespace, run-isolated by ID prefix)
                                                   │
   ┌───────────────────────────────────────────────┴─ multi-pass Controller (passes.go) ─────────────────────────────────┐
   │  each pass:                                                                                                          │
   │    Runner.RunPass(shards, flat|dag)                                                                                  │
   │      scheduler.New(K)  (flat)   /   spawn.DAGScheduler{MaxConcurrent:K}  (code)                                      │
   │        └─ shardFn (the I2 closure): provision worktree+box → worker writes artifact JSON                            │
   │             → evverify.ArtifactVerifier.Check  ◄── SOLE ship gate; overwrites self-claims; Passed iff all-pass      │
   │             → spawn.Result.Passed/Branch set ONLY on green                                                          │
   │    green code/commit shards ──► integrate.Integrator (serial, re-verify per merge, BaseRef = prior tip)             │
   │    requeue.Scan(worktree, ledger) ──► next Worklist     (requeue ONLY non-pass claims)                              │
   │    Board.Record / OnUsage / EmitSnapshot   (scoreboard, metadata-only events)                                       │
   │  loop until: empty Worklist (converged) | --passes N | budget wall | ctx                                            │
   └──────────────────────────────────────────────────────┬───────────────────────────────────────────────────────────┘
                                                           ▼
   final clean tip ──► optional route.Review ──► ONE gated policy.GateAction{PromoteToBase}  (human approver; nil ⇒ deny)
                                                           ▼
   Board.Snapshot + report.ReplaySwarmReport ──► scoreboard + report+matrix deliverables (.nilcore/reports/<run>.…)
```

**Two end-to-end flows:**

- **`research 100 EV companies`** — `PlanSharder` (or `--shard-file`) yields 100 `research-dossier`
  shards (one company each). Each worker (read-capable + web fetch) gathers sourced facts and
  writes an `Artifact` whose `Claim`s each carry `Evidence{Value, SourceURL, Verifier}`. The
  `finance`+`web` verify-pack re-resolves every source in-box and recomputes numeric claims;
  `Passed` iff all claims pass. Failed shards (dead link, claim mismatch) requeue **only their
  failed claims** for up to `--passes`. Non-code → **collate** fan-in (no git merge); the run-level
  deliverable is `report+matrix`.
- **`swarm code`** — `PlanSharder` yields a DAG of `spec` shards (one package/module each). Each
  worker (or a delegated Codex/Claude-Code backend, in-box) writes code + a `spec` artifact; the
  `software`+`code` pack runs typed claims **and** the autodetected build/test in-box. Green
  branches fold through the serial Integrator (best-of-N via `route.RaceN` where configured);
  red shards requeue. Fan-in = **merge**; the final tip is a `PromoteToBase` candidate.

---

## §4 The five product properties → mechanism

| # | Property | Mechanism (all reuse) |
|---|---|---|
| 1 | **Massive fan-out, verifier-owned quality** | Every shard's `shardFn` ends in `evverify.ArtifactVerifier.Check` (I2). It **overwrites** the worker's self-claimed `Status`, so a model can never self-certify; `Passed` requires `Green()` (all claims `StatusPass`). `ShipGate` refuses `verify.Pass{}`/nil. Unknown pack ⇒ fail-closed. **No majority-vote, no self-report path exists.** |
| 2 | **Shard-native task model** | `swarm.Shard{ID, Input, Kind, Pack, Role, Deps, Attempt, …}` maps DOWN to `spawn.Subtask`/`scheduler.Task`. Granularities: one company/source/package/benchmark/file-module/browser-flow → `ListSharder`/`PlanSharder`; **one test failure** → a `FailureSharder` (runs `verify.Detect` once, emits one shard per red test). `requeue.Scan` requeues **only** non-pass claims. |
| 3 | **Provider pool** | `internal/pool`: strong planner/verifier tier + cheap worker tier + per-tier **fallback** model (`model.Resilient([primary,fallback])`) + **per-provider concurrency caps** (`strongcap`) + a delegated-CLI selector (`CodeBackendFor` → `buildBackend` for Codex/Claude-Code coding shards). All config over shipped seams; **no new module, no standing authority.** |
| 4 | **Verification dashboard/report** | `internal/swarm/board` (live tally driven **strictly by the verifier verdict**) + `internal/report` swarm projection (`ReplaySwarmReport` → checked/passed/failed/retry-pass/remaining + final-clean + cost/time/token + **source–claim trace** + matrix). A pure read-only projection over the append-only log + persisted artifacts. |
| 5 | **Swarm presets** | `internal/swarm/preset`: named bundles (`research`/`code`/`fix`/`audit`/`benchmark`/`ui`), each binding {sharder, role+Profile, artifact Kind, verify-pack(s), tiers, egress, fan-in, flat\|dag}. `fix` selects `SharderFailure` (one shard per detected red test). `Resolve(name)` is **fail-closed** (unknown ⇒ refuse to start). |

---

## §5 The artifact-verification layer (verify-packs)

This is the foundation the prompt demands first. The shipped spine (`artifact` + `evverify` +
`packs/{web,software,finance,ui}` + `requeue` + `report`) already delivers most of it; this phase
closes the gaps so every preset has a real verify-pack (`fix` reuses the `code` preset's `software`+`code` packs).

### 5.1 The cheap-first composed verifier (per shard)

A `--verify-pack <name>` resolves (via `packs.Build`) to a `verify.Composite` run in cheapest-first
order, so a malformed artifact fails fast before any network reach:

```
verify.Composite[
  schema.SchemaVerifier{Reg, RelPath}     // Named[0]  structural: required fields, citations present,
                                          //           min claims, no dup ids, right Kind for the artifact
  evverify.ArtifactVerifier{Box, Reg, …}  // Named[1]  per-claim checks; OVERWRITES self-claimed Status (I2)
  <optional command/browser child>        // Named[2]  code→verify.Detect build/test in-box; ui→BrowserVerifier
]
```

- **Schema (`internal/artifact/schema`, SW-T01, new leaf).** A stdlib declarative `Schema{Kind,
  RequiredFields, CitationRequired, VerifierRequired, MinClaims}` + a pure `Validate` walk → closed
  `Defect` codes. A nil schema / unschematized Kind ⇒ `CodeWrongKind` (fail-closed). `SchemaVerifier`
  implements `verify.Verifier`, lives in this subtree (so `verify` gains no import — no cycle), reads
  the artifact via `worktreefs.OpenNoFollow`, reaches no network, and emits a metadata-only
  `schema_verify` event. **No JSON-schema module** (I6).
- **Per-claim (`evverify.ArtifactVerifier`, shipped).** The I2 gate, reused verbatim.
- **`packs.Build` (SW-T05).** `Build(name, box, relPath, schemaReg) → PackPlan{Verifier, Hosts}`.
  Unknown name ⇒ **error** (never `verify.Pass`, never a make-verify default — this **inverts**
  `verify.Detect`'s "unknown ⇒ true"). `box==nil` ⇒ networked claims resolve `Unverifiable` while
  schema/variance still run.

### 5.2 The five verify-packs

| Pack | Status | Checks |
|---|---|---|
| `web` | shipped | source resolves (URL reachable in-box), citation present, claim text on page. |
| `finance` | shipped | `sec_fact` / FRED / market-quote; keyed checks inject `$NILCORE_*_KEY` by name (I3). |
| `software` | shipped | typed code claims. |
| `ui` | shipped | browser flow + screenshot + console-clean + visual assertion (CI-only, fails closed). |
| **`audit`** | **SW-T02 (new)** | every finding cites a `file:line` that **reproduces against the local worktree** (`sed`/`grep` in-box, verbs are pack constants; a path-escape ⇒ `Unverifiable` with no box call). Pure functions of files on disk — hermetic. |
| **`benchmark`** | **SW-T03 (new)** | `benchmark.script_threshold` **re-runs the allowlisted bench K times in-box and the verifier itself computes the metric + CV from ITS OWN re-runs**, asserting `op bound` and `CV ≤ ceiling`. The verifier never trusts the worker's `samples[]` for the perf/variance claim (closes the I2-erosion the review flagged). Honest caveat baked into the spec + test: it verifies **claimed bounds + variance over re-runs**, never exact wall-clock reproduction. |
| **`code`** | **SW-T04 (new)** | `code.build_passes` / `code.test_passes` reuse `verify.Detect` to pick the build/test command and run it in-box; the assembler ANDs this with the typed `software` claims so **both** the typed artifact **and** the raw build gate. |

### 5.3 The two-layer I2 guarantee

1. **Per shard**, inside `shardFn`: the composed verifier is the **sole** decision; `Passed`/`Branch`
   are set **only** on a green report. `ShipGate` (SW-T09) refuses a nil or `verify.Pass{}` verifier,
   so no shard can ship on a vacuous gate.
2. **Per goal**, on integration: `integrate.Integrator` **re-verifies after every merge** with the
   same composite; a red combination is rolled back; the tip stays green.

Unknown pack / unschematized Kind / unregistered verifier-id ⇒ **fail-closed** at every layer.
The MCP-as-data-source path (an external typed finance source) is **out of scope** for v1 — the
packs are curl-in-box + stdlib only, so a verdict never depends on a standing external service
(§13). SW-T05's test asserts no pack's `Hosts()`/checks resolve to an MCP/hosted-index client.

---

## §6 The provider pool

`internal/pool` (SW-T07, new leaf) owns the swarm's tiered / capped / failover / metered providers
as **one composed unit** — pure composition over shipped seams, **no new module, no key ever to a
decorator or the model.**

```go
type TierSpec struct {
    Spec, Fallback string  // "provider:model" only — NEVER key material
    Cap            int     // per-provider concurrency cap (0 = uncapped)
    CodeBackend    string  // "" | "codex" | "claude-code"  (coding-shard delegation selector)
}
type PoolConfig struct {
    Planner, Verifier, Worker TierSpec
    Caps                      map[string]int  // provider → cap
    Jitter, CallTimeout       string          // large Jitter for a 300-agent fleet (avoid a 429 retry storm)
    Breaker                   int
}
```

`Build(cfg, ledger, cred, runID, opts) (*Pool, error)` composes **per tier** in the load-bearing
order — `meter.Provider` (outermost; charges once per logical call) → `strongcap` (only when cap>0;
one **shared** instance per distinct `provider:model`) → `model.NewResilient([primary, fallback],
model.Options{Jitter, BreakerThreshold, CallTimeout, MaxRetries})` (the `*Resilient` decorator) →
`provider.ResolveWith(spec, cred)` (the second arg is the env→SecretStore `getenv` resolver).
Identical specs **share** one `Resilient` (one breaker) and one `strongcap`.

- `Planner()` / `Verifier()` — scoped `swarm/<runID>/{planner,verifier}` (the **strong** tier).
- `WorkerFor(shardID)` — a fresh **stateless** `&meter.Provider{Task:"swarm/<runID>/<shardID>"}`
  over the **shared** cheap-worker stack, so per-shard spend rolls into the one global ledger.
- `CodeBackendFor(role) string` — selects native vs a delegated CLI; the shardFn **branches** on it
  (SW-T17): non-`native` ⇒ build via the existing `buildBackend(name, …, box, verifier, …)` **in-box**
  (I4), verified by the **same** per-shard `ArtifactVerifier` (`Result.SelfClaimed` stays advisory).
- `SetShardCeiling` → `ledger.SetTaskCeiling`; `Headroom`/`Usage`/`Spent` feed the scoreboard.

**Budget routing (shard-native).** `budget.ErrCeiling` is caught **at the shard boundary** and
`ClassifyCeiling` (SW-T09) distinguishes **per-shard** (that shard fails / maybe-requeues) from
**global** (stop the run) by a zero-token headroom probe of the shard key vs the global key.

`--budget D` → `SetGlobalCeiling(D)`; `--per-shard-budget` → `SetTaskCeiling`. The pool decorates
`model.Provider` only — the frozen backend contract is untouched. Delegated CLIs are referenced by
**name** (not held in the pool) for the existing `buildBackend`.

---

## §7 Scoreboard, report & source–claim trace

### 7.1 Live board (`internal/swarm/board`, SW-T14, new sub-leaf)

`Board` is an O(1)-per-update, concurrency-safe tally fed by the runner and read by a dashboard
goroutine. **`Record` is the only entry that moves passed/failed/retry-pass, and it is driven
strictly by the verifier verdict** (I2) — a `fail→pass` transition bumps `RetryPass`; `MarkClean`
is the green gate (empty worklist **and** chain ok). `OnUsage` folds `meter.OnUsage` (per-model
in/out tokens, priced by the conservative `Pricer`); `Cost` reads `Ledger.Total()` live. **Time:**
the runner stamps per-shard start/end (`MarkRunning`→`Record` elapsed) **and** total wall-clock
(`swarm_start`→`swarm_done`), so the `cost/time/token` line has a real time source. `Snapshot` is an
immutable copy-out. A `//go:build tui` Charm dashboard renders the same `Snapshot` (zero Charm in
the default binary). Keystone test: at run end `Board.Snapshot()` **equals**
`ReplaySwarmReport(...).Swarm` field-by-field (live == replay).

### 7.2 Report projection (`internal/report`, SW-T06, additive)

`SwarmReport{Base *ReportModel; Swarm SwarmDimension}` + `ClaimTrace`/`SourceRef`/`SchemaDefectRow`
projected in the **existing single** `ReplayReport` pass (no new file read). `ReplaySwarmReport(log,
worktreeRoot)` folds the swarm-only Kinds. A broken hash chain forces **both** `Base.FinalPass=false`
**and** `Swarm.FinalCleanPass=false` (`= ChainVerified && swarm_pass_clean present && Remaining==0`).

- **Source–claim trace:** each `ClaimTrace` ties a claim's **verdict** to its **key-free** `SourceRef`
  (`SourceURL` is trusted-as-provenance and key-free per I3). The board's `trace.go` projects **only
  trusted fields** (`Status`/`Verifier`/`SourceURL` — never the model-authored `Value`).
- **`RenderMatrix`:** rows = artifacts, columns = sorted claim/field union, cells = `status[redacted
  value]` + numbered source footnote. **Redacts** (I3) and **escapes** (I7); never a green cell over a
  non-pass status.
- **JSON deliverable:** the `json` format marshals a **redacted projection** (SourceURL through the
  same redaction the renderers use), **not** the raw `ReportModel` — so the json path can never leak
  an `api_key=` query param (I3) or an unescaped model `Value` (I7). Tested.

### 7.3 `nilcore report <run>` (`cmd/nilcore/report.go`, SW-T16, **extend the shipped file**)

`report` is **already wired** (the file exists; `main.go` dispatches `case "report"`). SW-T16
**extends** `reportMain`/`runReport` to call `ReplaySwarmReport` + `RenderMatrix` and to add the
`json`/`matrix` formats (`--format text|md|html|json|matrix`, `--dir <worktree>`, `[--out]`). The
swarm `--report` flag calls the **same** renderer, so live and replay share one path.

---

## §8 Presets & the CLI surface

### 8.1 Preset bundles (`internal/swarm/preset`, SW-T15)

```go
type Preset struct {
    Name        string
    Kind        artifact.Kind
    Role        roster.Role
    Profile     roster.Profile      // write roles MUST set ReadOnly:false (the Role.ReadOnly() gotcha)
    VerifyPacks []string
    Egress      []string            // = union of selected packs' HostsFor (derived, not hand-typed)
    FanIn       FanIn               // collate | merge
    Shape       Shape               // flat | dag
    Sharder     SharderKind         // list | plan | failure
    WorkerTier, PlannerTier string
}
```

The presets bind: `research`→{`research-dossier`, `RoleTypedResearch`, `web`+`finance`, collate,
flat}; `code`→{`spec`, `RoleImplementer`, `software`+`code`, merge, dag}; `fix`→{`spec`,
`RoleImplementer`, `software`+`code`, merge, flat, `SharderFailure`}; `audit`→{`report`,
`RoleAuditor` (**new**), `audit`+`web`, collate, flat}; `benchmark`→{`benchmark`, `RoleImplementer`,
`benchmark`, collate, flat}; `ui`→{`report`, `RoleUI` (**new**), `ui`, collate, flat}.

`Resolve(name)` is **fail-closed** (unknown ⇒ `ErrUnknownPack` → cmd FATALs before any work). The
returned registry contains **no** always-pass verifier. SW-T15 adds only `RoleAuditor`/`RoleUI` to the
open Role keyspace — `RoleResearcher` and `RoleTypedResearch` already exist; both write the artifact
file via `Profile.ReadOnly:false`. `preset` imports `swarm` **one-directionally** (`swarm` must never
import `preset` — the sharder carries `Kind`/`Pack`/`Role` as plain fields).

### 8.2 The `nilcore swarm` flag surface (SW-T17)

```
--goal STR             the swarm objective                        --preset NAME       research|code|fix|audit|benchmark|ui (default research)
--shard-file PATH      operator shard list (ListSharder)          --agents N          target shard count (PlanSharder budget)
--concurrency K        pool cap (default 1 = byte-identical)      --passes until-clean|N
--artifact LIST        '+'-joined deliverables: report+matrix|spec|benchmark|dossier|json
--verify-pack NAME     override the preset's pack(s)              --budget D          global ceiling (default 25.00)
--per-shard-budget D   per-shard ceiling                          --worker-model / --planner-model / --verify-model / --fallback-model  provider:model
--code-backend native|codex|claude-code                          --provider-cap K=V  per-provider concurrency cap
--jitter DUR (default 750ms)   --egress-allow host,…   --report text|md|html|json|matrix   --resume   --sandbox/--runtime/--image/--log/--config/--deadline
```

> _Features-review fix:_ `--egress-allow` (and each preset's derived egress) now actually reaches the shards — a per-shard allowlist proxy is stood up and applied to every shard box. Previously every shard ran `--network none`, so `--egress-allow` was inert and a web-needing preset (e.g. `research`) could never verify green. Relatedly, `--resume` now re-seeds skipped/queued/running shards (planned-DAG dependents were being dropped) and red budget-exhausted shards block a clean converge.

**`--artifact` consumer (SW-T17).** Parse the `+`-joined list into (a) the per-shard `artifact.Kind`
the shardFn enforces (overrides the preset default when given) and (b) the run-level **deliverable
set** the final renderer produces (text/md/html report **and/or** `RenderMatrix`). `matrix` always
triggers the cross-shard pivot regardless of per-shard Kind. Tested: `--artifact report+matrix`
yields **both** a rendered report and a rendered matrix (the headline command's contract).

**Exit code:** `0` iff the final `requeue.Worklist` is empty **and** the final integrate/collate tree
verifies **and** the event-log chain verifies; else `1` (scoreboard already printed).

**`buildSwarm` (`cmd/nilcore/swarm.go`, SW-T17)** composes, in order: `loadBoot` → `openLog` →
`budget.New()` + `SetGlobalCeiling` → `pool.Build` → `evverify.Default()` + `packs.Select` →
per-shard env factory (box + `ArtifactVerifier`; code/benchmark wrapped in `verify.Composite`) →
preset roster → the **shardFn I2 closure** (worktree+box, `worker.Run` *or* `buildBackend` for a
delegated coding shard, `Verifier.Check`, `Passed` only on green) → Integrator (code) / collate
(others) → `buildGateFunc` (never-land) → `swarm.Runner` + `Controller`. One shared `*budget.Ledger`.

---

## §9 Invariant & thesis ledger

The seven invariants hold **by reuse**, not by new mechanism. (Verified against the real code by an
adversarial thesis/EXT-drift review: the store is a local single-writer SQLite file; budget /
strongcap / scheduler / spawn have zero network code; `Resilient` failover is an in-process ordered
list; the egress proxy is a local in-process listener; integrate never pushes.)

| Invariant | How the swarm preserves it |
|---|---|
| **I1** frozen contract | The typed artifact is an on-disk worktree **file** (`.nilcore/artifacts/<id>.json`) read by the verifier — `backend.Task`/`Result`/`CodingBackend.Run` are **untouched**. Shard-extra data lives on `swarm.Shard` and maps **down** to `backend.Task`/`spawn.Subtask`. Any new per-shard terminal signal rides a **sentinel error** (the `backend.ErrSuspended` precedent), never a `Result` field. |
| **I2** verifier sole authority | Two layers: per-shard `evverify.ArtifactVerifier` (overwrites self-claims, all-pass `Green()`) and per-goal Integrator re-verify. `ShipGate` refuses `verify.Pass{}`/nil. Unknown pack/Kind ⇒ fail-closed (inverts `verify.Detect`). The benchmark pack re-measures variance itself (no trust of worker samples). **No self-report, no majority-vote path.** |
| **I3** no ambient authority | Keys resolve via `provider.ResolveWith` + the env→SecretStore `cred` resolver; meter/strongcap/Resilient wrap an already-constructed provider and never see a key; keyed pack checks inject by **name** via `box.ExecWithEnv`; `Evidence.SourceURL` is required key-free; events + the json/matrix deliverables are redacted. |
| **I4** sandboxed execution | Each shard gets its **own** `sandbox.Sandbox` bound to its **own** disposable worktree; no shard touches another's Dir. Pack checks run via `box.Exec`; a nil box fails network claims closed. Delegated Codex/Claude-Code run **in-box**. Host-side artifact I/O is `worktreefs` (`O_NOFOLLOW` + atomic temp+rename). |
| **I5** append-only audit | New swarm Kinds are free strings (no schema change), every Detail metadata-only, redacted on append; `Log.Err()` is polled each pass — a broken chain **HALTS** the run and shows RED. |
| **I6** zero-dep core | All new leaves are stdlib-only; `CGO_ENABLED=0`; **no `go.mod` change**. Schema is a Go struct + a stdlib walk (no JSON-schema lib). The Charm dashboard is `//go:build tui`-isolated. |
| **I7** untrusted-as-data | Every gathered source / sibling artifact is `guard.Wrap`'d before a prompt; only verifier-set `Status`/`Detail` are trusted; model-authored `Value`/`SourceURL` stay fenced; the scoreboard/trace/json project **only trusted fields** (`ProjectTrusted` enforces this at compile time). |
| **Never-land** | The Integrator returns a throwaway `integrate/<suffix>` tip; promote-to-base is exactly **one** gated `policy.GateAction{PromoteToBase}` through the human approver (nil approver default-denies). The swarm constructs no new `GateActionType` and never auto-lands. |

**EXT boundary (thesis).** `300@40` is a bounded in-process pool on **one** host over the existing
`scheduler`/`spawn.DAGScheduler` rails, with **local-process-restart** resume over the **local**
single-writer SQLite store (never a shared/networked DB; shard state is never read by a second host),
and **no standing authority**. The line it does **not** cross — multi-host shard dispatch /
cross-host task-state / a remote control plane — is `EXT-01` and is explicitly out of scope (§13). A
`deps_test`-style guard asserts `internal/swarm` imports no orchestrator (`agent`/`super`/`project`)
and no network/RPC/remote-DB package.

**Default-off, byte-identical (tested).** `main.go` gains one `case "swarm"` + one swarm usage line
(+ the missing `report` usage line, a one-line pre-existing omission); all logic is in new files.
SW-T17's verify proves it: (a) no existing package imports `internal/swarm*`/`internal/pool`; (b) the
new leaves contain **no `init()` with global side effects** (so merely linking can't change behavior);
(c) the default dispatch path (`nilcore` / `nilcore run`) reaches neither new arm — byte-identity is
established exactly as `-concurrency 1` and the `schedule`/`watch` cases are.

---

## §10 The task DAG

**Namespace `SW-T01 … SW-T18`.** One task = one branch (`task/SW-T0x`) = one PR. Owns sets are
disjoint (package dir = unit of ownership). The artifact-verification foundation (SW-T01…SW-T06)
lands **before** the swarm-orchestration tasks (SW-T09…SW-T17), per the prompt's explicit ordering.

| ID | Title | Depends on | Owns | Note |
|---|---|---|---|---|
| SW-T01 | Artifact schema/shape validation leaf | — | `internal/artifact/schema/` | new leaf |
| SW-T02 | `audit` verify-pack | SW-T01 | `internal/artifact/packs/audit/` | new leaf |
| SW-T03 | `benchmark` verify-pack | SW-T01 | `internal/artifact/packs/benchmark/` | new leaf |
| SW-T04 | `code` verify-pack | SW-T01 | `internal/artifact/packs/code/` | new leaf |
| SW-T05 | `packs.Build` assembler + `DefaultSchemas` + 3 registry entries | SW-T02, SW-T03, SW-T04 | `internal/artifact/packs/packs.go`, `internal/artifact/packs/build.go` | sole owner of `packs.go` |
| SW-T06 | report source/claim-trace projection (additive) | SW-T01 | `internal/report/` | additive; sole owner of `internal/report` for its duration |
| SW-T07 | Provider pool leaf | — | `internal/pool/` | new leaf |
| SW-T08 | `onboard.Config.Pool` field + Validate clause | SW-T07 | `internal/onboard/onboard.go` | **contract (config schema)** — serialized |
| SW-T09 | swarm `Shard` type + invariant guards | SW-T05 | `internal/swarm/` (whole package) | opens `internal/swarm` |
| SW-T10 | swarm durable Queue | SW-T09 | `internal/swarm/` (whole package) | serial after SW-T09 |
| SW-T11 | swarm Sharder (List/Plan/Failure) | SW-T10 | `internal/swarm/` (whole package) | serial after SW-T10 |
| SW-T12 | swarm Runner | SW-T11 | `internal/swarm/` (whole package) | serial after SW-T11 |
| SW-T13 | swarm multi-pass Controller | SW-T12 | `internal/swarm/` (whole package) | serial after SW-T12 |
| SW-T14 | swarm scoreboard board sub-leaf | SW-T06 | `internal/swarm/board/` | new sub-leaf; ∥ the T10–T13 chain |
| SW-T15 | swarm preset bundles + new roster roles | SW-T05, SW-T09, SW-T07 | `internal/swarm/preset/`, `internal/roster/roster.go`, `internal/roster/worker.go` | roster is a leaf (not frozen) |
| SW-T16 | extend `nilcore report` (swarm report/matrix/json) | SW-T06 | `cmd/nilcore/report.go` | **edit** the shipped file |
| SW-T17 | `nilcore swarm` + `buildSwarm` + dispatch wiring | SW-T13, SW-T14, SW-T15, SW-T08, SW-T16 | `cmd/nilcore/swarm.go`, `cmd/nilcore/main.go` | **serialized cmd-wiring** (one `case "swarm"`) |
| SW-T18 | Docs + CHANGELOG promotion | SW-T17 | `docs/TASKS.md`, `docs/ARCHITECTURE.md`, `docs/ROADMAP-EXTERNAL-INFRA.md`, `CLAUDE.md`, `CHANGELOG.md`, `README.md` | **contract (docs)** — serialized last |

> **Correction folded in (review):** `internal/swarm` is declared as the **whole package** the
> Owns unit for SW-T09…SW-T13 (not per-file), so the work-selection rule correctly forbids two of
> them open at once. SW-T02/T03/T04 depend on **SW-T01** (their `Schemas()` imports the schema
> package) so each is independently `make verify`-green in isolation. `nilcore report` is already
> wired, so SW-T16 **edits** the shipped `report.go` and SW-T17 adds only `case "swarm"`.

### Per-task specs

#### SW-T01 — Artifact schema/shape validation leaf
- **Goal:** a stdlib-only leaf asserting a typed `artifact.Artifact` has the right **structure** for
  its Kind (required fields, citations/verifier-ids when demanded, min claims, no dup claim ids,
  right Kind) **before** the per-claim network checks, adapted to `verify.Verifier` as the
  cheapest-first `Named[0]`. A malformed/under-populated artifact fails closed cheaply, reported
  distinctly from a claim failure.
- **Depends on:** — (reuses shipped `internal/artifact`, `internal/verify`, `internal/worktreefs`).
- **Owns:** `internal/artifact/schema/` (`schema.go`, `verifier.go`, `*_test.go`, `deps_test.go`).
- **Acceptance:** closed `Code` enum (`MissingField`/`EmptyValue`/`MissingCitation`/`MissingVerifier`/`DuplicateClaim`/`WrongKind`); `Defect{ClaimID,Field,Code,Reason}` with harness-authored `Reason` ≤256B that **never** echoes a model `Value`/`SourceURL` (I7); `Schema{Kind,RequiredFields,CitationRequired,VerifierRequired,MinClaims}` + pure deterministic `Validate(*artifact.Artifact) []Defect`; nil `*Schema` ⇒ `[CodeWrongKind]` (fail-closed); `Registry{Register,Lookup}` (miss ⇒ `(nil,false)`); `Default()` per-Kind shapes for `report`/`matrix`/`spec`/`benchmark`/`research-dossier`; `SchemaVerifier{Reg,RelPath}` implements `verify.Verifier` (compile-time `var _`), loads via `worktreefs.OpenNoFollow`, reaches no network, `Output` lists bounded `Code: Field — Reason` lines with **no** model field; emits a metadata-only `schema_verify` event via an injected `EventSink` (leaf never imports `eventlog`).
- **Verify:** `make verify`; `go test -race ./internal/artifact/schema/...`; golden Defect-ordering; nil-Schema + Lookup-miss fail-closed; missing/corrupt/clean `SchemaVerifier`; assert no `Value` in `Output`; `deps_test.go` runs `go list -deps` and asserts no `super`/`agent`/`project` import.
- **Notes:** no JSON-schema module (I6); `verify` stays a leaf importing only `sandbox` (the `SchemaVerifier` lives here, not in `verify`).

#### SW-T02 — `audit` verify-pack
- **Goal:** evidence-backed-findings pack — every finding cites a `file:line` that **reproduces against the local worktree** (no network).
- **Depends on:** SW-T01 (`Schemas()` returns `[]*schema.Schema`). **Owns:** `internal/artifact/packs/audit/`.
- **Acceptance:** `RegisterAll(*evverify.Registry)` registers `audit.file_line_exists` / `audit.pattern_matches` / `audit.finding_reproduces`; `Hosts()` returns `nil`; `Schemas()` returns the audit Kind shape; a `validateFileLine` rejects `..`/leading-`/`/quote/whitespace/control bytes — escape ⇒ `Unverifiable` with **no** box call; `checkFileLineExists` = `box.Exec("sed -n '<N>p' '<path>'")` (single-quoted, verbs are pack constants); the cited line is parsed host-side, never echoed (I7); compile guards `var _ evverify.CheckFunc`.
- **Verify:** `make verify`; `go test -race ./internal/artifact/packs/audit/...` with the stub-sandbox pattern (canned `sandbox.Result`, no real `sed`/`grep`): present⇒Pass, empty/exit-nonzero⇒Fail, locator-escape⇒Unverifiable (assert **no** box call); assert the command string is single-quoted.
- **Notes:** fully hermetic & deterministic. Mirror `software.validateName` defense-in-depth.

#### SW-T03 — `benchmark` verify-pack
- **Goal:** repeated-runs / variance pack where **the verifier itself re-measures** — `benchmark.script_threshold` re-runs an allowlisted bench **K times in-box** and computes the metric + CV from its **own** runs (asserting `op bound` and `CV ≤ ceiling`); `benchmark.variance_bounded` is a pure host-side CV check used only as a secondary self-consistency assertion.
- **Depends on:** SW-T01. **Owns:** `internal/artifact/packs/benchmark/`.
- **Acceptance:** `RegisterAll` registers both ids; `Hosts()` returns `nil`; `Schemas()` returns the benchmark Kind (MinClaims≥1); the worker's `Evidence.Value` is a JSON `{"metric":…,"bound":…,"op":"<=|>=","runs":K,"cv_max":…}` (DATA); `Evidence.ExtractionMethod` names a **pack-allowlisted** runner (`go test -bench`, a `make bench` target, or a declared worktree script — model supplies no free command); `checkScriptThreshold` re-runs the runner **K (bounded) times** via `box.Exec`, parses each metric host-side, asserts `op bound` on the aggregate **and** `CV ≤ cv_max` over the verifier's own samples; unparseable/runner-error ⇒ `Unverifiable`; `<2` runs ⇒ `Unverifiable`; the package docstring states it verifies **claimed bounds + variance over re-runs, never exact wall-clock reproduction**.
- **Verify:** `make verify`; `go test -race ./internal/artifact/packs/benchmark/...`: stub box returning a scripted sequence of K bench stdouts → within-envelope+low-CV⇒Pass, outside-bound⇒Fail, high-CV⇒Fail, unparseable/exit-nonzero⇒Unverifiable; the K-run loop is asserted (stub records K Exec calls). Hermetic.
- **Notes:** **this closes the I2-erosion** the review flagged — variance is verifier-measured, not worker-asserted. No new module.

#### SW-T04 — `code` verify-pack
- **Goal:** build/test pack reusing `verify.Detect` — `code.build_passes` runs the autodetected build in-box (exit 0 ⇒ Pass), `code.test_passes` runs an allowlisted test command; browser checks for a code shard reuse the `ui` pack via the assembler's Composite (no new browser code).
- **Depends on:** SW-T01 (+ reuses `internal/verify` for `Detect`, `internal/sandbox`). **Owns:** `internal/artifact/packs/code/`.
- **Acceptance:** `RegisterAll` registers both ids; `Hosts()` returns `nil`; `Schemas()` returns the spec Kind; `checkBuildPasses` picks the command via `verify.Detect(box.Workdir())` (the conservative make-verify/go/npm/cargo/pytest ladder — **reused**, not re-implemented) or a claim-supplied allowlisted command, runs in-box; exit 0⇒Pass, non-zero⇒Fail, sandbox error⇒Unverifiable; compile guards; a test asserts `verify.Detect` is reused.
- **Verify:** `make verify`; `go test -race ./internal/artifact/packs/code/...`: a temp dir with `go.mod` ⇒ `Detect` returns a go command; stub box exit 0⇒Pass, non-zero⇒Fail, error⇒Unverifiable. Hermetic.
- **Notes:** code is the one preset where the artifact-verifier and the command-verifier overlap; SW-T05 composes `Schema → ArtifactVerifier(code claims) → CommandVerifier(verify.Detect)`.

#### SW-T05 — `packs.Build` assembler + `DefaultSchemas` + 3 registry entries
- **Goal:** turn `--verify-pack <name>` into a composed `verify.Verifier` (`Schema THEN ArtifactVerifier THEN optional command/browser child`), fail-closed on unknown; aggregate every pack's `Schemas()` into a `schema.Registry`; register the three new packs. The single task touching the shared `packs.go`.
- **Depends on:** SW-T02, SW-T03, SW-T04 (and transitively SW-T01). **Owns:** `internal/artifact/packs/packs.go` (extend), `internal/artifact/packs/build.go` (new), `build_test.go` (new).
- **Acceptance:** `Build(name, box, relPath, schemaReg) (PackPlan{Verifier, Hosts}, error)` runs `evverify.Default()` + `Select([name], reg)`, sets `SchemaVerifier` as `Named[0]`, `ArtifactVerifier` as `Named[1]`, appends the preset's child (code → `verify.New(box, verify.Detect(box.Workdir()))`; ui → `verify.NewBrowser`; benchmark/research/audit → none) → a `verify.Composite`; **unknown pack ⇒ error** (never `verify.Pass`, never a make-verify default — inverts `verify.Detect`); `box==nil` ⇒ networked claims Unverifiable, schema/variance still run; `DefaultSchemas() *schema.Registry` aggregates `schema.Default()` + each pack's `Schemas()`; `packs.go` gains `NameAudit`/`NameBenchmark`/`NameCode` + `Hosts` entries; `Select` atomicity preserved (validate all names before any RegisterAll); a test asserts **no** pack's `Hosts()`/checks resolve to an MCP/hosted-index client (§13).
- **Verify:** `make verify`; `go test -race ./internal/artifact/packs/...`: `Build("finance",…)` short-circuits on a malformed shape (stub box records **zero** Exec — Schema is `Named[0]`); `Build("does-not-exist",…)` errors; a green finance artifact ⇒ `Passed:true`; flip one claim's source to a stub 404 ⇒ `Passed:false` + write-back confirmed; `DefaultSchemas()` round-trips every Kind.
- **Notes:** `packs.go`/`build.go` are **not** frozen-contract. Sole ownership of `packs.go` keeps Owns disjoint. Each pack's `Schemas()` lives in its own dir; SW-T05 only aggregates.

#### SW-T06 — report source/claim-trace projection (additive)
- **Goal:** a typed per-claim trace + a matrix renderer, additively, without changing `ReportModel` consumers.
- **Depends on:** SW-T01 (decodes the `schema_verify` Kind). **Owns:** `internal/report/swarmreport.go` (new), `internal/report/render/matrix.go` (new), `internal/report/writer.go` (1-line `allowedExts += "json"`, **existing-file edit**), `internal/report/report.go` (additive `ClaimTraces`/`SchemaDefects` fields + fold in the existing pass, **existing-file edit**), `*_test.go`.
- **Acceptance:** `ReportModel` gains `ClaimTraces []ClaimTrace` + `SchemaDefects []SchemaDefectRow`, populated in the **same** single log pass; `ClaimTrace{ArtifactID,ClaimID,Field,Value,Source SourceRef,Verifier,Status,Detail,Attempt}`; `SourceRef{Locator,RetrievedAt,Resolved}`; `SwarmReport{Base *ReportModel; Swarm SwarmDimension}` + `ReplaySwarmReport(logPath, worktreeRoot)` reusing `ReplayReport` for `Base` and folding swarm Kinds for `Swarm` in one read; a broken chain ⇒ both `Base.FinalPass=false` **and** `Swarm.FinalCleanPass=false`; `RenderMatrix(*SwarmReport, termui.Style) string` (deterministic column order, redacts I3 + escapes I7, no green cell over a non-pass status); the **json** deliverable marshals a **redacted projection** (not the raw model); `writer.go allowedExts += "json"` (traversal/unknown-ext rejections still hold).
- **Verify:** `make verify`; `go test -race ./internal/report/...`: swarm-Kind fold; broken-chain ⇒ both gates false; `RenderMatrix` escapes `<script>` + redacts `?api_key=` + deterministic columns; **json** over a `?api_key=secret` SourceURL emits **no** secret; existing `report_test`/`writer_test`/`render_test` stay green.
- **Notes:** SW-T06 is the **sole** open branch on `internal/report` for its duration (two of its four targets are existing-file additive edits requiring rebase-on-main). `report` stays a leaf.

#### SW-T07 — Provider pool leaf
- **Goal:** the swarm's tiered/capped/failover/metered providers as one composed unit (§6).
- **Depends on:** — (reuses `model`, `provider`, `meter`, `budget`, `strongcap`, `roster`). **Owns:** `internal/pool/` (`pool.go`, `config.go`, `pool_test.go`).
- **Acceptance:** `PoolConfig`/`TierSpec` (`provider:model` specs only, never keys; zero value == today's single cheap-worker wiring); `PoolConfig.Validate(validProviders)` rejects unknown vendor / negative cap, accepts zero; `Build(cfg, ledger, cred, runID, opts) (*Pool, error)` composes per tier in `meter.Provider → strongcap(cap>0, shared per spec) → model.NewResilient([primary,fallback], model.Options{Jitter,BreakerThreshold,CallTimeout,MaxRetries}) → provider.ResolveWith(spec, getenv)` order, dedup identical specs to one Resilient+strongcap; `Planner()`/`Verifier()` (strong scopes); `WorkerFor(shardID)` a fresh stateless `&meter.Provider{Task:"swarm/<runID>/"+shardID}` over the shared worker stack; `CodeBackendFor(role) string`; `SetShardCeiling`→`SetTaskCeiling`; `Headroom`/`Usage`/`Spent`/`Scope`.
- **Verify:** `make verify`; `go test -race ./internal/pool/...` with scripted fake providers + fake cred: meter charges **once** even when Resilient retried; strongcap absent at cap==0, present >0; fallback-on-ratelimit (p1 errors → p2 succeeds); per-shard meter isolation (shared Inner, independent `Spent` rolling into `Total`); global+per-shard ceiling routing; **per-provider cap under -race** (50 goroutines, cap=4, peak ≤4); shared breaker across two same-spec tiers; `OnUsage` tally under -race; `Validate` cases; `CodeBackendFor`.
- **Notes:** decorates `model.Provider` only — never the backend contract. Delegated CLIs are referenced by name (different seam). Leaf: imported only by `cmd/nilcore`, `internal/swarm`, and `internal/onboard` (for the type).

#### SW-T08 — `onboard.Config.Pool` field + Validate clause  · contract (config schema)
- **Goal:** additively extend `onboard.Config` with one optional `Pool *pool.PoolConfig` (`json:"pool,omitempty"`) + a Validate clause, v1-compatible.
- **Depends on:** SW-T07. **Owns:** `internal/onboard/onboard.go`, `onboard_test.go`.
- **Acceptance:** default-zero so every existing config parses unchanged under `DisallowUnknownFields`; `Validate()` gains a pool clause (vendor ∈ valid, caps ≥ 0, loud error otherwise); old configs without `pool` parse; a config with `pool` round-trips parse/Save/Load.
- **Verify:** `make verify`; `go test ./internal/onboard/...`: round-trip with `Pool` set; old config parses; `Validate` rejects unknown vendor + negative cap.
- **Notes:** **serialized** — `onboard.go` is the strict-decoded config schema (a stable interface), so it is treated as a contract surface even though it is not on the frozen `§5` list. `onboard → pool` is downward (no cycle).

#### SW-T09 — swarm `Shard` type + invariant guards
- **Goal:** open the `internal/swarm` leaf with `Shard` (mapping DOWN to `backend.Task`/`spawn.Subtask`), the closed `ShardState`, and the small testable guards making the invariant proofs executable: `ShipGate` (refuses `verify.Pass`/nil — I2), `ClassifyCeiling` (shard-vs-global `ErrCeiling` — budget), `ProjectTrusted`/`TrustedClaim` (scoreboard projects only trusted fields — I7).
- **Depends on:** SW-T05 (consumes `packs.Build`/`PackPlan` + `artifact.Kind`). **Owns:** `internal/swarm/` (whole package): `shard.go`, `invariant.go`, `*_test.go`, `deps_test.go`.
- **Acceptance:** `Shard{ID="swarm/<runID>/<n>", Input, Goal, Kind, Pack, Role, Tier, Deps, State, Attempt, Branch}`; closed `ShardState` (`queued/running/passed/failed/exhausted/skipped`); `toSubtask(Shard) spawn.Subtask` carries ID/Goal/Deps and **drops** shard-extra fields (asserts the I1 boundary); `NewShipGate(v) (ShipGate, error)` returns `ErrNoShipVerifier` for nil **or** `verify.Pass` (fail-closed); `ClassifyCeiling(ctx, *budget.Ledger, shardKey, runErr) BudgetScope` (`None`/`Shard`/`Global`, zero-token probes record nothing); `TrustedClaim{ClaimID,Field,Verifier,Status,SourceURL}` has **no** `Value`; `ProjectTrusted(*artifact.Artifact)` reads only `Status`/`Verifier`/`SourceURL` (the **key-free** SourceURL is carried so the trace shows sources).
- **Verify:** `make verify`; `go test -race ./internal/swarm/`: `toSubtask` drops Kind/Pack/Tier; `NewShipGate(nil)` and `NewShipGate(verify.Pass{})` both error; `ClassifyCeiling` shard/global/none under -race; `ProjectTrusted` over an artifact whose `Value` holds an injection phrase yields no `Value`; `deps_test.go` asserts no `agent`/`super`/`project` import **and** no network/RPC/remote-DB import.
- **Notes:** **establishes `internal/swarm` ownership.** SW-T10…T13 add sibling files in the **same package dir** → they serialize after SW-T09 (package = unit of ownership). This is the one intra-package chain.

#### SW-T10 — swarm durable Queue
- **Goal:** persist each shard as a `store.Task` in a swarm-distinct Status namespace (run-isolated by ID prefix, full-Detail read-modify-write), so "survives restart; requeue only failed shards" is durable; the `requeue.Ledger` + `SwarmState` ride in one run row.
- **Depends on:** SW-T09. **Owns:** `internal/swarm/` (whole package): `queue.go`, `queue_test.go`.
- **Acceptance:** `SwarmState{RunID,Goal,Preset,Pass,Ledger requeue.Ledger,TipSHA}` marshaled into the run row Detail; `shardDetail{Input,Kind,Pack,Role,Deps,Attempt,Branch,Green}` per shard (refs + verdict, **never** the artifact body); Status constants `swarm-run/swarm-queued/swarm-running/swarm-passed/swarm-failed/swarm-exhausted` so the native `InFlight`/`InFlightSupervise` sweeps **never** re-drive a swarm shard; `NewQueue`/`Enqueue`/`Mark` (**full-Detail RMW**, State set **only** from the verifier `Green`, metadata-only `shard_<state>`)/`Failed`/`SaveState` (one crash-atomic Upsert)/`InFlightSwarm`/`ShardsByRun` (filter by `swarm/<runID>/` ID prefix in Go, no store change). **Resume is local-process-restart over the local SQLite store only — never cross-host.**
- **Verify:** `make verify`; `go test ./internal/swarm/` with `:memory:`/temp `store.Open`: Enqueue round-trips `SwarmState`; `Mark` full-Detail RMW does **not** wipe a prior shard's blob (the `UpsertTask` Detail-overwrite regression: write `green=true`, re-mark status, assert `green` survives); namespace isolation (a swarm-running row not in native InFlight); run isolation (two runIDs by prefix); `Failed` returns only shards with eligible Units.
- **Notes:** the single SQLite writer is the serialization point — write a shard row **once** per pass on terminal disposition, never per token.

#### SW-T11 — swarm Sharder (List / Plan / Failure)
- **Goal:** turn a goal into `[]Shard` — `ListSharder` (operator list / `--shard-file`, no model), `PlanSharder` (strong-model decomposition mirroring `planner.Plan`/`Validate`, fail-closed, run **once**), and `FailureSharder` (runs `verify.Detect` once, parses the red tests, emits one shard per failure — the "one test failure" granularity).
- **Depends on:** SW-T10. **Owns:** `internal/swarm/` (whole package): `sharder.go`, `sharder_test.go`.
- **Acceptance:** `Sharder interface { Shards(ctx, goal, runID) ([]Shard, error) }`; `ListSharder` → N namespaced shards with `Kind`/`Pack`/`Role` carried as **plain fields** (NOT importing `preset`), deterministic order, no model call; `PlanSharder{Model}` mirrors `planner.Plan`'s JSON-only parse→`Validate` (valid tree → `DependsOn`→`Shard.Deps`; an unparseable/invalid plan → **error**, never a silent empty set); `FailureSharder{Box}` → one shard per detected red test (or documents that a CI pre-step feeds a `ListSharder`); a malformed/empty list yields zero shards (the controller surfaces `checked=0` ⇒ exit 1).
- **Verify:** `make verify`; `go test ./internal/swarm/`: `ListSharder` N namespaced shards (no model); `PlanSharder` against a canned `model.Provider` → Deps carried; invalid tree → error (asserts `planner.Validate` reuse); `FailureSharder` over a fake `Detect` output emits one shard per red test. Hermetic.
- **Notes:** to avoid a cycle with `internal/swarm/preset` (which imports `swarm`), the sharder must **not** import `preset`.

#### SW-T12 — swarm Runner
- **Goal:** map `[]Shard` onto the bounded pool — `scheduler.Scheduler` (flat) or `spawn.DAGScheduler` (code DAG) — under `--concurrency`, with write-under-mutex / read-after-`Wait` discipline; define the `shardFn` (`func(ctx, Shard) spawn.Result`) **type** the cmd wiring fills (the I2 ship gate lives inside it).
- **Depends on:** SW-T11. **Owns:** `internal/swarm/` (whole package): `runner.go`, `runner_test.go`.
- **Acceptance:** `Runner{Concurrency int; Fn shardFn}`; `RunPass(ctx, shards, flat bool) map[string]spawn.Result` (flat ⇒ one `scheduler.New(Concurrency)` wave; DAG ⇒ `spawn.DAGScheduler{MaxConcurrent:Concurrency}` honoring `Shard.Deps`); a shard failure/panic in `Fn` is a **recorded** Result, never a pool abort; the outcome map is written under a mutex, read only after `Wait` drains; `shardFn` sets `Passed`/`Branch` **only** on a green report (the I2 enforcement point, supplied by cmd wiring).
- **Verify:** `make verify`; **`go test -race ./internal/swarm/`**: flat 300 shards @ Concurrency 40 — peak in-flight ≤ 40 and all 300 ran; a panicking/erroring `Fn` recorded not fatal; outcome map race-clean; DAG case A→B releases B only after A passed, a failed A skips B.
- **Notes:** the per-provider cap is NOT here (it is `strongcap` in the pool); this layer owns only the shard pool cap. `scheduler` buffers 1024; `--agents` > 1024 would submit from a dedicated goroutine — 300 is well under, so direct bulk-submit is the v1 path.

#### SW-T13 — swarm multi-pass Controller
- **Goal:** the until-clean/N loop over `Runner` + `Queue`, reusing `requeue.Scan`/`Resolve`/`ShouldContinue` verbatim: run the worklist → `requeue.Scan` → next worklist → integrate green (code) with `BaseRef=tip` → repeat until empty Worklist / `--passes N` / budget; catch `ErrCeiling` at the shard boundary via `ClassifyCeiling`; surface `Outcome.TipBranch` as a PromoteToBase candidate (never auto-land); expose the live `Scoreboard`.
- **Depends on:** SW-T12 (and transitively SW-T10/T11). **Owns:** `internal/swarm/` (whole package): `passes.go`, `passes_test.go`.
- **Acceptance:** `PassPolicy{UntilClean bool; MaxPasses int}` (`MaxPasses<=1` ⇒ one pass, byte-identical default-off shape); `Controller{Runner,Queue,Worktree,Policy,Integrate,Budget,Log}`; `Run(ctx, SwarmState, initial []Shard) (Outcome, error)` with `Outcome{Done,Reason∈{converged,exhausted,budget,passes,ctx,error},Passes,TipBranch,Remaining}`; convergence = `len(requeue.Scan(worktree,led).Units)==0`; `Failed` requeues **only** failed shards (a passed shard's `Fn` is never invoked in pass 2); `Integrate` gets `BaseRef=st.TipSHA` (collate presets pass `Integrate=nil`); budget headroom probe before each pass (global ⇒ stop; shard ⇒ that shard fails/maybe-requeues); `ErrCeiling` is a **termination rail**, never a done-signal; `Scoreboard{Checked,Passed,Failed,RetryPass,Remaining,Pass}` per pass + metadata-only `scoreboard_snapshot`; resume recomputes the open worklist from `requeue.Scan` over persisted artifacts with zero lost progress; `Log.Err()` polled each pass (HALT on broken chain).
- **Verify:** `make verify`; `go test ./internal/swarm/` (hermetic, fake worktree + canned `Fn`): until-clean (flips green on attempt 2 → 2 passes, `Done:true`); exhausted-red (`Reason:exhausted`, `Remaining>0`); passes-N cut (`MaxPasses=1` still-red → `Reason:passes`); budget cut (`Reason:budget`, no further dispatch); requeue-only-failed (call-count map proves passed shards not re-run); Integrate BaseRef carry (TipSHA threads forward); resume (crash mid-pass → fresh Controller recomputes, green stays green).
- **Notes:** reuses `requeue` unchanged; the `Integrate` seam is `super.IntegrateFunc`'s exact signature.

#### SW-T14 — swarm scoreboard board sub-leaf
- **Goal:** the live, O(1)-per-update, concurrency-safe scoreboard (§7.1) + a pure renderer + the swarm event Kinds + an optional `//go:build tui` Charm dashboard over the same `Snapshot`.
- **Depends on:** SW-T06 (`ReplaySwarmReport`/`SwarmReport` for the live-vs-replay test). **Owns:** `internal/swarm/board/` (`board.go`, `render.go`, `kinds.go`, `trace.go`, `board_tui.go`, `*_test.go`).
- **Acceptance:** `Board{New,SetTotal,MarkQueued,MarkRunning,Record,OnUsage,MarkClean,Snapshot,EmitSnapshot}` + value types; `Record` is the **only** entry moving passed/failed/retry-pass and is driven **strictly by the verifier verdict** (I2); a `fail→pass` on `Pass>0` bumps `RetryPass`; `MarkClean` is the green gate (empty worklist + chain ok); `OnUsage` accumulates per-model tokens priced via `meter.Pricer`; the runner stamps per-shard start/end + total wall-clock so **time** has a real source; `Snapshot` is an immutable copy-out (`Cost` live from `Ledger.Total()`); `RenderScoreboard(Snapshot, termui.Style)` pure, zero ANSI on off-Style; `kinds.go` free-string Kinds (`swarm_start`/`shard_enqueued`/`shard_dispatched`/`shard_verified`/`shard_requeued`/`shard_exhausted`/`scoreboard_snapshot`/`swarm_pass_clean`/`swarm_done`), metadata-only, `EmitSnapshot` coalesced; `trace.go` projects only trusted fields **including the key-free SourceURL**; `board_tui.go` is `//go:build tui` (zero Charm in default).
- **Verify:** `make verify`; **`go test -race ./internal/swarm/board/...`**: 300 goroutines Record+OnUsage while one polls Snapshot (race-clean, correct tally); retry-pass detection; exhausted-red counts; cost rollup == `Ledger.Total()`; Snapshot immutable; `EmitSnapshot` coalesced + metadata-only + `eventlog.Verify` passes; zero-ANSI off-Style; **TestLiveVsReplayAgree** (keystone): drive a Board through a scripted multi-pass sequence while appending matching events to a real `eventlog.Log`; assert `Board.Snapshot()` == `ReplaySwarmReport(...).Swarm` field-by-field.
- **Notes:** leaf imports `budget`/`meter`/`eventlog`/`termui`/`artifact` (read-only) + stdlib; never the orchestrator or the swarm runner. **Distinct dir from `internal/swarm`**, so it is parallel-safe with the T10–T13 chain.

#### SW-T15 — swarm preset bundles + new roster roles
- **Goal:** the five named bundles (§8.1), a fail-closed `Resolve` (inverts `verify.Detect`), and the two additive roster roles (`RoleAuditor`/`RoleUI`).
- **Depends on:** SW-T05, SW-T09, SW-T07. **Owns:** `internal/swarm/preset/` (`preset.go`, `resolve.go`, `*_test.go`), `internal/roster/roster.go` (additive Role consts), `internal/roster/worker.go` (their Profiles/System).
- **Acceptance:** `Preset{Name,Kind,Role,Profile,VerifyPacks,Egress,FanIn∈{collate,merge},Shape∈{flat,dag},Sharder,WorkerTier,PlannerTier}`; the five bundles per §8.1 (research **reuses the existing `RoleTypedResearch`** — ReadOnly false, web fetch true; code reuses `RoleImplementer`; audit/ui use the **new** `RoleAuditor`/`RoleUI`); `Lookup`/`Resolve` return `(_,false)`/`ErrUnknownPack` for unknown (cmd FATALs); the returned registry has **no** always-pass verifier; `Profiles(...)` derives egress = union of selected packs' `HostsFor` (not hand-typed); `RoleAuditor`/`RoleUI` write capability via `Profile.ReadOnly:false` (**not** the hardcoded `Role.ReadOnly()` helper — a test asserts `Profile.ReadOnly==false` AND `Role.ReadOnly()==true` to exercise the documented gotcha).
- **Verify:** `make verify`; `go test ./internal/swarm/preset/... ./internal/roster/...`: each preset resolves with correct Kind/Role/FanIn/Shape/packs; `Resolve("garbage")` ⇒ `ErrUnknownPack`; write roles rely on `Profile.ReadOnly`; egress == pack `HostsFor` union; existing roster tests stay green (additive consts).
- **Notes:** `preset` imports `swarm` one-directionally (`swarm` never imports `preset`). `roster.go`/`worker.go` are leaves (not frozen). ui/browser is CI-only, fails closed (no browser image ⇒ all-red, never false green). audit/benchmark presets name packs that now exist (SW-T02/T03).

#### SW-T16 — extend `nilcore report` (swarm report/matrix/json)
- **Goal:** extend the **shipped** `cmd/nilcore/report.go` (the dispatch arm already exists) to call `ReplaySwarmReport` + `RenderMatrix` + the `json` format; shared by the swarm `--report` flag.
- **Depends on:** SW-T06. **Owns:** `cmd/nilcore/report.go` (**edit the shipped file** — do not create; the `case "report"` arm and `reportMain`/`runReport` already exist).
- **Acceptance:** extend `reportMain`/`runReport` with `--format text|md|html|json|matrix` + `--dir <worktree>` + `[--out]`, calling `ReplaySwarmReport` (folds artifacts when `--dir` given) → `render.*` / **redacted** `json` / `RenderMatrix` → stdout or `report.WriteReport`; `ReplayReport` runs `eventlog.Verify` so a broken chain shows RED; stdlib-only; default binary unaffected (the new formats are unreachable until the swarm/report flags pass them).
- **Verify:** `make verify`; a temp-log smoke test: build a fake event log (a verify + an `artifact_verify` event) + a fake `.nilcore/artifacts/<id>.json`, run `reportMain`, assert a non-empty text report; a corrupted chain ⇒ `FinalPass==false`; `--format json` over a `?api_key=` SourceURL emits no secret.
- **Notes:** lands before `swarm.go` (depends only on shipped `internal/report` + SW-T06). The swarm `--report` flag shares this renderer.

#### SW-T17 — `nilcore swarm` + `buildSwarm` + dispatch wiring  · serialized cmd-wiring
- **Goal:** the operator front door — the `swarm` subcommand, `buildSwarm` (the `buildStack` analogue), and the **one** new dispatch case (`case "swarm"`) + the swarm usage line (+ the missing `report` usage line) in `main.go`. Composes pool, presets, the Controller/Runner/Queue, the board, the verifier, the Integrator, the gate.
- **Depends on:** SW-T13, SW-T14, SW-T15, SW-T08, SW-T16. **Owns:** `cmd/nilcore/swarm.go` (new), `cmd/nilcore/main.go` (one `case "swarm"` + usage lines — the **only** edit to an existing file).
- **Acceptance:** `registerSwarmFlags` parses the §8.2 flags (defaults: preset=research, concurrency=1, passes=until-clean, budget=25.00, jitter=750ms, report=text); the **`--artifact` consumer** parses the `+`-joined list into the per-shard Kind override **and** the run-level deliverable set (`matrix` ⇒ `RenderMatrix`); `buildSwarm(swarmDeps) (swarmAssembly, error)` resolves in the §8.2 order with **one** shared `*budget.Ledger`; the **shardFn** branches on `pool.CodeBackendFor(role)` — `native` ⇒ `roster.NewWorker`, else ⇒ the existing `buildBackend(name,…,box,verifier,…)` **in-box** (I4), both verified by the same per-shard `ArtifactVerifier` (I2); unknown `--verify-pack`/`--preset` is FATAL at startup; `--per-shard-budget` → `SetTaskCeiling`; `meter.OnUsage` fans into `board.OnUsage`; the final clean tip promotes through **one** `policy.GateAction{PromoteToBase}` (optional `route.Review` first; nil approver default-denies); exit 0 iff empty final Worklist **and** the final tree verifies **and** `ChainVerified`; add `case "swarm"` + the usage lines; **no** other arm or shared helper edited.
- **Verify:** `make verify`; `go test ./cmd/nilcore/...` (hermetic, scripted fake provider + fake sandbox): default-flag parse; unknown-verify-pack → `buildSwarm` error; single-ledger wiring (a pre-exhausted ledger makes all metered providers charge it); presetVerifier collate-is-artifact-only / code-ANDs-checks / never-`verify.Pass`; **`--artifact report+matrix` yields both a report and a matrix**; **`--code-backend codex` routes through `buildBackend`, not `NewWorker`**; ui presetVerifier with a nil box ⇒ `Passed:false`, claims Unverifiable; an **import-graph test** asserting no existing package imports `internal/swarm*`/`internal/pool`; an **init()-free test** asserting the new leaves have no global-side-effect `init()`.
- **Notes:** **serialized cmd-wiring** — the only task editing `main.go`. Default binary byte-identical (one new arm + usage lines; all logic in new files). `--code-backend codex|claude-code` routes a coding shard through `buildBackend` in-box (I4). Live browser shards are the `browser-e2e` job's concern, not `make verify`.

#### SW-T18 — Docs + CHANGELOG promotion  · contract (docs), serialized last
- **Goal:** promote this plan into the canonical docs and ledger as Phase 12.
- **Depends on:** SW-T17. **Owns:** `docs/TASKS.md`, `docs/ARCHITECTURE.md`, `docs/ROADMAP-EXTERNAL-INFRA.md`, `CLAUDE.md`, `CHANGELOG.md`, `README.md`.
- **Acceptance:** `docs/TASKS.md` Phase-12 DAG rows + specs (note the artifact/pack rows **reuse** the shipped Phase-11 spine, not rebuild); `docs/ARCHITECTURE.md` a "Verified swarm mode (Phase 12, self-hostable)" subsection (artifact-as-file out-of-band I1, two-layer verifier authority I2, per-shard box I4, fail-closed pack inversion, never-land preserved, single-host/bounded/no-standing-authority, **the scoreboard is a local per-run single-process projection — any served/multi-tenant dashboard is EXT-02/EXT-05**) + the four new leaf rows in the layer-map table with their import sets + the restated leaf rule; `docs/ROADMAP-EXTERNAL-INFRA.md` one `EXT-01` boundary line (swarm = the single-host projection; multi-host shard dispatch / cross-host task-state / a remote control plane crosses into EXT-01); `CLAUDE.md` one repository-map line (no invariant text change — the invariants are unchanged, which is the point); `CHANGELOG.md` one `## [Unreleased]` entry per merged SW-T0x; `README.md` the `nilcore swarm`/`report` usage + preset list + the default-off note + the honest caveats (ui CI-only fails closed; benchmark verifies bounds+variance over re-runs).
- **Verify:** `make verify` (docs don't break the build); a markdown-format pass; manual review that the layer-map import sets match the actual `go list -deps` of each new leaf.
- **Notes:** **serialized — contract files.** Lands last. Per-task CHANGELOG lines are appended at each task's own merge; SW-T18 adds only the docs/README/layer-map prose and reconciles trivial append conflicts on rebase.

---

## §11 Pipelines & parallel-execution map

A fleet executes in ordered **waves**; every task in a wave has all deps merged to `main` and a
pairwise-disjoint Owns set, so the whole wave runs concurrently with zero collision. `internal/swarm`
is **one package = one Owns unit**, so its files form a serialized sub-chain — the only intra-wave
serialization.

```
WAVE 1  (2 concurrent — no-dep new leaves, each independently `make verify`-green)
  ├── SW-T01  internal/artifact/schema/      (shortest; everything downstream imports it)
  └── SW-T07  internal/pool/

WAVE 2  (4 concurrent — composers over wave-1 leaves)
  ├── SW-T02  internal/artifact/packs/audit/        (T01)
  ├── SW-T03  internal/artifact/packs/benchmark/    (T01)
  ├── SW-T04  internal/artifact/packs/code/         (T01)
  └── SW-T08  internal/onboard/onboard.go           (T07)   ← SERIAL pt: config-schema contract

WAVE 3  (2 concurrent)
  ├── SW-T05  internal/artifact/packs/{packs.go,build.go}  (T02,T03,T04)  ← SERIAL pt: sole packs.go owner
  └── SW-T06  internal/report/  (additive)                 (T01)         ← sole internal/report owner

WAVE 4  (3 concurrent)
  ├── SW-T09  internal/swarm/{shard,invariant}.go   (T05)   ← opens internal/swarm package
  ├── SW-T14  internal/swarm/board/                 (T06)
  └── SW-T16  cmd/nilcore/report.go  (edit)         (T06)

WAVE 5  (2 lanes; lane A serialized inside internal/swarm)
  lane A (serial — same package dir internal/swarm):
        SW-T10 queue.go (T09) → SW-T11 sharder.go (T10) → SW-T12 runner.go (T11) → SW-T13 passes.go (T12)
  lane B:  SW-T15  internal/swarm/preset/ + internal/roster  (T05,T09,T07)

WAVE 6  (1 — SERIAL pt: cmd-wiring)
        SW-T17  cmd/nilcore/swarm.go + main.go   (T13,T14,T15,T08,T16)   ← sole main.go editor

WAVE 7  (1 — SERIAL pt: docs contract)
        SW-T18  docs/* + CLAUDE.md + README.md + CHANGELOG.md   (T17)    ← sole docs editor
```

**Peak concurrency = 4 (wave 2).** Critical path (longest dependency chain) — **9 sequential merges:**

```
SW-T01 → SW-T05 → SW-T09 → SW-T10 → SW-T11 → SW-T12 → SW-T13 → SW-T17 → SW-T18
```

**Serialization points (parallelism intentionally throttled to one writer):**
1. `internal/artifact/packs/packs.go` — SW-T05 only.
2. `internal/onboard/onboard.go` — SW-T08 only (config schema).
3. `internal/swarm` package dir — SW-T09 opens; SW-T10/T11/T12/T13 serialize as sibling files
   (package = unit of ownership; splitting into per-file sub-packages would create an import-cycle
   with `preset`, so the chain is the correct trade).
4. `cmd/nilcore/main.go` — SW-T17 only.
5. `docs/*` / `CLAUDE.md` / `README.md` / `CHANGELOG.md` prose — SW-T18 only.

**No-cycle proof:** every edge points from a lower wave to a higher one; no task depends on a later
task; `internal/swarm`'s sub-chain is strictly increasing IDs. The work-selection rule deterministically
walks waves 1→7 without a forced collision. **Foundation-before-orchestration holds:** the swarm runner
literally cannot compile until `packs.Build` (SW-T05) exists.

---

## §12 Contract-file changes & docs impacts

**Frozen-§5 contract files — UNTOUCHED by every task:** `internal/backend/backend.go` (I1; the artifact
is a worktree file read by the verifier, shard-extra data on `swarm.Shard`, new per-shard signals ride a
sentinel error), `internal/channel/channel.go`, `go.mod` (I6; all new leaves stdlib-only, `CGO_ENABLED=0`,
no module added), `Makefile`.

**Serialized contract surfaces — each held by exactly one task:**
- `docs/ARCHITECTURE.md` / `docs/TASKS.md` / `CLAUDE.md` / `docs/ROADMAP-EXTERNAL-INFRA.md` / `README.md`
  prose — **SW-T18** only.
- `internal/onboard/onboard.go` (config schema) — **SW-T08** only (additive `Pool *pool.PoolConfig`
  `omitempty` + Validate; old configs byte-compatible under `DisallowUnknownFields`).
- `internal/artifact/packs/packs.go` — **SW-T05** only (Name* consts, `Select`/`HostsFor` entries,
  `Build`/`DefaultSchemas`).
- `cmd/nilcore/main.go` — **SW-T17** only (one `case "swarm"` + usage lines).
- `internal/report/report.go` + `writer.go` (existing-file additive edits) — **SW-T06** only.
- `internal/roster/roster.go` + `worker.go` (leaf, not frozen) — **SW-T15** only (additive
  `RoleAuditor`/`RoleUI`).
- `cmd/nilcore/report.go` (existing-file edit) — **SW-T16** only.

**CHANGELOG.md** — append-only under `## [Unreleased]`, one line per merged task (added at that task's
own merge; rebase resolves trivial append conflicts), e.g.:
```
- **SW-T01** — Stdlib artifact schema/shape validation leaf + SchemaVerifier (verify.Verifier). _Owns:_ internal/artifact/schema. _(Phase 12)_
- **SW-T03** — `benchmark` verify-pack (verifier re-measures K in-box; bounds+variance, not wall-clock). _Owns:_ internal/artifact/packs/benchmark. _(Phase 12)_
- **SW-T17** — `nilcore swarm` subcommand + buildSwarm + dispatch wiring. _Owns:_ cmd/nilcore/swarm.go,main.go. _(Phase 12)_
```

**README.md** (SW-T18): a "Verified swarm mode" section (the headline command, the five presets one line
each, the product line), the extended `nilcore report` usage, the default-off/byte-identical note, and the
honest caveats.

**docs/ARCHITECTURE.md** (SW-T18, contract): the Phase-12 subsection + four layer-map leaf rows with
import sets — `internal/pool` (model/provider/meter/budget/strongcap/roster), `internal/swarm`
(artifact/evverify/requeue/verify/spawn/scheduler/integrate/budget/meter/roster/backend/sandbox/worktree/
worktreefs/store/eventlog/guard/route/pool — **never** agent/super/project), `internal/swarm/board`
(budget/meter/eventlog/termui/artifact), `internal/swarm/preset` (roster/artifact/packs/policy/swarm/pool) —
plus the restated leaf rule.

---

## §13 Honest caveats & the future-EXT boundary

- **The EXT-01 line.** Verified swarm mode is the **single-host projection** of a fleet. The moment
  shards are dispatched to **other hosts**, or shard task-state is **read by a second host** to
  coordinate, or a **remote control plane** leases work, it is `EXT-01` and requires the §0 thesis gate
  in `docs/ROADMAP-EXTERNAL-INFRA.md`. Out of scope here; named as a future dependency, never designed in.
  A `deps_test` guard keeps `internal/swarm` provably in-process (no net/http server, no rpc, no remote
  DB driver).
- **MCP-as-data-source is out of scope.** v1 research/finance packs are **curl-in-box + stdlib** (the
  shipped finance pack). Pointing a verifier at an operator-configured MCP data server would make a
  shard's verdict contingent on a standing external API + a long-lived credential — drifting toward
  `EXT-06`/`EXT-07`. SW-T05's test asserts no pack resolves to an MCP/hosted-index client.
- **The scoreboard is local.** A per-run, single-process text/TUI projection over the local log + the one
  local budget ledger. A **served, multi-session, per-tenant** dashboard would be `EXT-02`/`EXT-05` — the
  SW-T18 ARCHITECTURE note pins this so a follow-on PR cannot quietly add an HTTP scoreboard endpoint.
- **Resume is local-restart only.** Over the local single-writer SQLite store; shard state is never read
  by a second host.
- **ui/browser shards are CI-only and fail closed.** No browser image ⇒ **all-red**, never a false green.
  Hermetic tests cover the wiring; a `browser-e2e` job covers live behavior. (Same posture as the shipped
  P9 browser verifier.)
- **The benchmark pack verifies bounds + variance over the verifier's OWN re-runs**, not exact wall-clock
  reproduction — stated in the pack docstring, the test, and the README so the product brief does not
  over-promise.
- **Delegated coding workers run in-box.** `--code-backend codex|claude-code` routes a coding shard
  through `buildBackend` inside the sandbox (I4); the same per-shard `ArtifactVerifier` governs (I2).

---

## §14 Verification gates (the proof obligations)

The plan ships only when **all** hold:

- `make verify` green at every task merge; `golangci-lint` 0; `gofmt`/`goimports` clean; `CGO_ENABLED=0`
  cross-compile across the release matrix (no `go.mod` change).
- **`go test -race`** green on every concurrent surface: the swarm Runner pool (peak in-flight ≤
  `--concurrency`), the Board tally (300 goroutines Record+OnUsage vs a polling Snapshot), the Queue
  full-Detail RMW, the per-provider cap (`strongcap`), the outcome maps.
- **I2 property tests:** no shard ships without a green `ArtifactVerifier` verdict; `ShipGate` refuses
  `verify.Pass`/nil; an unknown pack fails closed; the benchmark pack re-measures (no trust of worker
  samples); the Integrator tip stays verifier-green and a red combination never poisons it.
- **I7 trust tests:** an injection phrase in a model `Value` never appears in the scoreboard / matrix /
  json / trace; sources are `guard.Wrap`'d; only verifier-set fields are projected.
- **I3 redaction tests:** the json/matrix deliverables and the event log never emit a keyed SourceURL.
- **Durability test:** a crash mid-pass + restart recomputes the open worklist from `requeue.Scan` over
  persisted artifacts with zero lost progress; green stays green; the native InFlight sweeps never
  re-drive a swarm shard.
- **Live == replay:** `Board.Snapshot()` equals `ReplaySwarmReport(...).Swarm` field-by-field.
- **Default-off byte-identical:** no existing package imports a swarm/pool leaf; the new leaves have no
  global-side-effect `init()`; the default dispatch path reaches neither new arm; `--concurrency 1` is the
  single-flight path.
- **Headline command:** `nilcore swarm --goal … --agents 300 --concurrency 40 --artifact report+matrix
  --verify-pack finance --passes until-clean --budget 500` runs to a clean (or honestly-red) scoreboard +
  a report **and** a matrix deliverable, exit 0 iff every shard verified clean and the chain verifies.

*The whole point of the invariant ledger (§9) is that it is empty of changes: NilCore stays a small,
verifying, sandboxed, bounded core. Swarm mode adds throughput and a scoreboard — it does not add a way
to ship work the verifier did not bless.*
