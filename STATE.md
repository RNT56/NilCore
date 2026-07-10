# STATE.md — NilCore state snapshot

> **Point-in-time snapshot, 2026-07-10** (HEAD `573a4df`, on branch
> `claude/nilcore-features-review-931610`; **unmerged** — the features-completeness
> review + remediation pass). This is a derived report, not a contract. The sources of
> truth remain `CLAUDE.md` (constitution), `docs/ARCHITECTURE.md` (technical law +
> invariants), and `CHANGELOG.md` (the ledger). Where this file and those disagree,
> **they win** — regenerate this one.

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
| Non-test Go | **~89.8K LOC** across **375 files** |
| Test Go | **~87.7K LOC** across **406 files** (≈0.98 test:code ratio) |
| Go packages | **120** total (`go list ./...`); **111** under `internal/` |
| Direct module deps | **3 sanctioned families** (5 direct requires) — `modernc.org/sqlite`, `golang.org/x/sys`, Charm TUI (`bubbletea`/`bubbles`/`lipgloss`, behind `//go:build tui`) |
| `CGO_ENABLED` | **0** (pure-Go; cross-compiles cleanly) |
| Gate | `make verify` (build + vet + lint + test) — green. (`-race` + the `tui` build are separate lanes, not folded in.) |

---

## 2. The seven invariants (and how they're enforced)

These are the constitution (CLAUDE.md §2). Breaking one rejects the PR regardless of merit.

| # | Invariant | Enforcement |
|---|---|---|
| **I1** | Backend contract frozen: `backend.CodingBackend.Run(ctx, Task) (Result, error)` | native loop, Codex, Claude-Code all satisfy it; contract change is a dedicated serialized task |
| **I2** | The **verifier** is the sole authority on "done" — no backend self-report ships work | after any backend runs, the project's checks re-run and that verdict governs (`internal/verify`) |
| **I3** | No ambient authority — secrets via `SecretStore`, never on disk/log/prompt/model | `internal/secrets` (`SecretStore` at `secrets.go:19`); HTTP MCP auth headers resolved host-side, never to the model |
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
  `Run` over a Node/Envelope; `run` / `build` / `swarm` / `decompose` are **presets**, and the router
  classifies a goal → which preset, backing `nilcore do`. Default-on via `NILCORE_KERNEL` (escape
  hatch `=0`), equivalence-proven against the legacy machines. Pure leaf — machines inject as
  `RunFunc`/`Plan`/`Integrate`. `decompose` is the kernel's first recursion consumer (`kernel.Recursive`).
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

## 5. Features & surface (30 subcommands)

`chat` · `do` · `tui` · `serve` · `build` · `swarm` · `decompose` · `flows` · `init` · `doctor` ·
`config` · `secret` · `inspect` · `report` · `trust` · `selfacc` · `experience` · `lessons` ·
`flywheel` · `objective` · `auto-approvals` · `capability` · `trace` (`why` alias) · `mcp-call` ·
`propose-edit` · `watch` · `schedule` · `registry` · `browse` · `desktop`.

- **Bare `nilcore`** opens the interactive `chat` REPL. A single task is the **flag form**
  `nilcore -goal "…"` (there is no `run` subcommand; `run` is a kernel *preset*).
- **Front doors**: `chat` (stdlib streaming REPL), `tui` (Charm, `//go:build tui` — a stub that
  exits 2 in the default binary), `serve` (channels: Telegram/Slack), `do` (router picks the preset).
- **Backends** (I1) are `-backend` *values*, not verbs: `native` · `codex` · `claude-code` · `auto`.
  **Providers** (Phase 15, `internal/provider`) are model-vendor *adapters*: `anthropic` · `openai` ·
  `openrouter` · `openai-compatible`, with web search. **Channels** are `-channel` values: `telegram` · `slack`.
- **Agency**: `browse` (Phase 14 CDP set-of-marks), `desktop` (Phase CU computer use; `--mac-host` tier,
  gated by `NILCORE_COMPUTER_USE`).

---

## 6. MCP capability (SHIPPED)

NilCore connects MCP servers as **typed code APIs** (Anthropic's "code execution with MCP"):
descriptors are generated under a per-repo cache dir (`<cache-dir>/mcp/servers/<server>/<tool>.json`,
override `$NILCORE_MCP_DESC_DIR`) — **out of the operator's checkout** — and discovered on demand
(read/search), so unused tools cost ≈0 tokens. Servers are **operator-configured** (`mcp.json` /
`$NILCORE_MCP_CONFIG`), **never model-emitted**. The client is a clean stdlib JSON-RPC 2.0 speaker
(not a module — I6). What ships today:

1. **Host-dispatched native `mcp` tool** — registered in `loopTools()` (the execute registry) **only
   when `mcpMgr != nil`** (operator-configured servers present). It calls servers **host-side** —
   exactly like the structured read/write/git tools — so the call never needs `nilcore`/a runtime
   *inside* the sandbox; **MCP works on every sandbox tier, including the macOS container default.**
   The model discovers tools via the descriptors and invokes `{"server","tool","args"}` (server +
   tool + JSON args are data, I7-fenced; audited).
2. **Transport-abstracted** (`internal/mcp/transport.go`): **stdio** (local subprocess) **or
   Streamable HTTP** (remote `url` in `mcp.json`, JSON *or* SSE reply, `Mcp-Session-Id` echoed,
   `Accept: application/json, text/event-stream`, static auth `headers` resolved host-side; MCP
   output is treated as UNTRUSTED and size-capped). Stdlib `net/http` only.
3. **Persistent `Manager`** (`internal/mcp/manager.go`): one live, initialized connection per server,
   **reused** across calls (stdio spawned once, HTTP session kept); concurrency-safe (connect ctx is
   detached so one caller's cancel can't poison a peer); recovers a dropped connection (evict +
   reconnect once); never re-runs a tool-level failure (`ErrToolFailed` sentinel).
4. **Resources + prompts**, **opt-in** via `NILCORE_MCP_RESOURCES` (presence-gated — any non-empty
   value enables). When enabled, descriptors are also generated for resources/prompts and the `mcp`
   tool honors `{"server","resource"}` / `{"server","prompt","args"}`. Off by default ⇒ tools-only,
   byte-identical. (Binary/blob resource contents are intentionally omitted — text-only, I7.)

`nilcore mcp-call` is the host-side CLI bridge (operator use + the namespace-sandbox shell path) and
speaks both stdio and HTTP.

### mcp.json shape
```json
{ "servers": [
    { "name": "docs",   "command": ["npx","-y","@modelcontextprotocol/server-filesystem","/data"] },
    { "name": "remote", "url": "https://mcp.example.com/v1", "headers": {"Authorization": "Bearer {{secret:MCP_TOKEN}}"} }
] }
```

`headers` values may carry `{{secret:NAME}}` / `{{env:NAME}}` placeholders resolved host-side via the
`SecretStore` (unresolved ⇒ hard error); the secret never reaches the model.

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

- **EXT-01..08** (`docs/EXT-EXECUTION-PLANS.md`) — gated external-infra blueprints (~100 tasks,
  §0-gated). Genuinely NOT BUILT: no fleet/control-plane, web hosting, LSP server surface, remote
  vector index, SSO/SCIM/RBAC, cross-fleet secret distribution, remote registry, or Firecracker tier.
- **HORIZON** (`docs/HORIZON.md`) — several early ideas have since shipped (A1 Trust Ledger →
  Phase 13, A8 lessons → Phase-16 LRN, A9 verify-cache → `NILCORE_VCACHE`, B5 desktop CU → Phase CU,
  C6 self-eval flywheel, C7 blast-radius budget). Still unbuilt: the verify-pack research tier —
  A2 cross-model adversarial pack, A3 mutation/property/fuzz packs, A4 differential pack, A5
  impact-ordered fast-path verifier — plus B1–B4, C1–C5.
- **CI-only lanes** (sandbox-linux, namespace sandbox) can't run on a macOS host — they run in CI.
- MCP: `Deploy` is a **defined** gate-action class (`policy.Deploy`, classified irreversible) but
  **no production code emits a `Deploy` gate today** — there is no `internal/deploy`; NilCore cannot
  deploy (the graduated-approval `Deploy` branch is dormant). Resource/prompt **binary** (blob)
  contents are intentionally omitted (text-only, I7).

---

*Regenerate this snapshot when the architecture moves. It is a map, not the territory.*
