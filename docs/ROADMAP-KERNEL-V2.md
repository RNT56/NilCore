# ROADMAP — Kernel V2 (completing the UOK)

Status: **router shipped**; **recursive `decompose` preset SHIPPED (K2-1 + K2-2, opt-in)** — the recursive engine now has a real production consumer. The learned router Oracle (K2-3) remains future. §4 / §5 / §6 below are kept as the original record; the "deferred" framing they describe was superseded when `decompose` was built — see §5 for the as-shipped state.
Prereq: `docs/ROADMAP-KERNEL.md` (the original UOK / Pillar 8). Read that first.

This document is the honest answer to "should we upgrade the kernel, and how?" It records
a design **discovery** that changed the plan, what we built instead, and what we chose NOT
to build (with reasons), so a future agent does not re-attempt a dead end.

---

## 1. Diagnosis — what the kernel actually was

After the UOK shipped (PRs #77/#78), the kernel was, in production, a **transparent
pass-through**:

- Every production envelope (`runViaKernel` / `buildViaKernel` / `swarmViaKernel`) sets
  **only `Flat`** — the wrapped legacy machine. None sets `Decompose`, `Granularity`, or
  `MaxDepth > 1`.
- `kernel.Recursive` — the genuinely-new native fan-out engine — is **never called outside
  tests**. `MaxDepth` defaults to 1.
- So `kernel.Run` always picks `Flat` and calls the same `orch.Execute` / `loop.Run` /
  `controller.Run` the pre-kernel code did. The machines still do their **own** internal
  decomposition (the project loop's slice fan-out, the swarm's multi-pass), opaque to the
  kernel.

The kernel therefore delivered **one real thing** — a single routed entry + an escape
hatch — and held **one IOU**: the recursive engine, built and tested but dormant.

The question "upgrade the kernel" is really: **cash the IOU, or retire it.**

---

## 2. The discovery — `project.Loop` does not fit `Recursive`

The original Phase-1 idea was "re-express `project.Loop` as kernel `Plan`/`Integrate`."
Reading the code killed that idea, and it is important to record why:

- `kernel.Recursive` is a **one-shot fan-out**: `plan(n) → run every child once → integrate
  once`. It models a *static* decomposition (a fixed set of children).
- `project.Loop.Run` is an **iterative reflect-loop**: `for iteration { plan ONE slice →
  run it → judge → reflect/narrow/switch/stop-ask → repeat }`, bounded by interleaved rails
  (halt-gates, no-progress ladder, budget, deadline, log-broken). It is *dynamic* — the
  next slice depends on the verdict of the last.

Forcing the proven iterative loop onto the one-shot engine would be a **lossy, risky fit**:
either the kernel grows an iterate primitive (a large, invariant-adjacent overhaul of a
provably-terminating loop), or the loop is flattened and loses its reflect/recovery rails.
Per `CLAUDE.md` ("when in doubt do less; never guess on invariants; pick a different
approach rather than improvise into a bad fit"), **we do not do this.** The swarm
controller (requeue-until-clean) is likewise its own optimal shape and is not re-expressed.

**Conclusion:** the recursive engine is for genuinely *static* fan-out (a goal that splits
into independent sub-goals each run to green and merged). The two existing DECOMPOSE
machines are not that shape and stay as they are.

---

## 3. What we built — the router layer (the kernel's stated purpose)

The kernel's own package doc says its purpose is *"the conversational router picks an
ENVELOPE, not a machine."* That router **never existed** — the envelope was always chosen
by the human typing `run` / `build` / `swarm`. Building it is the upgrade that pays for the
kernel's existence, and it is **safe** (it routes to proven machines, re-expresses
nothing).

Shipped:

- **`internal/router`** — a pure stdlib leaf (deps-guarded), mirroring `agent.TrustOracle`:
  - `Classify(goal) Preset` — a deterministic keyword bucket choosing the cheapest preset
    that fits (`swarm` breadth signals → `build` project/scaffold signals → else `run`).
  - `Oracle` — the optional seam a learned/model-backed router (experience/lessons/trust
    informed) implements; `nil ⇒ Classify`.
  - `Route(ctx, oracle, goal, allowed) (Preset, provenance)` — fail-closed: a nil /
    no-opinion / out-of-bounds / invalid oracle pick falls back to the heuristic, and a
    heuristic pick outside `allowed` falls back to the first allowed preset. Never returns
    an unrunnable choice.
- **`nilcore do -goal "…"`** — one entry that routes the goal and dispatches to the chosen
  machine (`-dry-run` previews the decision; `-as` forces a preset; `-preset` passes a
  swarm preset). It adds NO execution of its own — it hands off to the existing,
  verifier-governed entrypoint, so **every invariant the chosen machine upholds is
  unchanged** (I2 verify, I3 gate, I5 log). The decision goes to stderr as the visible
  "agent picks how to work" moment.

Invariant posture: the router only ORDERS the choice of machine; it never decides "done"
(I2), never approves an irreversible action (I3), and treats the goal as inert data (I7).
It is additive and opt-in — the explicit `run` / `build` / `swarm` commands are unchanged.

---

## 4. What we deliberately did NOT build (and why)

These are specced here so they are a *choice*, not an oversight:

1. **Re-expressing `project.Loop` / `swarm` onto `Recursive`** — a forced fit (§2). Not done.
2. **Hardening the dormant `Recursive` engine** (bounded concurrency, budget threading,
   node-level event emission for resume) — real improvements, but they would harden code
   with **no production consumer**. We do not add machinery before a workload needs it
   ("the harness is small; do less"). When a genuine static-decompose workload appears,
   harden the engine *as part of building that consumer*, not before.
3. **A recursive `decompose` preset** (Plan = model goal-split, child = a full `run`,
   Integrate = merge N verified subtrees and re-verify the tip) — the genuine payoff of the
   recursive engine, and the natural next step. It is **not built now** because a correct
   cross-worktree **integrator** (merge each verified child branch, re-verify, drop a child
   whose merge breaks the build, conflict handling) is substantial and can only be
   validated against real repos/backends in CI — i.e. it is its own properly-scoped
   feature, not a safe side-effect of this change.

---

## 5. Cashing the recursion IOU — as shipped

- **K2-1 — SHIPPED (opt-in).** The `decompose` envelope wires `kernel.Recursive`: `Plan` =
  `decomposePlan` (a deterministic goal-splitter — newlines/list items, then top-level
  ` and `/`;`; a model splitter is the future seam), child `RunFunc` = a KeepBranch
  single-task run (via `buildRunOrchestrator` + `runViaKernel`), `Integrate` =
  `integrateBranches` — the cross-worktree merge-and-re-verify integrator (merge each
  verified child branch into a fresh integration worktree, re-verify after EVERY merge, and
  DROP any child that conflicts [`worktree.Merge` auto-aborts] or turns the tree red [new
  `worktree.Reset` undoes to the last green tip]). The integrated tip is the verifier's
  verdict, never the children's (I2). Reachable as `nilcore decompose`, `do -as decompose`,
  and a `router.Decompose` preset (OPT-IN — `Classify` never auto-selects it). The
  integrator's logic is hermetically tested against a real temp git repo (merge,
  drop-red-with-revert, drop-conflict, skip-unverified) + an end-to-end flow test through
  `kernel.Run`; the real multi-backend run is the field-validation step.
- **K2-2 — SHIPPED.** `Envelope.MaxChildren` (fail-closed width bound) + the `kernel.Observer`
  seam (node start/done events; the cmd `logObserver` records `decompose_node` to the log
  for audit/resume — I5), both pure-leaf. Bounded-concurrency fan-out (`MaxParallel`)
  remains future — `decompose` runs children sequentially today.
- **K2-3 — future.** The learned `router.Oracle`: route on experience/lessons/trust signals +
  a model estimate, replacing the heuristic when confident (heuristic stays the fail-closed
  floor), and selecting `decompose` for clearly-forking goals.

The recursive engine is no longer dormant — `decompose` is its production consumer.

---

## 6. Decision

Ship the router (done). Keep the recursive engine dormant-but-tested. Revisit §5 only when a
concrete static-decompose workload justifies the integrator. If that never comes, the
honest end-state is to **delete `Recursive`/`Granularity`/`MaxDepth`** and let the kernel be
the unified-entry + router layer it actually is — carrying a dormant engine indefinitely is
the one outcome to avoid.
