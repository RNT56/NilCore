# Changelog

All performed work across all parallel workstreams, in one place. This is how any agent sees what every other agent has shipped.

Format: [Keep a Changelog](https://keepachangelog.com/), adapted for parallel agents. **Every merged task appends exactly one entry** under `## [Unreleased]`, tagged with its task ID, the files it owned, and its phase. The log is **append-only** — never rewrite history; rebase before merge to resolve trivial append conflicts.

Entry shape:

```
- **<TASK-ID>** — <one-line summary of what shipped>. _Owns:_ <paths>. _(Phase N)_
```

On a release, the maintainer moves the accumulated `[Unreleased]` entries into a new dated, versioned section.

---

## [Unreleased]

_No tasks merged yet. Claim one from `docs/TASKS.md` and add your entry here._

---

## [0.1.0-phase0] — 2026-06-14

Phase 0 baseline — the smallest core that proves the loop converges. Established by the founding scaffold; recorded here as the starting point for all parallel work.

- **P0 (baseline)** — Core agent loop: model proposes a shell action → sandbox executes → verifier judges → repeat until green or budget exhausted. _Owns:_ internal/backend/native.go. _(Phase 0)_
- **P0 (baseline)** — `CodingBackend` contract (`Run(ctx, Task) (Result, error)`) with three implementations behind one seam: `native` (own loop), `codex` (`codex exec --json`), `claude-code` (`claude -p --output-format stream-json`). _Owns:_ internal/backend. _(Phase 0)_
- **P0 (baseline)** — Container sandbox executor (docker/podman, `--network none`, worktree bind-mounted at /work). _Owns:_ internal/sandbox. _(Phase 0)_
- **P0 (baseline)** — Verifier: runs the project's own checks and is the sole authority on "done." _Owns:_ internal/verify. _(Phase 0)_
- **P0 (baseline)** — Append-only JSONL event log. _Owns:_ internal/eventlog. _(Phase 0)_
- **P0 (baseline)** — Reversibility classifier + human gate (auto reversible, gate irreversible). _Owns:_ internal/policy. _(Phase 0)_
- **P0 (baseline)** — Orchestrator: runs a backend then re-verifies as the final gate. _Owns:_ internal/agent. _(Phase 0)_
- **P0 (baseline)** — Anthropic Messages API client, stdlib-only (zero external dependencies). _Owns:_ internal/model. _(Phase 0)_
- **P0 (baseline)** — Entrypoint, Makefile (`make verify`), README, and the docs suite (CLAUDE.md, PREREQUISITES, ARCHITECTURE, PERSONA, TASKS). _Owns:_ cmd/nilcore, Makefile, README.md, docs. _(Phase 0)_
- **P0 (baseline)** — Behavioral decisions folded into the plan: tool surface (shell + structured + MCP → tasks P1-T08/P1-T09), summarize-and-handover (P3-T06), proactive trigger (P3-T07), adaptive planning, terse persona, and the bounded self-improvement scope (P5-T01/P5-T02). _Owns:_ docs. _(Phase 0)_
- **P0 (baseline)** — Advisor-Executor (Anthropic's Advisor Strategy): cheap executor drives, strong advisor consulted on demand via `ask_advisor`; self-built + native-Advisor-Tool paths; advisor tier doubles as planner/reviewer (P3-T08). _Owns:_ docs. _(Phase 0)_
- **P0 (baseline)** — Renamed the project **Nullclaw → NilCore** across all code, the Go module/imports, the `nilcore` binary, env vars (`NILCORE_*`), and docs. _Owns:_ (whole tree). _(Phase 0)_
- **P0 (baseline)** — Updated the MCP model to **code execution with MCP** (Anthropic, Nov 2025): servers exposed as typed code APIs on the sandbox filesystem, on-demand discovery, in-sandbox result filtering, direct-call fallback (P1-T09). _Owns:_ docs. _(Phase 0)_
- **P0 (baseline)** — Multi-provider models (Anthropic + OpenAI + OpenRouter) behind one `Provider` interface, role→`provider:model` selection (P1-T10); `SecretStore` with keychain/encrypted/env backends and the never-to-the-model boundary (P1-T11, `docs/SECRETS.md`); `nilcore init` onboarding wizard (P1-T12); cross-platform paths + release matrix for macOS/Linux (P1-T13). _Owns:_ docs. _(Phase 0)_
- **P0 (baseline)** — **Design core** (`docs/PRINCIPLES.md`): ten ranked first principles ("the harness wins; the feedback loop is the product; engineer context ruthlessly; understand before you change; …"), anti-principles, and a definition of *good*. Added code intelligence / repo-map (P3-T09) to close the semantic-codebase-understanding gap, and elevated contract-first into the planner (P3-T01) and persona. _Owns:_ docs. _(Phase 0)_
- **P0 (baseline)** — **Runtime resilience & operations** (`docs/OPERATIONS.md`): the cluster that lets NilCore run **unattended** for months — the gaps an end-to-end audit surfaced. Adds **authorized control** (channel allowlist + gate-approval auth) as **P2-T07** (security), and a new **Phase 6 — runtime resilience & operations** (**P6-T01…P6-T08**): provider resilience (retry/backoff/failover/breaker), cost metering + ceiling enforcement, crash-safe task durability + resume + graceful shutdown, cross-task scheduler + resource arbitration, verification auto-detection for arbitrary repos, resource GC (worktrees/containers/logs), operator inspect/replay + health, and config schema/validation/migration. Context-window assembly is left intentionally distributed (not a task). No new runtime dependency; state is store-backed. _Owns:_ docs. _(Phase 0)_

> Note: the Phase-0 scaffold was authored offline and has **not** been compiled. Task **P0-T02** (compile + make verify green) is the first blocking task and must merge before any parallel Phase-1 work begins.
