# STATE.md — NilCore state snapshot

> **Point-in-time snapshot, 2026-06-30** (base `main` @ `f479ea9`, on branch `feat/mcp-upgrade`).
> This is a derived report, not a contract. The sources of truth remain `CLAUDE.md`
> (constitution), `docs/ARCHITECTURE.md` (technical law + invariants), and `CHANGELOG.md`
> (the ledger). Where this file and those disagree, **they win** — regenerate this one.

---

## 1. What NilCore is

A tiny, robust **autonomous coding agent** in Go. The thesis (CLAUDE.md §1): *the harness is
small; the model is the engine.* Coding fluency lives in the model, so the harness stays small
on purpose. Robustness comes from exactly three disciplines:

1. the agent **verifies** its own work — the project's own checks are the only authority on "done";
2. everything a model can use to **execute arbitrary code is sandboxed**;
3. the loop is **bounded and fully logged** (append-only, replayable).

Not "flawless" — **robust-via-verification**. Rigor is aimed at the verifier, the sandbox, and
the audit trail.

### Scale (this snapshot)

| Metric | Value |
|---|---|
| Non-test Go | **~77.4K LOC** across **363 files** |
| Test Go | **~68.5K LOC** across **353 files** (≈0.88 test:code ratio) |
| Internal packages | **~111** |
| Direct module deps | **3 sanctioned families** — `modernc.org/sqlite`, `golang.org/x/sys`, Charm TUI (`bubbletea`/`lipgloss`/`bubbles`, behind `//go:build tui`) |
| `CGO_ENABLED` | **0** (pure-Go; cross-compiles cleanly) |
| Gate | `make verify` (build + vet + test) — green |

---

## 2. The seven invariants (and how they're enforced)

These are the constitution (CLAUDE.md §2). Breaking one rejects the PR regardless of merit.

| # | Invariant | Enforcement |
|---|---|---|
| **I1** | Backend contract frozen: `backend.CodingBackend.Run(ctx, Task) (Result, error)` | native loop, Codex, Claude-Code all satisfy it; contract change is a dedicated serialized task |
| **I2** | The **verifier** is the sole authority on "done" — no backend self-report ships work | after any backend runs, the project's checks re-run and that verdict governs (`internal/verify`) |
| **I3** | No ambient authority — secrets via `SecretStore`, never on disk/log/prompt/model | `internal/secret`; HTTP MCP auth headers resolved host-side, never to the model |
| **I4** | Model-emitted execution is **sandboxed**; host-side structured tools are the bounded exception | `internal/sandbox` (container + namespace); structured file/git tools worktree-confined |
| **I5** | Event log is **append-only**, hash-chained, replayable | `internal/eventlog` (torn-line tolerant reader) |
| **I6** | Core has **zero external deps** beyond the 3 sanctioned families | per-package `deps_test.go` guards; `go.mod` diff is a PR red flag |
| **I7** | Untrusted input is **data, never instructions** | tool output / file contents / fetched web / **MCP output** fenced by the injection guard |

Recorded **relaxations** of I4 (each behind a separate opt-in, documented in ARCHITECTURE §0/§Execution model):
- the host-side structured **file/git tools** (scoped worktree I/O, never arbitrary execution);
- the **`--mac-host` desktop** tier (drives the real desktop; `NILCORE_DESKTOP_HOST=1` + forced approval + kill-switch + per-app allowlist);
- **operator-configured MCP** servers, invoked host-side via the `mcp` tool (see §6) — bounded: operator's sanctioned server set, declared surface only, I7-fenced, audited.

---

## 3. The execution spine

```
goal ─► router/kernel ─► backend (native loop | codex | claude-code) ─► sandbox
                              │                                              │
                              ▼                                              ▼
                          tool calls ──► host-side structured tools     model-emitted shell
                              │             (read/write/edit/search/        (run inside the box)
                              │              git/codeintel/mcp/…)
                              ▼
                          VERIFIER  ◄── the only authority on "done"
                              │
                              ▼
                          eventlog (append-only) ─► policy gate ─► merge/irreversible action
```

- **Unified kernel** (`internal/kernel` + `internal/router`, Phase 16/Pillar 8): one recursive
  `Run` over a Node/Envelope; `run` / `build` / `swarm` are **presets**, and the router classifies
  a goal → which preset, backing `nilcore do`. Default-on via `NILCORE_KERNEL` (escape hatch `=0`),
  equivalence-proven against the legacy machines. Pure leaf — machines inject as `RunFunc`/`Plan`/`Integrate`.
- **Native loop** (`internal/backend/native.go`): the stdlib model-call loop. Always-on `run`
  shell tool (sandboxed) unless `DisableShell` (read-only Discuss/Plan drives). Structured tools
  load from a host-dispatched `tools.Registry`; the `mcp` tool now rides that registry (§6).
- **Two sandbox tiers** (`internal/sandbox`): **Container** (podman/docker, default on macOS) and
  **Namespace** (Linux user/mount namespaces + Landlock + seccomp via `golang.org/x/sys`).

---

## 4. Loops & agent systems (how they interact)

- **run** — single-goal native loop over a worktree, verify-gated.
- **build** (`cmd/nilcore/build.go`) — the project loop: derives an advisor-proposed,
  sandbox-vetted, ADD-ONLY acceptance bar (`project.DeriveAcceptance` + `SeedCriteria`), then
  converges to green; `PromotionPermitted` asserted on greenfield-bootstrapped repos.
- **swarm** (`internal/swarm`, Phase 12, `docs/SWARM.md`) — in-process bounded fan-out over the
  Phase-11 verified-artifact spine; verified, human-gated.
- **decompose** (Kernel V2) — recursive decompose preset; the kernel's first recursion consumer.
- **autonomy daemon** (`internal/autosrc` + `internal/objective`) — bounded source queue +
  operator-only standing-objectives backlog; wired, wake-feeder fixed (deliver-then-disarm).
- **flywheel** (`internal/flywheel`) — self-improvement loop (selfeval · distiller · measure ·
  loop); verified, human-gated, **never edits the verifier of record**.
- **selfacc** (`internal/verify/selfacc`) — runtime closed loop over the agent's own evidence.
- **closed-loop spine** (`internal/experience`, Phase 16) — one derived, rebuildable projection
  over the log (Reader · OverLog · OverStore · Projector).

### Safety / trust subsystems
- **policy** (`internal/policy`) — reversibility classifier + human gate; irreversible actions
  (merge/push/deploy/prod/payments) require the gate.
- **graapprove** (`internal/graapprove`) — graduated auto-approval (Pillar 5): earned trust +
  operator envelope; a **second** human-gate relaxation, opt-in via `NILCORE_AUTOAPPROVE_PRESET`.
- **blastbudget** (`internal/blastbudget`) — hard runtime fence (hosts · irreversible · sandbox
  wall · per-day auto-approval $) the envelope reads.
- **capguard** (`internal/capguard`) — Rule-of-Two gate (untrusted ∧ private ∧ open-egress).
- **capability** (`internal/capability`) — one pure `For(Request)→Descriptor`; the legible
  "what may this drive do" surface (single source of truth for read-only/shell/command-policy).

---

## 5. Features & surface (~38 CLI verbs)

`chat` · `do` · `tui` · `serve` · `build` · `swarm` · `decompose` · `flows` · `init` · `doctor` ·
`config` · `secret` · `inspect` · `report` · `trust` · `selfacc` · `experience` · `lessons` ·
`flywheel` · `objective` · `auto-approvals` · `capability` · `trace` · `mcp-call` · `propose-edit` ·
`watch` · `schedule` · `registry` · `browse` · `desktop` · `codex` · `claude-code` · `native` ·
`telegram` · `slack` · `anthropic` · `openai` · `openrouter`.

- **Front doors**: `chat` (stdlib streaming REPL), `tui` (Charm, `//go:build tui`), `serve`
  (channels: Telegram/Slack), `do` (router picks the preset).
- **Providers** (Phase 15, `internal/provider`): Anthropic · OpenAI · OpenRouter · openai-compatible,
  with web search.
- **Agency**: `browse` (Phase 14 CDP set-of-marks), `desktop` (Phase CU computer use; `--mac-host` tier).

---

## 6. MCP capability — **upgraded in this branch**

NilCore connects MCP servers as **typed code APIs** (Anthropic's "code execution with MCP"):
descriptors are generated under `./mcp/servers/<server>/` and discovered on demand (read/search),
so unused tools cost ≈0 tokens. Servers are **operator-configured** (`mcp.json` /
`$NILCORE_MCP_CONFIG`), **never model-emitted**.

### Before this branch
- ✅ Clean stdlib JSON-RPC 2.0 client; secure (operator-configured, I7-fenced); descriptor codegen.
- ⚠️ **tools-only**, **stdio-only**, **one-shot per call**.
- 🔴 **The container gap**: the model invoked MCP by running `nilcore mcp-call` via the *sandboxed*
  `run` tool. That works in the **Linux namespace** sandbox (host `nilcore` + runtime reachable) but
  **fails in the default container** (debian-slim has no `nilcore`, no node/python) — i.e. **MCP did
  not work on macOS default**.

### After this branch (the four upgrades)
1. **Container gap closed.** A new **host-dispatched native `mcp` tool** (registered in
   `loopTools()`, the execute registry) calls servers **host-side** — exactly like the structured
   read/write/git tools — so the call never needs `nilcore`/a runtime *inside* the box. **MCP now
   works on every sandbox tier, including the macOS container default.** The model discovers tools
   via the descriptors and invokes `{"server","tool","args"}`. Trust boundary: operator-configured
   servers only; the model picks server + tool + JSON args (data, I7); audited; this is the same
   place `setupMCP` already spawned servers for discovery.
2. **HTTP/SSE transport** (`internal/mcp/transport.go`). The client is now transport-abstracted:
   **stdio** (local subprocess) **or Streamable HTTP** (remote `url` in `mcp.json`, JSON *or* SSE
   reply, `Mcp-Session-Id` echoed, static auth `headers` resolved host-side). Stdlib `net/http` only.
3. **Persistent `Manager`** (`internal/mcp/manager.go`). One live, initialized connection per
   server, **reused** across calls (stdio spawned once, HTTP session kept) — fixes one-shot cost;
   concurrency-safe; recovers a dropped connection (evict + reconnect once); never re-runs a
   tool-level failure (`ErrToolFailed` sentinel).
4. **Resources + prompts**, **opt-in** via `NILCORE_MCP_RESOURCES=1`. When enabled, descriptors are
   also generated for resources/prompts and the `mcp` tool honors `{"server","resource"}` /
   `{"server","prompt","args"}`. Off by default ⇒ tools-only, byte-identical.

`nilcore mcp-call` is retained as the host-side CLI bridge (operator use + the namespace-sandbox
shell path) and now also speaks HTTP.

### mcp.json shape
```json
{ "servers": [
    { "name": "docs",   "command": ["npx","-y","@modelcontextprotocol/server-filesystem","/data"] },
    { "name": "remote", "url": "https://mcp.example.com/v1", "headers": {"Authorization": "Bearer …"} }
] }
```

---

## 7. Code quality

- gofmt/goimports clean; `go vet` clean; `golangci-lint run` clean (`.golangci.yml`).
- Errors wrapped with `%w`; `ctx` first; no `panic` in library code; a non-zero sandbox exit is a
  *result*, not a Go error.
- Tests table-driven where it fits, hermetic (no network in unit tests — the MCP HTTP tests use
  `httptest`), fast. Per-package `deps_test.go` guards police I6.
- Parallel-agent protocol: one task = one branch = one PR, worktree-isolated; `make verify` is the
  Definition-of-Done gate; merge requires the human/CI gate.

---

## 8. Known gaps / roadmap pointers

- **EXT-01..08** (`docs/EXT-EXECUTION-PLANS.md`) — gated external-infra blueprints (~100 tasks, §0-gated).
- **HORIZON** (`docs/HORIZON.md`) — Phase-13 ideas (top: Trust Ledger over dormant signals).
- **CI-only lanes** (sandbox-linux, namespace sandbox) can't run on a macOS host — they run in CI.
- MCP: `Deploy` capability still **planned**; resource/prompt **binary** (blob) contents are
  intentionally omitted (text-only, I7).

---

*Regenerate this snapshot when the architecture moves. It is a map, not the territory.*
