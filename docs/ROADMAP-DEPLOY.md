# Roadmap — the deployment capability (PLAN, not yet built)

> **Status:** PLAN only. NilCore does **not** deploy today. The graduated-auto-approval
> machinery already has a `Deploy` *gate class* (target-environment allowlist + a
> per-UTC-day dollar ceiling + an always-on `prod*` deny), but **no part of NilCore ever
> emits a `policy.Deploy` action or runs a deployment** — it ships code via
> `PromoteToBase` / `OpenPR` only. This document plans the capability end-to-end while
> keeping all seven invariants intact. It is intentionally conservative: deployment is
> the single most dangerous thing an autonomous agent can do — outward-facing,
> irreversible, and prod-touching — so it builds last, behind the strongest gates.

## 1. What already exists (the gate is ready; the action is not)

| Piece | Where | State |
|---|---|---|
| `policy.Deploy` gate-action class | `internal/policy/gateaction.go` | ✅ defined, classified `Irreversible` |
| `GateAction{Type: Deploy, Branch: <env>, Detail}` shape | `internal/policy` | ✅ (Branch carries the **target environment** for Deploy) |
| Deploy target-environment allowlist (`ClassClause.Environments`) | `internal/graapprove/envelope.go` | ✅ enforced in `graded.go` (Deploy-only; empty ⇒ deny) |
| Per-UTC-day **dollar ceiling** (`MaxDollarsDay`) | `internal/graapprove/{envelope,graded}.go` + `blastbudget` | ✅ charged through the shared blast fence |
| `prod*` structural deny + `isProtectedBase` (main/master/release) | `internal/graapprove/{meter,graded}.go` | ✅ always denies prod / protected bases |

**So the decision layer is built.** What is missing is everything that *produces* a
`Deploy` action and everything that *executes* one.

## 2. What's missing (the work)

1. **A deploy executor** — the thing that actually performs a deployment (the analogue
   of `internal/forge` for PRs). It must be a **leaf** that takes a typed, host-authored
   `DeployRequest` and runs a bounded, allowlisted deploy command/API call. It must NOT
   be model-callable directly (the model proposes; the host executes after the gate).
2. **A deploy action emitter** — a run/serve path that, after a verified build is
   promoted, can *propose* a `Deploy{env}` action to the gate. Almost certainly **not**
   the native loop (the model must not freely emit deploys); rather an operator verb
   (`nilcore deploy -env staging -artifact <ref>`) and/or a standing-objective-driven
   path, both gated.
3. **A deploy target registry** — operator-authored config mapping an environment name
   → how to deploy there (command template, or API endpoint + which secret), validated
   host-side. Never model-authored (I7/I3).
4. **Verification of the deployed state** — a post-deploy health/smoke check folded into
   the verifier so "deployed" means "deployed AND healthy", not "the command exited 0"
   (mirrors the browser behavioral verifier composing into `verify.Check` — I2).
5. **Rollback** — every deploy target needs a defined rollback (previous-revision
   redeploy / `kubectl rollout undo` / blue-green swap-back). A deploy with no rollback
   path is refused at config validation.
6. **Secrets for the target** — deploy credentials resolved from the `SecretStore`,
   passed to the executor host-side, never to the model, never logged (I3) — exactly
   like the keyed-research and webhook-secret paths.
7. **Sandbox stance (the I4 tension)** — a deploy *reaches outside the worktree by
   design* (it talks to prod infra). This is a **recorded relaxation**, parallel to the
   `--mac-host` desktop tier: a deploy executor is the one path that may egress to a
   sanctioned deploy endpoint. It must be reached only behind its own opt-in, an explicit
   target allowlist, and the unconditional gate — never implied by any other capability.

## 3. Phased plan (each phase opt-in, default-off, invariant-preserving)

- **DEP-1 — Target registry + `DeployRequest` leaf.** A pure `internal/deploy` leaf:
  `Target{Name, Command|Endpoint, SecretRef, RollbackCommand}` loaded from operator
  config; `Validate()` rejects a target with no rollback, an empty command, or a `prod*`
  name without an extra explicit confirmation flag. No execution yet. *(deps_test leaf
  guard; imports no orchestrator.)*
- **DEP-2 — The executor (host-side, sandboxed-egress).** `Deploy(ctx, Target, artifact)`
  runs the bounded command (hardened-arg clamp, like the git tool) or the API call over
  the egress proxy with the target's allowlisted host only. Secret injected host-side.
  Returns a typed result; never auto-rolls-forward.
- **DEP-3 — The gate wiring.** A `nilcore deploy -env <name> -artifact <ref>` verb emits
  `policy.GateAction{Type: Deploy, Branch: env}` → the EXISTING graapprove path
  (Environments allowlist + `$/day` + prod deny). Headless serve deny-defaults; an
  attended operator approves; graduated auto-approval applies ONLY for an env that has
  earned trust inside the operator envelope (never prod).
- **DEP-4 — Post-deploy verification + rollback-on-red.** Compose a deploy health check
  into `verify.Check`; if the deployed state is red, the executor runs the target's
  rollback automatically and reports failure. "Done" = deployed AND verified-healthy (I2).
- **DEP-5 — Evidence + audit.** A `deploy` artifact pack (env, revision, health result,
  rollback-if-any) re-verified in-box; `deploy_*` append-only events for the trace.
- **DEP-6 (gated, EXT-tier) — autonomous deploys.** Only after DEP-1..5 prove out: allow
  a standing objective to *propose* a staging deploy, still gated, still never prod,
  still blast-fenced. This is the last and most-gated step (cf. the EXT §0 thesis gate).

## 4. Invariant analysis (why this is safe if built this way)

- **I2 (verifier is authority):** a deploy is "done" only when the post-deploy health
  check passes; a red deploy auto-rolls-back and reports failure. The model never
  self-reports a deploy as done.
- **I3 (no ambient authority):** deploy credentials live in the `SecretStore`, injected
  host-side into the executor, never reaching the model/prompt/log. The target registry
  is operator-authored.
- **I4 (sandbox boundary):** the deploy executor is a **recorded, explicit relaxation**
  (it must egress to prod infra) — reached only behind its own opt-in + target allowlist
  + the unconditional gate, exactly like the `--mac-host` tier. Every other path keeps I4.
- **I5 (append-only log):** `deploy_*` events record the gate decision, the target, the
  revision, and any rollback — replayable.
- **I7 (untrusted input is data):** the artifact/env names the model proposes are inert
  data; the *how* (command/endpoint/secret) is host config the model never authors.
- **The gate (irreversible ⇒ human):** Deploy is `Irreversible`; `prod*` and protected
  bases are structurally denied from auto-approval; staging auto-approval is earned-trust
  + envelope + blast-fenced only.

## 5. Risks & open questions (decide before building)

1. **Blast radius.** A wrong deploy is the highest-cost mistake the agent can make.
   Mitigation: prod is never auto-approvable; rollback is mandatory config; the `$/day`
   and per-day auto-approval count fence the staging case.
2. **Rollback correctness.** A rollback that itself fails is the nightmare path —
   require a *tested* rollback per target (a dry-run at config time) before DEP-3.
3. **Target-config trust.** The registry is operator-authored; if it could ever be
   model-influenced, I3/I7 break. Keep it strictly host-side (like the auto-approval
   envelope and objective backlog — operator-only surfaces).
4. **Health-check definition.** "Healthy" must be a real signal (smoke test / SLO probe),
   not just "process started". Reuse the browser/behavioral verifier pattern.
5. **Scope creep into orchestration.** Deploy stays a leaf the cmd layer wires; the
   orchestrator never imports it. Resist letting the loop emit deploys directly.

## 6. Recommendation

Build **DEP-1 → DEP-5** as the MVP (operator-verb-driven, staging-first, mandatory
rollback, health-verified, fully gated). Treat **DEP-6** (autonomous deploys) as
EXT-tier, behind the §0 thesis gate, only after the MVP has run in the field. Until
then, the existing graapprove `Deploy` gate class stays as the ready decision layer it
already is — documented here as forward-looking, not dead.
