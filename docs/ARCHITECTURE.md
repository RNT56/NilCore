# Architecture

The technical law of NilCore. `CLAUDE.md` §2 lists the invariants in brief; this document is their full statement and rationale, plus the layer map every task must respect. Changing anything here is a **serialized contract change** (see `CLAUDE.md` §5).

The *why* behind every choice below — the ranked first principles that make NilCore the best coding agent — is in **`docs/PRINCIPLES.md`**, which sits above this document in the philosophy stack.

## Decided choices

| Decision | Choice |
|---|---|
| Core role | **Hybrid** — own native coding loop *and* delegate to Codex / Claude Code |
| Language / runtime | **Go** — single static binary, minimal ops, runs anywhere |
| Autonomy | **Auto for reversible, gate irreversible** (merge, push, deploy, prod writes, payments) |
| Deployment | **Both** — same binary runs locally or on a VPS |
| Sandbox | **Containers** (Docker / Podman; Podman rootless preferred) |
| Routing | **Adaptive escalation, verifier as judge** — one backend by default → race best-of-N on hard/failed → cross-model review at the irreversible gate |
| Channel | **Chat bot** (Telegram / Slack) — drive it from a phone |
| Memory | **Cross-project long-term** (SQLite-backed) |
| Budget | **Generous** — high caps, optimize for finishing |
| Tool surface | **Shell + structured tools + MCP-as-code** — `run` escape hatch, structured read/write/edit/search/git as the auditable common path, MCP servers exposed as sandbox code APIs (code-execution MCP) |
| Planning | **Adaptive** — decompose complex tasks, interleave simple ones |
| Context mgmt | **Summarize-and-handover** — offload big subtasks to fresh-context subworkers seeded with a `ContextSummary` |
| Proactivity | **Proactive-act** — self-starts reversible work, gates the irreversible |
| Self-improvement | **Proactive trigger; prompts/skills/tools only, never the core; gated** |
| Persona | **Terse senior engineer** (runtime voice — see `docs/PERSONA.md`) |
| Model tiers | **Advisor-Executor** — a cheap executor (Sonnet/Haiku) drives the loop and consults a strong advisor (Opus) on demand |
| Providers | **Anthropic + OpenAI + OpenRouter** behind one `Provider` interface; model selection is role → `provider:model` |
| Credentials | **`SecretStore`** (OS keychain / encrypted-file vault / env / external) — secrets never reach the model (see `docs/SECRETS.md`) |
| Platforms | **macOS + Linux** (amd64/arm64) — one cross-compiled binary |

> Runtime behavior (voice, clarify-vs-act, proactivity, self-improvement, notifications, failure handling) is specified in **`docs/PERSONA.md`** and never overrides the invariants below.

## The core loop

```
gather context → model picks a tool → execute in sandbox → observe
      → VERIFY (the project's own build/typecheck/test/lint)
      → repeat until green or the step budget is exhausted
```

## Tool surface

The native loop exposes three tiers of tools, all registered through one registry (so adding a tool never edits the loop):

1. **Shell** (`run`) — the general-purpose escape hatch for the long tail.
2. **Structured tools** — `read`, `write`, `edit` (structured diff), `search`, and git operations. These are the **auditable, policy-scoped common path**: the tool-call policy engine (Phase 2) can constrain file access and commands precisely, which opaque shell cannot. Prefer these; fall back to shell.
3. **MCP via code execution** — MCP servers are presented as **typed code APIs on the sandbox filesystem** (e.g. `./mcp/servers/<server>/<tool>`), not as a wall of upfront tool definitions. The executor **discovers them on demand** by exploring the filesystem with its own `read`/`search` tools, loads only what it needs, and **writes code** that calls and chains them — filtering large results *inside the sandbox* before anything reaches context. A direct tool-call path remains for trivial one-shot calls.

This follows Anthropic's *Code execution with MCP* guidance (Nov 2025), which reported up to a ~98% token reduction versus loading every tool definition and routing each intermediate result through context. NilCore is unusually well-suited to it: it is *already* a sandboxed code-execution environment, so MCP-as-code reuses the container, the structured filesystem tools, and the same context discipline as summarize-and-handover — keep bulk data out of the model's window.

**MCP trust boundary:** MCP servers are third-party and **untrusted**. The wrappers are generated **deterministically from each server's declared schema** (not model-written); the executor's glue code runs in the sandbox under the gate (irreversible → human) and the prompt-injection guard (Phase 2), with per-server policy and default-deny egress. MCP is a sanctioned dependency (invariant I6), scoped to `internal/mcp`.

## Advisor-Executor (two-tier models)

Modeled on Anthropic's Advisor Strategy: a **cheap executor** model (Sonnet 4.6, or Haiku 4.5 for high volume) drives the native loop and does the bulk of the work; a **strong advisor** model (Opus 4.8, or Fable 5 for maximum capability) is consulted only when the executor needs it.

- **Escalation is a tool.** The executor has an `ask_advisor` tool in the registry. *It* decides when to call it — a decision above its skill, a task that needs planning, or a blocker it can't resolve. The harness seeds the advisor with a `ContextSummary`, returns the advisor's guidance (a plan, a correction, or a stop), and the executor resumes. **The advisor advises; the executor stays in control and executes.**
- **One strong model, three roles.** The advisor tier is also the Planner (up-front decomposition) and the cross-model reviewer (before the irreversible gate).
- **Two implementation paths.** Default: a **self-built `ask_advisor`** — a separate, fully-audited advisor call with a curated `ContextSummary` (provider-flexible, every escalation logged). Option (config toggle): Anthropic's **native Advisor Tool** — server-side, one request, minimal code, Claude-only.
- **Controls.** A per-task advisor-call ceiling and a separate advisor-token budget; every escalation logged as a distinct event (when / why / what); a harness fallback escalates after K consecutive verifier failures.
- **No new dependency** — both paths use the existing Messages client; the advisor is a model-string/config change.

This is orthogonal to backend routing (which selects native/codex/claude-code at the task level); Advisor-Executor is a model tier *inside* the native backend.

## Providers (multi-model)

NilCore's native loop talks to a `Provider`, not a single vendor. Three adapters implement it:

- **`anthropic`** — the Messages API (Opus 4.8, Sonnet 4.6, Haiku 4.5; Fable 5 is a configured option, currently disabled).
- **`openai`** — Chat Completions / Responses (GPT-5.5, 5.5-pro, 5.4-mini).
- **`openrouter`** — OpenAI-compatible; the OpenAI adapter pointed at `https://openrouter.ai/api/v1` with a `provider/model` namespace.

A **canonical internal message + tool format** is translated to each provider's wire shape (Anthropic `tool_use`/`tool_result` blocks ↔ OpenAI `tool_calls`/tool messages), so the loop, the tools, and the verifier are provider-agnostic. Model selection is **role → `provider:model`**: executor, advisor, and planner each resolve to any provider. Cross-provider Advisor-Executor (e.g. an Opus advisor over a GPT executor) works because the advisor call is NilCore's own (the self-built `ask_advisor`); Anthropic's native Advisor Tool remains a Claude-only fast-path.

## Credentials

Provider keys, delegated-CLI auth, channel tokens, and MCP credentials are held by a **`SecretStore`** (OS keychain / encrypted-file vault / env / external) and injected per run into request headers or child-process env — **never into a prompt, message, or context**. This is the operational form of invariant I3; the full design, including the headless-VPS master-key options and output/log redaction, is in **`docs/SECRETS.md`**.

## Platforms

One Go binary, cross-compiled for `darwin` and `linux` on `amd64`/`arm64`. A `paths` helper resolves per-OS config/data directories (XDG on Linux, Application Support on macOS); the container runtime (Podman/Docker) and the SecretStore backend auto-detect per host. Firecracker microVMs (the stronger Phase-2 isolation) are Linux/KVM only. Distribution: a Homebrew tap (macOS) and a curl-pipe-sh installer + systemd unit (Linux VPS).

## The frozen core contract (invariants)

These are the load-bearing guarantees. Code against them; do not erode them.

### I1 — One backend contract
```go
type Task struct {
    ID, Goal, Dir string
    Constraints   []string
}
type Result struct {
    Backend     string
    Summary     string
    SelfClaimed bool
}
type CodingBackend interface {
    Name() string
    Run(ctx context.Context, t Task) (Result, error)
}
```
The native loop, Codex, and Claude Code are interchangeable behind this. Adding a backend is additive and parallel-safe. Changing `Task`/`Result`/the interface is a dedicated serialized task and ripples to every backend at once.

### I2 — The verifier is the only authority on "done"
`Result.SelfClaimed` is advisory. After **any** backend runs, the orchestrator re-runs the project's checks (`verify.Verifier.Check`) and that boolean decides whether work ships. This is what makes delegating to black-box agents safe: their self-report never governs.

### I3 — No ambient authority
Secrets come from the environment, are injected per run, and are never written to disk, logged, prompted, or hard-coded. The process holds no broad filesystem or network authority by default.

### I4 — All execution is sandboxed
Every shell command originating from a model or a delegated agent runs inside the container sandbox against a bind-mounted worktree. Nothing the model emits runs on the host.

### I5 — Append-only audit
Every model call, tool execution, verify, and gate decision is appended to the event log. History is never mutated or deleted. The log is replayable and is the debugging spine.

### I6 — Zero-dependency core
Standard library only. A new module dependency requires justification in the PR + CHANGELOG. SQLite (Phase 4) is the first sanctioned exception, scoped to `internal/store`; the MCP client (Phase 1 tool surface) is the second, scoped to `internal/mcp`.

### I7 — Untrusted input boundary
Tool output, file contents, and fetched web content are data, never controlling instructions. The agent's directives never originate from tool results.

## Layer map & dependency direction

Dependencies point **inward/downward only**. Leaf packages must not import the orchestrator. This keeps the core acyclic and the seams clean.

```
cmd/nilcore ──▶ agent ──▶ backend (contract) ──▶ model, sandbox, verify, eventlog
                  │                                   ▲           ▲
                  └────────────▶ verify, eventlog ────┘           │
backend/native ──▶ model, sandbox, verify, eventlog ──────────────┘
policy  (leaf, imported by agent)
```

| Package | Responsibility | May import |
|---|---|---|
| `internal/model` | Messages API client | stdlib only |
| `internal/sandbox` | container command execution | stdlib only |
| `internal/verify` | run project checks, report pass/fail | `sandbox` |
| `internal/eventlog` | append-only JSONL audit | stdlib only |
| `internal/policy` | reversibility classifier + gate | stdlib only |
| `internal/backend` | `CodingBackend` + native/codex/claude-code | `model`, `sandbox`, `verify`, `eventlog` |
| `internal/agent` | orchestrator (run backend, final verify) | `backend`, `verify`, `eventlog`, `policy` |
| `cmd/nilcore` | wiring from flags/env | all of the above |

**Rule:** `backend` must never import `agent`. `model`/`sandbox`/`eventlog`/`policy` import nothing internal except, for `verify`, `sandbox`.

## Data flow

```
task (CLI or channel)
   └─▶ agent.Orchestrator.Execute
         ├─ pick backend  (Phase 3: routing — single | race best-of-N | review)
         ├─ backend.Run    (native loop  /  codex exec  /  claude -p)   in a worktree+sandbox
         ├─ verify.Check    ← SOURCE OF TRUTH
         ├─ policy.Gate     (irreversible actions: merge/deploy → human gate)
         └─ eventlog.Append (every step)  ─▶  (Phase 4) SQLite store + memory
```

## Extension points (where future phases plug in)

Each is owned by a specific task in `docs/TASKS.md`. The contract above does not change for any of them.

| Phase | New package(s) | Plugs in at |
|---|---|---|
| 1 | `internal/worktree` | orchestrator creates a fresh worktree per task |
| 1 | `internal/channel` (`Channel` interface, telegram, slack) | a thin layer that feeds tasks into `Execute` and surfaces gates as chat |
| 1 | `internal/tools` (structured), `internal/mcp` (code-execution client) | tools in the native loop's registry; MCP servers exposed as sandbox code APIs, discovered on demand, calls gated + guarded |
| 1 | `internal/provider` (anthropic/openai/openrouter), `internal/secrets`, `internal/onboard`, `internal/paths` | provider adapters behind one interface; SecretStore; the `nilcore init` wizard; per-OS paths |
| 2 | hardening within `sandbox`, `policy`, `eventlog`; optional `internal/guard`; authorized control in `internal/channel` | sandbox flags, egress allowlist, tool-call policy, prompt-injection boundary, hash-chained log; **allowlist of principals permitted to command the agent + gate-approval auth** (`docs/OPERATIONS.md` §1) |
| 3 | `internal/planner`, router/race/review inside `agent` | planner (run only for complex tasks — adaptive) decomposes a goal; router selects or races backends; review runs before the gate |
| 3 | `internal/summarize` (ContextSummary), `internal/trigger` (proactivity) | summarize-and-handover seeds fresh subworkers; the trigger self-starts reversible work |
| 3 | `internal/advisor` (+ two-tier `internal/model`) | executor consults the advisor via `ask_advisor`; advisor tier doubles as planner + reviewer |
| 3 | `internal/codeintel/{ast,graph,repomap,lsp,semantic,retrieve,impact,live}` | semantic codebase understanding — four lenses + fusion pipeline returning Context Bundles; Impact Set drives the verifier and the gate. Full design: `docs/CODE-INTELLIGENCE.md` |
| 4 | `internal/store` (SQLite), `internal/memory` | event log graduates to the store; memory retrieved into native context assembly, written back after tasks |
| 5 | `internal/skills` (Agent Skills + native plugins), `internal/selfimprove`, `eval/` | plugin capabilities in both formats; gated self-edits scoped to prompts/skills/tools only; the eval harness that earns routing data |
| 6 | `internal/budget`, `internal/scheduler`, `internal/maint`, `internal/inspect`, `internal/config`; resilience in `model`, durability in `agent`, auto-detect in `verify` | runtime resilience & operations — provider retry/failover, metered budgets, crash-safe resumption, concurrent-task scheduling, verify auto-detection, resource GC, operator inspect/health, config validation. Full design: `docs/OPERATIONS.md` |

## Security model (summary)

- **No ambient authority (I3)** — per-run, scoped, revocable credentials.
- **Sandbox all execution (I4)** — container per task/worktree; default-deny network; egress allowlist (Phase 2); delegated CLIs wrapped in our own container.
- **Untrusted input boundary (I7)** — fetched/file content never becomes instructions.
- **Bounded autonomy** — reversible actions auto-run; irreversible actions hit the human gate. Worktrees make coding reversible by construction, so gates concentrate at merge/deploy.
- **Full audit (I5)** — append-only, hash-chained (Phase 2), secrets redacted.

Operational detail and key handling: `docs/PREREQUISITES.md`.
