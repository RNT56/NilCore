# Roadmap — Phase 16: closing the loop on the agent's own evidence

> **Status:** Pillars 1–7 SHIPPED (default-off, opt-in, invariant-preserving), across Waves A–E. The headline — **graduated auto-approval** — is functional, fenced (the shared `internal/blastbudget` meter, all four axes live), audited (`nilcore trace` + `nilcore auto-approvals`), and inspectable (`nilcore experience`/`capability`); dynamic routing is activatable (`NILCORE_TRUST_DEFAULT=1`); the experience layer, lessons + verify-cache, the self-improvement flywheel (`nilcore flywheel`), and the autonomy daemon + objectives backlog (`nilcore objective`, `NILCORE_AUTONOMY`) are all wired. The §0 relaxation decisions (#1 graduated auto-approval + the exact preset blast-radius values; #2 the separate self-improve auto-merge opt-in) are RECORDED in [`docs/ARCHITECTURE.md`](ARCHITECTURE.md) §"Closed-loop autonomy". The cross-cutting guarantees (XC-T01..T06) are asserted in code. **Pillar 8 (the unified orchestration kernel) remains §0-deferred** — the cutover re-homes all entrypoints + edits contract files and is taken only after a separate human-signed decision. Eight pillars, ~64 tasks, five waves.
>
> **Read with:** [`CLAUDE.md`](../CLAUDE.md) (invariants), [`docs/ARCHITECTURE.md`](ARCHITECTURE.md) (the frozen contract + the execution model), [`docs/HORIZON.md`](HORIZON.md) (the candidate scan this program promotes), [`docs/PRINCIPLES.md`](PRINCIPLES.md) (#1 feedback loop, #9 earn improvement from evidence, #10 safety enables autonomy).

## Table of contents

- [§0 Where this sits — the thesis](#0-where-this-sits--the-thesis)
- [§1 The product surface — what changes for the user](#1-the-product-surface--what-changes-for-the-user)
- [§2 As-is: what already ships (reuse, do not rebuild)](#2-as-is-what-already-ships-reuse-do-not-rebuild)
- [§3 The architecture — one closed loop](#3-the-architecture--one-closed-loop)
- [§4 The eight pillars](#4-the-eight-pillars)
- [§5 Graduated auto-approval in depth](#5-graduated-auto-approval-in-depth)
- [§6 The invariant & safety ledger](#6-the-invariant--safety-ledger)
- [§7 Cross-cutting guarantees](#7-cross-cutting-guarantees)
- [§8 The master DAG & wave order](#8-the-master-dag--wave-order)
- [§9 Per-task specs](#9-per-task-specs)
- [§10 §0 gate decisions (recorded before code)](#10-0-gate-decisions-recorded-before-code)
- [§11 Contract-file changes & docs impact](#11-contract-file-changes--docs-impact)
- [§12 Honest caveats & risks](#12-honest-caveats--risks)
- [§13 Verification gates (proof obligations)](#13-verification-gates-proof-obligations)

---

## §0 Where this sits — the thesis

The sharpest structural finding in the codebase, from [`docs/HORIZON.md`](HORIZON.md):

> **NilCore measures everything and learns from almost none of it.** Every run emits a hash-chained event log, `race_outcome` verdicts, `eval.Report`s, traces, and memory — and almost nothing reads any of it back. Routing earns nothing by default, self-improvement is operator-triggered, the gate asks a human for every irreversible action every time.

Phase 16 **closes that loop**: the agent consumes its own verifier-judged audit trail to **route, plan, gate, and improve itself** — so it depends on the operator less while staying inside all seven invariants. The unifying move is one sentence: *turn the dormant evidence into earned behaviour.* That is simultaneously the **unification** (one experience layer, one capability descriptor, one autonomy daemon), the **dynamism** (learned routing/budgets/escalation), and the **autonomy** (graduated auto-approval, the self-improvement flywheel, the standing backlog).

**The safety stance is non-negotiable and is the whole point.** "Less user dependence" never means weakening the verifier (I2), the sandbox (I4), no-ambient-authority (I3), or the audit log (I5). It means **moving the human from per-action approval to policy + envelope + earned trust**, and making self-verification carry more weight. Per principle #10, *safety is what makes autonomy possible* — every pillar here strengthens the feedback loop so the gate is needed less *often*, never made weaker.

**Already shipped (Phase 13), reused here:** the Trust Ledger (`internal/trust`), live multi-backend routing via `-backends` + `trust.Selector`, and `nilcore trace`. Phase 16 makes that loop the *default-available* substrate and extends it from routing to gating and self-improvement.

---

## §1 The product surface — what changes for the user

Every change is **opt-in and default-off**; an operator who turns nothing on sees a byte-identical binary.

- **Less re-specifying.** The agent proposes acceptance criteria for under-specified goals and learns which approach works for a task-class (dynamic routing), so you steer less.
- **Less re-approving — graduated auto-approval (the headline).** You set a *policy once* ("may open PRs; may promote to non-`main` branches; may deploy to staging ≤ $X/day") instead of approving each action. Actions the agent has done verifier-green N times under recorded conditions auto-proceed within that envelope; everything else still hits the human gate. Default-off; one `nilcore init` choice or one env var turns it on with a safe preset; an instant kill-switch reverts it.
- **It gets better while idle — the self-improvement flywheel.** The agent periodically evals itself, mines its own failures, and proposes prompt/skill fixes that ship only if they measurably improve pass-rate (gated).
- **It works on its own toward your intent — the autonomy daemon + objectives backlog.** One long-running daemon unifies file-signals, cron, webhooks, and self-generated goals into one prioritized queue; a standing operator-intent backlog ("keep CI green," "keep deps current") it self-services when idle, reversibly, gating only at the irreversible edge.
- **One legible "what may it do" surface.** `nilcore capability` and `nilcore experience` print, respectively, a drive's exact capability descriptor and the unified learned-state scoreboard.

---

## §2 As-is: what already ships (reuse, do not rebuild)

| Shipped seam | File | Reused for |
|---|---|---|
| Trust Ledger (verifier-judged `race_outcome` + eval fold; fail-closed on broken chain) | `internal/trust/{ledger,selector,replay,router}.go` | Pillars 1, 2, 5 (earned-trust pattern) |
| Live multi-backend routing | `internal/agent/orchestrator.go` (`Selector`/`Backends`/`NewEnvFor`) | Pillar 2 |
| The policy gate (free-text + structured; closed `GateActionType`; nil approver default-denies) | `internal/policy/{policy,gateaction,approver}.go` | Pillars 5, 6 |
| Append-only hash-chained log + `eventlog.Verify` | `internal/eventlog` | Pillars 1, 3, 5 (the evidence source of truth) |
| SQLite store (events/memory/tasks; idempotent additive migrations) | `internal/store/{store.go,db/schema.sql}` | Pillars 1, 7 (projection + backlog) |
| Cross-project memory (`Write`/`Remember`/`Context`, "data not instructions") | `internal/memory/memory.go` | Pillar 3 (lessons) |
| Eval harness (`eval.Report` pass-rate/cost/latency) | `eval/eval.go` | Pillars 2, 4 |
| Reversibility classifier + structured gate + console approver | `internal/policy` | Pillars 5, 6 |
| `budget.Ledger` (tokens + dollars, ceiling, RWMutex) | `internal/budget/budget.go` | Pillar 6 (sibling), 5 ($ ceiling) |
| Egress proxy / sandbox exec / capguard / egress profiles | `internal/policy`, `internal/sandbox`, `internal/capguard`, `internal/egressprofile` | Pillars 1, 6 |
| Self-edit flow (scope allow/deny, verified, gated) | `internal/selfimprove`, `cmd/nilcore/selfimprove.go` | Pillar 4 |
| Self-start funnel (trigger) + cron + wake + webhook + drivegate + maint | `internal/{trigger,cron,wake,scmhook,maint}`, `cmd/nilcore/{watch,schedule,webhook,drivegate}.go` | Pillar 7 (unify) |
| `nilcore trace` / `report` read-over-log discipline | `internal/{trace,report}` | Pillars 5, 7 (audit surface) |

**No new Go module is added by any pillar (I6).** Every leaf is stdlib + permitted internal leaves, guarded by a `deps_test.go` (`go list -deps`) mirroring `internal/trust/deps_test.go`.

---

## §3 The architecture — one closed loop

```
                 ┌──────────────────────── append-only event log (I5, source of truth) ───────────────────────┐
                 │  race_outcome · verify · boundary_outcome · eval · capability · auto_approve · blast_* …     │
                 └───────▲───────────────────────────────────────────────────────────────────────────┬────────┘
   emits (every run)     │                                                                             │ read-only replay + eventlog.Verify (fail-closed)
                 ┌────────┴────────┐                                                          ┌─────────▼──────────┐
                 │  the agent loop │                                                          │ EXPERIENCE LAYER    │  ← Pillar 1 (the spine)
                 │ native/super/   │                                                          │ (derived, rebuild-  │
                 │ project/swarm   │                                                          │  able projection)   │
                 └────────▲────────┘                                                          └──┬───────┬──────┬──┘
   consumes (earned behaviour)                                                                   │       │      │
        ┌────────────────┼────────────────────────────┬───────────────────────────┐   routing  │ gating│ self-improve
        │                │                             │                           │            ▼       ▼      ▼
   Pillar 2          Pillar 3                       Pillar 5  ◄── envelope ──  Pillar 6      Pillar 4   Pillar 7
  dynamic routing   learn-from-scars            graduated auto-approval     blast-radius   flywheel   autonomy daemon
  (trust→default,   (lessons-memory,            (GradedApprover wraps the   budget (the    (eval→     + objectives
   cost-aware,       self-acceptance,            human gate; earned trust    hard runtime   trust→     backlog
   data-driven       verify cache)               + operator envelope)        fence)         gated      (one queue)
   escalation)                                                                               improve)
                                          Pillar 8 (DEFERRED, §0-gated): unify all four machines into one recursive kernel
```

The **experience layer (Pillar 1)** is the spine: a single derived, rebuildable projection over the existing store/log that every consumer reads (router, planner, auto-approval, flywheel). It is *never authoritative* — the append-only log is (I5); the projection is `Rebuild`-able from it. The **capability descriptor (Pillar 1)** is the legible "what may this drive do" struct the gate/sandbox/egress/capguard all read. Everything below consumes the spine; nothing below can mark work "done" or skip the verifier (I2).

---

## §4 The eight pillars

Each pillar is **default-off and byte-identical when unused**, proven by a golden test. Task IDs are namespaced per pillar; the full DAG is §8 and the specs are §9.

### Pillar 1 — Experience layer + capability descriptor (`EXP`)
Two stdlib leaves. **`internal/experience`**: one `Reader` interface unifying the trust scoreboard, eval rollups, memory lessons, and replayed event-log outcomes over the existing store, with a single write path (`Projector.Fold` + `Rebuild`) and many readers. The event log stays the source of truth; the projection is derived and rebuildable (`exp_backend_standing`/`exp_config_standing`/`exp_meta` tables, each carrying a `source_seq` watermark + `chain_ok`). **`internal/capability`**: one pure `For(Request) → Descriptor` that reproduces today's scattered tools/shell/guard/egress/capguard choices byte-for-byte, emitting one metadata-only `capability` event per drive. **Opt-in:** nil `Reader` ⇒ static behaviour; `-experience`/`NILCORE_EXPERIENCE` wires it; `nilcore experience --rebuild` backfills. **Verdict from review: needs-fix** — the byte-identical golden must be generated from *live legacy output* at each real call site (`chat.go:544`, `browse.go:150`, `desktop.go:200`), enumerating every mode.

### Pillar 2 — Dynamic data-driven routing (`RTE`)
Make trust-informed selection the **default** (a nil-safe `agent.TrustOracle` injected at the `cands` seam, `orchestrator.go:~285-301`), extended from backend to **model/tier**, **cost-aware** (combine pass-rate with `meter.Pricer` + `pool.Headroom` to pick the cheapest tier clearing a confidence bar, escalate on failure — HORIZON A6), and with **data-driven race-N and escalate-after** thresholds and adaptive budgets in place of fixed flags. A deterministic keyword `trust.Classify` buckets task-classes. **The oracle only orders/prunes/sizes candidacy — the verifier judges every race and decides shipping (I2).** Cold/low-confidence cell ⇒ static behaviour. **Opt-in:** `-trust-route`/`NILCORE_TRUST_DEFAULT=1`; `nilcore trust --route` shows what routing *would* do first. **Verdict: sound.**

### Pillar 3 — Learn from scars (`LRN`)
Three additive pieces. **A8 lessons-memory** (`internal/memory/lessons`): mine the log for recurring verifier-failure *patterns* and write them back as deduped memory **data** (structural fields only — `verifier_id`, `fail_class`, counts — **never raw failing output**, per the review's I7 fix), surfaced next same-class task. **Self-generated acceptance** (`internal/verify/selfacc`): propose acceptance criteria up front; where no pack exists, author a *candidate* verifier — which is itself untrusted and **may only ever run as a sandboxed command/artifact verifier, never an in-process Go `CheckFunc`** (review's I4 fix), and maps to `Unverifiable` until proven. **A9 verify cache** (`internal/verify/vcache`): skip a verifier when worktree-content-hash + verifier-id + toolchain match a prior **chain-verified** `Pass` — and `vcache.Lookup` **must call `eventlog.Verify` and fail-closed-to-recompute on any chain error** (review's I2 fix). **Verdict: risky** — ships only with all three fixes. **Opt-in:** each behind its own env (`NILCORE_LESSONS`, `NILCORE_SELFACC`, `NILCORE_VCACHE`).

### Pillar 4 — Self-improvement flywheel (`SIF`)
Four leaves under `internal/flywheel`: **selfeval** (run a *content-hash-frozen* eval suite on the agent, fold to trust — and the `selfeval_report` fold **must be verifier-judged outcomes only, behind `eventlog.Verify`**, per the review's I2 fix, so the agent can't inflate its own standing), **distiller** (shared with LRN's A8), **measure** (the regression fence — a *measured* eval-delta, not vibes), **loop** (the bounded standing cadence). Each candidate runs as a normal verified, **human-gated** `selfimprove` task editing only prompts/skills (never core/contracts — `selfimprove.DefaultScope` deny wins); it ships only if it improves pass-rate, with rollback and a regression fence. Guards the C6 feedback-loop pathologies: no self-modification of the eval set or the verifier-of-record. **Verdict: needs-fix** (apply the verifier-judged-fold fix). **Opt-in:** `NILCORE_FLYWHEEL`; the self-improve auto-approval *class* is a **separate** double-opt-in (§10).

### Pillar 5 — Graduated auto-approval (`GAA`) — the headline
A new `policy.Approver` (`internal/graapprove.GradedApprover`) that **wraps** the human approver and, per `GateAction`, auto-approves iff the action-class+scope has **earned trust** (verifier-green N times, recent, over a clean chain) **and** is within the operator **envelope** — else falls through to the human. Earned trust is folded from a *dedicated* `boundary_outcome` event (never `race_outcome`; never a self-report). Full design in §5. **Verdict: risky** — ships only after its fixes and its wave dependencies (§8). **Opt-in:** the deepest default-off discipline in the program (three layers; §5).

### Pillar 6 — Capability / blast-radius budget (`BR`)
A `budget.Ledger` sibling (`internal/blastbudget`) bounding four axes: **distinct egress hosts**, **irreversible/auto-approval count**, **sandbox wall-time**, and **per-UTC-day auto-approval dollars** — the hard runtime fence Pillar 5's envelope reads. Checked at exactly three choke-points (egress proxy `ServeHTTP`, sandbox `ExecWithEnv`, the gate path) plus the per-day window. **Composition law: blast budget is checked first and a breach is final; Pillar 5 may escalate only within the remaining envelope (`min(P5, blast)`).** Per the review, **`blastbudget` is the single $/rate/irreversible meter** — Pillar 5 *consults* it, never double-counts — and the wall-time fence **derives `ctx.WithTimeout` from the remaining budget** so it actually bounds, not just records; the per-day window **rebuilds from the log on restart** (no fail-open on restart). **Verdict: needs-fix** (apply those). **Opt-in:** all `-blast-*` default 0 (unlimited); `New()` is never called unless an axis is set.

### Pillar 7 — Autonomy daemon + objectives backlog (`AUTO`)
**`internal/autosrc`**: one pluggable event-source registry + bounded priority queue folding file-signals, cron, webhooks, wake, *and* self-generated goals into one queue routed through `drivegate`. **`internal/objective`**: a store-backed standing-objectives backlog the agent pulls from when idle, executes reversibly through the verified orchestrator, gating only at the irreversible edge (composing with Pillar 5). Headless ⇒ irreversible deny-defaults unless an envelope is configured. Per the review's I7-adjacent fix, **objective CRUD is an operator-only host surface, unreachable from any sandboxed model tool** (a model must not enqueue its own standing objectives). **Verdict: sound.** **Opt-in:** folds into `serve`; the backlog source is off unless objectives exist.

### Pillar 8 — Unified orchestration kernel (`UOK`) — DEFERRED, §0-gated
One recursive `internal/kernel` primitive that runs a task and *dynamically* decides to stay flat or decompose-and-fan-out, with `run`/`build`/`swarm` becoming presets and the chat router picking an *envelope*, not a machine. **This is the final wave, separately §0-gated**, because the cutover (`UOK-T10`) re-homes all four entrypoints and edits contract files. It builds last, only after Pillars 1–7 prove the substrate, behind an **equivalence harness** (`UOK-T09`) that golden-diffs legacy-vs-kernel event-log sequences across *every* I2/gate-bearing path. The decompose branch **must always re-verify at the integrated tip** even when children are green (review's I2 fix). **Verdict: risky** — by design; gated. **Opt-in:** `NILCORE_KERNEL` until the cutover; after cutover the equivalence harness is the sole guarantee.

---

## §5 Graduated auto-approval in depth

The capability the operator asked for, designed to be **robust, opt-in, and trivially easy to turn on — where the easy path is the safe path.**

### The seam (why it's clean)
`internal/policy` already exposes `Approver{ Approve(string) bool }`, `Gate` (free-text), and `GateStructured(GateAction, Approver)` over a **closed** `GateActionType` set `{PromoteToBase, Push, Deploy, OpenPR}`; a nil approver default-denies. Graduated auto-approval is a new approver that wraps the human one. The **only** policy edit is an additive optional interface + one branch:

```go
// GAA-T01 (serialized, contract-adjacent): additive, non-breaking.
type StructuredApprover interface{ ApproveStructured(a GateAction) bool }
// in GateStructured, ABOVE the existing `return ask.Approve(a.describe())`:
if sa, ok := ask.(StructuredApprover); ok { return sa.ApproveStructured(a) }
```
A `ConsoleApprover` doesn't implement `StructuredApprover`, so control reaches the **exact** existing line — byte-for-byte today's path (golden test required).

### The envelope (operator policy, set once)
`onboard.Config.AutoApprove *Envelope` (omitempty; nil ⇒ absent; config version bump with a no-op migration so a v2 config decodes identically):

```go
type Envelope struct{ Classes []ClassClause }
type ClassClause struct {
  Type          string   // "open-pr"|"promote-to-base"|"push"|"deploy"
  AllowBranches []string // glob allowlist of admitted scopes
  DenyBranches  []string // glob denylist; ALWAYS wins (main/master/release/*)
  Environments  []string // Deploy only; prod* always denied structurally
  MinSuccesses  int      // ≥N verifier-green for this (Type,scope)
  MinSample     int      // ≥ total observations (guards a 1-of-1 fluke)
  RecencyDays   int      // a green within this window
  MaxPerDay     int      // rate limit per UTC day, per class
  MaxDollarsDay float64  // $/day ceiling (Deploy); composed with blastbudget
}
```
`Validate` rejects an unknown `Type`, `MinSuccesses<1`, `MinSample<MinSuccesses`, `RecencyDays<1`, `MaxPerDay<1`, negative dollars — and **a blank trust bar is rejected, never read as "unlimited"** (fail-closed).

### Safe presets — "easy = safe"
The whole feature turns on with one choice in `nilcore init` (Enter = off) or `NILCORE_AUTOAPPROVE_PRESET` for CI. **No preset ever admits `main`/`master`/`release`/`prod`.**

| Preset | Classes | Trust bar | Rate | $ |
|---|---|---|---|---|
| **conservative** | OpenPR only | 5 green / 5 sample / 14d | 3/day | $0 |
| **standard** | + PromoteToBase on **non-main** branches | 10 / 10 / 14d | 2/day | $0 |
| **trusted** | + Deploy to **staging** (`prod*` always denied) | 20 / 20 / 7d | 2/day | $25/day |

### The `GradedApprover` algorithm (`ApproveStructured`)
On every decision, in order — any failure falls through to the human approver and logs an `auto_deny{reason}`:
1. **Kill-switch first** — sentinel file (`.nilcore/AUTOAPPROVE_OFF`) or `NILCORE_AUTOAPPROVE_OFF=1` ⇒ delegate to human, instantly, no restart (mirrors the `--mac-host` kill-switch).
2. **Eligibility** — no `ClassClause` for this `Type` ⇒ human.
3. **Blast radius** — `DenyBranches`/`prod*` always win; `AllowBranches` must admit the scope.
4. **Trust bar** — `Green≥MinSuccesses ∧ Total≥MinSample ∧ LastGreen within RecencyDays ∧ ChainOK`.
5. **Rate + dollars** — per-UTC-day count `<MaxPerDay`; day-spend via the **shared `blastbudget`** meter (never a second counter).
6. **Pass ⇒** emit `auto_approve` with the **full evidence object** and return true. The free-text `Approve(string)` path **always delegates to the human** — free-text gates are never auto-approved.

### The earned-trust source (`boundary_outcome`)
A **dedicated, hash-chained** event emitted at each gate site *after the verifier verdict on the tip* — `Detail:{action, scope, passed:<verifier verdict, never SelfClaimed>, chain}`. `graapprove.BuildTrust(logPath)` folds these by `(Type,scope)`, then runs `eventlog.Verify`; **on a broken chain it returns empty tallies + `ChainOK=false`** (earns nothing over a tampered log — a tampered log can only *remove* trust, never forge it). Per the review: **the trust numerator counts only verifier-judged downstream outcomes and excludes prior auto-approval grants** (no self-reinforcement), and a chain-verify *error* (distinct from *empty*) denies explicitly.

### Audit, undo, revocation
Every `auto_approve`/`auto_deny`/`boundary_outcome` is append-only and renders in `nilcore trace`/`report` (marked untrusted over a broken chain). The **undo story per class** is documented: OpenPR ⇒ close the draft (no merge ever happens); PromoteToBase(non-main) ⇒ reset/delete the non-main branch; Deploy(staging) ⇒ redeploy previous, bounded by the $/day + rate caps. The kill-switch reverts **all** classes instantly. The model **never sees** the envelope, the trust tallies, or the blast state (I3) — asserted by a cross-cutting test (§7).

### Default-off proof (three layers)
1. **No construction** — `MaybeWrap(human, env, …)` returns the human approver *unchanged* when no envelope/preset is configured; the `GradedApprover` is never allocated.
2. **Unchanged fall-through** — the one `GateStructured` branch is additive-above; a non-structured approver hits today's exact line (golden test).
3. **Fail-closed when on-but-unproven** — empty/zero envelope auto-approves nothing; unparseable ⇒ hard error + deny; broken chain ⇒ empty trust ⇒ human-gated.

---

## §6 The invariant & safety ledger

How all seven hold, with the adversarial review's fixes folded in (the load-bearing ones bolded):

- **I1 frozen backend contract** — untouched. New behaviour rides *additive optional fields* on the orchestrator (`Experience`, `Oracle`, `Blast`) and concrete `backend.Native` func fields (`EscalateAfterFn`), exactly as Phase 13 added `Selector`. `Run(ctx,Task)(Result,error)`, `Task`, `Result` unchanged.
- **I2 verifier sole authority** — the load-bearing one. Every learned signal is folded **only from verifier verdicts**, never `Result.SelfClaimed`. **Fixes:** vcache must re-verify on chain error and key on verifier-id+toolchain (never ship a cached verdict blindly); self-authored verifiers run only sandboxed, never in-process; the flywheel's `selfeval_report` fold is verifier-judged + chain-gated; auto-approval's trust excludes prior auto-approvals; the kernel always re-verifies the integrated tip even when children are green. The oracle/envelope gate **candidacy and who-presses-the-button, never shipping**.
- **I3 no ambient authority** — the envelope, trust tallies, and blast state are operator-authored host-side data with zero credentials, and **never enter a prompt, the `capability` event, planner context, or any model tool** (cross-cutting assertion, §7). Tier selection picks providers by name via the existing SecretStore cred resolver; the forge token stays a per-request header.
- **I4 model-emitted execution sandboxed** — the experience/capability/blast leaves are pure host-side read/compute; none executes model-emitted code. The capability descriptor *strengthens* I4 legibility (one place decides shell + egress). The blast wall-time fence adds a second bound at the same sandbox choke-point.
- **I5 append-only + hash-chained** — every new event (`capability`, `boundary_outcome`, `auto_approve`, `auto_deny`, `blast_charge`, `blast_breach`) is a normal `Append`; nothing is mutated. All projections are **derived and rebuildable** read-only replays that fail-closed on a broken chain. **Fix:** the per-day auto-approval window and any in-memory trust/rate cache **rebuild from the log on restart** (no fail-open window reset).
- **I6 zero-dependency core** — every leaf is stdlib + permitted internal leaves; `go.mod` untouched; each new package carries a `deps_test.go`.
- **I7 untrusted input is data** — distilled lessons template **only structural fields**, never raw attacker-influenced output; `GateAction.Branch`/`Detail` (possibly PR-title-derived) are matched as pure data via glob/equality, never interpreted as policy; objective text the model can't write.

---

## §7 Cross-cutting guarantees

The review flagged pieces no single pillar owns. Each becomes a dedicated task in the program:

- **One runtime meter.** `internal/blastbudget` is the *sole* owner of daily-dollars + irreversible-count; Pillar 5 consults it (no second counter that can drift). (`XC-T01`)
- **No transitive opt-in.** A program-wide test proves **no single flag/env** (`-experience`, `-trust-route`, `--autonomy`, …) can make any `auto_approve` event reachable — each powerful relaxation needs its own recorded gate (the `NILCORE_DESKTOP_HOST` separation pattern). (`XC-T02`)
- **Model never sees policy.** One cross-cutting test asserts the envelope, trust cells, blast state, and any secret never reach `model.Client` across *all* prompt-feeding paths (planner, capability event, lessons memory, objectives text). (`XC-T03`)
- **Rebuild-from-log on boot.** A unified startup path rebuilds the blast per-day window, the `GradedApprover` rate meter, and trust caches from the append-only log, fail-closed. (`XC-T04`)
- **Revocation/undo surface.** One command lists every auto-approval taken with its evidence and the per-class revert (the kill-switch stops *future* decisions; this accounts for *past* ones). (`XC-T05`)
- **Objectives are operator-only.** A test proves `internal/objective` CRUD is unreachable from any sandboxed model tool. (`XC-T06`)

---

## §8 The master DAG & wave order

The review's hard rule: **land and prove the evidence + fence substrate before any auto-approval can fire.** Five waves; pillars within a wave parallelize.

```
WAVE A  (foundations, parallel)
  EXP-T01..T08   experience layer + capability descriptor          (Pillar 1)
  BR-T01..T04    blastbudget leaf + the three fence seams          (Pillar 6, hard fence first)

WAVE B  (evidence accrual, parallel; needs A)
  RTE-T01..T08   dynamic data-driven routing                       (Pillar 2)
  LRN-T01..T07   lessons-memory + self-acceptance + vcache         (Pillar 3, with I2/I4/I7 fixes)
  XC-T01..T04    one-meter, no-transitive-opt-in, model-blind, rebuild-on-boot

WAVE C  (graduated auto-approval; needs A+B proven)
  GAA-T01..T08   GradedApprover                                    (Pillar 5)
  BR-T05..T06    blast wiring + canonical promotion
  Hard preconds: eventlog.Verify-gated TrustView proven fail-closed;
                 blastbudget is the single $/rate meter (BR before GAA-T06);
                 boundary_outcome (GAA-T04) has accrued verifier-judged samples.

WAVE D  (the flywheel; needs B+C)
  SIF-T01..T09   self-improvement flywheel                         (Pillar 4)
  XC-T05         revocation/undo surface
  The self-improve auto-approval CLASS is the LAST auto-approval consumer enabled (§10).

WAVE E  (autonomy surface + the kernel; E.daemon ∥ C/D, E.kernel dead last)
  AUTO-T01..T08  autonomy daemon + objectives backlog              (Pillar 7) + XC-T06
  UOK-T01..T10   unified orchestration kernel                      (Pillar 8, §0-gated cutover last)
```

**Hard ordering constraints:** `BR-T01..T04` before `GAA-T06` (single meter). `eventlog.Verify`-gated TrustView before any `auto_approve`. Pillar 5 **shipped + proven** before Pillar 4's auto-approve class. `UOK-T10` cutover after Pillars 1–7 merged.

---

## §9 Per-task specs

Format: `ID — goal · depends · owns · verify`. Acceptance criteria for the headline pillars (EXP, GAA, BR) are in their pillar sections (§4/§5) and the design record; the rest carry their goal + verify here. Every task's Definition of Done is `make verify` green + the named test + no invariant regression (the verifier decides, I2).

### Pillar 1 — experience layer + capability descriptor (`EXP`)
- **EXP-T01** — `experience.Reader` + `Aggregate` + `OverLog` replay (fail-closed). · — · `internal/experience/{reader,aggregate,overlog,deps_test}.go` · golden replay vs hand-computed; tamper ⇒ fail-closed.
- **EXP-T02** — store projection tables + typed queries *(SERIALIZED — store is a stable interface)*. · — · `internal/store/db/schema.sql`, `internal/store/experience.go` · old-DB opens clean; empty `exp_*` don't affect existing queries.
- **EXP-T03** — `Projector` single write path (`Fold`+`Rebuild`, one `apply()`). · T01,T02 · `internal/experience/{projector,apply}.go` · Rebuild==OverLog; double-Fold idempotent; self-claim ⇒ 0 passes.
- **EXP-T04** — `OverStore` hot reader. · T02,T03 · `internal/experience/overstore.go` · OverStore==OverLog parity.
- **EXP-T05** — `internal/capability` Descriptor + pure `For()`. · — · `internal/capability/{descriptor,for,deps_test}.go` · **golden generated from live legacy output at `chat.go:544`/`browse.go:150`/`desktop.go:200` for every mode** (review fix); `Event()` redaction asserts.
- **EXP-T06** — wire `capability.For` into chat (default-off, one `capability` event). · T05 · `cmd/nilcore/chat.go` · existing chat tests green; output unchanged per mode.
- **EXP-T07** — `nilcore experience` + `nilcore capability` read CLIs. · T01,T04,T05 · `cmd/nilcore/{experience,capability}.go`, main dispatch · broken-chain ⇒ non-zero; default path nil-field unchanged.
- **EXP-T08** — docs + CHANGELOG promotion *(SERIALIZED)*. · T01–T07 · `docs/ARCHITECTURE.md`, `docs/TASKS.md`, `CHANGELOG.md` · `make verify`.

### Pillar 2 — dynamic routing (`RTE`)
- **RTE-T01** — `trust.Classify` deterministic task-class buckets (keyword, no model call). · — · `internal/trust/classify.go` · table tests; pure.
- **RTE-T02** — ledger cost + per-class cell dimension. · T01 · `internal/trust/ledger.go` · fold cost/class; existing ledger tests green.
- **RTE-T03** — extend `Replay` to fold cost + class per attempt. · T02 · `internal/trust/replay.go` · parity + fail-closed preserved.
- **RTE-T04** — `agent.TrustOracle` + `RoutePlan` seam (nil-safe). · — · `internal/agent/oracle.go` · nil ⇒ static branch byte-identical.
- **RTE-T05** — wire oracle into single + race + escalate paths. · T04 · `internal/agent/orchestrator.go`, `internal/backend/native.go` (`EscalateAfterFn`) · static-equiv test with nil.
- **RTE-T06** — cost-aware `trust.Oracle` impl. · T02,T04 · `internal/trust/oracle.go` (injected `meter.Pricer` + nil-able `pool.Headroom` func) · cheapest-clearing-bar test; verifier still gates.
- **RTE-T07** — `ARCHITECTURE.md` records the `TrustOracle` seam *(SERIALIZED)*. · T04 · `docs/ARCHITECTURE.md` · reviewer confirm.
- **RTE-T08** — `wireTrustRoute` + gated cost event + flags (`-trust-route`/`NILCORE_TRUST_DEFAULT`). · T05,T06 · `cmd/nilcore/main.go` · gate-off log byte-identical (P11-T05-style assertion).

### Pillar 3 — learn from scars (`LRN`)
- **LRN-T01** — additive verify-event enrichment (verifier-id, fail-class, content-hash, toolchain). · — · `internal/verify` (new file) · additive Detail keys; gate-off identical.
- **LRN-T02** — A8 lessons distiller (read-over-log → `memory.Record`, **structural fields only**). · T01 · `internal/memory/lessons/` · dedupe; I7 no-raw-output test.
- **LRN-T03** — A8 wiring + `nilcore lessons` (default-off). · T02 · `cmd/nilcore/lessons.go` · `NILCORE_LESSONS` off ⇒ identical.
- **LRN-T04** — A9 content-hash verify cache (**`Lookup` calls `eventlog.Verify`, fail-closed-to-recompute**). · T01 · `internal/verify/vcache/` · chain-error ⇒ recompute (review I2 fix); key includes verifier-id+toolchain.
- **LRN-T05** — A9 cache wiring + optional store index (default-off). · T04 · `cmd/nilcore`, `internal/store` · `NILCORE_VCACHE` off ⇒ identical.
- **LRN-T06** — self-acceptance + candidate-verifier authoring + meta-check (**sandboxed verifiers only**, review I4 fix). · T01 · `internal/verify/selfacc/` · unproven ⇒ `Unverifiable`, never `Pass`.
- **LRN-T07** — docs + CHANGELOG *(SERIALIZED)*. · T03,T05,T06 · `docs/*`, `CHANGELOG.md` · `make verify`.

### Pillar 4 — self-improvement flywheel (`SIF`)
- **SIF-T01** — freeze a content-hashed self-eval suite. · — · `eval/self/` · hash stable; no model call to freeze.
- **SIF-T02** — `selfeval`: run harness on the agent, fold to trust (**verifier-judged + chain-gated**, review I2 fix). · T01 · `internal/flywheel/selfeval/` · self-report can't inflate standing.
- **SIF-T03** — `distiller`: mine recurring failure patterns (read-only, fail-closed; shared with LRN). · — · `internal/flywheel/distiller/` · tamper ⇒ empty.
- **SIF-T04** — `measure`: the regression fence (measured delta). · T01 · `internal/flywheel/measure/` · delta computed from eval, not vibes.
- **SIF-T05** — `selfimprove.Flow`: additive measured-delta fence (nil `Measure` ⇒ propose-edit byte-identical). · T04 · `internal/selfimprove/` · inert-when-nil test.
- **SIF-T06** — `loop`: bounded standing cadence driver. · T02,T03,T05 · `internal/flywheel/loop/` · bounded; no eval-set/verifier self-edit.
- **SIF-T07** — `ClassSelfImprove` auto-approval class (Pillar 5 compose seam; **separate §10 opt-in**). · T05 · `internal/graapprove` · off unless `NILCORE_SELFIMPROVE_AUTOAPPROVE`.
- **SIF-T08** — cmd wiring: serve background loop + `nilcore flywheel --once` + trust fold. · T06,T07 · `cmd/nilcore/flywheel.go`, `internal/server` · default-off.
- **SIF-T09** — `docs/ROADMAP-SELF-IMPROVEMENT.md` + ARCHITECTURE extension points *(SERIALIZED)*. · T08 · `docs/*` · reviewer confirm.

### Pillar 5 — graduated auto-approval (`GAA`)
- **GAA-T01** — additive `StructuredApprover` seam *(CONTRACT, serialized)*. · — · `internal/policy/gateaction.go` · golden: `ConsoleApprover` identical before/after, every `GateActionType`.
- **GAA-T02** — `Envelope`/`ClassClause` schema + `Validate` + `onboard.Config.AutoApprove` (v2→3 no-op migration). · — · `internal/graapprove/envelope.go`, `internal/onboard/onboard.go` · v2 config byte-identical; malformed ⇒ error.
- **GAA-T03** — safe presets `Preset(conservative|standard|trusted)`. · T02 · `internal/graapprove/presets.go` · **no preset admits main/master/release/prod** (asserted).
- **GAA-T04** — `boundary_outcome` event at the gate sites (verifier-judged source). · — · `internal/project/project.go`, `cmd/nilcore/{swarm,openpr}.go` · appended after a green promote; `passed` never a self-report.
- **GAA-T05** — `TrustView`: read-only fold + `eventlog.Verify` fail-closed (excludes prior auto-approvals). · T04 · `internal/graapprove/trust.go` · tamper ⇒ empty + `ChainOK=false`; chain-error denies explicitly.
- **GAA-T06** — `GradedApprover`: algorithm + kill-switch + rate/$ via **shared blastbudget** + audit events. · T02,T03,T05 · `internal/graapprove/{graded,killswitch,meter}.go` · table test per fall-through reason + one all-pass; injected clock; `-race`.
- **GAA-T07** — `MaybeWrap` construction gate + wire all five `GateStructured` consumers. · T01,T06 · `cmd/nilcore/{build,swarm,openpr,main}.go` · no-config ⇒ every approver byte-identical.
- **GAA-T08** — audit surfacing + undo story + `docs/ROADMAP-GRADUATED-APPROVAL.md` + ARCHITECTURE relaxation record. · T06,T07 · `internal/trace`, `docs/ARCHITECTURE.md`, `CHANGELOG.md` · trace renders evidence; relaxation recorded.

### Pillar 6 — blast-radius budget (`BR`)
- **BR-T01** — `blastbudget` leaf (4 axes; fail-closed `Charge*`; nil-receiver no-ops; `Sink`). · — · `internal/blastbudget/` · sentinels `errors.Is`; idempotent host set; per-day roll; `-race`; deps stdlib-only.
- **BR-T02** — egress-proxy host-fan-out fence (charge after allowlist; 403 on breach). · T01 · `internal/policy/egress_proxy.go` · K hosts pass, K+1 ⇒ 403 no dial; nil ⇒ identical.
- **BR-T03** — sandbox wall-time fence (**pre-charge + `ctx.WithTimeout` from remaining budget** + reconcile, review fix). · T01 · `internal/sandbox/sandbox.go` · breach ⇒ non-zero Result, command never runs; bound actually enforced.
- **BR-T04** — gate-path irreversible + per-day $ fence (the Pillar-5 read point; composition law `min(P5,blast)`). · T01 · `internal/agent/orchestrator.go`, `internal/policy/gateaction.go`, `cmd/nilcore/build.go` · auto-approval over ceiling falls to human; human-approved doesn't decrement; `ReasonBlastRadius`.
- **BR-T05** — wiring, `-blast-*` flags, `-blast-radius` preset, event `Sink`. · T02,T03,T04 · `cmd/nilcore/{build,blast}.go` · all-unset ⇒ event-for-event identical baseline.
- **BR-T06** — docs + canonical promotion *(SERIALIZED; CLAUDE.md §8 + ARCHITECTURE + TASKS)*. · T01–T05 · `CLAUDE.md`, `docs/{ARCHITECTURE,TASKS}.md` · reviewer confirm.

### Pillar 7 — autonomy daemon + objectives (`AUTO`)
- **AUTO-T01** — `objective` store table + typed CRUD. · — · `internal/store`, schema · additive; old-DB clean.
- **AUTO-T02** — `internal/objective` leaf + idle-selection. · T01 · `internal/objective/` · `NextIdle`/`MarkRun`; deps stdlib.
- **AUTO-T03** — `autosrc` registry + bounded priority queue. · — · `internal/autosrc/` · `container/heap`; drivegate-bounded.
- **AUTO-T04** — existing sources as `autosrc` adapters (signals/cron/webhook/wake). · T03 · `internal/autosrc/adapters` · parity with today's verbs.
- **AUTO-T05** — backlog source (idle self-service). · T02,T03 · `internal/autosrc/backlog.go` · reversible auto, irreversible gated.
- **AUTO-T06** — daemon fold-in to `serve` + drivegate routing. · T04,T05 · `internal/server`, `cmd/nilcore/main.go` · default-off when no sources.
- **AUTO-T07** — `nilcore objective` management verb *(operator-only; XC-T06 test)*. · T01,T02 · `cmd/nilcore/objective.go` · unreachable from model tools.
- **AUTO-T08** — docs + audit-trace surface *(SERIALIZED)*. · T06,T07 · `docs/*` · trace shows daemon-started work.

### Pillar 8 — unified orchestration kernel (`UOK`, deferred/§0-gated)
- **UOK-T01** — staging doc `docs/ROADMAP-KERNEL.md`. · — · `docs/ROADMAP-KERNEL.md`.
- **UOK-T02** — kernel leaf: `Node`/`Envelope`/`Outcome` + deps guard (no agent/session import). · T01 · `internal/kernel/`.
- **UOK-T03** — `Granularity` policy interface + default sizer-backed policy. · T02.
- **UOK-T04** — FLAT branch: single-task run + I2 re-verify + race escalation. · T03.
- **UOK-T05** — DECOMPOSE branch: recursive fan-out + serial integrate + **tip re-verify even when children green** (review I2 fix). · T04.
- **UOK-T06** — until-clean requeue (project loop + swarm controller unified). · T05.
- **UOK-T07** — durable resume + eventlog parity (trace/report/queue unchanged). · T06.
- **UOK-T08** — kernel presets: Run/Build/Swarm envelopes + Granularity policies. · T07 · `internal/kernel/presets/`.
- **UOK-T09** — equivalence harness: legacy-vs-kernel eventlog-sequence golden across **every** I2/gate-bearing path. · T08.
- **UOK-T10** — **THE CUTOVER** *(SERIALIZED, §0-recorded)*: route run/build/swarm + chat presets through the kernel + contract-file updates. · T09 · `CLAUDE.md §1/§8`, `docs/{ARCHITECTURE,TASKS}.md`, entrypoints.

### Cross-cutting (`XC`)
- **XC-T01** — `blastbudget` is the sole $/rate/irreversible meter (Pillar 5 consults it). · BR-T01 · composition test.
- **XC-T02** — no-transitive-opt-in test (no single flag reaches `auto_approve`). · GAA-T07.
- **XC-T03** — model-blind test (envelope/trust/blast/secret never reach `model.Client`). · EXP-T06, GAA-T06.
- **XC-T04** — rebuild-from-log-on-boot (blast window, rate meter, trust caches). · BR-T01, GAA-T05.
- **XC-T05** — revocation/undo surface (`nilcore auto-approvals` list + per-class revert). · GAA-T08.
- **XC-T06** — objectives operator-only (unreachable from sandboxed model tools). · AUTO-T07.

---

## §10 §0 gate decisions (recorded before code)

Per the project's thesis-gate discipline (like `CU-T00`/the EXT tier), these are **recorded operator decisions**, each documented in `docs/ARCHITECTURE.md` §"Closed-loop autonomy":

1. **RECORDED — Graduated auto-approval is a second human-gate relaxation** (parallel to the `--mac-host` I4 relaxation in `CLAUDE.md §2`). The exact granted blast-radius of each preset and the rule that **no preset ever admits main/master/release/prod** are recorded in the ARCHITECTURE preset table. *(Wave C — shipped.)*
2. **RECORDED — The self-improve auto-approval class** (`NILCORE_SELFIMPROVE_AUTOAPPROVE`) — the agent merging edits to its own prompts/skills without a human — is a **separate** double-opt-in from enabling the flywheel (`graapprove.SelfImproveGate`; XC-T02 enforces no transitive opt-in). *(Wave D — shipped, default-off.)*
3. **STILL DEFERRED — Letting the flywheel edit verifiers** (denied by `selfimprove.DefaultScope`) is a distinct decision to widen the self-edit allow-list past the frozen `verify` package — never an implicit scope widen. Not taken; the flywheel cannot author or edit the verifier of record.
4. **RECORDED — The blast-radius preset values** (`tight` = hosts 4 / irreversible 2 / wall 10m / $1-per-day; `standard` = hosts 8 / irreversible 5 / wall 20m / $5-per-day) are operator-approved policy, recorded in the ARCHITECTURE preset table.
5. **RECORDED + enforced in code — The composition rule** that **no single flag transitively enables auto-approval** — each powerful relaxation needs its own recorded gate (`XC-T02`).
6. **STILL DEFERRED — The kernel cutover (`UOK-T10`)** — re-homing run/build/swarm/chat onto one kernel and editing `CLAUDE.md`/`ARCHITECTURE.md`/`TASKS.md` — is a human-signed §0 decision taken only after Pillars 1–7 prove the substrate (now shipped, so the kernel is the remaining gated wave).

---

## §11 Contract-file changes & docs impact

Contract files are edited only in dedicated, serialized tasks (CLAUDE.md §5). The full footprint:

| Contract file | Tasks | Change |
|---|---|---|
| `internal/store` (schema + typed-query surface) | EXP-T02, AUTO-T01 | additive `exp_*` / `objective` tables + typed methods (`IF NOT EXISTS`) |
| `internal/policy/gateaction.go` | GAA-T01 | additive `StructuredApprover` + one branch |
| `docs/ARCHITECTURE.md` | EXP-T08, RTE-T07, SIF-T09, GAA-T08, BR-T06, UOK | new leaves in the layer map; the auto-approval relaxation record; the kernel |
| `docs/TASKS.md` | EXP-T08, BR-T06, UOK-T10 | the P16 task rows under "Later phases" |
| `CLAUDE.md §8` | BR-T06, UOK-T10 | repo-map entries (`internal/blastbudget`, kernel); I4/gate relaxation note |
| `CHANGELOG.md` | each pillar's last task | one `[Unreleased]` entry per merged task |
| **`internal/backend/backend.go`, `internal/channel/channel.go`, `go.mod`, `Makefile`** | — | **untouched** (I1, I6) |

`onboard.Config` (a leaf schema, not a §5 contract file) gains the `AutoApprove` block + objective/blast persistence with a versioned no-op migration so existing configs decode identically.

---

## §12 Honest caveats & risks

- **Auto-approval misconfiguration is the chief risk.** Mitigated by: fail-closed at every layer, presets that structurally deny `main`/`prod`, the shared blast fence as a hard ceiling, the instant kill-switch, and a per-class undo story. The feature is opt-in precisely because the right envelope is workload-specific.
- **Trust gaming via a tampered log** — defeated by `eventlog.Verify`: a broken chain earns *nothing* (empty tallies), so tampering can only *remove* trust.
- **Self-reinforcing trust** — the trust numerator excludes prior auto-approvals; only fresh verifier-judged outcomes count.
- **Feedback-loop pathologies in the flywheel** — bounded cadence, a regression fence (measured delta), rollback, and no self-modification of the eval set or the verifier-of-record.
- **Projection drift** — the projection is never authoritative; `Rebuild` re-derives it from the log and a test asserts `Rebuild==OverLog`.
- **The kernel cutover is the highest-risk change in the program** — hence dead last, behind a full equivalence harness and a §0 decision; every existing entrypoint stays byte-identical until the single cutover task.

---

## §13 Verification gates (proof obligations)

A pillar merges only when all hold (the verifier decides "done" — I2):

1. **`make verify` green** + the named per-task test + `-race` on every new concurrent leaf.
2. **Default-off golden** — with the feature's flag/env/config unset, a recorded run is *event-for-event identical* to the pre-pillar baseline (the byte-identical proof).
3. **`deps_test.go`** — every new leaf imports no orchestrator/agent and no module outside stdlib (I6).
4. **Fail-closed** — broken-chain, empty-config, and unparseable-config paths all deny (auto-approve nothing, rank nothing), proven by a tamper test reusing `trust_test.go`'s pattern.
5. **I2 chokepoint review** — a human confirms every learned fold reads only verifier verdicts, never `Result.SelfClaimed`, and that nothing added can ship unverified work or skip a verify.
6. **The cross-cutting tests (`XC-*`)** pass before Wave C ships: one meter, no transitive opt-in, model-blind, rebuild-on-boot.

---

*This plan promotes HORIZON candidates A6 (cost routing), A8 (lessons-memory), C6 (self-eval flywheel), and C7 (capability budget) and adds graduated auto-approval, the experience-layer unification, and the autonomy daemon into one coherent, invariant-preserving program. It was designed per-pillar against the real seams and adversarially reviewed against all seven invariants before being written.*
