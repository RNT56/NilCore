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

**Optional inbox + emit seam on the native loop (conversational front door, additive, contract-untouched).** `backend.Native` carries three further optional fields alongside `Advisor`/`Peer`: `Inbox backend.Inbox` (the user→agent message seam), `Seed []model.Message` (prior conversation history to continue on), and `Emitter backend.Emitter` (the live reasoning/intent sink). When all three are nil — the single-task `run`/`build`/`serve` default — the loop is **byte-identical**: the inbox is never drained, the steer signal is never checked, and nothing is emitted (gated exactly like `Advisor`/`Peer`). When `Inbox` is set, at each loop boundary the loop **QUEUE-drains** the inbox and folds the messages in as ordinary user turns. `Model.Complete` runs under the **task** ctx — a **STEER never cancels the in-flight think**; its reasoning is preserved (the task ctx still cancels on shutdown/deadline, unchanged). Instead the loop **pauses-and-reconsiders** (CV-T01): after `Complete` returns and the assistant turn is appended, but BEFORE any tool dispatches, a non-blocking receive on the steer signal decides — if a steer is pending, the loop **HOLDS** every proposed `tool_use` (one "paused" `tool_result` per block, never executed), `Drain`s the inbox and folds the steered feedback as a user turn, emits a `steer_ack`, and `continue`s. The step counter still advances, so a steer storm stays bounded; the model reconsiders next step with its held action's paused results plus the feedback in view. `Box.Exec`/`Tools.Dispatch`/`Peer.Dispatch`/`Verifier.Check` already keep the **task** ctx, so a steer mid-tool is simply buffered and folded at the next boundary — "no half-applied tool state" is true by construction (a SIGKILL of a sandbox mid-write would tear the RW-bind-mounted `/work`). A `Complete` error with `Inbox` set and the task ctx done is a clean shutdown (`interrupted` Result); otherwise it is the existing `model step %d` fault. The `Inbox` interface (`Drain() []model.Message`; `Steer() <-chan struct{}`) and `Emitter` interface (`Emit(emit.Event)`) are declared in `backend` itself (the concrete `*inbox.Box` and `internal/emit` sinks satisfy them), keeping the frozen-contract package a leaf — `internal/emit` is a stdlib-only leaf with no channel/session imports. A steer is the **principal's** trusted instruction folded as an un-`Wrap`'d user turn (the trust line, I7); only authorization at the channel boundary promotes a message to principal-trust. `Task`, `Result`, and the `CodingBackend` interface are untouched (I1); these are additive fields, mirroring the `Advisor`/`Peer` gate.

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

## The conversational front door (session · inbox · emit)

One conversational entrypoint sits above the machinery: the user opens a chat —
the interactive terminal REPL (`nilcore chat`) or an existing serve channel — and
just talks, from a typo fix to "plan and ship a whole service." The harness infers
internally which machine runs (native loop / supervisor / project loop), the
conversation **persists** (a follow-up *continues* the work, it does not restart
it), the model's prose is **streamed live** as it is generated, and the user can
**queue or steer** messages mid-work — a steer mid-stream interrupts the generation
yet keeps the partial reasoning. The full design is `docs/CONVERSATIONAL.md` (and
the streaming layer is detailed in the *Live token streaming* subsection below);
this section records the shipped seams and how they sit
inside the invariants. Four leaf packages compose the existing machinery **without
touching the frozen `backend.CodingBackend` contract** — the loop seams are
additive and nil-gated (nil = byte-identical), exactly like `Advisor`/`Peer`:

| Package | Responsibility | May import |
|---|---|---|
| `internal/emit` | live reasoning/intent sink (`Emitter`, `Event`, `WriterEmitter`, `NopEmitter`) | stdlib only |
| `internal/inbox` | the user→agent message seam (`Box`: `Push`/`Drain`/`Steer`, `Queue`/`Steer` modes) | `model`, `eventlog` |
| `internal/session` | state container + auto-router + drivers (`Session`, `Turn`, `WorkState`, `Phase`, `SupervisorFirstRouter`, `Driver`s) | composes `agent`, `backend`, `super`, `project`, `summarize`, `model`, `eventlog`, `policy`, `inbox`, `emit`, `store` |

`emit` and `inbox` are leaves the frozen `backend` leaf can hold the
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
  signal. This triggers **pause-and-reconsider** — it **never cancels the in-flight
  work**. After `Model.Complete` returns and the assistant turn is appended, but
  BEFORE any proposed tool runs, the loop checks the steer signal; if a steer is
  pending it **HOLDS** every proposed `tool_use` (one "paused" `tool_result` per
  block, never executed), folds the steered feedback in as the next user turn,
  emits a `steer_ack`, and continues — so the model reconsiders its held action in
  light of the user's correction after reading the agent's live reasoning.

**Pause-and-reconsider — a steer never cancels in-flight work.** This is
load-bearing and is stated in invariant I1's inbox/emit seam note above:
`Model.Complete` runs under the **task** ctx, so a steer leaves the think intact —
its reasoning is preserved, not discarded. The steer instead lands at the
**post-think gate**: the loop holds the proposed actions (they are paused, never
dispatched) and re-prompts the model with the feedback. `Box.Exec` /
`Tools.Dispatch` / `Peer.Dispatch` / `Verifier.Check` already run under the task
ctx, so a steer that arrives mid-tool is simply buffered and folded at the next
boundary; it never SIGKILLs a sandbox command (which would tear the RW-bind-mounted
`/work`). This makes "no half-applied tool state" true by construction — there is
never a dangling, half-run `tool_use`. The hold is logged `steer_interrupt` and the
step counter still advances, so a steer storm cannot loop forever (the bounded-loop
rail holds). There is no per-iteration cancellable context and no watcher goroutine:
the steer signal is a plain cap-1 edge-notify the loop polls non-blocking after the
think, so the native and supervisor loops share the same tiny shape and `go test
-race` is green by construction. **Shutdown** is handled by the task ctx as before:
a `Complete` cancelled by a SIGTERM/deadline returns cleanly (`interrupted` Result /
`ctx` outcome), never confused with a steer (a steer no longer cancels `Complete` at
all). nil `Inbox` ⇒ no `Drain`, no steer check — byte-identical to the
pre-conversational loop.

### Live token streaming + interrupt-but-preserve (ST)

The conversational front door paints the model's prose **as it is generated**, not
in one block when the turn finishes — and a steer can now cut a *generation* short
while keeping the reasoning it already produced. This is the streaming layer. It is
purely **additive**: `Provider.Complete` is unchanged (I1), the frozen
`backend.CodingBackend` contract is untouched, and every non-conversational path
(`run`/`build`/`serve` scripting/CI) keeps using `Complete` byte-for-byte.

**The optional `model.Streamer` seam (additive to `Provider`).** A provider MAY
also implement `model.Streamer` — `Stream(ctx, system, msgs, tools, maxTokens,
onChunk func(model.Chunk)) (Response, error)` — alongside `Complete`. The loops
type-assert for it and fall back to `Complete` when a provider does not implement
it, so `Stream` never changes `Provider`. The contract a `Streamer` MUST honor:
(1) it assembles and returns the **same** `Response` `Complete` would for the same
inputs (identical `Content` blocks, `StopReason`, `Usage`) — the stream is a
delivery detail, not a different reply; (2) it forwards each output-text delta to
`onChunk` (a `model.Chunk{Text}`) as it is decoded, before the full `Response` is
ready, with the concatenation of forwarded chunks equal to the response's output
text; (3) `onChunk` is called synchronously on the read loop and must not block;
(4) **interrupt-but-preserve** — if `ctx` is cancelled mid-stream, `Stream` stops
reading and returns the **partial** `Response` assembled so far **together with**
`ctx.Err()`, so a caller can cut an in-flight generation short yet keep the text
already produced.

**Three provider SSE adapters (stdlib only, I6).** All three vendor adapters in
`internal/provider` implement `Streamer` with the standard library only —
`net/http` + `bufio` + `encoding/json`, no SDK and no new module:

- **`anthropic`** — POSTs the same `/v1/messages` body as `Complete` with
  `"stream":true`, scans the Messages SSE event stream with `bufio.Scanner`, and
  assembles text + `tool_use` blocks from `content_block_start` /
  `content_block_delta` (`text_delta` and `input_json_delta`) / `message_delta`
  frames. Text deltas are forwarded to `onChunk`; `tool_use` argument JSON streams
  as fragments joined at block stop.
- **`openai`** — POSTs the same `/chat/completions` body with `"stream":true` and
  `stream_options.include_usage` (so a trailing usage-only frame carries token
  counts), scans the SSE frames, and assembles text + `tool_use` from the streamed
  `delta.content` and `delta.tool_calls` fragments, mapping `finish_reason` to the
  canonical `StopReason`.
- **`openrouter`** — inherits the OpenAI adapter verbatim (same code, different base
  URL); it is OpenAI-compatible.

Each adapter's read loop checks `ctx.Err()` on every scan and returns the partial
`Response` + the context error on cancellation, satisfying interrupt-but-preserve.
The SSE frames are parsed **as data, never executed** (I7) — only the fields the
assembler reads are decoded. Tests are hermetic `httptest.Server`s that serve a
canned SSE body (no network), run under `-race`.

**Decorator delegation (the wrappers stay transparent).** Both provider decorators
implement `Stream` so streaming is invisible to the loop regardless of what the
underlying provider supports:

- **`meter.Provider`** (the budget wall) delegates to `Inner.Stream` when the inner
  is a `Streamer` (passing `onChunk` through untouched), else falls back to
  `Inner.Complete` and replays the whole reply as one `Chunk`. Either way it
  **charges** the assembled `Response`'s usage to the shared `budget.Ledger` exactly
  as `Complete` does — including a partial-on-cancel response (those tokens were
  produced and are billable) — so the ceiling stays a real termination rail on the
  streaming path.
- **`model.Resilient`** (retry/failover/breaker) implements `Stream` with the same
  retry + backoff + failover + circuit-breaker logic around each provider's `Stream`
  (or a non-streaming provider's `Complete` replayed as one chunk). Crucially it does
  **not** forward chunks live: each attempt **buffers** its deltas and flushes them
  to `onChunk` in order **only on the attempt that ultimately succeeds**, so a
  retried-then-succeeded (or failed-over) stream emits exactly one committed
  sequence — never the chunks of a discarded attempt.

Because both decorators satisfy `Streamer`, the loop always sees a `Streamer`
through the wrapper stack (meter charges, Resilient retries) while the live-token
property and the single-Response contract are preserved end to end.

**The `emit.KindToken` live-token surface.** The loop forwards each streamed delta
to the wired `emit.Emitter` as a `KindToken` event. `WriterEmitter` renders a
`KindToken` raw and inline (no glyph/step framing, no trailing newline) so a run of
tokens flows as one continuous line as the model thinks; the next framed event
(`KindIntent`/`KindTool`/`KindVerify`/`KindSteerAck`) flushes a newline first so it
starts cleanly on its own line.

### Streaming is gated on a wired Emitter

The loop streams **only** when both an `Emitter` is wired **and** `n.Model` is a
`model.Streamer`; otherwise it calls `Complete` under the task ctx exactly as
before. So `run`/`build`/`serve` (nil `Emitter`) and any non-streaming provider stay
byte-identical to the pre-streaming loop — the conversational front door is the only
caller that pays for streaming, and it pays for it only when a sink is actually
watching.

### The three steer behaviors (and `/cancel`, Ctrl-C)

With streaming there are now **three** distinct steer behaviors, selected by whether
the loop is mid-stream and whether streaming is active at all. All three keep "no
half-applied tool state" true by construction (the task ctx still governs tools), and
none of them lets the user shortcut the verifier (I2) or un-fence prior data (I7):

1. **Queue (fold at the boundary)** — the default for any message without a steer
   affordance. The message is appended to `inbox.Box` and **`Drain`'d at the next
   loop boundary** as an ordinary user turn (logged `queue_drain`). The drive never
   pauses.
2. **Steer *with* streaming = INTERRUPT-BUT-PRESERVE** — a steer arrives while the
   model is mid-`Stream`. The loop wraps **only** the `Stream` call in a
   per-iteration `context.WithCancelCause` child of the task ctx; a steer watcher
   cancels it with `loopctl.ErrSteer`. On that cancel the loop **keeps the partial
   reasoning** (the `text` blocks streamed so far) as the assistant turn, **drops the
   partial `tool_use`** (a half-built tool call has no matching `tool_result` and
   would corrupt the conversation), folds the steered feedback in as a user turn, and
   **re-thinks** next step with its partial reasoning + the feedback in view (logged
   `steer_interrupt` with `phase:"stream"`, emits a `KindSteerAck`). A steer landing
   exactly at the finish line (the watcher consumed it after `Stream` already
   returned) is carried into the post-think gate below so it is never dropped.
3. **Steer *without* streaming = pause-at-boundary (CV-T01)** — when the loop is not
   streaming (no `Emitter`, or a non-streaming provider), `Complete` runs under the
   task ctx and a steer **never cancels the in-flight think**. Instead, after
   `Complete` returns and the assistant turn is appended but **before any tool runs**,
   a non-blocking check of the steer signal **HOLDS** every proposed `tool_use` (one
   "paused" `tool_result` per block, never executed), folds the steered feedback in,
   and the model reconsiders next step (logged `steer_interrupt` with `phase:"model"`).

Two control verbs sit above all three and are **aborts**, not steers:

- **`/cancel`** (and its alias `/stop`) — aborts the in-flight run by cancelling the
  task ctx, but keeps the session/conversation alive so the next message starts a
  fresh drive. A `Complete`/`Stream` cancelled this way returns the clean
  `interrupted` Result (logged `task_cancel`), distinct from a steer.
- **Ctrl-C** — abort **and exit**: a graceful shutdown that cancels the task ctx and
  ends the process. Shutdown **strictly dominates** any racing steer.

**`loopctl` is back for the stream-interrupt discrimination.** Because a cancelled
`Stream` can now mean three different things — a **shutdown** (task ctx died:
SIGTERM, deadline, Ctrl-C, `/cancel`), a **steer** (the watcher cancelled with
`loopctl.ErrSteer`), or a genuine **fault** — the loop cannot just treat any cancel
the same way. `loopctl.ClassifyCancel(taskCtx, iterCtx)` is the single stdlib-only
discriminator (re-created for ST; it had been removed under CV-T01 when steer no
longer cancelled `Complete`): it checks `taskCtx.Err()` first so **shutdown strictly
dominates** a shutdown-vs-steer race, then reads `context.Cause(iterCtx)` for
`ErrSteer`, else falls through to fault. The cause travels inside the context (no
shared mutable flag), so the discriminator is race-free by construction. Both the
native loop and the supervisor loop import the same `loopctl`, so the two loops share
one judgment and cannot drift.

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
| `internal/inbox` | user→agent message seam (queue/steer) | `model`, `eventlog` |
| `internal/agent` | orchestrator (run backend, final verify) | `backend`, `verify`, `eventlog`, `policy` |
| `internal/session` | conversational state container + auto-router + drivers | `agent`, `backend`, `super`, `project`, `summarize`, `inbox`, `emit`, `model`, `eventlog`, `policy`, `store` |
| `cmd/nilcore` | wiring from flags/env | all of the above |

**Rule:** `backend` must never import `agent`. `model`/`sandbox`/`eventlog`/`policy` import nothing internal except, for `verify`, `sandbox`. `emit` is a stdlib-only leaf the frozen `backend` leaf may hold (like `Advisor`/`Peer`); `session` sits above `agent` and composes the machinery — nothing below it imports `session`.

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
| C | `internal/emit`, `internal/inbox`, `internal/session`; nil-gated `Inbox`/`Seed`/`Emitter` seam on `backend.Native` + `super.Supervisor`; per-thread `Session` map in `internal/server`; `nilcore chat` in `cmd/nilcore` | the conversational front door — one chat where the agent auto-routes (native / supervisor / project), the conversation persists (continue, not restart), and the user queues or steers mid-work. Steer is pause-and-reconsider (never cancels in-flight work): it holds the proposed action and folds the feedback in so the model reconsiders; the principal's message is an un-`Wrap`'d user turn, tool/bus stays fenced. Full design: `docs/CONVERSATIONAL.md` |
| ST | additive `model.Streamer` seam (`Stream`, `Chunk`) — `Provider.Complete` unchanged; SSE adapters in `internal/provider` (anthropic/openai/openrouter, stdlib-only); `Stream` on the `meter`/`Resilient` decorators; `emit.KindToken`; `loopctl` re-added for the stream-interrupt discrimination; streaming on `backend.Native` + `super.Supervisor`, gated on a wired `Emitter` | live token streaming + interrupt-but-preserve — the model's prose is painted as it is generated, and a steer mid-stream cancels the generation, keeps the partial reasoning, drops the partial `tool_use`, and re-thinks. Three steer behaviors now: queue (fold at boundary), steer-with-streaming (interrupt-but-preserve), steer-without-streaming (pause-at-boundary); `/cancel` and Ctrl-C are aborts. Streaming is gated on a wired `Emitter`, so `run`/`build`/`serve` use `Complete` unchanged. See the *Live token streaming* subsection above |

## Security model (summary)

- **No ambient authority (I3)** — per-run, scoped, revocable credentials.
- **Sandbox model-emitted execution (I4)** — shell + delegated CLIs + MCP glue run in a container per task/worktree; default-deny network; SSRF-safe egress allowlist (Phase 2); delegated CLIs wrapped in our own container. The structured file/git tools run host-side but stay worktree-confined and cannot execute arbitrary code (see §Execution model).
- **Untrusted input boundary (I7)** — fetched/file content never becomes instructions.
- **Bounded autonomy** — reversible actions auto-run; irreversible actions hit the human gate. Worktrees make coding reversible by construction, so gates concentrate at merge/deploy.
- **Full audit (I5)** — append-only, hash-chained (Phase 2), secrets redacted.

Operational detail and key handling: `docs/PREREQUISITES.md`.
