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
| selfeval | `internal/flywheel/selfeval` | run the suite, fold to trust — **verifier-judged + chain-gated**, so a self-report can't inflate standing |
| distiller | `internal/flywheel/distiller` | mine recurring failure patterns (read-only, fail-closed; structural fields only — I7) |
| measure | `internal/flywheel/measure` | the regression fence: a *measured* eval-delta, not a hunch |
| loop | `internal/flywheel/loop` | the bounded standing cadence composing the four above |
| flow | `internal/selfimprove` | the gated self-edit pipeline (scope-check → verified task → gate); optional measured-delta hook (SIF-T05) |
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
