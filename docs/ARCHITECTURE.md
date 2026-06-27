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
| Sandbox | **Containers** (Docker / Podman; Podman rootless preferred) **or host-native Linux namespaces + Landlock** — no runtime, image, or daemon; auto-detected and preferred when the kernel supports it |
| Routing | **Adaptive escalation, verifier as judge** — one backend by default → race best-of-N on hard/failed → cross-model review at the irreversible gate; with `-backends` (two or more), the Trust Ledger orders *which* backends compete (Selector orders, verifier still judges — I2) |
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

**Optional code-intelligence + capability seams on the native loop (additive, contract-untouched).** Beyond `Advisor`/`Peer`/`Inbox`/`Emitter`, `backend.Native` carries two more nil-gated seams, gated identically (nil = byte-identical). `MemoryContext func(ctx, goal) string` prepends relevant cross-project memory at task start. `LiveSession func(dir) (update, query, close)` opens a per-run, worktree-aware incremental code graph: the loop opens it at `Run` start and closes it at `Run` end (the graph handle is **task-scoped**, no leak), advertises a read-only `live` tool only when a session opened, re-indexes just the edited file on each successful `write`/`edit` (P3-T16 "no full re-index"), and fences every query result with `guard.Wrap` (I7). Both are **funcs** so `backend` imports no `memory`/`codeintel` machinery — keeping the frozen-contract leaf import graph intact (the cmd layer wires concrete closures over `internal/memory` + `internal/codeintel/{graph,live}`). Opt-in via `NILCORE_LIVE_INDEX`; `Task`/`Result`/`CodingBackend` untouched (I1). The `codeintel/semantic` index this seam feeds is now a content-hash-cached, pure-Go **HNSW** vector index (replacing the old linear cosine scan), activated opt-in via `NILCORE_EMBED_KEY` (an OpenAI-compatible embedder, `internal/embed`); off ⇒ lexical fallback, byte-identical (D2). The `codeintel/ast` parser seam is now **multi-language** — a language-parser interface with pure-Go backends for **19 languages across 34 extensions**: Go (via `go/parser`), Python, TypeScript/JavaScript, Rust, Java, C, C++, C#, Ruby, PHP, Kotlin, Swift, Scala, Dart, Zig, Bash, Lua, Elixir, and SQL (`ast.SupportedExtensions` covers all 34 — `.go .py .js .jsx .ts .tsx .mjs .cjs .rs .java .c .h .cc .cpp .cxx .hpp .hh .hxx .cs .rb .php .kt .kts .swift .scala .sc .dart .zig .sh .bash .lua .ex .exs .sql`; CGO-free, **not** tree-sitter; only the Go backend is precise — every non-Go backend is a broad, structural **heuristic line scanner** (symbols / references / call-graph / repo-map), with the LSP seam the precise lens for type resolution where a server exists), so the live + codeintel index walks cover all of them — registration auto-broadened the walks via `SupportedExtensions` (D3/R2; the non-Go backends beyond TS/JS+Rust landed in the Phase-13 languages batch). Both stay **pure stdlib** — no module added (I6).

**Operator steering seam (`SteeringContext`, trusted-input I7 exception — P10-T01).** `backend.Native` carries one more nil-gated func-seam, `SteeringContext func(ctx, dir) string`. When nil — the default — nothing is loaded (byte-identical). When set, it loads an authoritative project steering file (`NILCORE.md` / `AGENTS.md`) as **trusted instructions** prepended at task start — the deliberate, scoped **I7 exception**: it is the operator's own steering, not tool/file/web output, so it is folded un-`Wrap`'d. It is bounded **below the safety core** — steering text can shape the agent's approach but **cannot widen capability, bypass the gate, or shortcut the verifier** (I2/I4/I3 hold above it). A **func** so `backend` imports no steering machinery (the cmd layer wires a closure over `internal/steering`); `Task`/`Result`/`CodingBackend` untouched (I1). Wired into chat + run/build.

**Multimodal image block (additive to `model`, `Provider.Complete` unchanged — I1).** `model.Block` gained an additive **image** shape alongside its text / `tool_use` / `tool_result` cases; the anthropic and openai adapters carry it on the wire. `Provider.Complete` and the frozen `backend.CodingBackend` contract are **untouched** — images ride inside the existing `[]Block`, so a model can be handed a screenshot (the behavioral-verification path below) as a multimodal block with no signature change (I1, P9-T01). A matching dispatch seam carries tool output that is itself an image: a tool MAY implement `tools.ImageRunner`, and the registry's `Registry.DispatchRich` returns rich blocks (not just a text string), so a tool result can be a screenshot folded into the next turn. Both are additive — a text-only tool and a non-image provider stay byte-identical.

**Behavioral-verification seam (`browser_view`, composite verifier, opt-in — I2 intact, D1; flow-driving R3).** A sandboxed headless browser navigates a running app and hands the model a **screenshot** as a multimodal image block via the `browser_view` tool, driven by a pure-Go `nilcore-browser` driver (`cmd/tools/nilcore-browser`) baked into the sandbox image. Given an optional `actions` script the tool first **drives a flow** (navigate/click/type/key/wait — e.g. log in, submit a form) over a pure-Go Chrome DevTools Protocol client (`internal/cdp`, RFC6455 WebSocket + CDP, stdlib only — R3) and then observes; model-supplied selectors/text/URLs are **data**, replayed as CDP params, never shell or code (I7), and the run stays egress-confined (I4). (Full **agentic** browser *and* desktop computer use have since shipped as their own observe→plan→act→verify loops — see the Phase-14/CU extension point below; `browser_view` remains the lightweight one-shot behavioral-verification seam.) A composite verifier — opt-in via `NILCORE_BROWSER_VERIFY` — folds a browser behavioral check **into** the verdict, so the verifier stays the **sole** authority on "done" (I2): the behavioral check is one input to `verify.Check`, never a separate ship path. *Honest caveat:* the live browser run is **CI-only** (no Chromium in hermetic unit tests), and the driver **fails closed** without a browser — off ⇒ no browser tool advertised, byte-identical. Wired via `NILCORE_BROWSER` / `NILCORE_CHROMIUM` (env / SecretStore, never logged, never given to the model — I3); the `nilcore-browser` driver is **pure stdlib**, adding no module (I6).

**The shell-off gate for user-set read-only modes (additive, contract-untouched).** `backend.Native` carries one more nil/false-gated field, `DisableShell bool`. When false — every default path — the always-on `run` shell tool is advertised exactly as before (byte-identical). When true, the loop **never advertises `run`** and refuses any `run` the model emits anyway. It is the structural half of the conversational front door's read-only **modes** (`/discuss`, `/plan` — see `docs/CONVERSATIONAL.md` §"Modes, context, and web"): combined with a write-free registry (`tools.ReadOnly`/`tools.ReadOnlyWithCodeintel`, no write/edit/git) and a `verify.Pass` pass-through verifier, a read-only drive has **no registered path to mutate the tree** — the "Plan writes no code" guarantee is a property of the wiring, not of a command denylist or the model behaving (I7). A user-set mode (`session.Mode`, carried on `WorkState` so it persists and survives a restart) **overrides the auto-router** when pinned; `ModeAuto` (the default/zero value) leaves the router and full capability exactly as before. Modes are set only by a front-door control verb, never from `Turn`/inbox/tool text (the trust line, I7).

**Read-only context roots on the structured read tools (additive, contract-untouched).** `tools.ReadTool`/`tools.SearchTool` carry an optional `ReadRoots []string` — additional **read-only** roots they may resolve against beyond the worktree (the user's `/add <path>`). A **relative** path still resolves only against the worktree (byte-identical default; `../escape` rejected); an extra root is addressed by **absolute** path and allowed only if it resolves symlink-safe inside an added root (the shared `confine` check, so `/etc/passwd` is rejected). `WriteTool`/`EditTool` are untouched and resolve only against the worktree, so **extra roots are never writable** — the single-writable-root half of I4 holds. The structured read tools run host-side, so no sandbox mount is needed; the roots are registered principal-only and threaded into each drive at launch.

**Egress proxy lifecycle (web access).** `policy.EgressProxy.Start(ctx, bindAddr)` is the listener + ctx-bounded goroutine + clean shutdown around the existing `ServeHTTP` allowlist handler. `nilcore chat -allow-egress host,host` stands it up and routes a **container** sandbox through it via `Container.AllowEgressVia` (using the runtime host alias; `Container.ExtraHosts` adds `--add-host` for docker-Linux); the sandboxed `web_fetch` tool is then advertised (its body `guard.Wrap`'d as untrusted data, I7). Default stays default-deny: no flag ⇒ no proxy, `--network none`, no `web_fetch`. The namespace backend runs in an empty network namespace (`CLONE_NEWNET`, no interface), so it has no proxy egress path — web access requires the container backend (fail-closed).

**Nil-gated branch-preservation on the orchestrator (additive, contract-untouched, D4).** The orchestrator's `Outcome` carries an additive `Branch` field, and a nil-gated `KeepBranch` hook preserves the verified worktree branch instead of the default disposable cleanup. When unset — every default path — cleanup is **byte-identical** (the worktree is disposed as before). When set, the verified branch survives so a gated PR can be opened from it. This underpins **gated PR (D4):** `nilcore watch --open-pr` / `nilcore schedule --open-pr` open a **draft** PR via `internal/forge` **only after the human gate** — the push runs inside the approved prepare step, the token comes from the SecretStore (`NILCORE_FORGE_TOKEN`, never logged, never given to the model — I3), and **the agent never merges** (merge stays the human gate). `internal/forge` is **pure stdlib** (no module — I6); credentials are scrubbed from logs (I5).

**Multi-backend strength-routing seam (the Trust Ledger goes live, additive, contract-untouched, Phase 13).** The orchestrator carries a nil/empty-gated `Selector` seam plus `Backends []string` and `NewEnvFor func(dir, name) Env`. `multiBackend()` holds when `len(Backends) > 1 && NewEnvFor != nil`; with either unset — every default path — `executeSingle`/`raceEscalate` are **byte-identical** to the single `-backend` path. The `-backends native,codex,claude-code` flag (on `run` and the run-style commands sharing `buildRunOrchestrator`) activates it: `executeSingle` runs the trust-strongest backend first (`orderBackends` → `NewEnvFor(dir, names[0])`), and on a verify-FAIL `raceEscalate` cuts one fresh worktree per **distinct** backend so `route.Race` competes *different* backends and the verifier picks the winner. `agent.Selector` is satisfied by `trust.Selector` (built from `trust.Replay(<log>)`) **without** `internal/trust` importing `agent` — the Ledger plugs in, ranks by smoothed verifier-judged pass-rate, and a broken-chain `Replay` degrades to the configured order, never aborting. **I2 is preserved by construction:** the Selector only ORDERS attempt order; the verifier still decides "done" and judges the race. Per-backend providers/creds resolve through the SecretStore seam, never reaching the model (I3).

**Agentic browser + computer use (Phase 14 / Phase CU, additive, opt-in, contract-untouched).** Two GUI-agent loops drive a *persistent* session over many turns — observe → plan → act → verify — atop the same native loop, egress proxy, sandbox, capguard Rule-of-Two gate, and verifier. **Browser** (`nilcore browse`, `docs/ROADMAP-BROWSER-USE.md`): an accessibility **Set-of-Marks** snapshot (`internal/cdp` stamps `data-nilref`) the model acts on by ref; the browser runs in ONE long-lived in-sandbox `nilcore-browser --serve` Exec (I4) reached over a file-queue on `/work`; `internal/browsersession` version-stamps refs (stale-ref guard) and does host-side `{{secret}}` substitution so a credential never enters the model context (I3); `record_finding` writes typed `artifact.Claim`s re-verified in-box (I2). **Desktop** (`nilcore desktop`, `docs/ROADMAP-COMPUTER-USE.md`): a contained virtual X11 desktop driven through a **3-rung Set-of-Marks ladder** (AT-SPI refs → SoM-marked screenshot → raw coordinate), the fat driver shelling to image-baked tools (`internal/{desktopwire,desktopsession,desktopagent,som,desktop}`). Two paths: generic **Path B** (default, provider-agnostic) and native Anthropic **Path A** (opt-in, via the lone `model.Tool.Builtin` contract addition — byte-identical for every normal tool). A **native-macOS** tier (`docs/ROADMAP-COMPUTER-USE-DARWIN.md`) adds a pure-Go darwin driver (`screencapture`+`cliclick`, Rungs 2/3) and a `--mac-host` host-control mode — the one path where I4's sandbox boundary is *deliberately* relaxed, behind a separate opt-in + forced gate + kill-switch/allowlist (see CLAUDE.md §2 I4). Both default to byte-identical when their env gate is unset; **no module added** (I6).

**Provider upgrade — OpenAI / OpenRouter / openai-compatible (Phase 15, additive, contract-untouched).** The `model.Provider` seam now backs a configurable-BaseURL OpenAI adapter + a generic **`openai-compatible`** vendor (vLLM/Ollama/Groq/Fireworks/Azure/…) with an auth descriptor (Bearer/Azure/None), a single-key `max_tokens`/`max_completion_tokens` marshal (gpt-5.x/o-series), SOTA request fields (reasoning effort, structured outputs, tool_choice, service tier), OpenRouter typed routing/`models[]`/plugins + attribution headers, a typed `model.APIError` (terminal-vs-retryable + `Retry-After`) that `resilience.go` consumes, and prompt-cache/reasoning-token + authoritative-cost accounting in the meter. Anti-exfiltration (I3): a compat BaseURL **rejects** a canonical key-env with a key-free error. Every new request field is `omitempty` set once in `newRequest`, so a zero-valued config is **byte-identical** (`docs/ROADMAP-PROVIDERS.md`). `backend.CodingBackend` untouched (I1); stdlib-only (I6). Web search (native `BuiltinTool` + the client-side fenced fallback) is the remaining Phase-15 wave.

**Earned backend selection — `-backend auto` + preferred-backend (additive, Phase 13).** The default backend is no longer hard-wired to native. `-backend auto` (and config `backend: auto`, mapped by `applyConfigDefaults`) resolves to a single concrete backend at run time via `resolveAutoBackend`: `availableBackends(cfg, cred)` reports which of {native, codex, claude-code} actually have their CLI + key present on the host, and the survivors are ordered PREFERENCE-FIRST — the one-run `-prefer-backend` flag, else durable config `preferred_backend` (a CONCRETE backend, validated never to be "auto") — then re-ordered by the verifier-judged Trust Ledger (`trust.Replay` → `Order`), so a cold install honors the stated preference and accumulated evidence overtakes it. `-backends auto` expands the same available set into the multi-backend race (the `Selector` seam above). The selector and the preference only ORDER which backend RUNS; the verifier still judges every result (I2), and per-backend creds resolve through the SecretStore seam (I3).

### I2 — The verifier is the only authority on "done"
`Result.SelfClaimed` is advisory. After **any** backend runs, the orchestrator re-runs the project's checks (`verify.Verifier.Check`) and that boolean decides whether work ships. This is what makes delegating to black-box agents safe: their self-report never governs.

### I3 — No ambient authority
Secrets are held by the `SecretStore` (environment / OS keychain / encrypted vault / external), are injected per run, and are never written to disk in plaintext, logged, prompted, hard-coded, or given to the model. The process holds no broad filesystem or network authority by default. (See the Security section above and `docs/SECRETS.md` for the operational form.)

### I4 — Model-emitted execution is sandboxed
Every *shell command* a model emits, and every delegated coding CLI (Codex, Claude Code), runs inside the sandbox — a container, **or** host-native Linux namespaces + Landlock (see §Execution model) — against the worktree, so a model can never run an arbitrary program on the host. *Which* backend is a swappable implementation detail behind one interface; the guarantee is the same. The native loop's structured tools are the one deliberate, bounded exception: they run host-side but are confined to the worktree and cannot execute arbitrary code.

### I5 — Append-only audit
Every model call, tool execution, verify, and gate decision is appended to the event log. History is never mutated or deleted. The log is replayable and is the debugging spine.

### I6 — Zero-dependency core
Standard library only. A new module dependency requires justification in the PR + CHANGELOG. The sanctioned third-party exceptions are: **SQLite** (`modernc.org/sqlite`, Phase 4) — the persistent backbone for `internal/store` and the code-intelligence graph (`internal/codeintel/{graph,semantic}`); a pure-Go driver, so releases keep `CGO_ENABLED=0`; the **Charm TUI stack** (`bubbletea`/`lipgloss`/`bubbles`), isolated behind the `//go:build tui` tag so the default `nilcore` binary links **zero** Charm; and **`golang.org/x/sys`** (Phase 7), scoped to `internal/sandbox` for the namespace backend's Landlock / `no_new_privs` / seccomp syscalls — the Go project's own extended standard library (`golang.org/x/…`), already in the module graph transitively via the SQLite exception, so it adds nothing newly linked. The MCP client is **not** a dependency: it speaks JSON-RPC 2.0 over the standard library (`internal/mcp`), so MCP-as-code adds no module.

### I7 — Untrusted input boundary
Tool output, file contents, and fetched web content are data, never controlling instructions. The agent's directives never originate from tool results.

## Closed-loop autonomy (Phase 16) — recorded relaxations & layer additions

Phase 16 closes the loop on the agent's own verifier-judged evidence so it depends on the operator less while staying inside all seven invariants (full plan: `docs/ROADMAP-CLOSED-LOOP.md`). Every pillar is **opt-in and default-off** — an operator who turns nothing on sees a byte-identical binary. The verifier (I2), the sandbox (I4), no-ambient-authority (I3), and the append-only log (I5) are never weakened; the program moves the human *from per-action approval to policy + envelope + earned trust*, and makes self-verification carry more weight.

**§0 recorded relaxation — graduated auto-approval (the SECOND human-gate relaxation, parallel to the `--mac-host` I4 relaxation).** `internal/graapprove.GradedApprover` *wraps* the human approver and auto-approves a structured `policy.GateAction` ONLY when the action-class+scope has EARNED trust (verifier-green ≥ N times, recent, over an unbroken chain — folded from a dedicated `boundary_outcome` event, never a self-report) AND the action sits within the operator-authored envelope AND the shared blast-radius budget still admits it; anything else falls through to the human. A free-text `Approve(string)` gate is **never** auto-approved. This relaxes the *default that every irreversible action needs a human* — it grants **no ambient authority** (the envelope is operator-authored host-side data, fail-closed, and never reaches the model — I3) and never lets unverified work ship (I2). It is reached only behind its own opt-in (`onboard.Config.AutoApprove` / a `nilcore init` preset / `NILCORE_AUTOAPPROVE_PRESET`), is bounded by the shared `internal/blastbudget` fence, and is revocable by an instant kill-switch (`.nilcore/AUTOAPPROVE_OFF` / `NILCORE_AUTOAPPROVE_OFF=1`).

The three operator-approved presets, and **the rule that NO preset ever admits `main`/`master`/`release`/`prod`** (deny always wins; `prod*` is denied structurally for Deploy):

| Preset | Classes | Trust bar (green / sample / recency) | Rate | $/day | Blast (`-blast-radius`) |
|---|---|---|---|---|---|
| **conservative** (default) | OpenPR only | 5 / 5 / 14d | 3/day | $0 | — |
| **standard** | + PromoteToBase on **non-main** | 10 / 10 / 14d | 2/day | $0 | `tight` = hosts 4 · irrev 2 · wall 10m · $1/day |
| **trusted** | + Deploy to **staging** (`prod*` denied) | 20 / 20 / 7d | 2/day | $25/day | `standard` = hosts 8 · irrev 5 · wall 20m · $5/day |

The blast presets (`-blast-radius tight|standard`; `off` = unfenced, the default) are operator-approved policy, not developer defaults; `internal/blastbudget` is the **single** $/rate/irreversible/host/wall meter the envelope reads (no second counter — XC-T01), and its per-UTC-day $ window **rebuilds from the log on boot** (no fail-open on restart — XC-T04).

**§0 recorded relaxation — self-improve auto-merge (a SEPARATE double opt-in).** The flywheel may auto-merge an edit to its OWN prompts/skills only behind `NILCORE_SELFIMPROVE_AUTOAPPROVE` (DISTINCT from enabling the flywheel — no transitive opt-in, XC-T02), and only after the verifier is green and the measured-delta fence has accepted it; `selfimprove.DefaultScope` denies `internal/verify/` so the flywheel can **never author or edit the verifier of record** (charter §0). Every auto-merge is audited (`auto_approve_selfimprove`).

**No transitive opt-in (XC-T02), model-blind (XC-T03), operator-only objectives (XC-T06).** No single flag/env transitively reaches `auto_approve` — each powerful relaxation needs its own recorded gate. The graduated-approval policy (`internal/graapprove`) never links into the model-facing loop (`internal/backend`/`internal/model`), so the envelope, trust tallies, and blast state never reach the model (I3). Objective CRUD (`internal/objective`) never links into the model tool surface (`internal/tools`), so a model can only DO the work a standing objective names, never enqueue/edit its own.

**New leaves (additive; each carries a `deps_test.go` leaf guard — I6):** `internal/experience` (the closed-loop spine — one derived, rebuildable projection over the log: `Reader`/`OverLog`/`OverStore`/`Projector`, folded from verifier-judged `race_outcome` only, kept warm by an optional `eventlog.Log.OnAppend` hook behind `NILCORE_EXPERIENCE`); `internal/capability` (`For(Request)→Descriptor`, the legible "what may this drive do"); `internal/graapprove` (graduated auto-approval); `internal/blastbudget` (the four-axis runtime fence, threaded into the egress proxy `ServeHTTP`, the sandbox `ExecWithEnv`, and the gate); `internal/flywheel/{selfeval,distiller,measure,loop}` (the self-improvement flywheel — `nilcore flywheel`, verified + human-gated, bounded cadence); `internal/autosrc` + `internal/objective` (the autonomy daemon + the operator-only standing-objectives backlog — `nilcore objective`, folded into `serve` behind `NILCORE_AUTONOMY`); `internal/verify/{vcache,selfacc}` + `internal/memory/lessons` (learn-from-scars: a chain-verified content-hash verify cache behind `NILCORE_VCACHE`, self-acceptance, and distilled-failure lessons behind `NILCORE_LESSONS`). Routing gains a nil-safe `agent.TrustOracle` seam (`NILCORE_TRUST_DEFAULT=1`) that only orders/prunes/sizes candidacy — the verifier still judges every race (I2).

**Pillar 8 — the unified orchestration kernel (`internal/kernel`, default-on via `NILCORE_KERNEL`; `docs/ROADMAP-KERNEL.md`, `docs/ROADMAP-KERNEL-V2.md`).** The three orchestration machines — the single-task orchestrator (FLAT), the project loop, and the verified swarm (both DECOMPOSE) — collapse onto ONE recursive engine so `run`/`build`/`swarm` become `Envelope` presets and the conversational router picks an envelope, not a machine (the answer to "five products at once"). `kernel.Run(ctx, Envelope, Node)` dispatches a node to the envelope's `Flat` or `Decompose` branch per a `Granularity` policy, bounded by `MaxDepth`; `kernel.Recursive` is the native fan-out — plan → run each child back through `Run` (a child may itself decompose) → integrate + **re-verify the integrated tip even when every child verified** (green children can integrate red — I2). It is a **pure-stdlib leaf** importing NO machine (`deps_test`-guarded — I1/I6): `backend`/`agent`/`project`/`swarm` plug in as injected `RunFunc`/`Plan`/`Integrate` closures the cmd layer wires; the kernel NEVER marks a node done (`Outcome.Verified` comes only from the injected runner's verifier — I2), appends nothing (the runners own their event sequences — I5), and carries only structural data on `Node` (I3/I7). **The cutover is equivalence-proven:** the `*ViaKernel` helpers wrap the SAME machine as the envelope's runner and route it through `kernel.Run`, which an equivalence harness proves is **event-for-event identical** to the legacy run; with the escape hatch set (see below) every entrypoint calls its legacy machine directly (byte-identical). `MaxDepth` defaults to 1 (reproducing the legacy single-level fan-out); >1 is the new recursive capability, reachable once a machine is re-expressed as `Plan`/`Integrate`. `NILCORE_KERNEL` is now **default-on** (escape hatch `NILCORE_KERNEL=0|off|false|no` calls the legacy machine directly, byte-identical). **UOK V2 (`internal/router`, `docs/ROADMAP-KERNEL-V2.md`) builds the router the kernel was designed for:** `router.Classify(goal)→run|build|swarm` plus an `Oracle` seam (the learned/model-backed path; `nil ⇒` heuristic), backing `nilcore do -goal "…"` — one entry that routes a goal to the cheapest preset that fits and dispatches to that proven machine, so the agent picks how to work. The router only ORDERS the machine choice (fail-closed); the chosen machine still owns verify (I2), gate (I3), and log (I5). The native `Recursive` engine stays available, tested, and **dormant** (no production consumer yet) — the roadmap records why the *iterative* project-loop is deliberately NOT force-fit onto the *one-shot* recursive engine, and what a real recursive `decompose` preset would require.

## Execution model (the two tiers under I4)

I4 is precise about *where* model-influenced work runs. There are exactly two tiers, and the boundary is "can this run an arbitrary program?":

**Tier 1 — sandboxed (arbitrary execution).** Anything a model can use to run an arbitrary program is isolated in the container:
- the `run` shell tool — every command the model emits;
- delegated coding CLIs (Codex, Claude Code) — wrapped in *our* container, not trusted to self-sandbox;
- MCP glue code — runs in the sandbox under the gate + egress allowlist.

Nothing in this tier touches the host. The container is rootless, drops capabilities, mounts the rootfs read-only, and defaults to deny-all egress (an allowlist proxy is the only way out — `internal/policy`, and it refuses loopback/link-local/private destinations so it can't be turned into an SSRF pivot).

**Two Tier-1 backends, one interface (the isolation spectrum).** `sandbox.Sandbox` has two implementations and `sandbox.New` auto-detects between them (override with `-sandbox auto|namespace|container` or `NILCORE_SANDBOX`):

- **Container** (`sandbox.Container`, podman/docker) — the portable choice wherever a runtime is present: a separate image rootfs, `--cap-drop=ALL`, `--security-opt no-new-privileges`, a read-only rootfs + tmpfs, the worktree bind-mounted at `/work`, and `--network none`.
- **Namespace** (`sandbox.Namespace`, Linux only) — needs **no runtime, image, or daemon**, so the loop stays sandboxed (I4) on a bare host (a cheap VPS, a Pi, a locked-down CI runner). Each command re-execs the nilcore binary inside fresh user/mount/pid/net/ipc/uts namespaces (mapped to a userns-only root that holds no host capability); the re-exec'd child — `sandbox.MaybeRunInit`, the first call in `main` — sets `no_new_privs`, a **Landlock** domain (read+execute the host toolchain everywhere, read+write **only** the worktree, a `/tmp` scratch, and the usual character devices like `/dev/null`), and a **seccomp-bpf** syscall denylist (P7-T02: EPERMs `mount`/`ptrace`/`kexec`/module-(un)load/`setns`/`unshare`/`chroot`/keyring/clock-set/… and allows the rest — defense-in-depth, applied via `seccomp(2)`+TSYNC and graceful when the kernel lacks it), then `execve`s `/bin/sh -c <cmd>`. `execve` carries the Landlock domain, `no_new_privs`, and the seccomp filter into the command, so the model's command runs *only after* confinement is in place; the pre-`execve` code is trusted harness code, never model input. `CLONE_NEWNET` with no configured interface is the default-deny-egress equivalent of `--network none`.

The isolation strength runs **Firecracker microVM (strongest; Linux/KVM; future) → container → namespace + Landlock (lightest, most portable)**. `New` prefers the namespace backend wherever the kernel offers Landlock (≥5.13) and unprivileged user namespaces, but is **conservative**: it treats an AppArmor- or sysctl-restricted userns as unsupported and falls back to a container rather than risk an `EPERM` at exec, so `auto` is always correct. The one deliberate trade for needing no runtime: the namespace backend shares the host's filesystem **read-only** (enforced by Landlock) instead of a separate image rootfs. Because the security property (real confinement) is only observable on Linux, a dedicated `sandbox-linux` CI job runs the escape tests with `NILCORE_SANDBOX_MUST_RUN=1` — they fail rather than skip — as the authoritative verifier.

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

**Typed model errors: `model.APIError` (terminal-vs-retryable + Retry-After).** A
model call can fail for two very different reasons, and the resilience wrapper now
distinguishes them. `model.APIError` is a small, vendor-neutral typed error a
provider adapter may return for an HTTP-layer failure:
`{ StatusCode int; Retryable bool; RetryAfter time.Duration; Type, Code, Message string }`.
Its `Error()` is **key-free** (I3): it renders only the status, the vendor's
machine-readable `Type`/`Code`, and the human `Message` — never an API key, a
header, or the request body. `NewAPIError` classifies the status: **429/500/502/
503/504 ⇒ retryable**, **400/401/403/404/422 ⇒ terminal** (an unlisted 5xx is
treated as retryable, anything else terminal). A minimal stdlib `Retry-After`
parser reads both wire forms — integer seconds and an HTTP-date — yielding `0` for
an empty, malformed, or past value.

`model.Resilient` inspects the failure with `errors.As(err, &apiErr)`:

- A **terminal** `*APIError` (e.g. a bad key or malformed request) **fails fast** —
  no further retry of that provider **and no failover**, because a different
  provider cannot fix a request the caller got wrong. The typed error surfaces to
  the caller verbatim (still extractable via `errors.As`).
- A **retryable** `*APIError` retries using the existing exponential-backoff +
  jitter + breaker logic, with one addition: its `RetryAfter` is treated as the
  backoff **floor** — when the server's hint exceeds the computed backoff, the
  wrapper waits at least that long (a shorter hint is ignored; the computed backoff
  still dominates).
- A **plain (untyped) error** is, by construction, neither terminal nor
  Retry-After-bearing, so it retries, fails over, and times its backoff **exactly
  as before** — the backward-compatibility guarantee, proven by GATE-1 tests. Every
  existing provider that returns untyped errors is unaffected.

**Additive `model.Usage` fields.** `Usage` gains three `omitempty` fields beyond the
frozen `InputTokens`/`OutputTokens`: `ReasoningTokens` (hidden thinking tokens a
vendor breaks out separately), `CachedTokens` (input tokens served from a prompt
cache), and `CostUSD` (per-call cost when a vendor or the meter computes it). They
are purely additive — a `Usage` that leaves them zero marshals byte-identically to
the pre-change shape, so neither the `Provider.Complete`/`Stream` contract (I1) nor
any existing adapter changes.

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

**Work-route authority (Phase 13).** For the auto-router (`SupervisorFirstRouter`),
the frontier-model **classifier is AUTHORITATIVE** for the work-route: a parseable
proposal (`chat` < `native` < `supervise` < `project`, sized by the work via a
capability+cost manifest) is honored as-is by `reconcile`. The string heuristic
(`ShouldSupervise`) is now ONLY the no-model / unparseable-output **fallback**
(`fallback`), never an overrule of a parseable proposal. Routing is still a
dispatcher, never a rail — every route terminates in the same verifier (I2) and the
same conversation budget ceiling, so a mis-route can only cost money, never defeat a
rail. `nilcore run` gains an optional default-OFF `-auto-supervise` seam that wires
the SAME classifier (when a native provider exists) to let a complex goal scale up to
the supervised loop; off ⇒ byte-identical single-task run.

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
| `internal/sandbox` | container **and** namespace+Landlock command execution | stdlib + `golang.org/x/sys` (namespace backend, Linux) |
| `internal/verify` | run project checks, report pass/fail | `sandbox` |
| `internal/eventlog` | append-only JSONL audit | stdlib only |
| `internal/policy` | reversibility classifier + gate | stdlib only |
| `internal/backend` | `CodingBackend` + native/codex/claude-code | `model`, `sandbox`, `verify`, `eventlog` |
| `internal/emit` | live reasoning/intent sink (conversational) | stdlib only |
| `internal/inbox` | user→agent message seam (queue/steer) | `model`, `eventlog` |
| `internal/steering` | trusted operator steering file (`NILCORE.md`/`AGENTS.md`) loader (I7 exception) | stdlib only |
| `internal/embed` | OpenAI-compatible embedder for semantic search (opt-in) | stdlib only |
| `internal/scmhook` | HMAC-verified SCM/CI webhook → `trigger.Signal` | stdlib only |
| `internal/cron` | cron / interval schedule parser + self-start clock | stdlib only |
| `internal/forge` | draft-PR opener over the SCM HTTP API (gated, post-approval) | stdlib only |
| `internal/registry` | versioned skills/MCP registry (`list`/`install`, local) | stdlib only |
| `internal/agent` | orchestrator (run backend, final verify) | `backend`, `verify`, `eventlog`, `policy` |
| `internal/session` | conversational state container + auto-router + drivers | `agent`, `backend`, `super`, `project`, `summarize`, `inbox`, `emit`, `model`, `eventlog`, `policy`, `store` |
| `internal/worktreefs` | symlink-safe worktree FS confinement (`SafeJoin`/`OpenNoFollow`/`WriteAtomic`) — one audited copy (Phase 11) | stdlib only |
| `internal/browserwire` | shared shell-quote + browser-observation contract (the I4 quoting boundary) | stdlib only |
| `internal/artifact` | typed evidence artifacts (`Artifact`/`Claim`/`Evidence`) + worktree persistence (Phase 11) | `internal/worktreefs` + stdlib |
| `internal/artifact/evverify` | verifier-id registry + `ArtifactVerifier` (folds into `verify.Composite`, I2) | `artifact`, `verify`, `sandbox`, `worktreefs` |
| `internal/artifact/packs/{web,software,finance,ui}` | reusable domain verifier packs (curl-in-box, no module — I6) | `artifact`, `evverify`, `sandbox`, `worktreefs` (ui: `+browserwire`) |
| `internal/requeue` | field-granular requeue worklist + bounded retry ledger | `artifact` + stdlib |
| `internal/egressprofile` | named research egress presets + project-local `.nilcore/egress.json` | `policy` + stdlib |
| `internal/report` | read-only verification-report model + swarm source/claim-trace + matrix (Phase 12) | `eventlog`, `artifact`, `worktreefs` |
| `internal/report/render` | text / HTML / markdown / matrix renderers for the verification report | `internal/report`, `termui` |
| `internal/artifact/schema` | structural artifact-shape validation + `SchemaVerifier` (Phase 12, fail-closed `Named[0]`) | `artifact`, `verify`, `sandbox`, `worktreefs` |
| `internal/artifact/packs/{audit,benchmark,code}` | the three Phase-12 verify-packs + `packs.Build` assembler (unknown pack ⇒ error) | `artifact`, `evverify`, `sandbox`, `verify`, `schema` |
| `internal/pool` | tiered/capped/failover/metered provider pool (Phase 12, swarm) | `model`, `provider`, `meter`, `budget`, `strongcap` |
| `internal/swarm` | shard model + durable queue + sharder + runner + multi-pass controller (Phase 12) | `artifact`, `evverify`, `requeue`, `verify`, `spawn`, `scheduler`, `integrate`, `budget`, `meter`, `store`, `eventlog`, `worktree(fs)`, `sandbox`, `planner`, `pool` — **never** `super`/`agent`/`project` |
| `internal/swarm/board` | live verifier-driven scoreboard + `//go:build tui` dashboard (zero Charm by default) | `budget`, `meter`, `eventlog`, `store`, `termui` (read-only `report` in tests) |
| `internal/swarm/preset` | the five named swarm bundles (plain data; **does not import `swarm`**) | `artifact`, `packs`, `roster`, `backend`, `policy` |
| `internal/trust` | the Trust Ledger: replays `race_outcome` + `eval.Report` into a verifier-judged per-backend scoreboard; `trust.Selector` orders backend names for the orchestrator (Phase 13) — **never** imports `agent` | `eventlog`, `backend`, `termui` + stdlib |
| `internal/trace` | causal "why did it do that" tree over the hash-chained log (metadata-only, untrusted over a tampered chain, Phase 13) | `eventlog` + stdlib (`termui` for the `//go:build tui` explorer) |
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
| 7 | second `sandbox.Sandbox` backend (`sandbox.Namespace`, Linux) + `sandbox.New` auto-detect/`Options`/`Available` + `sandbox.MaybeRunInit` re-exec hook in `cmd/nilcore`; `-sandbox` flag + `NILCORE_SANDBOX` env; `golang.org/x/sys` promoted to a direct dep (I6 exception); a `sandbox-linux` CI job | **portability & efficiency** — drop the hard container-runtime requirement. A host-native namespaces + Landlock sandbox confines model-emitted execution (I4) with no runtime, image, or daemon, auto-detected and preferred over a container wherever the kernel supports it. Additive: the container backend and every caller are unchanged (the factory returns the existing `Sandbox` interface); off Linux and on unsupported kernels it falls back to a container, byte-identical. See the *Execution model* §isolation spectrum above |
| 8 | nil-gated `Peer backend.Peer` seam on `backend.Native` (bus tools `ask_supervisor`/`share_finding`/`request_review`); `internal/agent/bus` (`*bus.AgentPeer`); supervisor/sub-agent concurrency in `internal/super` | **full multi-agent concurrency** — a supervisor races/coordinates sub-agents over a bus; every peer reply is `guard.Wrap`-fenced as data (I7). Nil `Peer` ⇒ single-agent default, byte-identical (gated exactly like `Advisor`). See I1's *bus seam* note above |
| 9 | additive image shape on `model.Block` + `tools.ImageRunner`/`Registry.DispatchRich` (P9-T01); the `browser_view` tool + pure-Go `nilcore-browser` driver (`cmd/tools/nilcore-browser`); composite verifier (opt-in `NILCORE_BROWSER_VERIFY`); `internal/scmhook` + `internal/cron`; `nilcore serve --webhook`, `nilcore schedule` | **behavioral verification & event-driven autonomy** — a sandboxed headless browser hands the model a screenshot multimodal block and folds a behavioral check **into** `verify.Check` (the verifier stays sole authority, I2; live run CI-only, fails closed). Event-driven/scheduled self-start: an HMAC-verified webhook (`NILCORE_WEBHOOK_SECRET`/`NILCORE_WEBHOOK_LABEL`) and cron/interval both route through the **existing** reversible-auto-start / human-gate machinery (headless ⇒ irreversible work deny-defaults). All additive + opt-in (`Provider.Complete` unchanged — I1; no module — I6). See I1's *multimodal* + *behavioral-verification* notes above |
| 10 | `internal/steering` + nil-gated `SteeringContext` seam on `backend.Native` (P10-T01); pure-Go HNSW + content-hash cache in `internal/codeintel/semantic` (D2) over `internal/embed`; multi-language `internal/codeintel/ast` (19 languages / 34 extensions, CGO-free; heuristic except Go, LSP the precise lens) (D3/R2; non-Go backends beyond TS/JS+Rust shipped in the Phase-13 languages batch); `internal/registry` + `nilcore registry list\|install` (P10-T06) | **context depth, trusted steering & distribution** — a trusted operator steering file (`NILCORE.md`/`AGENTS.md`) as the scoped I7 exception (bounded below the safety core); semantic search graduates to a pure-Go HNSW vector index (opt-in `NILCORE_EMBED_KEY`, lexical fallback when off); code intelligence parses 19 languages (Go precise via `go/parser`, the rest broad heuristic scanners — Java, C/C++, C#, Ruby, PHP, Kotlin, Swift, Scala, Dart, Zig, Bash, Lua, Elixir, SQL, …); a versioned local skills/MCP registry. Remote fetch stays **gated** as EXT-07 (`docs/ROADMAP-EXTERNAL-INFRA.md`). All additive + opt-in, no module (I6) |
| D1–D4 | `cmd/tools/nilcore-browser` + composite verifier (D1); `internal/embed` + semantic HNSW (D2); Python/JS/TS/Rust backends in `internal/codeintel/ast` (D3/R2); `internal/forge` + nil-gated orchestrator `KeepBranch`/`Outcome.Branch` + `watch/schedule --open-pr` (D4) | the four formerly-deferred items (`docs/IMPLEMENTATION-PLANS.md`), now implemented + merged. Gated PR (D4): a **draft** PR is opened via `internal/forge` **only after the human gate**; the agent never merges; `NILCORE_FORGE_TOKEN` from the SecretStore, scrubbed from logs (I3/I5). All pure-stdlib (no module — I6); each is additive + opt-in (default disposable cleanup byte-identical). See I1's *branch-preservation* + *behavioral-verification* notes above |
| 11 | `internal/{worktreefs, browserwire, artifact, artifact/evverify, artifact/packs/*, requeue, egressprofile, report, report/render}` + additive `spawn.Result.Artifact`, `super.RequeueHook`, `onboard.WebConfig.{Profile,ProfileFile}`; cmd wiring (`NILCORE_EVIDENCE_VERIFY`/`NILCORE_VERIFY_PACKS`/`NILCORE_REQUEUE`/`-egress-profile`) + `nilcore report` | **verifier-backed artifact factory** — code becomes one artifact type among many (reports/matrices/audits/benchmarks/dossiers). A typed `artifact.Artifact{Claims[]}` (each `Claim` carrying `Evidence{value,source_url,retrieved_at,extraction_method,verifier,status}`) rides **out-of-band** as worktree JSON (I1 untouched); `evverify.ArtifactVerifier` folds into `verify.Composite` so artifact-GREEN is **verifier-produced** (I2), reusable domain packs reach live data only in-box (curl + `encoding/json`, no module — I4/I6) with keys staying in the SecretStore (I3). Typed worker results merge only verified fields (prose fenced — I7); granular requeue re-dispatches one failed claim via the existing DAG; named egress profiles widen the tree while `EgressFor` clamps narrow-only (default-deny preserved); a read-only `nilcore report` renders the trust story and refuses GREEN over a broken chain (I5). All additive + opt-in (default binary byte-identical). Full plan: `docs/ROADMAP-EVIDENCE-ARTIFACTS.md` |
| 12 | `internal/{pool, swarm, swarm/board, swarm/preset, artifact/schema, artifact/packs/{audit,benchmark,code}}` + additive `internal/{report, roster, onboard}`; `nilcore swarm` + `buildSwarm` (one `cmd/nilcore` dispatch arm); `nilcore report --format json\|matrix` | **verified swarm mode** — a bounded **in-process** `nilcore swarm` surface over the Phase-11 spine: N shards fan into a capped pool (`scheduler`/`spawn.DAGScheduler`), each producing a **typed artifact** judged by a **verify-pack** (`evverify.ArtifactVerifier`, the sole ship gate, I2; `ShipGate` refuses `verify.Pass`/nil; unknown pack ⇒ fail-closed), requeuing **only failed shards** until clean (`internal/requeue`) and folding green work through the serial Integrator (never lands to base). A tiered/capped/failover provider **pool** (`meter`/`strongcap`/`model.Resilient`, no key to a decorator — I3), five presets (research/code/audit/benchmark/ui), and a live verifier-driven **scoreboard** (`internal/swarm/board`, `//go:build tui` Charm linking zero by default). **In-process / single-host / bounded** — multi-host dispatch crosses into `EXT-01`, out of scope. Additive + default-off byte-identical (one dispatch arm; no existing package imports a swarm/pool leaf); stdlib only (`go.mod` unchanged, I6). Full plan: `docs/SWARM.md` |

## Security model (summary)

- **No ambient authority (I3)** — per-run, scoped, revocable credentials.
- **Sandbox model-emitted execution (I4)** — shell + delegated CLIs + MCP glue run in a container per task/worktree; default-deny network; SSRF-safe egress allowlist (Phase 2); delegated CLIs wrapped in our own container. The structured file/git tools run host-side but stay worktree-confined and cannot execute arbitrary code (see §Execution model).
- **Untrusted input boundary (I7)** — fetched/file content never becomes instructions.
- **Bounded autonomy** — reversible actions auto-run; irreversible actions hit the human gate. Worktrees make coding reversible by construction, so gates concentrate at merge/deploy.
- **Full audit (I5)** — append-only, hash-chained (Phase 2), secrets redacted.

Operational detail and key handling: `docs/PREREQUISITES.md`.
