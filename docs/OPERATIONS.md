# Runtime resilience & operations

The capability arc (understand → plan → code → verify → delegate → remember → improve) makes NilCore **able**. This layer makes it **survive running unattended for months on a VPS** — the unglamorous seams that separate a capable demo from a system you can trust to run while you sleep. Governed by `docs/PRINCIPLES.md` (#1 the feedback loop, #8 recover don't thrash, #10 safety enables autonomy).

This is the cluster the end-to-end audit surfaced: each concern below was genuinely absent or thin in the capability spec.

## Principles (subsystem)

1. **Fail safe, resume clean** — a crash or transient error must never lose work or corrupt state.
2. **Make the ceiling real** — a budget that isn't metered isn't a budget.
3. **Least privilege at the perimeter** — only authorized principals may command the agent.
4. **Bound everything** — disk, concurrency, spend, and retries all have hard caps.
5. **Observable and replayable** — every run can be inspected and replayed from the log.

## The concerns

### 1. Authorized control — *security* (P2-T07)
An autonomous agent with shell + repo-write reachable over a chat bot must not take orders from whoever finds it. An **allowlist of principals** (per-channel user/workspace IDs) guards every inbound command; unauthorized senders are rejected and logged; **gate approvals** are accepted only from authorized principals. The SecretStore holds the bot *token* (how NilCore authenticates **to** Telegram/Slack); this is the missing layer that decides **who** may drive it. Pulled into Phase 2 because it is a security boundary.

### 2. Provider resilience (P6-T01)
429s, timeouts, and 5xx are constant in production. A resilience wrapper below the loop does **retry with exponential backoff + jitter**, per-call **timeouts**, **failover** across configured providers, and a simple **circuit-breaker** so a degraded provider is skipped. The loop sees a clean call; transient faults never surface as task failures.

### 3. Cost metering & enforcement (P6-T02)
Budgets exist as caps throughout the spec but nothing meters real spend. A **budget ledger** meters tokens and dollars **per task and globally**, persists to the store, enforces the ceiling (a task that would exceed it stops and surfaces), and exposes live spend to the router (routing evidence) and the operator. This is what makes "generous budget, optimize for finishing" safe.

### 4. Durability & resumption (P6-T03)
If the process dies mid-task — reboot, OOM, crash — an unattended agent must resume, not silently drop the work. Orchestrator **task state is persisted** (the append-only event log + a task-state record); on restart, in-flight tasks **resume from their last checkpoint or fail cleanly** with a surfaced reason. **Graceful shutdown** on SIGTERM checkpoints first.

### 5. Concurrency & scheduling (P6-T04)
`serve` mode takes channel → orchestrator, and subworkers parallelize *within* a task — but multiple top-level requests have no home. A **queue + scheduler** runs concurrent tasks under **resource arbitration**: max concurrent sandboxes, a global rate/spend budget, and fair ordering. Backpressure when limits are hit, not a thundering herd.

### 6. Verification auto-detection (P6-T05)
The verifier is the source of truth, but a *general* agent meets repos it has never seen. Auto-detection inspects a repo (languages via the AST layer, build files, test config) and produces a **verify plan** (build / test / lint commands), with a safe fallback and a way to pin per-project overrides. Without this, "verify" only works on repos you pre-configure.

### 7. Resource cleanup / GC (P6-T06)
Long-running unattended operation accumulates state. A maintenance pass **garbage-collects** merged/stale worktrees and dead containers, **rotates** logs, and keeps **disk bounded** — so the agent doesn't fill the VPS over a month of work.

### 8. Operator observability & health (P6-T07)
Beyond the audit trail, the operator needs to **see and debug**. `nilcore` subcommands **inspect and replay** the event log, show **task status** and **spend**, and `serve` exposes a **health/readiness** check for the supervisor. Built on the hash-chained log (P2-T06).

### 9. Config integrity (P6-T08)
`nilcore init` writes config; something must load it safely every run. A **versioned schema** with **validation** (clear errors, sane defaults) and **migration** across versions turns a malformed config into a precise message instead of a confusing runtime failure. This now lives in `internal/onboard` — the live `Config`'s `Load` decodes strictly (unknown fields rejected), migrates by `version`, and `Validate`s — so there is one config schema, not two. (The standalone `internal/config` package was retired: it was never wired into boot and its schema had diverged from the live one.)

## Web access — enable it once in `nilcore init`

Web access is **off by default** (default-deny). Turn it on the easy way during `nilcore init` → the **Web access** step: opt in, list any hosts the agent may reach, and pick a search backend. It is persisted to `config.json` (`web.{enabled,allow,search,search_key_ref}`) and `nilcore chat`/`serve` then enable it automatically — no flag to remember. A one-off `nilcore chat -allow-egress host,host` still works and **adds** to the configured hosts.

**Search backends** (the search host is auto-added to the allowlist, so search works the moment web is on):
- **`ddg`** (default, **keyless**) — DuckDuckGo Lite. No signup; best-effort HTML results (the agent extracts links). Fragile to layout changes and rate-limited — the no-friction path, not the high-quality one.
- **`brave`** (keyed upgrade) — the Brave Search API (clean JSON). Needs a free key (`BRAVE_API_KEY`, or captured into the SecretStore by `init`). Selected automatically when a key is present.
- **`off`** — `web_fetch` only (read a known URL), no search.

The agent auto-falls back to `ddg` if `brave` is chosen but no key resolves, so search never silently dies. `nilcore doctor` reports whether web is on, the backend, and whether the Brave key resolves.

**Web access requires the container backend.** The egress allowlist proxy (`AllowEgressVia`) is wired for the **container** sandbox. The **namespace** backend is born in a fresh, empty network namespace (`CLONE_NEWNET`, no interface) — the cleanest possible default-deny — so it has **no egress path at all**, and the web tools are simply not advertised there (fail-closed). This is deliberate: bolting userspace networking (slirp4netns/pasta/veth) onto the namespace backend would add an external dependency and a second egress path competing with the proxy as the single enforcement point (against I4/I6). So run on the container backend for web (`-sandbox container`, or any host without Landlock/userns where `auto` already picks it). Either way, fetched/searched bodies are `guard.Wrap`'d as untrusted data (I7) and the Brave key reaches only the in-box request via per-run env, never a prompt or the log (I3).

`/add <path>` context roots are honored host-side by the read/search tools on every backend; the container backend additionally bind-mounts them read-only so the execute-mode shell can read them too (the namespace backend's shell cannot — it sees only the worktree).

## Event-driven & scheduled autonomy — running without a keyboard

The unattended posture (Phases 8–10) adds two ways for the agent to *start itself*, both routed through the **existing** reversible-auto-start / human-gate machinery — they introduce no new authority. When there is no human at the console (headless), **irreversible work deny-defaults and does not start** — by design.

### `nilcore serve --webhook <addr>`
Stands up an SCM/CI webhook intake alongside the `serve` channel loop. A signed forge webhook (GitHub-style HMAC-SHA256 over the raw body) becomes a `trigger.Signal` and routes through the same reversible-auto-start gate as a chat command.

- Needs **`NILCORE_WEBHOOK_SECRET`** — the HMAC secret shared with the forge's webhook config. If it is empty the intake is **disabled (fail-closed)**: `--webhook` was set but no secret resolved, so nothing listens. The signature check is constant-time; an empty secret or a missing/garbled header is rejected.
- **`NILCORE_WEBHOOK_LABEL`** (optional) narrows which events act — only matching labels become signals.
- Listens at `http://<addr>/webhook` (e.g. `127.0.0.1:8099`, behind a TLS-terminating reverse proxy). Headless ⇒ the gate denies irreversible work; self-started tasks otherwise run as normal verified tasks.

### `nilcore schedule`
The time-driven counterpart to `nilcore watch` — on a fixed cadence it emits a goal as a self-started run.

- `-goal` (required) is the work to attempt; `-name` tags the run in the audit log.
- `-every <duration>` (default `1h`) sets an interval; `-at <spec>` sets a wall-clock schedule (local time): `@hourly`, `@daily`, or `HH:MM` (24h). `-at` overrides `-every`; an invalid `-at` is rejected up front.
- Same headless posture: reversible scheduled work runs as a normal verified task; irreversible work deny-defaults and does not start.

### `--open-pr` (gated draft PR — D4)
`nilcore watch --open-pr` and `nilcore schedule --open-pr` offer a **draft** PR after a verified self-start, opened via `internal/forge` **only after the human gate clears** — the push runs inside the approved prepare step. Needs **`NILCORE_FORGE_TOKEN`** (from the SecretStore) and an `origin` remote. The agent **never merges**; forge credentials are scrubbed from logs. When enabled, the orchestrator's nil-gated `KeepBranch` preserves the verified branch instead of the default disposable cleanup — off ⇒ cleanup is byte-identical.

## Versioned skills/MCP registry — `nilcore registry`

A versioned, manifest-driven install layer over local skills (P10-T06), turning a hand-placed capability into a tracked, updatable artifact.

- `nilcore registry list` — list installed skills and their versions.
- `nilcore registry install <manifest.json>` — install the manifest's skill entries from **local** sources. Skill entries carry version metadata; MCP server specs carry version metadata too, but only `KindSkill` installs today. **Remote fetch stays GATED as EXT-07** (see `docs/ROADMAP-EXTERNAL-INFRA.md`) — the registry resolves local manifests only.

## Operator env vars for the opt-in surfaces

These newer surfaces are **all additive and opt-in** — when their env vars are unset the default binary behaves exactly as before (byte-identical). Every secret-bearing var resolves via the environment / SecretStore, is **never logged**, and is **never placed in a prompt or given to the model** (I3).

| Env var | Enables | Notes |
|---|---|---|
| `NILCORE_WEBHOOK_SECRET` | `serve --webhook` HMAC verification | empty ⇒ webhook intake fail-closed (disabled) |
| `NILCORE_WEBHOOK_LABEL` | label filter for webhook events | optional; only matching events become signals |
| `NILCORE_FORGE_TOKEN` | `--open-pr` draft-PR push | from SecretStore; scrubbed from logs; agent never merges |
| `NILCORE_EMBED_KEY` | semantic code search (D2) | OpenAI-compatible embedder (`internal/embed`); off ⇒ lexical fallback, byte-identical |
| `NILCORE_EMBED_MODEL` | embedder model id | optional override for the embedder |
| `NILCORE_BROWSER` | the `browser_view` tool / behavioral navigation (D1) | the `nilcore-browser` driver baked into the sandbox image |
| `NILCORE_BROWSER_VERIFY` | composite browser verifier | folds a browser behavioral check INTO the verdict (I2 holds) |
| `NILCORE_CHROMIUM` | Chromium-binary override for the driver | defaults to `chromium` on `$PATH`; missing browser ⇒ fail-closed |
| `NILCORE_CODEX_MODEL` | override the Codex delegated-CLI model (R1) | beats `config.json` `codex.model`; unset ⇒ config/default |
| `NILCORE_CODEX_EFFORT` | override the Codex reasoning effort (R1) | passed as `-c model_reasoning_effort=<e>` |
| `NILCORE_CLAUDE_MODEL` | override the Claude Code delegated-CLI model (R1) | beats `config.json` `claude.model`; unset ⇒ config/default |
| `NILCORE_CLAUDE_EFFORT` | override the Claude Code reasoning effort (R1) | injected as `CLAUDE_CODE_EFFORT_LEVEL` (env, not a flag) |

### Delegated coding CLIs — configuring Codex / Claude Code (R1)
When NilCore delegates a task to **Codex** or **Claude Code** (`-backend codex` / `-backend claude-code`) instead of the native loop, both CLIs are **configurable**, not hardcoded key-only. Every knob is optional and **zero knobs ⇒ byte-identical** to the bare command:

- **Model** → Codex `--model`, Claude `--model`.
- **Effort** → Codex `-c model_reasoning_effort=<e>`; Claude `CLAUDE_CODE_EFFORT_LEVEL=<e>` (the env, since the flag name drifts across CLI versions).
- **Extra args** → raw extra CLI tokens appended before the goal (e.g. `-c key=value`).
- **Env** → extra per-run environment merged with the API key — notably `CODEX_HOME` / `CLAUDE_CONFIG_DIR` to surface a host config dir despite the sandbox's `HOME=/tmp`.

Set them in `config.json` under `codex` / `claude` (written by `nilcore init`), e.g. `"codex": {"model": "o3", "effort": "high", "extra_args": ["-c", "foo=bar"], "env": {"CODEX_HOME": "/work/.codex"}}`. The env overrides above win over the config file at runtime. The API key is still injected **per run** and merged **last** (an operator `env` can't shadow it); it is never logged — the event log records only `{cli, exit}` (I3).

### Behavioral verification — operational requirements (D1, flow-driving R3)
The `browser_view` tool drives a running app and hands the model a **screenshot** as a multimodal image block; given an optional **`actions`** script it first **drives a flow** (navigate/click/type/wait — e.g. log in, submit a form — over a pure-Go CDP client, R3) and then observes, returning the same `{title, text, console, screenshot_b64}`. Model-supplied selectors/text/URLs are **data** replayed as CDP params, never shell or code (I7). With `NILCORE_BROWSER_VERIFY` set, a composite verifier folds a browser behavioral check **into** the verdict, so the verifier stays the **sole authority** on "done" (I2). Operationally this needs the **container backend plus a Chromium-bearing sandbox image** — the driver runs in the same sandbox box as the build, inheriting I4 confinement. The driver **fails closed without a browser** (a misconfigured verifier is red, never a false green). The **live browser run (batch and the `--actions` flow) is CI-only** — hermetic unit tests carry no Chromium — so this path is exercised in CI, not in the local fast suite.

## A note on context assembly (deliberately *not* a task)

Window construction (system prompt + persona + repo-map + Context Bundle + memory + history, within budget) is **intentionally distributed** — each source contributes through its own seam (`ContextSummary` P3-T06, Context Bundle P3-T14, memory retrieval P4-T04). This is a design choice, not a gap; consolidating it behind a single assembler is a future refactor if the seams prove hard to reason about, not a missing capability.

## Tech

All store-backed state (budget ledger, task state, spend history) lives in the **Phase-4 SQLite store** — no new datastore. Supervision via **systemd** (restart, SIGTERM); resilience, scheduling, GC, and config are plain stdlib Go. Nothing here adds a runtime dependency.

The later opt-in surfaces hold the line too: webhook intake (`internal/scmhook`), the cron scheduler (`internal/cron`), the draft-PR client (`internal/forge`), the skills registry (`internal/registry`), trusted steering (`internal/steering`), the OpenAI-compatible embedder (`internal/embed`), and the `nilcore-browser` driver (`cmd/tools/nilcore-browser`) are **all pure stdlib** — no module was added. The default binary still links exactly **two** core dependencies (pure-Go SQLite `modernc.org/sqlite` + `golang.org/x/sys`); the Charm TUI stack links only under `make tui`. I6 holds, and `CGO_ENABLED=0` across the release matrix.

## Task cluster

| Task | Owns | What |
|---|---|---|
| P2-T07 | `internal/channel/` | authorized-control allowlist + gate-approval auth (*security*) |
| P6-T01 | `internal/model/` | provider resilience: retry/backoff, timeout, failover, breaker |
| P6-T02 | `internal/budget/` | cost metering + ceiling enforcement (store-backed) |
| P6-T03 | `internal/agent/` | task-state persistence, resume on restart, graceful shutdown |
| P6-T04 | `internal/scheduler/` | concurrent-task queue + resource arbitration |
| P6-T05 | `internal/verify/` | verification auto-detection (build/test/lint plan) |
| P6-T06 | `internal/maint/` | GC of worktrees/containers, log rotation, bounded disk |
| P6-T07 | `internal/inspect/` | inspect/replay log, status, spend, health endpoint |
| P6-T08 | `internal/onboard/` | versioned config schema, validation, migration (retired the standalone `internal/config`) |
