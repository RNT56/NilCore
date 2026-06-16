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

## Web access requires the container backend

The web tools (`web_fetch`, `web_search`) reach the network only through the **egress allowlist proxy**, enabled with `nilcore chat -allow-egress host,host`. That proxy is wired for the **container** sandbox (`AllowEgressVia` routes the box through it via the runtime host alias). The **namespace** backend is born in a fresh, empty network namespace (`CLONE_NEWNET`, no interface) — the cleanest possible default-deny — so it has **no egress path at all**, and the web tools are simply not advertised there (fail-closed). This is deliberate: bolting userspace networking (slirp4netns/pasta/veth) onto the namespace backend would add an external dependency and a second egress path competing with the proxy as the single enforcement point (against I4/I6). **To use the web tools, run on the container backend** (`-sandbox container`, or any host without Landlock/userns where `auto` already picks it). `web_search` additionally needs `BRAVE_API_KEY` set and `api.search.brave.com` in the allowlist.

`/add <path>` context roots are honored host-side by the read/search tools on every backend; the container backend additionally bind-mounts them read-only so the execute-mode shell can read them too (the namespace backend's shell cannot — it sees only the worktree).

## A note on context assembly (deliberately *not* a task)

Window construction (system prompt + persona + repo-map + Context Bundle + memory + history, within budget) is **intentionally distributed** — each source contributes through its own seam (`ContextSummary` P3-T06, Context Bundle P3-T14, memory retrieval P4-T04). This is a design choice, not a gap; consolidating it behind a single assembler is a future refactor if the seams prove hard to reason about, not a missing capability.

## Tech

All store-backed state (budget ledger, task state, spend history) lives in the **Phase-4 SQLite store** — no new datastore. Supervision via **systemd** (restart, SIGTERM); resilience, scheduling, GC, and config are plain stdlib Go. Nothing here adds a runtime dependency.

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
