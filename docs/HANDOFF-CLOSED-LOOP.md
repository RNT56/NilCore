# Handoff — Phase 16 "closing the loop" (Pillars 1–7 SHIPPED)

> **Status update:** Pillars 1–7 are now SHIPPED end-to-end — the activation wiring, the cross-cutting tests (XC-T01..T06), and the contract docs are all done (default-off, opt-in, invariant-preserving; `make verify` + `make tui-verify` green). The authoritative status is the [`docs/ROADMAP-CLOSED-LOOP.md`](ROADMAP-CLOSED-LOOP.md) header + §10, the [`docs/ARCHITECTURE.md`](ARCHITECTURE.md) §"Closed-loop autonomy" relaxation record, and the `CHANGELOG.md` entries. **Pillar 8 (the unified orchestration kernel — `UOK`) is now SHIPPED too** — the §0 cutover was authorized + merged (all entrypoints route through one `kernel.Run`, default-on, equivalence-proven), and **UOK V2** added the preset router (`nilcore do`); see [`docs/ROADMAP-KERNEL.md`](ROADMAP-KERNEL.md) + [`docs/ROADMAP-KERNEL-V2.md`](ROADMAP-KERNEL-V2.md). The historical "what remains" lists below are kept for provenance but are superseded by the shipped state.

Self-contained handover (historical, from the #71 era). **The hard, novel logic is built, tested, and committed; what remains is activation wiring across shared files + the cross-cutting tests + contract docs.**

## 0. Where the work lives

- **Worktree:** `/Users/mt/Programming/Schtack/nilcore-closed-loop` — **work here, NOT the shared checkout** (`…/NilCore`), which gets branch-switched by concurrent agents mid-session (see memory `worktree-collision-hazard`).
- **Branch:** `feat/closed-loop-roadmap`, based off `origin/main`. Clean tree, **15 commits, every one green** (`make verify` + `go test -race` + `golangci-lint` 0 issues).
- **The plan (source of truth):** [`docs/ROADMAP-CLOSED-LOOP.md`](ROADMAP-CLOSED-LOOP.md) — 8 pillars, the full task DAG (§8/§9), the §0 gate decisions (§10), the invariant ledger (§6).

## 1. Read first

| Doc | Why |
|---|---|
| [`docs/ROADMAP-CLOSED-LOOP.md`](ROADMAP-CLOSED-LOOP.md) | The plan + task specs + wave order + §0 decisions |
| [`docs/REFERENCE.md`](REFERENCE.md) | System map + package inventory + verified defaults |
| [`CLAUDE.md`](../CLAUDE.md) §2 (invariants), §5 (parallel-agent protocol, contract files) | The rules every change must hold |
| [`docs/ARCHITECTURE.md`](ARCHITECTURE.md) | The frozen contract + execution model (a contract file — serialized edits only) |
| [`docs/HORIZON.md`](HORIZON.md) | The diagnosis this program answers (A6/A8/C6/C7) |
| Memory `phase16-closed-loop` | Charter (locked decisions) + live build status |

**Locked charter (operator decisions — do not re-litigate):** build Pillars 1–7 (kernel/Pillar 8 stays §0-deferred); graduated auto-approval ships all 3 presets, default **conservative**, never auto-acts on main/prod; auto-merge of prompt/skill self-improvements is on the roadmap (separate double-opt-in); the flywheel may **not** edit the verifier-of-record.

## 2. What is DONE (don't rebuild)

All committed on the branch (commit → task):

| Commit | Task(s) | Landed |
|---|---|---|
| `9bdb612` | docs | The roadmap + REFERENCE + pointers |
| `513a368` | BR-T01 | `internal/blastbudget` — 4-axis fail-closed capability budget |
| `3386166` | EXP-T01 | `internal/experience` — Reader + OverLog (fail-closed replay) |
| `5679c1f` | EXP-T05 | `internal/capability` — per-drive Descriptor + pure `For()` |
| `dea627c` | GAA-T01 | `policy.StructuredApprover` seam (additive, byte-identical) |
| `fb95ae2` | GAA-T03/T05/T06 + Envelope | `internal/graapprove` — GradedApprover, Envelope/Validate, Preset, TrustView, MaybeWrap |
| `17e8e3e` | AUTO-T02/T03 | `internal/autosrc` (daemon + bounded PQ) + `internal/objective` (backlog leaf, narrow Store iface) |
| `846c712` | LRN-T02/T04/T06 | `internal/memory/lessons`, `internal/verify/vcache`, `internal/verify/selfacc` (all w/ review fixes) |
| `e3d6b33` | SIF-T02/T03/T04 | `internal/flywheel/{selfeval,distiller,measure}` |
| `535e71f` | BR-T02/T03 | blast fence wired into the egress proxy + sandbox wall-time |
| `691e3ed` | EXP-T02 | `internal/store` exp_* projection tables + typed methods |
| `7bf79d5` | GAA-T02 | `onboard.Config.AutoApprove` block, v2→v3 additive |
| `4fd2022` | RTE-T01 | `trust.Classify` (pure task-class buckets) |
| `eb4af95` | EXP-T03/T04 | `experience.Projector` (Rebuild/Fold) + `OverStore` — **experience layer complete** |
| `e1d0d63` | GAA-T07 (partial) | auto-approval **live at the supervised PromoteToBase gate** (`cmd/nilcore/autoapprove.go`) |

**The 9 new leaf packages + the foundations are complete and unit-tested.** Every leaf has a `deps_test.go` guard.

## 3. ⚠️ Critical integration gotchas (read before wiring)

1. **Auto-approval is wired but cannot fire yet.** `graapprove.TrustView` folds **`boundary_outcome`** events — and nothing emits them yet (**GAA-T04**). Until GAA-T04 lands, the trust ledger is always empty → every gate falls through to the human. **GAA-T04 is the #1 priority to make the headline functional.**
2. **The blast budget is threaded as `nil`** in `wrapAutoApprove` (build.go) and the GradedApprover's `$` meter until **BR-T05** mints + threads a `*blastbudget.Budget`. Until then the per-day `$` ceiling is structural-only.
3. **The experience layer is complete but unconsumed.** Nothing reads `experience.OverStore`/`Projector` yet — the orchestrator has no `Experience` field wired, and no `Projector.Fold` runs after event append. EXP-T06 + RTE wiring activate it.
4. **Standings are global (`Class:""`)** until **RTE-T02** adds the per-class cell and `race_outcome` Detail carries the class. `experience` and `store` already accept a class arg (reserved).
5. **The `objective` leaf uses a *narrow Store interface*** — confirm whether a **concrete `objective` table + `*store.Store` methods** exist; they are likely still TODO as part of AUTO wiring.
6. **Deploy gate scope:** `graapprove.scopeFor()` derives the scope from `GateAction.Branch` (the closed `GateAction` struct has **no Environment field**), so a Deploy gate site must put the environment name in `Branch`.
7. **Never wrap `swarmApprover()` (it returns nil)** — `wrapAutoApprove` already guards `human == nil`; keep that guard.
8. **The wiring helper already exists:** `cmd/nilcore/autoapprove.go` has `wrapAutoApprove(human, cfg, logPath, log, blast)` + `autoApproveSink`. Reuse it for the remaining gate sites; the default-off proof is `MaybeWrap`→`Empty()` returning the human unchanged.

## 4. Remaining TODOs

Format: `ID — what · owns · deps`. Acceptance details are in [`docs/ROADMAP-CLOSED-LOOP.md`](ROADMAP-CLOSED-LOOP.md) §9.

### Wave C — finish graduated auto-approval (HIGHEST VALUE)
- **GAA-T04** — emit `boundary_outcome{action,scope,passed:<verifier verdict, NEVER SelfClaimed>,chain}` after a verifier-green boundary. · `internal/project/project.go` (converge/PromoteToBase, ~line 334), `cmd/nilcore/openpr.go` (OpenPR), `cmd/nilcore/swarm.go` (clean path) · — **(makes auto-approval functional)**
- **GAA-T07 (finish)** — wrap the remaining approver sites with `wrapAutoApprove`: the open-pr caller (`cmd/nilcore/{watch,schedule}.go` → `openGatedPR`) and serve (`cmd/nilcore/main.go` ~1296/1456/1579/1599). · those files · GAA leaf
- **GAA-T08** — render `auto_approve`/`auto_deny` in `nilcore trace`; record the relaxation in `docs/ARCHITECTURE.md` (CONTRACT); `docs/ROADMAP-GRADUATED-APPROVAL.md`; the per-class undo story; CHANGELOG. · `internal/trace`, docs · GAA-T06/T07
- **BR-T04** — gate-path blast fence: optional `Orchestrator.Blast`; `Gate`/`GateStructured` charge irreversible + per-day-$ on the auto-approval branch only; breach → human; `ReasonBlastRadius`; composition law `min(P5, blast)`. · `internal/agent/orchestrator.go`, `internal/policy/gateaction.go`, `cmd/nilcore/build.go` · BR-T01
- **BR-T05** — `-blast-hosts/-irreversible/-wall/-auto-dollars` + `-blast-radius <preset>` + env; thread `*blastbudget.Budget` through `buildStack` → orchestrator/egressproxy/sandbox; sink onto eventlog; mint only when an axis is set (default-off); **then replace the `nil` blast in `wrapAutoApprove`**. · `cmd/nilcore/build.go`, `cmd/nilcore/blast.go` (new) · BR-T02/T03/T04
- **BR-T06** — docs (CLAUDE §8 + ARCHITECTURE + TASKS). · contract docs · BR-T01..T05

### Wave B — dynamic routing + experience activation
- **EXP-T06** — `capabilityForMode` delegates to `capability.For`; emit one `capability` event. Golden generated from **live legacy output** at the real call sites. · `cmd/nilcore/chat.go` · EXP-T05
- **EXP-T07** — `nilcore experience` + `nilcore capability` read CLIs. · `cmd/nilcore/{experience,capability}.go` + main dispatch · EXP-T01/T04/T05
- **Experience activation** — optional `Orchestrator.Experience` (OverStore) + run `Projector.Fold` after each event append. · `internal/agent`, `cmd/nilcore` · EXP-T03/T04
- **EXP-T08** — docs (CONTRACT). 
- **RTE-T02** — ledger cost + per-class cell. · `internal/trust/ledger.go`
- **RTE-T03** — extend `Replay` to fold cost+class. · `internal/trust/replay.go`
- **RTE-T04** — `agent.TrustOracle` + `RoutePlan` seam (nil-safe). · `internal/agent/oracle.go` (new)
- **RTE-T05** — wire oracle into single+race+escalate. · `internal/agent/orchestrator.go`, `internal/backend/native.go` (`EscalateAfterFn`)
- **RTE-T06** — cost-aware `trust.Oracle` impl (inject `meter.Pricer` + nil `pool.Headroom` func). · `internal/trust/oracle.go` (new)
- **RTE-T07** — ARCHITECTURE record (CONTRACT). **RTE-T08** — `wireTrustRoute` + `-trust-route`/`NILCORE_TRUST_DEFAULT`; gate-off log byte-identical. · `cmd/nilcore/main.go`

### Wave: learning + flywheel + autonomy activation
- **LRN-T01** — additive verify-event enrichment (verifier-id, fail-class, content-hash, toolchain). · `internal/verify` (new file)
- **LRN-T03** — A8 wiring + `nilcore lessons` (`NILCORE_LESSONS`, default-off). **LRN-T05** — A9 `vcache` wired into the verify path (`NILCORE_VCACHE`). **LRN-T07** — docs.
- **SIF-T01** — freeze a content-hashed self-eval suite (`eval/self/`). **SIF-T05** — `selfimprove.Flow` measured-delta fence (nil Measure ⇒ propose-edit byte-identical). **SIF-T06** — `internal/flywheel/loop` (composes selfeval+distiller+measure+selfimprove). **SIF-T07** — `ClassSelfImprove` auto-approve class (off unless `NILCORE_SELFIMPROVE_AUTOAPPROVE` — **separate §0 double-opt-in**). **SIF-T08** — serve background loop + `nilcore flywheel --once`. **SIF-T09** — docs.
- **AUTO-T01** — concrete `objective` store table + `*store.Store` methods (see gotcha #5). **AUTO-T04** — existing sources as `autosrc` adapters. **AUTO-T05** — backlog source (idle self-service). **AUTO-T06** — daemon fold into `serve` + drivegate routing. **AUTO-T07** — `nilcore objective` CLI (operator-only). **AUTO-T08** — docs.

### Cross-cutting tests (before/with Wave C)
- **XC-T01** blastbudget is the sole $/rate/irreversible meter (GAA-T06 consults it). **XC-T02** no single flag transitively enables `auto_approve`. **XC-T03** model never sees envelope/trust/blast/secret. **XC-T04** rebuild-from-log-on-boot (blast window, rate meter, trust caches). **XC-T05** revocation/undo surface (`nilcore auto-approvals` list + revert). **XC-T06** objectives unreachable from model tools.

### Final, serialized — contract docs
`docs/ARCHITECTURE.md` (layer map + relaxation record + TrustOracle/blastbudget extension points), `docs/TASKS.md` (P16 rows under "Later phases"), `CLAUDE.md §8` (repo-map: blastbudget, experience, capability, graapprove, autosrc, objective, flywheel), `CHANGELOG.md` ([Unreleased] entries).

### §0 operator decisions still pending (see roadmap §10)
Record before the dependent wave ships: the auto-approval relaxation + exact preset blast-radius values (before BR-T05/GAA-T08); enabling the self-improve auto-approve class (before SIF-T07 ships on); the kernel cutover (Pillar 8 — far future, deferred).

## 5. How to work (conventions)

- **Every commit:** `gofmt -w` + `go vet` + `golangci-lint run` (0 issues) + **whole-module `make verify`** + `go test -race` on touched packages. Commit messages: conventional, scoped, end with `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.
- **Default-off proof:** every feature must be byte-identical when unwired — add a golden test asserting the nil/empty path is unchanged.
- **New leaf:** add a `deps_test.go` (`go list -deps`) forbidding the orchestrator (and stdlib-only where applicable), mirroring `internal/trust/deps_test.go` or `internal/blastbudget/deps_test.go`.
- **Invariants (CLAUDE.md §2):** fold ONLY verifier verdicts (never `Result.SelfClaimed`, I2); read the event log read-only + `eventlog.Verify` fail-closed (I5); no secret/policy to the model (I3/I7); no new Go module (I6). Contract files (`backend.go`, `channel.go`, `CLAUDE.md`, `ARCHITECTURE.md`, `TASKS.md`, `go.mod`, `Makefile`) only in dedicated serialized tasks.
- **Parallelization technique that worked:** for NEW disjoint packages, fan out agents writing into THIS worktree (scoped self-verify `go test -race ./internal/<pkg>/...`, NO commit, NO whole-module build), then integrate (`make verify`) + commit yourself. For shared-file edits (cmd/orchestrator/etc.) → serial. *(Note: a subagent session limit was hit ~11am; it resets 1pm Europe/Berlin.)*

## 6. Recommended order
1. **GAA-T04** (boundary_outcome — unblocks the headline), then **GAA-T07 finish** + **BR-T04/T05** (auto-approval live + fenced on every gate).
2. **EXP-T06** + experience activation → **RTE-T02..T08** (dynamic routing turns the experience layer on).
3. **LRN/SIF/AUTO** activation wiring.
4. **XC-T01..T06** cross-cutting tests.
5. **Contract docs** (ARCHITECTURE/TASKS/CLAUDE/CHANGELOG) — last.
