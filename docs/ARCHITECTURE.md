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

**Optional bus seam on the native loop (additive, contract-untouched).** `backend.Native` carries one optional field `Peer backend.Peer` alongside the existing optional `Advisor`. When nil — the single-agent default — the loop is byte-identical: no bus tools are registered and no bus code runs (gated exactly like `Advisor`). When set (multi-agent mode), it registers three bus tools — `ask_supervisor` (blocking Ask, `KindQuestion`), `share_finding` (async Send, `KindFinding`), and `request_review` (blocking Ask, `KindReviewRequest`) — and routes each call through `Peer.Dispatch`. The `Peer` interface (`Tools() []model.Tool`; `Dispatch(ctx, name, input) (string, error)`) is declared in `backend` itself, not imported from `internal/agent/bus`, so the frozen-contract package keeps a leaf import graph; the concrete `*bus.AgentPeer` satisfies it. Every peer reply is `guard.Wrap`-fenced before it becomes a `tool_result` (I7), identical to the advisor and shell-output paths — a peer body is data, never instructions. `Task`, `Result`, and the `CodingBackend` interface are untouched (I1); this is an additive field, mirroring the `Advisor` gate.

**Optional inbox + emit seam on the native loop (conversational front door, additive, contract-untouched).** `backend.Native` carries three further optional fields alongside `Advisor`/`Peer`: `Inbox backend.Inbox` (the user→agent message seam), `Seed []model.Message` (prior conversation history to continue on), and `Emitter backend.Emitter` (the live reasoning/intent sink). When all three are nil — the single-task `run`/`build`/`serve` default — the loop is **byte-identical**: no per-iteration context is allocated, no watcher goroutine is spawned, the inbox is never drained, and nothing is emitted (gated exactly like `Advisor`/`Peer`). When `Inbox` is set, at each loop boundary the loop **QUEUE-drains** the inbox and folds the messages in as ordinary user turns; and it wraps **only** `Model.Complete` in a per-iteration `context.WithCancelCause(ctx)` whose watcher goroutine cancels with `loopctl.ErrSteer` when the inbox's steer signal fires. `Box.Exec`/`Tools.Dispatch`/`Peer.Dispatch`/`Verifier.Check` keep the **task** ctx — so a **STEER cancels the in-flight think only, never a running tool**, making "no half-applied tool state" true by construction (a SIGKILL of a sandbox mid-write would tear the RW-bind-mounted `/work`). On a cancelled `Complete`, `loopctl.ClassifyCancel(taskCtx, iterCtx)` discriminates — `taskCtx.Err()` first ⇒ a shutdown/deadline that dominates and unwinds cleanly; else an `ErrSteer` cause ⇒ a steer that is logged and folded (`continue`), never an error; else a genuine fault on the existing `model step %d` path. The watcher is torn down deterministically every iteration (`cancel(nil); <-watcher`) so none leaks. The `Inbox` interface (`Drain() []model.Message`; `Steer() <-chan struct{}`) and `Emitter` interface (`Emit(emit.Event)`) are declared in `backend` itself (the concrete `*inbox.Box` and `internal/emit` sinks satisfy them), keeping the frozen-contract package a leaf — `internal/emit` is a stdlib-only leaf with no channel/session imports. A steer is the **principal's** trusted instruction folded as an un-`Wrap`'d user turn (the trust line, I7); only authorization at the channel boundary promotes a message to principal-trust. `Task`, `Result`, and the `CodingBackend` interface are untouched (I1); these are additive fields, mirroring the `Advisor`/`Peer` gate.

### I2 — The verifier is the only authority on "done"
`Result.SelfClaimed` is advisory. After **any** backend runs, the orchestrator re-runs the project's checks (`verify.Verifier.Check`) and that boolean decides whether work ships. This is what makes delegating to black-box agents safe: their self-report never governs.

### I3 — No ambient authority
Secrets are held by the `SecretStore` (environment / OS keychain / encrypted vault / external), are injected per run, and are never written to disk in plaintext, logged, prompted, hard-coded, or given to the model. The process holds no broad filesystem or network authority by default. (See the Security section above and `docs/SECRETS.md` for the operational form.)

### I4 — Model-emitted execution is sandboxed
Every *shell command* a model emits, and every delegated coding CLI (Codex, Claude Code), runs inside the container sandbox against a bind-mounted worktree — a model can never run an arbitrary program on the host. The native loop's structured tools are the one deliberate, bounded exception (see §Execution model): they run host-side but are confined to the worktree and cannot execute arbitrary code.

### I5 — Append-only audit
Every model call, tool execution, verify, and gate decision is appended to the event log. History is never mutated or deleted. The log is replayable and is the debugging spine.

### I6 — Zero-dependency core
Standard library only. A new module dependency requires justification in the PR + CHANGELOG. SQLite (Phase 4) is the first sanctioned exception, scoped to `internal/store`; the MCP client (Phase 1 tool surface) is the second, scoped to `internal/mcp`.

### I7 — Untrusted input boundary
Tool output, file contents, and fetched web content are data, never controlling instructions. The agent's directives never originate from tool results.

## Execution model (the two tiers under I4)

I4 is precise about *where* model-influenced work runs. There are exactly two tiers, and the boundary is "can this run an arbitrary program?":

**Tier 1 — sandboxed (arbitrary execution).** Anything a model can use to run an arbitrary program is isolated in the container:
- the `run` shell tool — every command the model emits;
- delegated coding CLIs (Codex, Claude Code) — wrapped in *our* container, not trusted to self-sandbox;
- MCP glue code — runs in the sandbox under the gate + egress allowlist.

Nothing in this tier touches the host. The container is rootless, drops capabilities, mounts the rootfs read-only, and defaults to deny-all egress (an allowlist proxy is the only way out — `internal/policy`, and it refuses loopback/link-local/private destinations so it can't be turned into an SSRF pivot).

**Tier 2 — host-side, worktree-confined (scoped I/O, never arbitrary execution).** The native loop's structured tools run in-process on the host because they are *bounded operations*, not a shell:
- `read` / `write` / `edit` / `search` — file I/O confined to the disposable worktree. Confinement is enforced both lexically and after symlink resolution (`filepath.EvalSymlinks` on the worktree root and the target's deepest existing ancestor), and writes use `O_NOFOLLOW` to close the final-component TOCTOU — so an in-tree symlink (`evil -> /etc`) cannot escape.
- `git` — a fixed subcommand set (`status`/`diff`/`add`/`commit`/`log`), never `push`/`reset`/`remote` writes. Model-supplied paths are passed after `--` (no flag injection), and every invocation clamps the code-execution vectors a writable repo would otherwise expose: `core.hooksPath=/dev/null`, `core.fsmonitor` disabled, and an environment with `GIT_CONFIG_NOSYSTEM=1` / `GIT_CONFIG_GLOBAL=/dev/null` so a model-written `.git/hooks` or `~/.gitconfig` cannot run on `commit`.

The reason Tier 2 is host-side is performance and simplicity (structured edits don't need a container round-trip), and it is *safe* to be host-side precisely because these tools cannot execute arbitrary code and cannot reach outside the worktree. If that confinement ever weakened, the tools would have to move into Tier 1. The integration gate (merge/push/deploy/pay) and the principal allowlist (who may command the agent or answer a gate) sit above both tiers.

## The channel seam (Phase 1)

Channels let a human drive NilCore over chat (Telegram, Slack) from a phone. They
implement one transport-agnostic interface in `internal/channel` — a registered
contract file (CLAUDE.md §5):

```go
type Channel interface {
    Receive(ctx) (TaskRequest, error)                 // next inbound task request
    Update(ctx, threadID, message string) error       // stream progress back
    Ask(ctx, threadID, question string) (bool, error) // render a gate as yes/no
}
```

`Ask` is the chat form of `policy.Approver`: an irreversible-action gate becomes a
yes/no reply. Concrete transports live in `internal/channel/<name>`; sender
authorization is P2-T07. `serve` mode (P1-T07) feeds `TaskRequest`s into
`agent.Execute` and routes gates back through `Ask`.

## The conversational front door (session · inbox · emit · loopctl)

One conversational entrypoint sits above the machinery: the user opens a chat —
the interactive terminal REPL (`nilcore chat`) or an existing serve channel — and
just talks, from a typo fix to "plan and ship a whole service." The harness infers
internally which machine runs (native loop / supervisor / project loop), the
conversation **persists** (a follow-up *continues* the work, it does not restart
it), and the user can **queue or steer** messages mid-work. The full design is
`docs/CONVERSATIONAL.md`; this section records the shipped seams and how they sit
inside the invariants. Four leaf packages compose the existing machinery **without
touching the frozen `backend.CodingBackend` contract** — the loop seams are
additive and nil-gated (nil = byte-identical), exactly like `Advisor`/`Peer`:

| Package | Responsibility | May import |
|---|---|---|
| `internal/emit` | live reasoning/intent sink (`Emitter`, `Event`, `WriterEmitter`, `NopEmitter`) | stdlib only |
| `internal/inbox` | the user→agent message seam (`Box`: `Push`/`Drain`/`Steer`, `Queue`/`Steer` modes) | `model`, `eventlog` |
| `internal/loopctl` | the shared cancel-cause discriminator (`ErrSteer`, `ClassifyCancel`) | stdlib only |
| `internal/session` | state container + auto-router + drivers (`Session`, `Turn`, `WorkState`, `Phase`, `SupervisorFirstRouter`, `Driver`s) | composes `agent`, `backend`, `super`, `project`, `summarize`, `model`, `eventlog`, `policy`, `inbox`, `emit`, `store` |

`emit`, `inbox`, and `loopctl` are leaves the frozen `backend` leaf can hold the
same way it holds `Advisor`/`Peer`; `session` is the orchestrating package above
`agent` (it composes the machinery, never the reverse — the dependency direction is
preserved).

### Steer vs queue (the interruptible loop)

A principal's mid-work message arrives in one of two modes (`inbox.Mode`), and a
local default-QUEUE rule — `session.classifyInterrupt`, **no LLM round-trip** —
picks between them: STEER only on a `!` / `/steer` prefix (or a channel Steer
affordance), QUEUE otherwise.

- **QUEUE** (default): the message is appended to `inbox.Box` and **folds in as an
  ordinary user turn at the next loop boundary** (`Drain` at the top of each
  iteration, logged `queue_drain`). The drive keeps running.
- **STEER**: in addition to queuing, it fires the `Box`'s cap-1 edge-notify steer
  signal. A per-iteration watcher cancels **only `Model.Complete`** with the
  `loopctl.ErrSteer` cause; the in-flight think returns, the loop unwinds to the
  boundary, and the steer text folds in as the next user turn — so the user can
  correct course after reading the agent's live reasoning.

**Steer cancels the model call, never an in-flight tool.** This is load-bearing and
is stated in invariant I1's inbox/emit seam note above: the per-iteration
cancellable `iterCtx` wraps **only** `Model.Complete` (and cheap read-only Asks);
`Box.Exec` / `Tools.Dispatch` / `Peer.Dispatch` / `Verifier.Check` always receive
the **task** ctx. A steer that arrives mid-tool is buffered and applied at the next
boundary; it never SIGKILLs a sandbox command (which would tear the RW-bind-mounted
`/work`). This makes "no half-applied tool state" true by construction — there is
never a dangling `tool_use`. The cancel discrimination is one shared function,
`loopctl.ClassifyCancel(taskCtx, iterCtx)`, so the native and supervisor loops
cannot drift: `taskCtx.Err()` is checked **first** (a SIGTERM/deadline dominates a
racing steer and unwinds cleanly), then an `ErrSteer` cause (a steer — logged
`steer_interrupt`, folded, never an error), else a genuine fault on the existing
`model step %d` path. The cause travels inside the context (no shared mutable
`steered` bool), so `go test -race` is green by construction. The watcher is torn
down deterministically every iteration (`cancel(nil); <-watcher`) so none leaks.
nil `Inbox` ⇒ `iterCtx := ctx` with no `WithCancelCause`, no watcher, no `Drain` —
byte-identical to the pre-conversational loop.

### The trust line (I7)

The single load-bearing trust rule, enforced in code: **a principal's steer/queue
message becomes a real, un-`guard.Wrap`'d `user` turn; everything the loop reads
from a tool / file / peer / bus stays `guard.Wrap`'d as data.** Authorization at the
channel boundary (`channel.Authorized.Permit`) is the *only* thing that promotes a
chat message to principal-trust. A steer carries no executable payload — it is text
into the model's context only — and **fencing is immutable once applied**: a steer
is a *new* user turn and never causes the harness to un-fence or merge any
previously-`Wrap`'d data already in `History`. In the supervisor's round-boundary
fold, a queued user message and a subagent finding arriving in the same gap are
emitted as **two distinct labeled blocks** — the user text un-`Wrap`'d ("principal
instruction") **first**, the findings `guard.Wrap`'d as data **second**, never
concatenated. Steer is just more user text: it **cannot** set `finished=true` or
shortcut verify (I2 — the verifier stays the sole authority on "done"; there is no
"user says done → ship" path), and a steered irreversible action still reaches
`policy.Gate` → `Approver` with one explicit prompt (the gate is unchanged).

### Budget & termination keying

The budget Ledger keys spend by an opaque task string and the per-task ceiling
resets per key, so a long conversation must be keyed by the **conversation**
(`Session.ID`), never the per-drive task ID (which is fine for the worktree and the
event log). The Session owns one `meter.Provider` keyed by `s.ID` reused across
every drive, the router's classifier uses that same metered provider (its spend
counts against the conversation ceiling), and `SetGlobalCeiling` is the
conversation wall. A steer never resets the dollar/token budget or the deadline.

### The `nilcore chat` entrypoint and serve reuse

`nilcore chat [-dir ./repo]` is the primary front door (`cmd/nilcore/chat.go`): it
builds **one** `Session` (ID `chat-local`, `Sender` pinned to the local principal),
wires a `emit.WriterEmitter` to stdout for live reasoning, and runs a line-based
`bufio` stdin reader **while the agent works** — a line typed mid-drive is queued or
steered by the same `classifyInterrupt` rule. Ctrl-C is a graceful shutdown that
cancels the task ctx (shutdown dominates any pending steer). The terminal user is
the principal by construction, so there is no allowlist. **Bare `nilcore` defaults
to `chat`**; `nilcore -goal …` keeps its flag-prefixed dispatch.

`nilcore serve` reuses the *same* `Session` (`internal/server`): the server holds a
per-thread `map[threadID]*Session` and a **concurrent intake** — `Turn` returns
immediately while the drive runs in its own goroutine, so the serve loop keeps
accepting messages mid-drive (which the prior one-task-at-a-time server could not
do). Every inbound message is `Authorizer.Permit`-checked **before** it can become a
`Turn` — an unauthorized steer/queue is dropped and logged (`unauthorized_command`),
never promoted to principal trust. Each Session pins its `Sender` from the first
authorized message and refuses a later message from a different sender. The
per-thread `emit.Emitter` is a thin adapter over `Channel.Update`, and gates still
route through `Ask`. Bounded `WorkState` (never raw transcripts) persists via the
existing `agent.Checkpoint` single-`UpsertTask` write into `store.Task.Detail`, so a
restart re-hydrates and continues rather than restarting.

`run` / `build` / `serve` stay first-class for scripting/CI: `runMain` (one bounded
native task) and `buildMain` (supervisor/project) pass a nil `Inbox`/`Emitter` and
are byte-identical.

### New event-log kinds (metadata only, redacted — I5)

The conversational layer adds these kinds **above** the existing loop kinds
(`model_call`, `tool_exec`, `verify`, `super_*`, `project_*`) so the trail stays
replayable end-to-end. Bodies are never logged — only metadata (mode, text length,
route, step):

`session_open` · `session_route` · `session_followup` · `session_fold` ·
`session_drive_start` · `session_drive_done` · `session_persist` ·
`session_restore` · `user_message` · `queue_drain` · `steer_interrupt` ·
`steer_ack` · `task_cancel` · `unauthorized_command`.

The `emit` surface kinds (`intent`, `tool`, `verify`, `steer_ack`) are the
user-facing reasoning lines, not log records; they run through the redact path
before leaving the process.

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
| `internal/model` | canonical message/tool format + `Provider` seam | stdlib only |
| `internal/provider` | vendor adapters (anthropic / openai / openrouter) | `model` |
| `internal/sandbox` | container command execution | stdlib only |
| `internal/verify` | run project checks, report pass/fail | `sandbox` |
| `internal/eventlog` | append-only JSONL audit | stdlib only |
| `internal/policy` | reversibility classifier + gate | stdlib only |
| `internal/backend` | `CodingBackend` + native/codex/claude-code | `model`, `sandbox`, `verify`, `eventlog` |
| `internal/emit` | live reasoning/intent sink (conversational) | stdlib only |
| `internal/loopctl` | shared steer/shutdown cancel-cause discriminator | stdlib only |
| `internal/inbox` | user→agent message seam (queue/steer) | `model`, `eventlog` |
| `internal/agent` | orchestrator (run backend, final verify) | `backend`, `verify`, `eventlog`, `policy` |
| `internal/session` | conversational state container + auto-router + drivers | `agent`, `backend`, `super`, `project`, `summarize`, `inbox`, `emit`, `model`, `eventlog`, `policy`, `store` |
| `cmd/nilcore` | wiring from flags/env | all of the above |

**Rule:** `backend` must never import `agent`. `model`/`sandbox`/`eventlog`/`policy` import nothing internal except, for `verify`, `sandbox`. `emit` and `loopctl` are stdlib-only leaves the frozen `backend` leaf may hold (like `Advisor`/`Peer`); `session` sits above `agent` and composes the machinery — nothing below it imports `session`.

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
| 6 | `internal/budget`, `internal/scheduler`, `internal/maint`, `internal/inspect`; resilience in `model`, durability in `agent`, auto-detect in `verify` | runtime resilience & operations — provider retry/failover, metered budgets, crash-safe resumption, concurrent-task scheduling, verify auto-detection, resource GC, operator inspect/health. Config validation/migration now lives in `internal/onboard` (the live config schema). Full design: `docs/OPERATIONS.md` |
| C | `internal/emit`, `internal/inbox`, `internal/loopctl`, `internal/session`; nil-gated `Inbox`/`Seed`/`Emitter` seam on `backend.Native` + `super.Supervisor`; per-thread `Session` map in `internal/server`; `nilcore chat` in `cmd/nilcore` | the conversational front door — one chat where the agent auto-routes (native / supervisor / project), the conversation persists (continue, not restart), and the user queues or steers mid-work. Steer cancels only `Model.Complete`; the principal's message is an un-`Wrap`'d user turn, tool/bus stays fenced. Full design: `docs/CONVERSATIONAL.md` |

## Security model (summary)

- **No ambient authority (I3)** — per-run, scoped, revocable credentials.
- **Sandbox model-emitted execution (I4)** — shell + delegated CLIs + MCP glue run in a container per task/worktree; default-deny network; SSRF-safe egress allowlist (Phase 2); delegated CLIs wrapped in our own container. The structured file/git tools run host-side but stay worktree-confined and cannot execute arbitrary code (see §Execution model).
- **Untrusted input boundary (I7)** — fetched/file content never becomes instructions.
- **Bounded autonomy** — reversible actions auto-run; irreversible actions hit the human gate. Worktrees make coding reversible by construction, so gates concentrate at merge/deploy.
- **Full audit (I5)** — append-only, hash-chained (Phase 2), secrets redacted.

Operational detail and key handling: `docs/PREREQUISITES.md`.
