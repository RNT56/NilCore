# Roadmap — Pillar 8: the Unified Orchestration Kernel (UOK)

> **Status:** SHIPPED + **default-ON** (Phase 16, Pillar 8; flipped to default-on in PR #78 after the equivalence harness stayed green — escape hatch `NILCORE_KERNEL=0|off|false|no` reverts to the legacy machine path, byte-identical). The §0 cutover routed `run`/`build`/`swarm`/`chat` through one kernel, proven against the legacy machines by an equivalence harness. **For the routing design this roadmap is superseded by [`docs/ROADMAP-KERNEL-V2.md`](ROADMAP-KERNEL-V2.md)** — which builds the preset router (`nilcore do`) the kernel was designed for and records that the native recursive engine (`MaxDepth>1`) is built + tested but **dormant in production** (every production envelope sets `Flat` only; the iterative project loop does not fit the one-shot recursion — see V2 for the full reasoning).
>
> **Read with:** [`CLAUDE.md`](../CLAUDE.md) §1/§2/§8, [`docs/ARCHITECTURE.md`](ARCHITECTURE.md) (the execution model + the frozen contract), [`docs/ROADMAP-CLOSED-LOOP.md`](ROADMAP-CLOSED-LOOP.md) §4 Pillar 8.

## §0 The thesis — five machines, one engine

NilCore today runs three orchestration machines behind four entrypoints:

| Entry | Machine | Shape |
|---|---|---|
| `nilcore run` / chat-native | `internal/agent` Orchestrator (`executeSingle`) | **FLAT** — one task in a worktree, verifier-judged, adaptive best-of-N race on verify-fail |
| `nilcore build` / chat-supervise | `internal/project` Loop (`Run`) | **DECOMPOSE** — slice a goal into units, integrate serially, re-verify the tip until clean |
| `nilcore swarm` | `internal/swarm` Controller (`Run`) | **DECOMPOSE** — bounded fan-out over a worklist, multi-pass until converged + verified |
| chat / serve / tui | `internal/session` (`SupervisorFirstRouter`) | picks WHICH machine per turn |

That is the "five products at once" the public-product feedback names. The unification: **one recursive `kernel.Run` over a `Node` that, per a `Granularity` policy, either runs the node flat or fans it out, integrates, and re-verifies the tip.** `run`/`build`/`swarm` become **`Envelope` presets**; the conversational router picks an envelope, not a machine.

## §1 The engine (shipped: `internal/kernel`, a pure leaf)

```
Run(ctx, env, node):
  if env.Granularity says Decompose  (and env has a Decompose runner, and node.Depth < env.MaxDepth):
      env.Decompose(node)      // fan out → integrate → RE-VERIFY THE TIP (I2)
  else:
      env.Flat(node)           // one task, verifier-judged
```

- **`Node{ID, Goal, Depth}`** — one unit of work; structural data only (no secret/policy — I3).
- **`Outcome{Verified, Summary, Detail, Branch, Backend}`** — `Verified` is ALWAYS the injected runner's verifier verdict; the kernel never sets it (I2).
- **`RunFunc` / `Plan` / `Integrate`** — the injected seams. The kernel imports NO machine (`deps_test.go` enforces the pure-stdlib leaf); the cmd layer wires the proven machines in.
- **`Granularity`** — `Decide(node, env) → Flat|Decompose`. Generalizes the router's machine-pick into one extensible policy. `AlwaysFlat` / `AlwaysDecompose` ship; the default is sizer-backed (the existing classifier).
- **`Recursive(&env, plan, integrate)`** — the kernel's NATIVE recursive fan-out: plan a node, run EACH child back through `Run` (so a child may itself decompose — the recursion), then integrate. `Integrate` MUST re-verify the integrated tip even when every child verified (green children can integrate red — the review's I2 fix). Bounded by `MaxDepth` (default **1**, which reproduces the legacy single-level fan-out exactly; **>1** is the new capability).

## §2 The two ways an Envelope drives a machine

1. **Monolithic wrap (the cutover path — safe + equivalent).** The envelope's `Flat` (or `Decompose`) runner is a thin closure over an EXISTING entry — `orch.Execute`, `loop.Run`, `controller.Run`. The kernel becomes the unified *entry* over the proven machines: same code, same event sequence, so a kernel-routed run is **event-for-event identical** to the legacy run (the equivalence harness proves it). This is what `NILCORE_KERNEL` activates at the cutover.
2. **Native recursion (the new capability — opt-in, `MaxDepth>1`).** The envelope's `Decompose` is `kernel.Recursive(plan, integrate)`; the kernel itself drives the fan-out and the tip re-verify. A machine re-expressed as `Plan`/`Integrate` seams gains genuine recursive decomposition (a unit too big for one slice decomposes again) — available, tested, and reachable without touching the entrypoints again.

The cutover ships (1); (2) is the engine's reason to exist, built and tested up front so the unification is real, not aspirational.

## §3 Presets

| Envelope | Flat | Decompose | Granularity | MaxDepth |
|---|---|---|---|---|
| `run` | `orch.Execute` wrap | — (or project-loop wrap under `-auto-supervise`) | pass-through (orch owns its own dispatch) | 1 |
| `build` | single-task fallback | `loop.Run` wrap | AlwaysDecompose | 1 |
| `swarm` | single-task fallback | `controller.Run` wrap | AlwaysDecompose | 1 |

The chat router maps its `Route` (native/supervise/project) onto these envelopes, so "the router picks an envelope, not a machine" — with the legacy `Route`→Driver path unchanged when `NILCORE_KERNEL` is off.

## §4 The invariant ledger

- **I1** — the kernel imports no `backend`/`agent`/`session`/`project`/`swarm` (pure leaf, `deps_test`); the frozen `backend.CodingBackend` contract is untouched (the machines plug in as closures).
- **I2** — the kernel NEVER marks a node done. `Outcome.Verified` comes only from the injected runner's verifier; the DECOMPOSE path re-verifies the integrated tip even when children are green. The kernel cannot ship.
- **I3** — `Node` carries only structural data; no secret/policy/envelope reaches it. Gating stays in the injected runners.
- **I4** — the kernel touches no filesystem/sandbox; the injected runners own all worktree/sandbox execution.
- **I5** — the kernel appends nothing; the injected runners own their event sequences, so the log is the legacy log (the equivalence harness asserts it).
- **I6** — stdlib-only leaf; `go.mod` untouched.
- **I7** — `Node.Goal` is inert data, never interpreted as policy by the kernel.

## §5 Task DAG

- **UOK-T01** — this doc. ✓
- **UOK-T02/T03** — `internal/kernel` leaf: `Node`/`Outcome`/`Branch`/`RunFunc`/`Plan`/`Integrate`/`Granularity`/`Envelope`/`Run` + `Recursive` + `deps_test`. ✓
- **UOK-T04/T05/T06** — cmd envelope adapters: FLAT (`orch.Execute` wrap) + DECOMPOSE (`loop.Run` / `controller.Run` wrap) + the until-clean (owned by the wrapped machines) + the I2 tip re-verify (the recursive engine + the wrapped machines' own converge).
- **UOK-T07/T08** — resume + eventlog parity (reuse the machines' checkpoint/log unchanged) + the `run`/`build`/`swarm` presets wired behind `NILCORE_KERNEL`.
- **UOK-T09** — the equivalence harness: a kernel-routed run produces the SAME event-log Kind sequence as the legacy machine, asserted across the FLAT + both DECOMPOSE shapes.
- **UOK-T10** — THE CUTOVER (§0-recorded): `run`/`build`/`swarm` + the chat router route through the kernel under `NILCORE_KERNEL`, legacy retained as the default fallback until equivalence is proven in the field; contract docs (`CLAUDE.md` §1/§8, `ARCHITECTURE.md`, `TASKS.md`).

## §6 Honest caveats

- The cutover ships the **monolithic-wrap** envelopes (equivalent + safe). **Native recursion** (`MaxDepth>1`, re-expressing the project loop / swarm as `Plan`/`Integrate`) is built + tested in the engine but is **dormant in production** — NO production envelope uses it (every envelope sets `Flat` only). [`docs/ROADMAP-KERNEL-V2.md`](ROADMAP-KERNEL-V2.md) records why the *iterative* project loop does not fit the *one-shot* recursion, and what a real recursive `decompose` preset would require before the engine has a live consumer.
- `NILCORE_KERNEL` is now **default-ON** (flipped in PR #78 after the equivalence harness stayed green): the kernel routes every entrypoint when unset; set `NILCORE_KERNEL=0|off|false|no` to revert to the legacy `Route`→machine path, byte-identical (the escape hatch retained for instant revert).
