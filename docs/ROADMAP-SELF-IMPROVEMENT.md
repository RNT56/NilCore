# Roadmap — the self-improvement flywheel (Phase 16, Pillar 4)

> **Status:** shipped, opt-in, default-off. The flywheel is wired end-to-end (`nilcore flywheel` + an optional serve cadence); the auto-merge of its proposals is a separate double opt-in. Read with [`docs/ROADMAP-CLOSED-LOOP.md`](ROADMAP-CLOSED-LOOP.md) (the full program) and [`CLAUDE.md`](../CLAUDE.md) §2 (invariants).

## What it is

"It gets better while idle." The flywheel periodically evaluates the agent against a frozen self-eval suite, mines its own recurring verifier-failure scars, and proposes a prompt/skill remediation — which ships **only** if it is verifier-green (I2) and a human (or the separate auto-merge opt-in) approves. It never edits the verifier of record.

```
(1) BASELINE  score the content-hash-FROZEN self-eval suite (eval/self) → a verifier-judged pass-rate
(2) DISTILL   mine the append-only log for RECURRING verifier-failure patterns (fail-closed on a broken chain — I5)
(3) FENCE     keep a candidate ONLY if it MEASURABLY raises pass-rate over the frozen suite (the C6 regression fence)
(4) PROPOSE   route the candidate through the GATED selfimprove flow — verifier (I2) + human gate own the ship
```

## The pieces (all stdlib leaves, each with a `deps_test.go`)

| Piece | Package | Role |
|---|---|---|
| frozen suite | `eval/self` | a content-hashed, immutable self-eval set — never mutated by the loop (C6: no eval-set self-modification) |
| selfeval | `internal/flywheel/selfeval` | fold a verifier-judged eval report into the per-config trust evidence view — **verifier-judged + chain-gated**, so a self-report can't inflate standing. **Wired** (flywheel emits `selfeval_report`; `trust.Replay` folds it) — see "Selfeval trust-fold: wired" below |
| distiller | `internal/flywheel/distiller` | mine recurring failure patterns (read-only, fail-closed; structural fields only — I7) |
| measure | `internal/flywheel/measure` | the regression fence: a *measured* eval-delta, not a hunch |
| loop | `internal/flywheel/loop` | the bounded standing cadence composing the four above |
| flow | `internal/selfimprove` | the gated self-edit pipeline (scope-check → verified task → gate); the measured-delta fence lives at the loop level (one fence, one guarantee) |
| cmd | `cmd/nilcore/flywheel.go` | `nilcore flywheel [--once]` + the optional serve cadence |
| auto-merge class | `internal/graapprove.SelfImproveGate` | the SEPARATE double opt-in (`NILCORE_SELFIMPROVE_AUTOAPPROVE`) |

## Safety stance (the whole point)

- **I2 — the verifier is the sole authority on "done".** The loop folds nothing and ships nothing on its own. Baseline/candidate pass-rates are verifier-judged; the keep/drop is a measured delta; the edit only merges if the verifier is green. The loop can only ever DELAY or SKIP a proposal, never force a ship.
- **The verifier of record is never self-modified.** `selfimprove.DefaultScope` denies `internal/verify/` and the loop additionally screens every proposal's paths up front, so a target aimed at the verifier is dropped before it is ever run (charter §0).
- **The eval set is never mutated.** The loop loads a defensive copy of the content-hashed frozen suite and re-uses it for the baseline and every candidate, so a candidate cannot drop the cases it fails.
- **Bounded cadence.** `MaxIterations` caps cycles per run and `Interval` throttles them; the serve cadence runs one bounded cycle per (long, 6h) tick and honors ctx.
- **Auto-merge is a SEPARATE double opt-in.** Enabling the flywheel (`NILCORE_FLYWHEEL` / `nilcore flywheel`) does NOT enable auto-merge; that needs `NILCORE_SELFIMPROVE_AUTOAPPROVE` (no transitive opt-in — XC-T02), is reversible (a prompt/skill commit), and is audited (`auto_approve_selfimprove`).

## How to run it

```sh
nilcore flywheel --once       # run one bounded cycle, print a structural summary
nilcore flywheel -iterations 3 -interval 10m   # a short bounded standing cadence
NILCORE_FLYWHEEL=1 nilcore serve …             # fold a long-cadence flywheel into serve (one cycle / 6h)
# auto-merge a verifier-green, measured-improving self-edit (separate double opt-in):
NILCORE_FLYWHEEL=1 NILCORE_SELFIMPROVE_AUTOAPPROVE=1 nilcore serve …
```

## Candidate-aware fence (implemented; live behaviour field-validated)

The loop's regression fence is now **candidate-aware**: an optional `loop.Config.ScoreCandidate` seam scores the frozen suite WITH the candidate edit *applied* — `cmd/nilcore`'s `scoreFlywheelCandidate` cuts a scratch worktree, runs the proposal there (`KeepBranch`), merges the verified edit, and re-scores against the edited tree, so the fence reads a true before/after rather than two scores of the unchanged state. It is **FAIL-CLOSED**: any error in that pipeline (worktree, an unverified edit, a merge conflict) yields an empty report, which the fence reads as "no improvement" → the candidate is dropped (it can then only ever merge via the human gate in `Propose`, never auto). So a flaw in the scorer can only ever be *conservative* — the verifier (I2) and the gate remain the sole ship authority regardless of the scorer's sensitivity.

The seam is hermetically tested (the loop uses `ScoreCandidate` for the "after" score; an empty report drops the candidate); the **live** behaviour — does applying a prompt/skill edit measurably move the eval suite — is the field-validation step (the same posture the kernel/decompose recursive engine shipped under: opt-in + tested at the seam + proven in the field). When `ScoreCandidate` is unset the loop falls back to `RunSuite` (byte-identical to the prior conservative path).

## Rotation vs. distillation (B5-autonomy.8)

`serve` caps the live event log via `maint.RotateLog(logPath, serveLogMaxBytes)` (64 MiB): when the live log exceeds the cap it is renamed to `logPath+".1"` (single generation, replacing any prior `.1`) and a fresh, empty live log is recreated — which starts a **new genesis hash chain** (seq 0 / prev `""`), independently verifiable. The distiller previously replayed only the live log, so a recurring verifier-failure scar that *straddled* a rotation boundary could lose the occurrences that landed in `.1` and drop below `DefaultThreshold` (2), going silent exactly when it most needs surfacing.

The miner now replays **across generations**: `distiller.DistillAcross(threshold, paths…)` clusters the failures from every passed generation into one Pattern set, chain-verifying **each generation independently** and **failing closed per file** (a tampered or corrupt generation erases the whole result — never forges a scar, I5; a missing generation is a clean skip). `distiller.Distill` is now a single-generation shorthand for `DistillAcross`. The standing loop threads the rotated generation through the new `loop.Config.RotatedLogPaths` (the cmd layer passes `logPath+".1"`), so a scar crossing the rotation boundary still clears the recurrence threshold. The interaction is single-generation by design (matching `maint.RotateLog`'s single `.1`); a host needing deeper retention raises `serveLogMaxBytes` so rotation is rarer, or extends `RotatedLogPaths`.

## Selfeval trust-fold: wired (flywheel emits → trust.Replay folds)

`internal/flywheel/selfeval` ships the safety-sensitive **trust fold**: `NewVerifierJudged` wraps an `eval.Report` produced by the harness (so only a verifier-judged report can fold — I2), and `Fold` verifies the event-log chain first (fail-closed — I5) and emits one metadata-only `selfeval_report` event. It is now **wired end-to-end**: `cmd/nilcore/flywheel.go`'s `newFlywheelLoop` calls `selfeval.Fold(ctx, logPath, selfeval.NewVerifierJudged(report), nil, log)` after each baseline `RunSuite`, so every baseline emits a durable, chain-gated `selfeval_report`; and `trust.Replay` folds that event into the per-config **evidence view** (`Ledger.FoldEvalReport` → `Snapshot().Configs`, surfaced by `nilcore trust`). 

The fold is deliberately into the **config evidence view, NOT the backend routing standings** — only `race_outcome` feeds routing — so a self-eval pass-rate informs the operator ("which config earned its standing") without ever steering backend choice. `Fold` can only ever *raise* a config's recorded pass-rate from a verifier-judged, chain-verified report (I2/I5), and a broken chain folds nothing (fail-closed). The in-memory `*trust.Ledger` fold path (`Fold`'s `ledger` arg) is still available for a caller that holds a live routing ledger — the flywheel passes `nil` because its durable record is the event, which `trust.Replay` reconstructs.
