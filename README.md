# NilCore

A tiny, robust coding agent. The harness is small; the model is the engine.
Intelligence is borrowed, not encoded — so the core stays small precisely
*because* the coding fluency lives in the model. Robustness comes from three
disciplines: the agent verifies its own work, everything executable is
sandboxed, and the loop is bounded and fully logged.

This repo is **Phase 0**: the smallest core that proves the loop converges —
take an instruction, change a sandboxed repo, run the project's checks, and
iterate until they pass. Everything else grows around this frozen core.

## Decided architecture

| Decision | Choice |
|---|---|
| Core role | **Hybrid** — own native coding loop *and* can delegate to Codex / Claude Code |
| Language / runtime | **Go** — single static binary, minimal ops, runs anywhere |
| Autonomy | **Auto for reversible, gate irreversible** (merge, push, deploy, prod writes, payments) |
| Deployment | **Both** — the same binary runs locally or on a VPS |
| Sandbox | **Containers** (Docker / Podman; Podman rootless preferred) |
| Routing | **Adaptive escalation, verifier as judge** — one backend by default → race best-of-N on hard/failed tasks → cross-model review at the irreversible gate |
| Channel | **Chat bot** (Telegram / Slack) — drive it from your phone |
| Memory | **Cross-project long-term** (SQLite-backed) |
| Budget | **Generous** — high caps, optimize for finishing |

## The core loop

```
gather context → model picks a tool → execute in sandbox → observe
      → VERIFY (the project's own build/typecheck/test/lint)
      → repeat until green or the step budget is exhausted
```

The verifier is the **source of truth for "done"**, not the model's claim — and
not a sub-agent's self-report. That single rule is what makes delegating coding
to black-box agents (Codex, Claude Code) safe: whatever writes the diff, *your*
checks decide whether it ships.

## Layers (each thin and swappable)

- `internal/model` — Anthropic Messages API client (stdlib http, zero deps).
- `internal/backend` — the `CodingBackend` contract and its three implementations:
  - `native` — NilCore's own loop (model → sandbox → verify).
  - `codex` — drives `codex exec --json --full-auto`.
  - `claude-code` — drives `claude -p --output-format stream-json`.
- `internal/sandbox` — container executor (microVM/namespace backends fit the same interface later).
- `internal/verify` — runs the project's checks; reports pass/fail.
- `internal/eventlog` — append-only JSONL audit trail (replayable; graduates to the SQLite memory store).
- `internal/policy` — reversibility classifier + human gate.
- `internal/agent` — the orchestrator: run a backend, then re-verify as the final gate.

Zero external dependencies — stdlib only. The whole core is meant to fit in your
head; capability is added at the edges as new backends, tools, and adapters.

## Run it (Phase 0)

Requires Go 1.23+ and a container runtime (`podman` or `docker`).

```sh
export ANTHROPIC_API_KEY=sk-...

# native loop: make a failing test pass in ./repo
go run ./cmd/nilcore \
  -dir ./repo \
  -goal "make the failing test in math_test.go pass" \
  -verify "go build ./... && go test ./..." \
  -runtime podman

# delegate the same task to Claude Code or Codex instead
go run ./cmd/nilcore -dir ./repo -goal "..." -backend claude-code
go run ./cmd/nilcore -dir ./repo -goal "..." -backend codex
```

Flags: `-backend native|codex|claude-code`, `-image`, `-max-steps`, `-log`.
Model is configurable with `NILCORE_MODEL` (default `claude-sonnet-4-6`).
Secrets come from the environment and are never written to disk or into a prompt.

Every step is appended to `nilcore.events.jsonl` — read it to see exactly what
the agent did and why.

## Roadmap

- **Phase 0 (here)** — the loop + 3 backends behind one contract + container sandbox + verifier + event log.
- **Phase 1** — git worktree-per-task, the irreversible-action gate wired to a real approver, the Telegram/Slack channel (gates become chat replies).
- **Phase 2** — security hardening: rootless containers, egress allowlist, per-run secret injection, tool-call policy engine, full audit.
- **Phase 3** — orchestration: planner/executor split, scoped subworkers with budgets, and the routing policy (race best-of-N, cross-model review).
- **Phase 4** — cross-project memory in SQLite: conventions, decisions, learned facts retrieved into context at task start.
- **Phase 5** — gated self-improvement: the agent proposes changes to its own tools/prompts, behind human review and the same verifier.

## Soul

- The harness is small; the model is the engine. Borrow intelligence, don't reimplement it.
- The agent earns trust by verifying, not asserting. Nothing is "done" until it builds, types, tests, and lints clean.
- No ambient authority. Every capability is granted, scoped, and revocable.
- One loop, fully observable. You can always read the trace and pull the plug.
- Evolve from a core that fits in your head. If you can't hold it all in your head, it's too big.
- A little copying beats a premature abstraction.
