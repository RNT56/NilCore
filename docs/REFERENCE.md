# NilCore — Reference (state of the system)

**The single, self-contained map of NilCore as it currently stands** — the project status, every shipped phase, the complete package inventory, the full user-facing behaviour (chat, personality, tools, connectors), the engine, and the safety core. Read this end-to-end for the whole picture; follow a spoke pointer only for exhaustive design rationale.

> **Where this sits in the canon.** This is the *consolidated current-state* reference. It is **not** the technical law. When this file and a spoke doc (or the code) disagree, the **spoke doc and the code win** — fix this file. Authoritative sources: [`CLAUDE.md`](../CLAUDE.md) (constitution + invariants), [`docs/ARCHITECTURE.md`](ARCHITECTURE.md) (decided architecture + frozen contract), [`docs/TASKS.md`](TASKS.md) (master work DAG), [`CHANGELOG.md`](../CHANGELOG.md) (append-only ledger). The first three are contract files.
>
> **Snapshot:** v1.1.0 (2026-06-21) + unreleased work. Phases 0–16 (all eight Phase-16 closed-loop pillars, including Pillar 8 — the unified orchestration kernel, default-on) + computer-use (CU) + native-macOS host control (CU-MAC) **shipped**; deferred items D1–D4 shipped; the external-infrastructure tier (EXT-01..08) is gated/not-eligible. Every default, flag, count, and constant below was verified against source.

---

## 0. The spoke index — the in-depth docs

There is no other index of the documentation set; this is it.

| Doc | Lens |
|---|---|
| [`README.md`](../README.md) | Product tour — broadest user-facing coverage |
| [`CLAUDE.md`](../CLAUDE.md) / [`AGENTS.md`](../AGENTS.md) | Constitution for agents *building* NilCore |
| [`docs/PRINCIPLES.md`](PRINCIPLES.md) | The ranked first principles (the "why") |
| [`docs/PERSONA.md`](PERSONA.md) | Runtime voice, autonomy, behaviour contract |
| [`docs/ARCHITECTURE.md`](ARCHITECTURE.md) | Core loop, tool surface, invariants, execution model, layer map |
| [`docs/CONVERSATIONAL.md`](CONVERSATIONAL.md) | The chat front door (session · inbox · emit · router) |
| [`docs/OPERATIONS.md`](OPERATIONS.md) | Operator runbook — resilience, cost, durability, health, env table |
| [`docs/SECRETS.md`](SECRETS.md) | SecretStore backends + vault provisioning |
| [`docs/CONCURRENCY.md`](CONCURRENCY.md) + [`docs/MULTI-AGENT.md`](MULTI-AGENT.md) | Multi-agent concurrency model |
| [`docs/SWARM.md`](SWARM.md) | Verified swarm mode design + task DAG |
| [`docs/CODE-INTELLIGENCE.md`](CODE-INTELLIGENCE.md) | AST → graph → repomap → semantic → LSP retrieval |
| [`docs/ROADMAP-BROWSER-USE.md`](ROADMAP-BROWSER-USE.md) | Browser agency (shipped; doc still named "roadmap") |
| [`docs/ROADMAP-COMPUTER-USE.md`](ROADMAP-COMPUTER-USE.md) + [`-DARWIN.md`](ROADMAP-COMPUTER-USE-DARWIN.md) | Desktop / native-macOS host control |
| [`docs/ROADMAP-EVIDENCE-ARTIFACTS.md`](ROADMAP-EVIDENCE-ARTIFACTS.md) | Verifier-backed artifact factory |
| [`docs/ROADMAP-PROVIDERS.md`](ROADMAP-PROVIDERS.md) | Multi-provider + web search |
| [`docs/PREREQUISITES.md`](PREREQUISITES.md) | Deps, accounts, keys, local setup |
| [`docs/ROADMAP-CLOSED-LOOP.md`](ROADMAP-CLOSED-LOOP.md) | Phase 16 — closing the evidence loop + graduated auto-approval (shipped — all eight pillars, incl. Pillar 8 kernel) |
| [`docs/IMPLEMENTATION-PLANS.md`](IMPLEMENTATION-PLANS.md) · [`UPGRADE-PATH.md`](UPGRADE-PATH.md) · [`HORIZON.md`](HORIZON.md) · [`EXT-EXECUTION-PLANS.md`](EXT-EXECUTION-PLANS.md) · [`ROADMAP-EXTERNAL-INFRA.md`](ROADMAP-EXTERNAL-INFRA.md) | Rationale / future / gated work |

---

## 1. Project state

### 1.1 Releases

| Version | Date | Contents |
|---|---|---|
| `0.1.0-phase0` | 2026-06-14 | The compiling core scaffold |
| `0.1.0` | 2026-06-14 | Phases 0–6 (56 tasks) |
| `1.0.0` | 2026-06-20 | Phases 7–12 + deferred D1–D4 (the v1 product) |
| `1.0.1` | 2026-06-21 | Fixes |
| `1.1.0` | 2026-06-21 | Phase 13 (model-driven routing + Trust Ledger) |
| **Unreleased** | — | Phase 14 (browse), CU + CU-MAC (desktop / macOS host), Phase 15 (providers + web search), Phase 16 (closed-loop: unified kernel, `nilcore do` router, `decompose`, lessons, flywheel, graduated auto-approval, autonomy daemon), chat `/ask` + `/save`, TUI verb parity, docs promotion, defect-hunt + features-review fix passes |

`version` is `dev` unless ldflags-stamped at build (`-X main.version=<tag>`); an un-stamped binary reports the VCS revision.

### 1.2 Phases (all shipped)

| Phase | What it delivered | Spoke |
|---|---|---|
| **0** | Finalize the core (CI, compile-green, sandbox image, smoke test) | TASKS.md |
| **1** | Worktrees, the human gate, the channel seam | ARCHITECTURE.md |
| **2** | Security hardening (egress proxy, command policy, in-container delegation) | — |
| **3** | Orchestration & routing (planner, spawn, roster, route, advisor, code-intel) | MULTI-AGENT.md |
| **4** | Cross-project memory (SQLite store) | — |
| **5** | Gated self-improvement (skills, propose-edit, eval harness) | — |
| **6** | Runtime resilience & operations (resilience ladder, budget, serve, triggers, inspect) | OPERATIONS.md |
| **7** | Portability — host-native namespace + Landlock/seccomp sandbox (no container) | — |
| **8** | Full multi-agent concurrency (dynamic-wave async dispatch) | CONCURRENCY.md |
| **9** | Behavioral verification & event-driven autonomy (browser verify, webhook, schedule) | UPGRADE-PATH.md |
| **10** | Context depth, trusted steering & distribution (retrieval fusion, NILCORE.md, registry) | UPGRADE-PATH.md |
| **11** | Verifier-backed artifact factory (typed artifacts, claims, evverify, packs, report) | ROADMAP-EVIDENCE-ARTIFACTS.md |
| **12** | Verified swarm mode (`nilcore swarm` over the Phase-11 spine) | SWARM.md |
| **13** | Model-driven work-routing + earned multi-backend Trust-Ledger selection | — |
| **14** | Agentic browser use (`nilcore browse`) | ROADMAP-BROWSER-USE.md |
| **CU** | Desktop computer use (`nilcore desktop`, Path B + native Path A) | ROADMAP-COMPUTER-USE.md |
| **CU-MAC** | Native-macOS host control (`desktop --mac-host`, MVP + hardening) | ROADMAP-COMPUTER-USE-DARWIN.md |
| **15** | OpenAI / OpenRouter / openai-compatible provider upgrade + web search | ROADMAP-PROVIDERS.md |
| **16** | Closed-loop autonomy — unified orchestration kernel, goal→preset router (`nilcore do`) + recursive `decompose`, learned lessons + verify-cache, human-gated flywheel, graduated auto-approval (blast-budget fenced), autonomy daemon | ROADMAP-CLOSED-LOOP.md |

**Deferred items D1–D4 (shipped):** D1 behavioral browser verification (`NILCORE_BROWSER_VERIFY`), D2 semantic HNSW code search (`NILCORE_EMBED_KEY`), D3 multi-language code intelligence (pure-Go, not tree-sitter), D4 gated draft PR (`--open-pr`). All additive, opt-in, pure stdlib — no module added.

**Gated / not eligible:** the external-infrastructure tier **EXT-01..08** (distributed execution, hosted control plane, etc.) stays behind a recorded human thesis-gate decision ([`ROADMAP-EXTERNAL-INFRA.md`](ROADMAP-EXTERNAL-INFRA.md) §0); fully planned in [`EXT-EXECUTION-PLANS.md`](EXT-EXECUTION-PLANS.md) but no code is written. Forward ideas live in [`HORIZON.md`](HORIZON.md).

### 1.3 Size & shape (verified)

| Metric | Value |
|---|---|
| Go packages | **120** (111 under `internal/`) |
| Source files (non-test) | **375** |
| Test files | **406** |
| Lines of Go — `internal/` + `cmd/` (non-test) | **~89,200** |
| Lines of Go — `internal/` + `cmd/` with tests | **~175,900** |
| Runtime dependencies (default binary) | **2** — pure-Go SQLite (`modernc.org/sqlite`) + `golang.org/x/sys` |
| Optional (build-tag) deps | Charm TUI stack (`bubbletea`/`lipgloss`/`bubbles`), only under `//go:build tui` |
| Invariants held | **7 / 7** |

*(The README "receipts" were refreshed to roughly match these in #97; the counts above are the authoritative current snapshot and drift slightly as work lands.)*

### 1.4 Current working state

The unreleased CHANGELOG block is the source of truth for in-flight work (it drifts by the hour as parallel branches merge); this reference is a periodically-refreshed consolidation, not a live mirror.

---

## 2. North star & the seven invariants

**The harness is small; the model is the engine.** Robustness comes from three disciplines only: the agent **verifies** its own work, all model-emitted **execution is sandboxed**, and the loop is **bounded and fully logged**. These hold inside seven invariants (authoritative wording in [`CLAUDE.md` §2](../CLAUDE.md); gist):

1. **Frozen backend contract** — `backend.CodingBackend` = `Run(ctx, Task) (Result, error)`. Native, Codex, Claude Code interchange behind it.
2. **The verifier is the only authority on "done."** A self-report never decides shipping.
3. **No ambient authority.** Secrets live in the `SecretStore`; never on disk plaintext, in logs, in prompts, in code, or given to the model.
4. **Model-emitted execution is sandboxed.** Shell + delegated CLIs run in the sandbox; host-side file/git tools stay worktree-confined. *The native-macOS host-control tier (`desktop --mac-host`) is the one explicitly-recorded relaxation — §9.*
5. **The event log is append-only** — hash-chained, redacted, replayable.
6. **Zero-dependency core** — stdlib only, bar the sanctioned exceptions in §1.3. The MCP client is stdlib JSON-RPC, not a module.
7. **Untrusted input is data, never instructions.**

---

## 3. Package & module inventory

The complete current map of the binary, grouped by role. One line per package, synopsis from source.

### Entrypoints (`cmd/`)
| Package | Role |
|---|---|
| `cmd/nilcore` | The CLI: argv dispatch + every subcommand's wiring (chat/do/tui/serve/build/swarm/decompose/flows/browse/desktop/report/trace(·why)/trust/experience/capability/lessons/flywheel/objective/auto-approvals/selfacc/inspect/watch/schedule/mcp-call/propose-edit/registry/init/doctor/config/secret/version; single-task run is the flag form `nilcore -goal …`) |
| `cmd/tools/nilcore-browser` | In-sandbox pure-Go headless-browser driver (CDP + interactive flow mode), baked into the image |
| `cmd/tools/nilcore-desktop` | In-sandbox virtual-desktop driver (Xvfb + scrot/xdotool/AT-SPI, runs the Set-of-Marks ladder) |
| `cmd/tools/nilcore-desktop-darwin` | Native-macOS host desktop driver (shells `screencapture` + `cliclick`; the CU-MAC MVP) |

### Core spine
| Package | Role |
|---|---|
| `internal/model` | Vendor-neutral message/tool client + typed `APIError` + the `BuiltinTool` seam |
| `internal/provider` | Vendor adapters: anthropic (Messages), openai/openrouter (Chat Completions), openai-compatible |
| `internal/backend` | The frozen `CodingBackend` contract + native / codex / claude-code |
| `internal/agent` (+`bus`) | Orchestrator: task → fresh worktree → backend → verifier, every step logged; `bus` = supervisor↔subagent transport |
| `internal/verify` | The source of truth for "done" (+ auto-detect, composite, browser, `Pass`) |
| `internal/sandbox` | Isolated execution boundary — container + namespace/Landlock/seccomp (I4) |
| `internal/eventlog` | Append-only hash-chained, secret-redacted audit trail (I5) |
| `internal/policy` | Reversibility classifier + human gate + SSRF-safe egress proxy + command denylist |
| `internal/worktree` | Disposable git worktree + branch per task |
| `internal/worktreefs` | The single audited worktree-confinement primitive (SafeJoin + `O_NOFOLLOW` + atomic write) |
| `internal/guard` | The untrusted-input fence (I7) |

### Conversational front door
| Package | Role |
|---|---|
| `internal/session` | Persistent conversational state container — modes, the router, persistence, drivers, control parsing |
| `internal/inbox` | User→agent mid-work message seam (queue / steer) |
| `internal/ask` | The attended-only `ask_user` outbound box (1–5 clarifying questions; nil ⇒ never advertised, fail-closed) |
| `internal/emit` | Live reasoning/intent sink |
| `internal/termui` | Terminal renderer — the live line + spinner + context gauge, TTY/plain degradation |
| `internal/verb` | "Thinking" microcopy engine (spinner frame + cycling present-participle verb) |
| `internal/steering` | Loads `NILCORE.md`/`AGENTS.md` as authoritative project instructions |
| `internal/summarize` | Compact `ContextSummary` for handover and context-bounding |
| `internal/loopctl` | Cancel-cause discriminator (shutdown vs steer vs fault) |
| `internal/server` | Long-running `serve` mode — channel threads get the same Session |
| `internal/onboard` | The `nilcore init` flow + the config schema |

### Orchestration & multi-agent
| Package | Role |
|---|---|
| `internal/super` | The agentic supervisor (plan / spawn / message / integrate / code / finish) |
| `internal/project` | The outer project loop (plan→slice→integrate→verify→reflect→re-plan) |
| `internal/agenticflows` | Declarative agentic-flows DAG (`nilcore flows`), routed through the DAG-honoring swarm code preset |
| `internal/planner` | Goal → inspectable, contract-first task tree |
| `internal/spawn` | Subtasks as scoped subworkers in parallel worktrees + the DAG scheduler |
| `internal/roster` | The role system (research / understand / plan / implement / review) |
| `internal/integrate` | Merge parallel subagent branches into one verifier-green tree |
| `internal/scheduler` | Fixed-cap FIFO concurrent task pool with backpressure |
| `internal/route` | Adaptive routing — race best-of-N judged by the verifier + cross-model review |
| `internal/advisor` | The strong-model advisor tier (`ask_advisor`) |
| `internal/strongcap` | Process-global, ctx-honoring limiter on the worker `ask_advisor` path |
| `internal/pool` | Tiered swarm provider pool (strong planner/verifier + cheap workers + fallback + caps) |

### Swarm & verifier-backed artifacts
| Package | Role |
|---|---|
| `internal/swarm` (+`board`,+`preset`) | Verified-swarm data root; `board` = live scoreboard; `preset` = the 5 bundles |
| `internal/artifact` | The typed artifact data contract (Claim / Evidence / Status / `Green`) |
| `internal/artifact/schema` | Structural check that runs before any per-claim network verification |
| `internal/artifact/evverify` | Binds claims to runnable verifier checks — the "green because checked" seam |
| `internal/artifact/packs` (+`web`,`software`,`finance`,`ui`,`audit`,`benchmark`,`code`) | Domain verifier packs |
| `internal/requeue` | Granular requeue — re-run exactly the failed claims |
| `internal/report` (+`render`) | Read-only verification-report projection + pure text/HTML/Markdown/matrix renderers |

### Safety, secrets & connectors
| Package | Role |
|---|---|
| `internal/secrets` | The `SecretStore` — keychain / encrypted-file vault / env / external hook (I3) |
| `internal/capguard` | The Rule-of-Two gate (untrusted ∧ private ∧ open-egress) |
| `internal/egressprofile` | Named, opt-in research egress presets |
| `internal/mcp` | MCP servers as on-disk typed wrappers ("code execution with MCP") |
| `internal/skills` | Agent Skills (`SKILL.md`) + native tool plugins, via the one tool registry |
| `internal/registry` | Versioned, manifest-driven local install for skills |
| `internal/channel` (+`slack`,+`telegram`) | The chat-transport seam + Slack (Socket Mode) and Telegram bots |
| `internal/scmhook` | Inbound signed SCM/CI webhook → `trigger.Signal` |
| `internal/forge` | Gated draft-PR creation on GitHub from a verified branch |

### Autonomy & operations
| Package | Role |
|---|---|
| `internal/trigger` | Self-start reversible work; route irreversible to the gate |
| `internal/cron` | Time-driven trigger source (`@hourly`/`@daily`/`HH:MM` or a fixed interval) |
| `internal/wake` | The durable self-scheduled timer behind serve's `sleep` tool |
| `internal/maint` | Housekeeping — stale worktree GC, dead delegate containers, log rotation |
| `internal/selfimprove` | The gated self-edit flow (scope allow/deny; verified + human-gated; a real `Flow.Merge` lands the verified branch) |

### Closed-loop autonomy (Phase 16)
| Package | Role |
|---|---|
| `internal/kernel` | The unified orchestration kernel — one recursive `Run` over Node/Envelope; run/build/swarm/decompose are presets (pure leaf; machines inject as RunFunc/Plan/Integrate) |
| `internal/router` | The preset router — `Classify(goal)` → run \| build \| swarm \| decompose (+ an Oracle seam); backs `nilcore do` (only orders the machine choice, never overrides a verdict/gate) |
| `internal/experience` | One derived, rebuildable projection over the log (Reader · OverLog · OverStore · Projector; rotation-aware) |
| `internal/capability` | One pure `For(Request)→Descriptor` — the legible "what may this drive do" surface |
| `internal/graapprove` | Graduated auto-approval (Pillar 5) — `GradedApprover` wraps the human gate; earned trust (per scope-family) + operator envelope; the second human-gate relaxation |
| `internal/blastbudget` | The hard runtime fence (hosts · irreversible · sandbox wall · per-day auto-approval $) the auto-approval envelope reads |
| `internal/flywheel` (+`distiller`,`loop`,`measure`,`selfeval`) | The verified, human-gated self-improvement flywheel — never edits the verifier of record |
| `internal/autosrc` | The autonomy daemon's bounded source queue |
| `internal/objective` | The operator-only standing-objectives backlog (idle self-service) |

### Observability, store & cost
| Package | Role |
|---|---|
| `internal/store` (+`db`) | SQLite backbone — events, cross-project memory, tasks |
| `internal/trace` | Causal "why did the agent do that" view from the log |
| `internal/trust` | The Trust Ledger — per-backend strength routing earned from verifier outcomes |
| `internal/inspect` | Read-only event-log rollup + a 0/1 health probe |
| `internal/meter` | Token→dollar pricing table |
| `internal/budget` | Concurrent-safe cost metering with ceiling enforcement |

### Code intelligence & memory
| Package | Role |
|---|---|
| `internal/codeintel/ast` | Symbol + call extraction (19 parser backends / 34 extensions, pure-Go) |
| `internal/codeintel/graph` | SQLite call graph with recursive-CTE reachability |
| `internal/codeintel/repomap` | PageRank repo-map for orientation |
| `internal/codeintel/semantic` | Pure-Go HNSW semantic index (degrades to lexical without an embedder) |
| `internal/codeintel/lsp` | Minimal LSP client (compiler-grade cross-language defs/refs) |
| `internal/codeintel/impact` | Impact set + affected tests (blast radius / which tests to run) |
| `internal/codeintel/retrieve` | Fusion of the lenses → a budgeted, provenance-tagged Context Bundle |
| `internal/codeintel/live` | Incremental worktree-aware re-index + memory fusion (the `live` tool) |
| `internal/embed` | Provider-backed text embedder satisfying `semantic.Embedder` |
| `internal/memory` | Cross-project conventions / decisions / learned facts in SQLite |

### Computer use
| Package | Role |
|---|---|
| `internal/browseragent` (+`plan`) | The stateful `browse` tool + the trusted plan-then-verify prompt |
| `internal/browsersession` | Host-side handle to a persistent in-sandbox browser session |
| `internal/browserwire` | The browser shell-escape + JSON wire primitives |
| `internal/cdp` | Pure-Go Chrome DevTools Protocol client + accessibility set-of-marks snapshot |
| `internal/desktopagent` | The stateful `computer` tool (generic Path B + Anthropic-native Path A) |
| `internal/desktopsession` | Host-side persistent desktop session (+ host transport for `--mac-host`) |
| `internal/desktopwire` | The desktop wire contract |
| `internal/desktop` | Pure-Go CV box detection + the per-step rung-ladder decision |
| `internal/som` | The stdlib-only Set-of-Marks overlay (embedded 5×7 digit bitmap) |

### Platform
| Package | Role |
|---|---|
| `internal/paths` | Per-OS config / data / cache directory resolution |
| `eval`, `eval/browse`, `eval/desktop`, `eval/self` | The measure-first evaluation harness (scores configs; pass@1/pass^k) — not linked into the binary |

---

## 4. The chat front door (primary user experience)

Bare **`nilcore`** = `nilcore chat`. One terminal, one conversation (`session.Session`, principal `local`); you talk and the harness picks the machine. Requires a native model provider. Design: [`docs/CONVERSATIONAL.md`](CONVERSATIONAL.md).

### Driving it
- **Plain line** → instruction. While the agent works, a plain line **QUEUES** (folded at the next loop boundary).
- **`!…` / `/steer …`** → **STEER**: cancels the in-flight *model call only* (`ErrSteer`), takes feedback, resumes/changes course — never tears an in-flight tool write.
- **`/cancel` / `/stop`** → abort the running drive, stay in the conversation (throwaway worktree discarded).
- **Ctrl-C / SIGTERM** → cancel the whole conversation and exit (shutdown beats a racing steer). **Ctrl-D / `/quit`** → leave cleanly.

### Slash commands (complete, verified set)
`session.ParseControl` handles these on **principal top-level input only** (REPL, TUI, and serve):

| Command | Effect |
|---|---|
| `/discuss`, `/ask` | Pin **discuss** (read-only). `/ask` is an alias of `/discuss`. |
| `/plan` | Pin **plan** (read-only) |
| `/execute` | Pin **execute** (full capability) |
| `/auto` | Pin **auto** (router decides) — the default |
| `/mode` | Show the active mode |
| `/add <path\|url>` | Attach a read-only context root, or fetch a URL via sandboxed `web_fetch` |
| `/save <file.md>` | Write the agent's last answer/plan to a `.md`/`.markdown`/`.txt` file — relative-only, symlink-confined, **no overwrite**. Principal-initiated (not a model write tool). Acted on by the local terminal/TUI only; serve **refuses** it. |
| `/diff` | Preview the verified work kept from the last `execute` run — a bounded, read-only diffstat + diff head of the kept branch. No kept branch ⇒ "nothing to preview". |
| `/apply` | Merge that kept verified branch into your branch — an **irreversible** action routed through the structured promote-to-base gate (asks for approval; the graduated-auto-approval envelope may auto-admit). |
| `/questions <less\|more\|off\|normal>` | Dial how often the agent may ask clarifying questions; `/ask-less` and `/ask-more` are one-notch sugar. Bare `/questions` shows the current level. |
| `/context` | Window usage; warns it will auto-compact at ≥80% |
| `/clear` | Reset history (keeps mode + roots); refused mid-drive |
| `/status` | Phase, mode, attached-root count, gauge |
| `/cancel`, `/stop` | Abort the run, stay in the conversation |

Terminal/TUI-local (`parseChatLine`): `/quit`, `/exit`, `/help`, `/?`. `/steer` and a bare `!` are **steer messages**, not controls. An unknown `/foo` warns rather than sending the typo to the model. Control verbs are wired identically across all three front doors (REPL, TUI, serve) through the one parser.

### Modes — clarify-vs-act as a structural posture
`Mode.String()` ∈ {`auto`, `discuss`, `plan`, `execute`}; **auto is the default** (zero value). Read-only modes are enforced by **wiring, not prompt** (write-free registry + `DisableShell` + read-only command policy + `verify.Pass`).

| Mode | Capability | Glyph |
|---|---|---|
| `auto` | Router sizes each message (full capability) | `◇` |
| `discuss` / `ask` | Read-only — converse / research, ships nothing | `◆` |
| `plan` | Read-only — returns a plan via `finish` | `▣` |
| `execute` | Full capability, sized native-vs-supervise | `▶` |

A mode sticks until changed and survives restart. `/plan <text>` pins **and** submits. A mid-drive switch applies to the next turn.

### The auto-router (machine selection)
In `auto`, a cheap metered ~256-token JSON classifier (with a no-model fallback and a "continues the active goal?" local rule) sizes each idle message into the cheapest honest machine:

| Machine | When | What runs |
|---|---|---|
| **chat reply** | A meta-question about the work | One metered `Complete`, no loop/worktree. Prompt: *"You are the conversational front door… NOT taking any action or writing any code… If the user is asking you to do coding work… restate it as an instruction."* |
| **native** | One coding task | A single native loop, fresh worktree (chat rail: max-iter **8**) |
| **supervise** | Multi-step feature | Bounded fan-out (chat rails: fanout **4**, agents **16**, depth **1**) |
| **project** | A whole service | The project loop |

### Live surface, budget, persistence
- **Streaming UI** (`termui`): a bottom live line — your prompt when idle; a braille spinner + cycling verb + elapsed + token estimate + "`!` to steer" when working; finalized glyph lines (`·` intent, `▸` tool, `✓`/`✗` verify, `⤺` steer-ack) scroll above. Off a TTY (SSH/pipe/CI/`TERM=dumb`/`NO_COLOR`) it degrades to clean plain lines (I6).
- **Context gauge:** ring runes `○`(<25) `◔`(≥25) `◑`(≥50) `◕`(≥75) `●`(≥100); colour green <60 / amber 60–85 / red >85. The red `/clear` nudge fires above **85%** (at exactly 85% the ring is still amber, so no nudge yet); auto-compaction near **80%** (summarizes prior turns, keeps the latest verbatim).
- **Budget wall:** one `budget.Ledger` keyed by the conversation id — default **$10** (`-budget`). Every drive, the classifier, chat replies, and the summarize fold-back charge the *same* ceiling; a breach (`ErrCeiling`) aborts.
- **Resume:** a SQLite checkpointer persists *bounded* WorkState (summary + active route + branch + last outcome + pinned mode), never transcripts. Restart prints `↻ resumed the previous conversation`.
- **`/add` roots** mount read-only into each drive's read/search tools (never writable).
- **Web** is off by default; `-allow-egress` / `-egress-profile` enables `web_fetch`/`web_search` in the sandbox.

### The TUI track
**`nilcore tui`** is an opt-in full-screen Bubble Tea interface over the *same* Session, with the same verbs (`/save` fully supported — it is local) and gates as a centered y/n modal. It links Charm only under `//go:build tui`; the default binary links zero Charm.

---

## 5. Personality and how it is enforced

The *voice* is the model's; each *behaviour* is enforced at a code site, not a prompt. Spec: [`docs/PERSONA.md`](PERSONA.md).

| Trait | Means | Enforced at |
|---|---|---|
| **Terse senior engineer** | Lead with the answer; no preamble/emoji; push back | Model voice; terse operational prompts; quality via verifier + cross-model review |
| **Clarify vs act** | Default to acting on stated assumptions; when a human is synchronously reachable the loop may put **1–5 sharp `ask_user` questions** (attended-only — a headless front door leaves the tool unadvertised) | `native.go` "Default to acting: proceed on reasonable assumptions and state them…" + the advertised `ask_user`/`set_ask_level` tools; router routes meta-questions to a reply; workers ask the *supervisor* |
| **Adaptive planning** | Cheap → interleave; complex → plan | The router's one cheap classifier; the advisor doubles as planner |
| **Advisor escalation** | Consult a strong advisor when stuck — knowing *when* is the character | `advisor`: `ask_advisor` + auto-escalate after K verify failures; advice only, seeded with a summary |
| **Proactive-act** | Self-start *reversible* work; irreversible hits the gate; announce it | `trigger`: auto-start reversible, gate irreversible; default-off; audited |
| **Bounded self-improvement** | Edit only own prompts/skills/tools | `selfimprove`: allow/deny scope (deny wins), real worktree task, human gate |
| **Low-noise notifications** | Ping only on gates, completion, hard failures | `session.Notify` fires once per *work* drive; chat replies + self-suspends fire nothing |
| **Failure ladder** | retry → race a second backend → cross-model review → surface what it tried | `route` + orchestrator; hard ceilings; never loop past budget |

**Operator steering** (`NILCORE.md`/`AGENTS.md`) loads as trusted, authoritative project instructions (capped 32KB) — shapes *how* the agent works but can never widen capability or bypass the gate/verifier. The one hard-coded brand surface is the spinner microcopy (`verb`).

---

## 6. Tool access — what the model can call

Two confinement tiers (the split **is** I4). Every tool result is `guard.Wrap`-fenced as untrusted data (I7); trusted exceptions (your steering file, your steer turns) are folded un-fenced.

**Host-side structured — worktree-confined, never arbitrary execution:** `read`, `search` (symlink-safe + `O_NOFOLLOW`, search capped 500); `write`, `edit` (atomic, single writable root); `git` (fixed hardened subcommand set); `codeintel`/`live` (read-only).

**Sandboxed execution:** `run` (arbitrary shell, command-guarded, in the sandbox under default-deny egress, never on the host; off in read-only modes); `web_fetch`, `web_search`, `browser_view` (only when egress is enabled on a container box).

**Control/meta:** `finish` (triggers the verifier — the self-claim never decides shipping), `ask_advisor`, `sleep` (durable self-timer), `bus` tools (multi-agent).

**`web_search` has two mutually-exclusive paths:** provider-native server-side (opt-in `NILCORE_WEB_SEARCH_NATIVE` + `NILCORE_WEB_SEARCH_MAX_USES`; reaches the web *outside* the local allowlist — a deliberate trust choice, gated per vendor: Anthropic/OpenRouter broadly, OpenAI only on a search-capable model, generic openai-compatible never) **or** the sandboxed, egress-confined fallback. Exactly one is exposed (a second would orphan a `tool_use`).

---

## 7. Connectors & extensibility

| Seam | Wire it with | Effect |
|---|---|---|
| **MCP** (`mcp`) | `{name, command}` (stdio) or `{name, url, headers}` (HTTP/SSE) in `mcp.json` (`NILCORE_MCP_CONFIG` else `<workdir>/mcp.json`) | On-disk JSON wrappers under `mcp/servers/…`, discovered on demand, invoked via the host-dispatched `mcp` tool (works on every sandbox tier incl. macOS container) or the `nilcore mcp-call` CLI; unused tools cost ~zero context. Resources + prompts opt-in (`NILCORE_MCP_RESOURCES=1`) |
| **Skills** (`skills`) | A `SKILL.md` in `$NILCORE_SKILLS_DIR` (else `<config>/nilcore/skills`) | A `skill_<name>` tool returning its instructions; auto-loaded into run/chat/serve/build |
| **Registry** (`registry`) | `nilcore registry install <manifest.json>` (local sources) | Versioned skill install; `registry list` shows installed |
| **Channels** (`channel`) | `serve -channel telegram\|slack` + tokens | Drive NilCore from a phone; progress streams; gates become inline / Block Kit Yes-No buttons |
| **Webhooks** (`scmhook`) | `serve --webhook ADDR` + `NILCORE_WEBHOOK_SECRET` | An HMAC-verified GitHub event becomes a self-started verified task (headless ⇒ irreversible deny-defaults) |
| **Forge** (`forge`) | `--open-pr` + `NILCORE_FORGE_TOKEN` | A **gated draft** GitHub PR from a verified branch; the agent never merges |

**Deny-all authorization:** `serve` refuses to start until the principal allowlist — the **union** of config `channel.allow` and `NILCORE_ALLOWLIST` — is non-empty. Unauthorized senders and gate-clickers are logged and ignored. Tokens come from the SecretStore as env (`TELEGRAM_BOT_TOKEN`; `SLACK_APP_TOKEN` + `SLACK_BOT_TOKEN`).

---

## 8. The engine — backends, providers, models, routing

**Backends** (`-backend native|codex|claude-code|auto`, default `native`): **native** (own model→sandbox→verify loop); **codex** (`codex exec --json --full-auto`, in the sandbox); **claude-code** (`claude -p … --output-format stream-json --permission-mode acceptEdits`, in the sandbox). Delegated backends report `SelfClaimed`; the orchestrator **always re-verifies** (I2). `-backend auto` picks the best *available* (seeded by `-prefer-backend`, learned by the Trust Ledger); `-backends a,b,c` enables multi-backend strength-routing.

**Providers** (`provider`): **anthropic** (Messages API), **openai** + **openrouter** (shared Chat Completions adapter), **openai-compatible/compat** (`NILCORE_COMPAT_BASE_URL`/`_AUTH_SCHEME`/`_KEY_ENV`; rejects a real first-party key by name to block exfiltration). Selection is `provider:model`; bare id ⇒ anthropic.

**Models:** default executor **`claude-sonnet-4-6`**; default GUI/computer-use **`claude-opus-4-8`**; bare `openrouter` ⇒ **`openrouter/fusion`**. Precedence: `NILCORE_MODEL` > config `Executor` > default; advisor `NILCORE_ADVISOR` > config `Advisor` > none.

**The robustness ladder** (innermost → outermost):
1. **Per-call resilience** (`model.Resilient`) — retry + backoff (base 200ms / max 5s) + per-provider breaker (cooldown 30s) + failover + call timeout. *Values are per call-site* — the native path sets retry **2**, breaker threshold **4**; an unset field disables that rung. Terminal 4xx fail fast.
2. **Adaptive race** (`route.Race`, only after a cheap attempt fails) — best-of-N (`-race-n`) or a cross-backend race of distinct `-backends`, **judged by the verifier**.
3. **Cross-model review** (`route.Review`) — a reviewer model gates a change before an irreversible promote; denies on unparseable output.
4. **Budget ceiling** (`budget.Ledger` via the `meter` decorator, priced by `meter.Table`; `CtxWindow` feeds the chat gauge) — a hard dollar wall that aborts before a breaching charge.

The **Trust Ledger** (`trust`) folds verifier-judged `race_outcome` events into Laplace-smoothed per-backend pass rates that **order** candidates — the verifier still decides "done."

---

## 9. Computer use — browser & desktop

Two sibling subcommands reuse the same native backend, egress proxy, sandbox, capguard, and verifier; each runs a bounded observe→plan→act→verify loop over a thin tool driving a fat in-image driver via a file-queue. Both set `DisableShell`; secrets stay host-side via `{{secret:NAME}}`; every step is logged metadata-only.

| | `nilcore browse` | `nilcore desktop` |
|---|---|---|
| Availability | Always | **Gated** — inert unless `NILCORE_COMPUTER_USE` is set |
| Target | Persistent in-sandbox headless Chromium (pure-Go CDP) | Contained virtual X11 desktop (or native macOS host) |
| `-max-steps` / `-deadline` | 40 / 15m | 50 / 20m |
| `-egress-profile` default | `browse` | `""` (deny-all) |
| Interaction | Accessibility set-of-marks snapshot | **3-rung SOM ladder**: AT-SPI refs → CV-marked screenshot `[N]` → raw coordinate (macOS MVP = rungs 2/3) |
| Notable flags | `-url`, `-read`, `-extract <id>`, `-model` | `-read`, `-native` (`NILCORE_COMPUTER_NATIVE`), `-mac-host`, `-mac-probe`, `-model` |
| Default sandbox | `container` | `container` |

The **Rule-of-Two capguard** gates untrusted ∧ private (`-read`) ∧ open-egress and fails closed headless.

**Host-control tier — `nilcore desktop --mac-host`** drives the operator's *real* Mac, the one place I4 is **explicitly relaxed** (driver runs as a host subprocess, no sandbox). It requires **both** `NILCORE_COMPUTER_USE` **and** `NILCORE_DESKTOP_HOST` set to exactly `1`, forces an **unconditional** human approval, and is bounded at runtime by:
- **Permission probe** (`--mac-probe`) — a live TCC behaviour check (real `screencapture`; `cliclick` presence), exit 0/1.
- **Kill-switch** — while a sentinel file exists (`NILCORE_DESKTOP_STOP`, else `~/.nilcore/desktop/STOP`, else `$TMPDIR/nilcore-desktop-STOP`), every mutating act is refused.
- **Per-app allowlist** — `NILCORE_DESKTOP_ALLOW_APPS="App1,App2"` pins acting; an unidentified frontmost app fails closed. Both evaluated before every mutating act.

---

## 10. Orchestration & autonomy

Three nested machines over one contract, one rule (the verifier decides): the **native loop** (bounded by `-max-steps`); the **supervisor** (`nilcore build` / chat `/execute`) that plans, spawns role-workers over a typed bus, integrates their parallel worktrees into one green tree, re-plans to convergence (greenfield `-new` or `-dir`); and the **project loop** (plan→slice→integrate→verify→reflect→re-plan, provably terminating).

**Multi-agent concurrency** (`-concurrency`): siblings run through a DAG scheduler over a race-tested pool; the supervisor reasons serially between waves, the integrator stays serial, so the tip is always green. `-concurrency 1` is byte-identical to serial. Subworkers get fresh context (a `ContextSummary` + facts from the shared store/event log), never the parent's transcript. ([`CONCURRENCY.md`](CONCURRENCY.md), [`MULTI-AGENT.md`](MULTI-AGENT.md))

**Verified swarm** (`nilcore swarm`) — fan N units into a bounded in-process pool on one host (`-agents`, `-concurrency`); each produces a **typed artifact judged by a verify-pack**; only verifier-green shards ship, failed shards **requeue until clean** (or a budget/pass limit). Five presets (research/code/audit/benchmark/ui), a tiered provider pool, a live scoreboard. ([`SWARM.md`](SWARM.md))

**Unattended & reactive:** `nilcore watch` (self-start from signal files, reversible auto / irreversible gated, via `trigger`); `nilcore schedule` (a `cron.Scheduler` whose Fire is `trigger.Handle`; specs `@hourly|@daily|HH:MM` or `-every`, not full crontab); both support `--open-pr`. `nilcore propose-edit` (gated self-improvement). `serve` ops: `-max-concurrent` (default 4 — registered as 0, resolved to 4), `-max-lifetime` (checkpoint-and-exit for restart), a maintenance ticker (worktree GC), and durable resume of in-flight tasks. **Sleep/wake**: the `sleep` tool arms a durable `wake.Registry` so an agent self-suspends and is re-fired after a restart. The `requeue` ledger (`NILCORE_REQUEUE*`) is the bounded retry layer behind swarm and auto-supervise.

---

## 11. Verification, sandbox & security core

- **Verifier** (`verify`) — sole authority on done (I2). Runs one project check in the sandbox; exit 0 = done. Auto-detects the command for an unfamiliar repo (Makefile `verify:` → go.mod → package.json → Cargo.toml → pyproject, else a safe no-op). A composite verifier folds an opt-in headless-**browser behavioral check** into the verdict (`NILCORE_BROWSER_VERIFY`). Read-only drives get `verify.Pass`.
- **Sandbox** (`sandbox`) — one interface, two auto-selected backends, both default-deny network: a **rootless container** (`--cap-drop=ALL`, no-new-privileges, read-only rootfs, worktree the only writable mount, `--network none`) and a daemonless **Linux namespace + Landlock + seccomp** backend (no runtime/image/root). Neither forwards `os.Environ()` (I3).
- **Gate** (`policy`) — a reversibility classifier auto-runs reversible actions and pauses for approval on irreversible ones (merge/push/deploy/pay), plus a type-based gate for the integration boundary. An **SSRF-safe egress allowlist proxy** is the only way out (refuses non-allowlisted hosts; blocks loopback/link-local/metadata/private IPs; pins the dial vs DNS rebinding).
- **Egress profiles** (`egressprofile`) — opt-in presets that *widen* deny-all to a sanctioned host set: **`finance`**, **`docs`**, **`web-research`**, **`browse`** (hosts only; widen-only; per-role egress still intersects).
- **Capguard** (`capguard`) — Rule-of-Two: untrusted ∧ private ∧ open-egress never run unattended (≤2 = allow; 3 + gate = require approval; 3 headless = refuse).
- **SecretStore** (`secrets`) — four auto-detected backends: OS keychain, AES-256-GCM encrypted-file vault, read-only env, external command hook. Values are injected per-run or set as the host's own request header — **never on disk plaintext, in logs, in prompts, or given to the model**. `nilcore init -vault key-file|passphrase` picks the headless master-key strategy (`NILCORE_VAULT_PASSPHRASE`); `nilcore secret set <name>` rotates one. ([`SECRETS.md`](SECRETS.md))
- **Audit log** (`eventlog`) — append-only JSONL, monotonic Seq, SHA-256/HMAC chain (`NILCORE_LOG_HMAC_KEY`), secrets redacted pre-write. **Every reader fails closed on a broken chain**: a tampered log can never render green, earn a rank, pass a health probe, or produce a clean trace.

---

## 12. Memory, code intelligence, evidence & observability

- **Cross-project memory** (`memory`) — durable scope-keyed records in SQLite, fused into context (the `live` tool returns worktree edits fused with project memory; the orchestrator writes durable conventions back). Distinct from the per-conversation checkpoint.
- **Code intelligence** (`codeintel`) — **19 parser backends / 34 extensions**, pure-Go (no tree-sitter/CGO; JavaScript and TypeScript share one backend). Pipeline: AST → SQLite call graph (recursive-CTE reachability) → PageRank repo-map → semantic HNSW (opt-in `NILCORE_EMBED_KEY`/`NILCORE_EMBED_MODEL`, degrades to lexical) → LSP (`NILCORE_LSP_COMMAND`) → impact set + affected tests → live worktree-aware index (`NILCORE_LIVE_INDEX`) → a budgeted, provenance-tagged retrieval bundle. ([`CODE-INTELLIGENCE.md`](CODE-INTELLIGENCE.md))
- **Verifier-backed artifacts** (`artifact`) — code is one artifact type among reports/matrices/specs/benchmarks/dossiers. Every `Claim` carries `Evidence{value, source_url, verifier, status}`; green only if a sandboxed check *affirmatively passed* — the worker's self-claim is **overwritten**; an unregistered verifier ⇒ `unverifiable`. Domain **verify-packs** supply the checks. ([`ROADMAP-EVIDENCE-ARTIFACTS.md`](ROADMAP-EVIDENCE-ARTIFACTS.md))
- **Observability** (read-only over the log): `report` (text/md/html/json/matrix; refuses green over a broken chain), `trace`/`why` (causal tree), `trust` (backend strength scoreboard), `inspect [health]` (rollup + 0/1 probe). Durable backbone: SQLite `store`.

---

## 13. Complete CLI surface

Dispatch (`cmd/nilcore/main.go`): bare `nilcore` → chat; a `-`-prefixed argv (`nilcore -goal …`) → the `run` default; otherwise the subcommand.

| Command | What it does |
|---|---|
| `nilcore` / `chat` | Interactive conversational front door |
| `tui` | Full-screen Charm TUI variant (needs `tui` build tag) |
| `do -goal "…"` | Route a goal to the fitting preset (run \| build \| swarm \| decompose) and dispatch (`-dry-run` previews, `-as` forces one) |
| `-goal "…"` *(run)* | Run one task to completion in a disposable worktree |
| `build -goal "…" [-new ./svc]` | Drive a whole project to a verifier-green tree (multi-agent) |
| `swarm -goal "…" -preset …` | Verified swarm: typed artifacts, requeue-until-clean |
| `decompose -goal "…"` | Split a goal into independent sub-goals, run each in its own worktree, merge-and-re-verify into one tip (kernel recursion) |
| `flows validate\|run <file>` | Preflight / execute a portable agentic-flows DAG (routed through the swarm code preset) |
| `serve -channel telegram` | Listen on Telegram/Slack (+ `--webhook`) |
| `watch` / `schedule` | Self-start from signals / on cron (+ `--open-pr`) |
| `browse` / `desktop` | Browser / desktop computer-use (`--mac-host` gated) |
| `propose-edit -goal … -paths …` | Gated self-edit of own prompts/skills/tools |
| `flywheel [--once]` | Self-improvement loop (eval → mine failures → propose a gated fix; human-gated, a real `Flow.Merge` lands the verified branch) |
| `mcp-call <server> <tool>` | Invoke a configured MCP tool |
| `registry list\|install` | Manage local skills |
| `report` / `trace` (`why`, `--tui`) / `trust` / `inspect [health]` | Read-only views over the audit log (`trace --tui` = interactive explorer, needs the `tui` build) |
| `experience` / `capability` | Learned-state scoreboard / a drive's exact capability descriptor |
| `lessons` | Recurring verifier-failure patterns the agent has learned from |
| `auto-approvals [-denied]` | Account of past graduated auto-approvals + the per-class undo story |
| `objective <list\|add\|disable\|enable>` | Manage the standing-objectives backlog (operator-only) |
| `selfacc <propose\|check>` | Review self-authored acceptance verifiers (operator-gated; `NILCORE_SELFACC`) |
| `init` / `doctor` / `config show` / `secret set <name>` | Onboard / readiness gate / show config / store a secret |
| `version` (`-v`) / `help` (`-h`) | Build version / usage |

### Verified defaults

| Scope | Flag | Default |
|---|---|---|
| Shared (`registerCommon`) | `-dir` / `-backend` / `-verify` | `.` / `native` / `make verify` |
| | `-runtime` / `-sandbox` | `podman` / `auto` |
| | `-max-steps` / `-advisor-max-calls` / `-escalate-after` / `-race-n` | `60` / `4` / `2` / `1` (off) |
| `chat` | `-budget` | `$10` |
| `build` | `-budget` / `-deadline` / `-max-steps` / `-max-iterations` | `$25` / `2h` / `80` / `12` |
| | `-max-fanout` / `-max-agents` / `-max-depth` / `-concurrency` | `8` / `64` / `1` / `1` |
| `swarm` | `-budget` | `$25` |
| `browse` | `-max-steps` / `-deadline` / `-egress-profile` / `-sandbox` | `40` / `15m` / `browse` / `container` |
| `desktop` | `-max-steps` / `-deadline` / `-egress-profile` / `-sandbox` | `50` / `20m` / `""` (deny-all) / `container` |
| `watch` | `-signals` / `-interval` | `./signals` / `5s` |
| `schedule` | `-every` | `1h` |
| `serve` | `-max-concurrent` | `4` (registered `0` → resolves to `4`) |
| `init` | `-vault` | `key-file` |

---

## 14. Configuration & environment

**`nilcore init`** writes a secret-free `config.json` (`onboard.Config`: `Providers[]`, `Executor`, `Advisor`, `Backend`, `PreferredBackend`, `Runtime`, `Image`, `Channel{Type,TokenRefs,Allow}`, `Web{Enabled,Allow,Search,SearchKeyRef,Profile,ProfileFile}`, `Codex`/`Claude` delegated config, pool tiers, routing). **`nilcore config show`** prints it — the de-facto config-key reference. **`nilcore doctor`** is the exit-0/1 host-readiness gate (keys resolve, runtime on PATH, sandbox probe, allowlist sane) — distinct from `inspect health` (which probes the log). Operator runbook (opt-in surfaces, web access, autonomy, registry): [`OPERATIONS.md`](OPERATIONS.md).

### Key environment variables
| Area | Variables |
|---|---|
| Model / providers | `NILCORE_MODEL`, `NILCORE_ADVISOR`, `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `OPENROUTER_API_KEY`, `NILCORE_COMPAT_BASE_URL`/`_AUTH_SCHEME`/`_KEY_ENV`, `NILCORE_OPENROUTER_PROVIDER`/`_MODELS`/`_REASONING`/`_TRANSFORMS`/`_PLUGINS`, `NILCORE_RESPONSE_FORMAT`, `NILCORE_TOOL_CHOICE` (OpenRouter/OpenAI extras; JSON where noted, ignored if malformed) |
| Delegated backends | `NILCORE_CLAUDE_MODEL`/`_EFFORT`, `NILCORE_CODEX_MODEL`/`_EFFORT`, `CODEX_API_KEY` |
| Sandbox / verify | `NILCORE_SANDBOX`, `NILCORE_RUNTIME`, `NILCORE_IMAGE`, `NILCORE_BROWSER_VERIFY`, `NILCORE_VERIFY_PACKS`, `NILCORE_EVIDENCE_VERIFY`, `NILCORE_EVIDENCE_MAX_AGE` (all four evidence toggles fold into the verify-cache key; `NILCORE_VERIFY_PACKS` + `NILCORE_EVIDENCE_MAX_AGE` are additionally validated at boot — a bad value exits 2), `NILCORE_VCACHE` / `NILCORE_FLAKEPROBE` (verify-cache + one-shot flake re-run, both default-ON), `NILCORE_TIERED_VERIFY` (scoped fast-red path, opt-in) |
| Web / egress | `NILCORE_EGRESS_PROFILE`, `BRAVE_API_KEY`, `NILCORE_WEB_SEARCH_NATIVE`, `NILCORE_WEB_SEARCH_MAX_USES` |
| Connectors | `NILCORE_ALLOWLIST`, `TELEGRAM_BOT_TOKEN`, `SLACK_APP_TOKEN`, `SLACK_BOT_TOKEN`, `NILCORE_MCP_CONFIG`, `NILCORE_MCP_RESOURCES`, `NILCORE_SKILLS_DIR`, `NILCORE_WEBHOOK_SECRET`, `NILCORE_WEBHOOK_LABEL`, `NILCORE_FORGE_TOKEN` |
| Computer use | `NILCORE_COMPUTER_USE`, `NILCORE_COMPUTER_NATIVE`, `NILCORE_COMPUTER_MODEL`, `NILCORE_BROWSE_MODEL`, `NILCORE_DESKTOP_HOST` (=`1`), `NILCORE_DESKTOP_ALLOW_APPS`, `NILCORE_DESKTOP_STOP`, `NILCORE_DESKTOP_DRIVER`, `NILCORE_BROWSER`, `NILCORE_MAC_SCALE` (Retina backing-scale override, 1–4) |
| Code intel | `NILCORE_EMBED_KEY`, `NILCORE_EMBED_MODEL`, `NILCORE_EMBED_BASE_URL` (OpenAI-compatible embeddings endpoint), `NILCORE_LSP_COMMAND`, `NILCORE_LIVE_INDEX` |
| Secrets / audit | `NILCORE_VAULT_PASSPHRASE`, `NILCORE_LOG_HMAC_KEY`, `NILCORE_SECRET_EXTERNAL_CMD` (activates the external-command SecretStore backend — the 4th I3 backend) |
| Orchestration & closed-loop | `NILCORE_KERNEL` (route through the unified kernel; default-ON, `=0` escape hatch), `NILCORE_EXPERIENCE` (derived experience projection), `NILCORE_LESSONS` (distil verifier-failure scars), `NILCORE_FLYWHEEL` / `NILCORE_AUTONOMY` (serve-only: background flywheel / autonomy daemon), `NILCORE_REQUEUE`, `NILCORE_REQUEUE_MAX_ATTEMPTS`, `NILCORE_TRUST_DEFAULT` (=`1`; cost-aware trust oracle for single-backend runs) |
| Graduated auto-approval | `NILCORE_AUTOAPPROVE_PRESET` (`conservative\|standard\|trusted` — seeds the envelope), `NILCORE_AUTOAPPROVE_OFF` (=`1`; global kill-switch, also `.nilcore/AUTOAPPROVE_OFF`), `NILCORE_SELFIMPROVE_AUTOAPPROVE` (=`1`; separate double-opt-in for auto-merging self-improve edits — **see the behaviour-change note below**), `NILCORE_SELFACC` / `_MAX` / `_FILE` (closed-loop self-acceptance checks) |
| Non-interactive init (`onboard.FromEnv`) | `NILCORE_BACKEND`, `NILCORE_EXECUTOR`, `NILCORE_WEB_SEARCH`, `NILCORE_WEB_ALLOW` (scripted, prompt-free `nilcore init` inputs) |

**Names written in suffix form above, spelled out** (so they are findable by their exact name):
`NILCORE_COMPAT_BASE_URL`, `NILCORE_COMPAT_AUTH_SCHEME`, `NILCORE_COMPAT_KEY_ENV`;
`NILCORE_OPENROUTER_PROVIDER`, `NILCORE_OPENROUTER_MODELS`, `NILCORE_OPENROUTER_REASONING`, `NILCORE_OPENROUTER_TRANSFORMS`, `NILCORE_OPENROUTER_PLUGINS`;
`NILCORE_CLAUDE_MODEL`, `NILCORE_CLAUDE_EFFORT`, `NILCORE_CODEX_MODEL`, `NILCORE_CODEX_EFFORT` (these four are read by prefix construction in `resolveDelegated`, `cmd/nilcore/main.go:2323`);
`NILCORE_SELFACC`, `NILCORE_SELFACC_MAX`, `NILCORE_SELFACC_FILE`.

**Value semantics (a footgun worth stating):** most feature flags gate on *presence* — any non-empty value, **including `=0`**, enables them. The exceptions are the default-ON verify/kernel flags (`NILCORE_VCACHE`, `NILCORE_FLAKEPROBE`, `NILCORE_KERNEL`), which honour `0`/`off`/`false`/`no` to turn OFF, and `NILCORE_EXPERIENCE` (a default-OFF opt-in that likewise honours those negatives). `NILCORE_TIERED_VERIFY` is a default-OFF opt-in that needs `1`/`on`/`true`/`yes`. The `=1`-exactly gates are `NILCORE_DESKTOP_HOST`, `NILCORE_AUTOAPPROVE_OFF`, `NILCORE_SELFIMPROVE_AUTOAPPROVE`, and `NILCORE_TRUST_DEFAULT`. The grouped table above is the fullest env reference in the docs; [`OPERATIONS.md`](OPERATIONS.md) adds the operator runbook for the opt-in surfaces.

> **Behaviour changes at `573a4df` that alter what an existing setting DOES.** Two settings that previously did nothing now take real effect — check yours before upgrading:
> - **`NILCORE_SELFIMPROVE_AUTOAPPROVE=1` now performs a real merge.** Before, `selfimprove.Flow.Propose` logged `self_edit_merged` and reported success while **nothing was merged**; the verified branch was only preserved. It now lands the edit. The guards are unchanged — the edit must be verifier-green, `selfimprove.DefaultScope` still forbids `internal/verify/`, the core loop and every contract file, and the execution-time changed-paths screen still fails closed — but if you had this set, it was a no-op and now it is not.
> - **`nilcore swarm` shards now reach their preset's declared hosts.** The per-shard egress allowlist was computed and then dropped, so every shard ran `--network none` (which is why the `research` preset could never verify green and `--egress-allow` was inert). The role-intersected allowlist is now enforced by a proxy for each shard box. Deny-all presets (`audit`, `ui`) stay `--network none`; a proxy that cannot bind fails closed. The active allowlist is printed at start and recorded as a `swarm_egress` event.

---

### Maintenance contract

This file duplicates facts owned by the spoke docs and the code, so it drifts unless tended. When you change a default, flag, command, env var, package, phase status, or user-facing behaviour, update this file in the **same** change (it is not a contract file, so it never blocks a parallel task). On a release, refresh §1 (releases, counts) — regenerate counts with `go list ./... | wc -l` and the package table from `go list -f '{{.ImportPath}}|{{.Doc}}' ./...`. For exhaustive rationale, follow the spoke pointer; the spoke and the code win on any conflict.
