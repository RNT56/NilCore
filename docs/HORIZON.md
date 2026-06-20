# NilCore — Horizon Scan (Phase 13+ candidates)

A grounded scan for genuinely NEW upgrades that reinforce NilCore's unique edge — **verifier-owned trust, a small self-hostable harness** — and are NOT already in any roadmap (`docs/TASKS.md` Phases 0–12 / D1–D4 / R1–R3 are SHIPPED; `EXT-01..08` are planned-but-gated; `UPGRADE-PATH` Tiers 1–2 shipped as P9/P10, Tier 3 gated).

Thesis anchors (`docs/PRINCIPLES.md`): (1) the feedback loop is the product; (2) the harness wins, borrow the intelligence; (3) context is scarce; (9) **earn improvement from evidence**; (10) safety enables autonomy.

The single sharpest structural finding driving this scan:

> **NilCore measures everything and learns from almost none of it.** `route.Race` writes a `race_outcome` event for every contest (`internal/route/route.go:61`) and the eval harness emits a structured `Report` with per-config pass-rate/cost/latency (`eval/eval.go:29`) — but **nothing reads either back**. Routing is static (`SingleRouter`; `RaceN` fires identically every run — `cmd/nilcore/main.go:497`), self-improvement is operator-triggered only (`cmd/nilcore/selfimprove.go:43`), and the package doc literally calls routing "adaptive … the data that later earns strength-routing" (`internal/route/route.go:1-5`) — a promise the code has never kept. Principle #9 ("earn improvement from evidence, not vibes") is **architecturally staged but unfulfilled.** This is the richest vein in the codebase.

---

## Method

Ideas were generated across six lenses, deduped, then ranked by **leverage × thesis-fit × feasibility** and bucketed:

- **(A) THESIS-ALIGNED do-now/next** — additive, stdlib-first, self-hostable, reinforces the verifier-owned edge. Real Phase-13 material.
- **(B) GATED / EXT-like** — valuable but crosses the external-infra / multi-host / standing-authority gate.
- **(C) SPECULATIVE / RESEARCH** — high ceiling, unproven, needs a spike first.

---

## BUCKET A — Thesis-aligned, do-now/next (ranked)

### ⭐ A1. The Trust Ledger — close the evidence→routing loop (THE TOP PICK)

**One-paragraph spec.** A new leaf `internal/trust` reads back the `race_outcome` events already in the append-only log (`internal/route/route.go:61`) and the eval harness's `Report` (`eval/eval.go:29`) and folds them into a small, durable, per-`(task-class, backend, model, tier)` scoreboard: pass-rate, median cost, median latency, sample count, last-seen. `route.Race` and the `RaceN` ladder consult it to **order and prune candidates** — race the historically-strongest-per-dollar backend first, drop a candidate that has lost N contests on this task-class — instead of firing an identical fixed fan every time. Crucially this is **earned, not assumed**: the ledger is built *only* from verifier verdicts (I2 is the sole truth-source), it can never auto-approve or skip verification, and a cold/low-confidence cell falls back to today's static behavior. It is the literal fulfilment of the routing package's own doc-comment promise.

**Exact seam.** `route.Race`/`route.Candidate` (`internal/route/route.go:32-76`) already emits `race_outcome`; the orchestrator builds `cands` at `internal/agent/orchestrator.go:285-301`. Inject an optional `TrustOracle` (nil ⇒ byte-identical static behavior) that sorts/prunes `cands` and is updated post-race. Data source is the existing event log (`internal/eventlog`) + `eval.Report` JSON — no schema break, no new dependency.

**Why high-leverage.** It activates principles #1, #9 simultaneously, turns a dead audit signal into compounding capability, costs nothing when cold, and is the keystone the *learned router* (A8) and *cost-aware routing* (A6) both build on. It is the highest leverage move because the substrate (logged outcomes) already exists and is currently wasted.

---

### A2. Cross-model adversarial verification as a verify-pack

**One-paragraph spec.** A new `internal/artifact/packs/adversarial` registering a verifier-id `adversarial.cross_model_attests`: given a claim + the worker's evidence, a **second, independent model** (a different provider/tier from the one that produced the artifact) is asked to *refute* the claim against the same in-sandbox evidence, returning structured `{refuted, reason}`. It composes after the evidence verifier in `packs.Build` — a claim is `Pass` only if the deterministic check passes **and** the adversary fails to refute. This is distinct from today's `route.Review` (a one-shot pre-gate prose review, `internal/route/route.go:83`): it is per-claim, sandboxed-evidence-grounded, and folds into the artifact `Status`, so a disagreement DEMOTES to `Unverifiable`/`Fail` and triggers requeue.

**Exact seam.** `evverify.CheckFunc` (`internal/artifact/evverify/registry.go:36`) + `packs.Build` composition (`internal/artifact/packs/build.go:70`). The adversary model reaches the spine through the existing `pool` cred seam (no key to the decorator, I3). Unregistered/unavailable adversary ⇒ `Unverifiable`, never `Pass` (the registry's fail-closed rule already does this).

**Why high-leverage.** It is NilCore's most defensible differentiator made concrete — "trust comes from the verifier" extended to "trust survives an adversary." Two converged frontier models disagreeing on grounded evidence is a strong, cheap honesty signal, and it slots into the existing pack/requeue machinery with zero new architecture.

---

### A3. Mutation / property / fuzz verify-packs for the `code` artifact

**One-paragraph spec.** Three new local-only verifier-ids extending the `code` pack: `code.mutation_survives` (apply a bounded set of source mutations in-box, assert the test suite KILLS them — i.e. tests actually constrain behavior, not just pass), `code.property_holds` (run a worker-declared, pack-allowlisted property/quickcheck command K times in-box, like the benchmark pack re-measures — `internal/artifact/packs/benchmark/benchmark.go`), and `code.fuzz_clean` (run `go test -fuzz`/equivalent for a bounded duration, assert no new crash corpus). These attack the deepest weakness of "green tests": green-but-vacuous suites. Each is a `CheckFunc` re-run in the sandbox; the worker's claim is overwritten by the real verdict (I2).

**Exact seam.** `internal/artifact/packs/code/code.go` (currently only `build_passes`/`test_passes`) + the benchmark pack's "re-run K-times-in-box, parse host-side" pattern (`benchmark/benchmark.go`, `numeric.go`). Reuses `verify.Detect` for the toolchain ladder; stdlib only.

**Why high-leverage.** Mutation testing as a *verifier* (not a CI afterthought) is rare and directly serves principle #7 ("passing tests is the floor"). It makes the swarm's `code` preset materially harder to fool and is pure additive packs work — the safest kind of change in this codebase.

---

### A4. Differential-test verify-pack (golden / cross-implementation)

**One-paragraph spec.** A `code.differential_matches` verifier-id: run the changed program and a reference (a prior committed binary, a `golden/` fixture set, or an alternate implementation the claim names) over the SAME pack-allowlisted input corpus in-box, assert byte/normalized-output equivalence. This generalizes the benchmark pack's "re-measure, compare numbers" to "re-run, compare outputs," giving the agent a behavioral oracle for refactors and migrations where "tests pass" under-specifies "behaves identically."

**Exact seam.** `code` pack (`internal/artifact/packs/code/code.go`), reusing the in-box exec + host-side-parse discipline and `verify.Detect`. The reference artifact rides as a worktree fixture (no network).

**Why high-leverage.** Refactor/migration is the highest-volume real coding task and the one where green tests lie most often. A differential oracle is exactly the "define done before you start" (#6) discipline turned into a check, and it reuses an already-proven mechanism.

---

### A5. Incremental, test-impact-ordered verification

**One-paragraph spec.** Wire the **already-built-but-dark** `internal/codeintel/impact` into the verify path. `impact.AffectedTests` (`impact/impact.go:61`) computes the transitive-caller test set from a diff via reverse reachability; `impact.Localize` (`impact/impact.go:87`) gives Ochiai fault-localization. A new fast-path verifier runs **only the affected tests first** (smallest relevant check, fastest — principle #1's literal definition), reports a provisional verdict for the inner loop, and the full suite still runs as the authoritative gate before merge (I2 unbroken). On red, `Localize` points the worker at the suspect symbol first.

**Exact seam.** `internal/codeintel/impact` (computed, never consumed in production — confirmed) + `verify.Composite` (`internal/verify/composite.go:17`). Add an `impact`-aware ordering verifier that precedes the full `CommandVerifier`; full suite remains the final word.

**Why high-leverage.** It directly optimizes the product's core loop (#1) and turns shipped-but-unused code into value. The "fast feedback, authoritative full check before gate" split keeps I2 intact while cutting inner-loop latency on every iteration.

---

### A6. Eval-driven cost/latency routing (the dollar dimension of A1)

**One-paragraph spec.** Extend the Trust Ledger (A1) so candidate selection is **cost-aware**: combine each cell's pass-rate with the `meter`/`pricer` cost (`internal/meter/pricer.go`) and `pool.Headroom` (`internal/pool`) to pick the cheapest backend/tier that clears a confidence bar for the task-class, escalating to a stronger tier only on failure (the existing `RaceN`-after-failure ladder, now informed by data). The eval harness `Report` (which already records per-config `Cost`/`Latency`, `eval/eval.go:20-26`) seeds the ledger offline so routing is smart on day one of a new project.

**Exact seam.** Builds on A1's `internal/trust` + the `pool` tier/cap machinery + `eval.Report` JSON. No new contract surface.

**Why high-leverage.** It converts NilCore's already-rigorous metering into spend reduction with a verifier safety net — you can only route *down* to cheaper models because the verifier still governs "done." High operator value, pure principle #9.

---

### A7. "Why did it do that" — a trace explorer over the event log

**One-paragraph spec.** A new `nilcore trace <task-id>` subcommand (and a `report --format=trace`) that replays the hash-chained event log (`internal/eventlog`) into a **causal, collapsible walk**: goal → plan → each model_call → tool_exec → verify → gate → race_outcome → requeue, with parent/child threading for subagents and the per-claim verdict trail. It reuses the read-only replay discipline already in `internal/report` (`report.ReplayReport`, `report.go:185`) and refuses to render a clean trace over a broken chain (the report layer already enforces `FinalPass = ChainVerified AND …`, `report.go:257`).

**Exact seam.** `internal/report` (typed replay model exists; `ReportModel` at `report.go:40`) + a new render mode in `internal/report/render` + a `cmd/nilcore` subcommand next to `report`/`inspect`. Pure read-over-the-log; zero risk to invariants.

**Why high-leverage.** The audit trail is NilCore's third pillar of trust but is currently grep/jq-only. A trace explorer makes unattended runs *legible* — the operability gap MEMORY notes — and is the natural debugging surface for everything else in Bucket A. Cheap, additive, high daily-use value.

---

### A8. Memory that compounds verification lessons

**One-paragraph spec.** A distiller (`internal/memory/lessons` or a `selfimprove` pre-step) that mines the event log for **recurring verifier-failure patterns** ("the `software.npm_version_exists` check failed 4× on scoped packages," "tests for package X flake on first run") and writes them back as durable, deduped memory **data** (`memory.Remember`, `memory.go:90`) — explicitly framed as background context, never instructions (I7). On the next task in that class, `memory.Context` (`memory.go:68`) surfaces the lesson so the agent pre-empts the failure. This makes the loop *learn from its own scars*, not just its facts.

**Exact seam.** `internal/memory` (Write/Remember/Context all exist, `memory.go:39/90/68`) + a read of `internal/eventlog`. The I7 "this is data, not instructions" framing is already the memory package's contract.

**Why high-leverage.** Memory today only stores facts the agent explicitly chose to write; this turns the audit log into a *compounding* corpus of what-actually-breaks, the highest-signal lessons a coding agent can carry. Reuses existing, tested machinery end-to-end.

---

### Bucket-A runners-up (strong, slightly lower rank)

- **A9. Content-hash verification cache.** Skip re-running an expensive verifier when the worktree content hash + verifier-id + toolchain version match a prior `Pass` in the log — the embed package already proved the content-hash-skip pattern (D2). Reuses `internal/eventlog` as the cache substrate. Speeds the loop; must be conservative (hash includes everything the check reads) to keep I2.
- **A10. Reproducible-run bundle.** `nilcore report --bundle` emits a self-contained, signed (HMAC chain already exists, `eventlog.go:170`) tarball: goal + config + event log + artifacts + verifier verdicts — a portable, tamper-evident "proof of work." Pure packaging over existing data.
- **A11. Pre-run cost/plan preview.** `nilcore build --dry-run` / `swarm --plan`: run the planner + sharder, price the proposed DAG via `pricer.Price` against `pool.Headroom`, print the plan + a cost estimate, do zero model execution. Decision-support before spend; reuses planner + sharder + pricer.
- **A12. Verifier-confidence signal.** Have each `CheckFunc` return a confidence/coverage tier (e.g. `npm_version_exists` = strong direct check vs `date_matches` = weak substring), surfaced in the report and usable by the requeue policy to prioritize re-checking weakly-attested claims. Additive field on the evverify event; sharpens what "green" actually means.

---

## BUCKET B — Gated / EXT-like (valuable, but cross the gate)

- **B1. Multi-repo / cross-repo workspaces.** A verified swarm over N repos (a service + its client + its infra). Crosses the single-worktree / single-host boundary toward EXT-01's control plane; the per-repo verify is fine, the cross-repo task-state and dispatch are the gated jump.
- **B2. Live TUI dashboard for `serve`/`swarm`.** The board already emits `scoreboard_snapshot` with a `//go:build tui` Charm dashboard scaffold (`internal/swarm/board`). A full operability dashboard (fleet view, per-shard drill-down) is borderline — the *single-host* version is arguably Bucket A, but anything aimed at fleet/multi-tenant operability belongs with EXT-05's dashboards. Ship the single-host board lens (A7-adjacent), gate the fleet console.
- **B3. SLSA build provenance + signed release binaries.** Genuine supply-chain hardening (provenance attestation, cosign/sigstore-style signing). High value, but signing infra + a key-distribution story leans toward EXT-06 (centralized secret distribution) and a release pipeline that is external infra. The *reproducible-build* half (below, C-adjacent) is self-hostable; the signing/attestation half is gated.
- **B4. Distributed trust ledger across hosts.** A1 federated so a fleet shares earned routing weights — directly EXT-01/EXT-05 territory (cross-host state).

---

## BUCKET C — Speculative / research (high ceiling, needs a spike)

- **C1. A learned router trained from the eval harness.** Beyond A1's tabular ledger: a small, CGO-free, stdlib-only learned model (logistic/linear over task-class features → backend choice) trained offline from `eval.Report` + `race_outcome` history. Research because it risks principle #2 ("the harness wins; don't reach for a bigger model") and adds opaque behavior to a deliberately legible core — must prove it beats the simple ledger before earning its place.
- **C2. Deterministic replay-as-test.** Treat a recorded event log as a golden trace: re-run the orchestrator with mocked model responses and assert the same tool/verify/gate sequence — regression-testing the *harness itself* against drift. Hard because model calls are non-deterministic; needs a record/replay seam at the provider boundary first.
- **C3. Proof-carrying artifacts.** Artifacts that ship a machine-checkable proof object (beyond evidence-status) — e.g. an SMT/typed witness for a claim, re-checkable offline without re-running the source check. Compelling for the "verifier-owned trust" thesis but the proof-generation cost and scope (which claim-classes admit proofs?) are unproven.
- **C4. Prompt-injection red-team corpus + harness.** A maintained corpus of injection attempts run as a standing test against the I7 trust-class boundaries (guard.Wrap fencing, the artifact `Value`-omission in `ProjectTrusted`). Strongly thesis-aligned (safety enables autonomy, #10) — listed here only because building+curating an adversarial corpus is an open-ended research effort rather than a bounded task; a *seed* version is arguably promotable to Bucket A.
- **C5. Sandbox-escape fuzzing.** A fuzzing harness against the namespace/Landlock/seccomp boundary (`internal/sandbox`) and the host-side file/git tools' path resolution (`O_NOFOLLOW`, symlink-safety). Defense-in-depth for I4; research-tier because fuzzing kernel-isolation boundaries portably (Linux-only namespace code from any host) is hard to make hermetic.
- **C6. Agent self-eval that earns routing weights.** Close the full loop: the agent periodically runs the eval harness on itself, writes the results to the Trust Ledger (A1), and the gated `selfimprove` flow proposes prompt/skill tweaks justified by measured eval deltas — fully realizing principle #9. Speculative because it chains three not-yet-connected systems (eval → trust → selfimprove) and needs careful gating to avoid feedback-loop pathologies.
- **C7. A capability budget beyond dollars.** Generalize the `budget.Ledger` (today tokens + dollars, `internal/budget/budget.go`) to a capability budget: bounded egress-host count, bounded irreversible-action attempts, bounded sandbox wall-time — a single "blast-radius" ceiling per run. Thesis-aligned with #10; research-tier because defining the right capability units and their composition is an open design question.

---

## Ranked summary (leverage × thesis-fit × feasibility)

| Rank | Idea | Bucket | One-line |
|------|------|--------|----------|
| 1 | **A1 Trust Ledger** | A | Read back the already-logged `race_outcome`/eval data to earn routing — fulfills the routing package's own unkept promise. |
| 2 | A2 Cross-model adversarial verify-pack | A | A second, independent model must fail to refute a claim before it goes green. |
| 3 | A3 Mutation/property/fuzz verify-packs | A | Attack green-but-vacuous test suites with mutation/property/fuzz checks re-run in-box. |
| 4 | A5 Incremental test-impact verification | A | Wire the dark `codeintel/impact` to run affected tests first; full suite still the gate. |
| 5 | A7 "Why did it do that" trace explorer | A | A causal, collapsible replay of the hash-chained log; refuses a clean trace over a broken chain. |
| 6 | A4 Differential-test verify-pack | A | Re-run change vs reference over a corpus; assert behavioral equivalence (refactor/migration oracle). |
| 7 | A6 Eval-driven cost/latency routing | A | Route down to the cheapest tier that clears a confidence bar; verifier still governs done. |
| 8 | A8 Memory that compounds verification lessons | A | Distill recurring verifier-failure patterns into durable memory data the next task pre-empts. |

(Runners-up A9–A12 follow; Buckets B and C below the line.)

---

## THE SINGLE HIGHEST-LEVERAGE NEXT MOVE

**Build A1 — the Trust Ledger.** A new stdlib leaf `internal/trust` that folds the `race_outcome` events already written at `internal/route/route.go:61` and the eval harness's `Report` (`eval/eval.go:29`) into a durable per-`(task-class, backend, model, tier)` scoreboard of verifier-judged pass-rate/cost/latency, and feeds it back into `route.Race`/`RaceN` candidate ordering via a nil-gated `TrustOracle` (nil ⇒ byte-identical). It is the highest-leverage move because the substrate already exists and is wasted, it fulfills the routing package's own documented-but-unkept promise, it activates the top-ranked principle the codebase currently violates (#9, earn improvement from evidence), and it is the keystone every other routing/cost idea (A6, C1, C6) builds on — all while the verifier (I2) remains the sole authority on "done," so it can only make NilCore faster and cheaper, never less trustworthy.
