# Tasks — the work queue

The full build plan, decomposed into parallelizable units. Read `CLAUDE.md` §5 first — it defines how you **claim** a task (open `task/<ID>`), the **work-selection rule**, the **collision rule** (disjoint `Owns` sets), and the **Definition of Done**. This file is read-only spec; status lives in git, not in edits to this file.

## Status model

- **Todo** — no `task/<ID>` branch, no CHANGELOG entry.
- **In progress** — a `task/<ID>` branch exists (`git branch -a`).
- **Done** — merged to `main` + a CHANGELOG entry exists.

Pick the lowest-ID task whose dependencies are all **Done** and whose `Owns` set is disjoint from every in-progress task. **Treat a package directory as the unit of ownership** — two agents must not both own `internal/agent/` at once.

## Master DAG

| ID | Phase | Title | Depends on | Owns | Note |
|---|---|---|---|---|---|
| P0-T01 | 0 | CI pipeline | — | `.github/`, `.golangci.yml` | |
| P0-T02 | 0 | Compile & `make verify` green | — | (whole tree) | **BLOCKING / solo** |
| P0-T03 | 0 | Sandbox container image | P0-T02 | `images/sandbox/` | |
| P0-T04 | 0 | Test fixtures + smoke test | P0-T02, P0-T03 | `test/` | |
| P1-T01 | 1 | Worktree manager | P0-T02 | `internal/worktree/` | |
| P1-T02 | 1 | Orchestrator uses worktrees + injection seams | P1-T01 | `internal/agent/` | |
| P1-T03 | 1 | Approver + gate wiring | P1-T02 | `internal/policy/`, `internal/agent/` | |
| P1-T04 | 1 | `Channel` interface | P1-T02 | `internal/channel/channel.go` | **contract** |
| P1-T05 | 1 | Telegram channel | P1-T03, P1-T04 | `internal/channel/telegram/` | |
| P1-T06 | 1 | Slack channel | P1-T04 | `internal/channel/slack/` | ∥ P1-T05 |
| P1-T07 | 1 | `serve` mode (channel → orchestrator) | P1-T05 | `cmd/nilcore/`, `internal/server/` | |
| P1-T08 | 1 | Structured native tools | P1-T02 | `internal/tools/` | |
| P1-T09 | 1 | MCP via code execution | P1-T08, P2-T04, P2-T05 | `internal/mcp/`, `go.mod` | **contract (go.mod)** |
| P1-T10 | 1 | Providers (Anthropic/OpenAI/OpenRouter) | P0-T02 | `internal/provider/`, `internal/model/` | |
| P1-T11 | 1 | SecretStore (keychain/encrypted/env) | P0-T02 | `internal/secrets/` | |
| P1-T12 | 1 | Onboarding wizard (`nilcore init`) | P1-T10, P1-T11, P1-T04, P0-T03 | `internal/onboard/`, `cmd/nilcore/` | |
| P1-T13 | 1 | Cross-platform paths + release matrix | P0-T02 | `internal/paths/`, `.github/workflows/release.yml`, `scripts/` | |
| P2-T01 | 2 | Hardened container flags | P0-T02 | `internal/sandbox/` | |
| P2-T02 | 2 | Egress allowlist | P2-T01 | `internal/sandbox/`, `internal/policy/` | |
| P2-T03 | 2 | Per-run secret injection | P2-T02 | `internal/sandbox/`, `internal/backend/codex.go`, `internal/backend/claudecode.go` | |
| P2-T04 | 2 | Tool-call policy engine | P1-T03 | `internal/policy/`, `internal/backend/native.go` | |
| P2-T05 | 2 | Prompt-injection boundary | P2-T04 | `internal/guard/`, `internal/backend/native.go` | |
| P2-T06 | 2 | Hash-chained log + redaction | P0-T02 | `internal/eventlog/` | |
| P2-T07 | 2 | Authorized control (channel allowlist + gate auth) | P1-T04, P1-T07, P2-T04 | `internal/channel/` | |
| P3-T01 | 3 | Planner (goal → task tree) | P1-T02 | `internal/planner/` | |
| P3-T02 | 3 | Subworker spawner | P3-T01, P1-T01, P3-T06 | `internal/spawn/` | |
| P3-T03 | 3 | Blackboard | P3-T02, P4-T01 | `internal/blackboard/` | never built (see spec note) |
| P3-T04 | 3 | Routing (escalation + race + review) | P3-T02, P2-T01 | `internal/route/` | |
| P3-T05 | 3 | Wire planner/spawn/route into orchestrator | P3-T01..T04, P3-T06 | `internal/agent/` | |
| P3-T06 | 3 | Summarizer + `ContextSummary` handoff | P1-T02 | `internal/summarize/` | |
| P3-T07 | 3 | Proactive trigger (self-start reversible work) | P3-T05, P1-T03 | `internal/trigger/` | |
| P3-T08 | 3 | Advisor-Executor (two-tier model + `ask_advisor`) | P1-T08, P3-T06 | `internal/advisor/`, `internal/model/` | |
| P3-T09 | 3 | Code intel: AST + symbols (tree-sitter) | P1-T08 | `internal/codeintel/ast/` | |
| P3-T10 | 3 | Code intel: graph + SQLite + queries | P3-T09, P4-T01 | `internal/codeintel/graph/` | |
| P3-T11 | 3 | Code intel: PageRank repo-map | P3-T10 | `internal/codeintel/repomap/` | |
| P3-T12 | 3 | Code intel: LSP client (SCIP-aligned) | P3-T09 | `internal/codeintel/lsp/` | |
| P3-T13 | 3 | Code intel: semantic index (hybrid) | P3-T10, P4-T01, P1-T10 | `internal/codeintel/semantic/` | |
| P3-T14 | 3 | Code intel: retrieval + Context Bundle | P3-T10, P3-T11, P3-T12, P3-T13 | `internal/codeintel/retrieve/` | |
| P3-T15 | 3 | Code intel: Impact Set + test-impact + SBFL | P3-T10 | `internal/codeintel/impact/` | SBFL/`Localize` pruned (#98) |
| P3-T16 | 3 | Code intel: living updates + memory fusion | P3-T10, P4-T03 | `internal/codeintel/live/` | |
| P4-T01 | 4 | SQLite store (schema, migrations, queries) | P0-T02 | `internal/store/`, `db/`, `go.mod` | **contract (go.mod)** |
| P4-T02 | 4 | Event log → store backing | P4-T01, P2-T06 | `internal/eventlog/`, `internal/store/` | |
| P4-T03 | 4 | Memory model + write API | P4-T01 | `internal/memory/` | |
| P4-T04 | 4 | Retrieval into context | P4-T03, P2-T05 | `internal/memory/`, `internal/backend/native.go` | |
| P4-T05 | 4 | Memory write-back | P4-T03, P3-T05 | `internal/memory/`, `internal/agent/` | |
| P5-T01 | 5 | Skill / plugin system | P3-T05 | `internal/skills/` | |
| P5-T02 | 5 | Gated self-edit flow | P5-T01, P2-T05 | `internal/selfimprove/` | |
| P5-T03 | 5 | Eval harness | P3-T04 | `eval/` | |
| P6-T01 | 6 | Provider resilience (retry/backoff/failover/breaker) | P1-T10 | `internal/model/` | |
| P6-T02 | 6 | Cost metering + ceiling enforcement | P1-T10, P4-T01 | `internal/budget/` | |
| P6-T03 | 6 | Task durability + resume + graceful shutdown | P3-T05, P4-T02, P1-T07 | `internal/agent/` | |
| P6-T04 | 6 | Cross-task scheduler + resource arbitration | P1-T07, P3-T02, P6-T02 | `internal/scheduler/` | |
| P6-T05 | 6 | Verification auto-detection | P0-T02, P3-T09 | `internal/verify/` | |
| P6-T06 | 6 | Resource cleanup / GC (worktrees, containers, logs) | P1-T01, P0-T03 | `internal/maint/` | |
| P6-T07 | 6 | Operator observability + health | P2-T06, P6-T02, P6-T03 | `internal/inspect/` | |
| P6-T08 | 6 | Config schema + validation + migration | P1-T12 | `internal/config/` | **retired** — folded into `internal/onboard` (the live config) |
| P9-T01 | 9 | Multimodal content blocks (model + providers) | — | `internal/model/`, `internal/provider/`, `docs/ARCHITECTURE.md` | **contract · solo** |
| P9-T02 | 9 | Sandboxed headless-browser tool | P9-T01 | `internal/tools/` | |
| P9-T03 | 9 | Behavioral verifier (composite + browser check) | P9-T02 | `internal/verify/` | |
| P9-T04 | 9 | SCM/CI webhook intake → `trigger.Signal` | — | `internal/scmhook/` | ∥ P9-T05/06 |
| P9-T05 | 9 | Gated PR/push action (`GateAction` + forge) | — | `internal/policy/`, `internal/forge/` | ∥ P9-T04/06 |
| P9-T06 | 9 | Cron / scheduled trigger source | — | `internal/cron/` | ∥ P9-T04/05 |
| P9-T07 | 9 | Tier-1 CLI wiring | P9-T02, P9-T03, P9-T04, P9-T05, P9-T06 | `cmd/nilcore/` | shares `cmd/nilcore` |
| P10-T01 | 10 | Authoritative steering-file loader + trusted injection seam | — | `internal/steering/`, `internal/backend/` | ∥ P10-T03..06 |
| P10-T02 | 10 | Steering front-door plumbing (principal-only, persisted) | P10-T01 | `internal/session/` | |
| P10-T03 | 10 | Provider-backed Embedder | — | `internal/embed/` | ∥ |
| P10-T04 | 10 | Pure-Go ANN/HNSW semantic index | — | `internal/codeintel/semantic/` | go.mod if dep |
| P10-T05 | 10 | Multi-language AST + broaden live index | — | `internal/codeintel/ast/`, `internal/codeintel/live/` | go.mod if dep |
| P10-T06 | 10 | Versioned skills/MCP-server registry | — | `internal/skills/`, `internal/mcp/`, `internal/registry/` | |
| P10-T07 | 10 | Tier-2 CLI wiring | P10-T02, P10-T03, P10-T04, P10-T05, P10-T06 | `cmd/nilcore/`, `internal/tools/` | shares `cmd/nilcore`, `internal/tools` |
| P11-T00 | 11 | Extract worktree-confinement leaf | — | `internal/worktreefs` | shipped |
| P11-T01 | 11 | artifact leaf: data model, JSON, status lifecycle | P11-T00 | `internal/artifact` | shipped |
| P11-T02 | 11 | artifact worktree persistence | P11-T01 | `internal/artifact` | shipped |
| P11-T03 | 11 | evverify: Registry + CheckFunc dispatch seam | P11-T01 | `internal/artifact/evverify` | shipped |
| P11-T04 | 11 | evverify.ArtifactVerifier (I2 keystone) | P11-T02, P11-T03 | `internal/artifact/evverify` | shipped |
| P11-T05 | 11 | Wire evidence verification (`NILCORE_EVIDENCE_VERIFY`) | P11-T04 | `cmd/nilcore/verifier.go` | shipped |
| P11-T06 | 11 | Staging doc: spine | — | `docs/ROADMAP-EVIDENCE-ARTIFACTS.md` | consolidated |
| P11-T07 | 11 | web-research pack | P11-T03 | `internal/artifact/packs/web` | shipped |
| P11-T08 | 11 | software-research pack | P11-T03 | `internal/artifact/packs/software` | shipped |
| P11-T09 | 11 | finance/market pack (keyed+keyless) | P11-T03 | `internal/artifact/packs/finance` | shipped |
| P11-T10 | 11 | ui-browser pack | P11-T03, P11-T11a | `internal/artifact/packs/ui` | shipped |
| P11-T11 | 11 | pack aggregator + selector | P11-T07, P11-T08, P11-T09, P11-T10 | `internal/artifact/packs/packs.go` | shipped |
| P11-T11a | 11 | Extract browserwire leaf | — | `internal/browserwire` | shipped |
| P11-T12 | 11 | Wire pack selection (`NILCORE_VERIFY_PACKS`) | P11-T11, P11-T05 | `cmd/nilcore/verifier.go` | shipped |
| P11-T13 | 11 | Staging doc: domain verifier packs | P11-T06 | `docs/ROADMAP-EVIDENCE-ARTIFACTS.md` | consolidated |
| P11-T14 | 11 | spawn.Result typed-artifact field | — | `internal/spawn` | shipped |
| P11-T15 | 11 | Typed-research Role/Profile | — | `internal/roster` | shipped |
| P11-T16 | 11 | buildSpawnFunc reads verified artifact; compose into env.Verifier | P11-T14, P11-T15, P11-T02, P11-T04, P11-T05 | `cmd/nilcore/build.go` | shipped |
| P11-T17 | 11 | renderReport: typed claims trusted, prose fenced | P11-T14 | `internal/super` | shipped |
| P11-T17a | 11 | Enrich subagent_report Detail with continue_from/base | P11-T17 | `internal/super` | shipped |
| P11-T18 | 11 | Staging doc: typed worker results | P11-T13 | `docs/ROADMAP-EVIDENCE-ARTIFACTS.md` | consolidated |
| P11-T19 | 11 | requeue leaf: Unit, Worklist, Scan | P11-T02 | `internal/requeue` | shipped |
| P11-T20 | 11 | requeue.Ledger | P11-T19 | `internal/requeue` | shipped |
| P11-T21 | 11 | requeue.Plan + Resolve | P11-T19, P11-T20 | `internal/requeue` | shipped |
| P11-T22 | 11 | super: nil-gated RequeueHook | P11-T17a | `internal/super` | shipped |
| P11-T23 | 11 | Wire granular requeue (`NILCORE_REQUEUE`) | P11-T21, P11-T22, P11-T05, P11-T16 | `cmd/nilcore/requeue_wiring.go` | shipped |
| P11-T24 | 11 | Staging doc: granular requeue | — | `docs/ROADMAP-EVIDENCE-ARTIFACTS.md` | consolidated |
| P11-T25 | 11 | egressprofile leaf: named presets + Resolve | — | `internal/egressprofile` | shipped |
| P11-T26 | 11 | egressprofile project-local allowlist file | P11-T25 | `internal/egressprofile` | shipped |
| P11-T27 | 11 | onboard.WebConfig persistence + validation | P11-T25 | `internal/onboard` | shipped |
| P11-T28 | 11 | Wire `-egress-profile` through both front doors | P11-T26, P11-T27, P11-T05, P11-T12, P11-T16, P11-T23, P11-T33 | `cmd/nilcore` (egress wiring) | shipped |
| P11-T29 | 11 | Staging doc: research egress profiles | P11-T18 | `docs/ROADMAP-EVIDENCE-ARTIFACTS.md` | consolidated |
| P11-T30 | 11 | report leaf: ReportModel + log-replay projection | P11-T01, P11-T02 | `internal/report` | shipped |
| P11-T31 | 11 | report worktree writer | P11-T30 | `internal/report` | shipped |
| P11-T32 | 11 | report/render: text + HTML + markdown | P11-T30 | `internal/report/render` | shipped |
| P11-T33 | 11 | Wire `nilcore report` subcommand | P11-T31, P11-T32 | `cmd/nilcore/report.go`, `cmd/nilcore/main.go` (dispatch) | shipped |
| P11-T34 | 11 | Staging doc: verification report | — | `docs/ROADMAP-EVIDENCE-ARTIFACTS.md` | consolidated |
| P11-T35 | 11 | Cross-check: pack hosts ⊆ egress profiles | P11-T07, P11-T08, P11-T09, P11-T10, P11-T11, P11-T25 | `cmd/nilcore/egress_packs_test.go` | shipped |
| P11-T36 | 11 | Promotion into the canonical DAG | P11-T06, P11-T13, P11-T18, P11-T29, P11-T24, P11-T34 | `docs/TASKS.md`, `docs/ARCHITECTURE.md`, `CHANGELOG.md` | **contract** |
| SW-T01 | 12 | artifact schema/shape validation leaf + SchemaVerifier | — | `internal/artifact/schema` | shipped |
| SW-T02 | 12 | `audit` verify-pack (file:line evidence reproduces in-box) | — | `internal/artifact/packs/audit` | shipped |
| SW-T03 | 12 | `benchmark` verify-pack (verifier re-measures K in-box) | — | `internal/artifact/packs/benchmark` | shipped |
| SW-T04 | 12 | `code` verify-pack (build/test via `verify.Detect`) | — | `internal/artifact/packs/code` | shipped |
| SW-T05 | 12 | `packs.Build` assembler + `DefaultSchemas` (fail-closed) | SW-T01..T04 | `internal/artifact/packs/{packs,build}.go` | shipped |
| SW-T06 | 12 | report source/claim-trace + matrix projection (additive) | SW-T01 | `internal/report` | shipped |
| SW-T07 | 12 | provider pool (tiered/capped/failover/metered) | — | `internal/pool` | shipped |
| SW-T08 | 12 | `onboard.Config.Pool` field + Validate (config schema) | SW-T07 | `internal/onboard/onboard.go` | shipped |
| SW-T09 | 12 | swarm `Shard` + invariant guards (ShipGate/ClassifyCeiling/ProjectTrusted) | SW-T05 | `internal/swarm` | shipped |
| SW-T10 | 12 | swarm durable Queue (swarm-* status namespace, resume) | SW-T09 | `internal/swarm` | shipped |
| SW-T11 | 12 | swarm Sharder (List/Plan/Failure) | SW-T10 | `internal/swarm` | shipped |
| SW-T12 | 12 | swarm Runner (scheduler/DAG bounded pool) | SW-T11 | `internal/swarm` | shipped |
| SW-T13 | 12 | swarm multi-pass Controller (until-clean over requeue) | SW-T12 | `internal/swarm` | shipped |
| SW-T14 | 12 | swarm scoreboard board sub-leaf (+ tui) | SW-T06 | `internal/swarm/board` | shipped |
| SW-T15 | 12 | swarm presets + RoleAuditor/RoleUI | SW-T05, SW-T09, SW-T07 | `internal/swarm/preset`, `internal/roster` | shipped |
| SW-T16 | 12 | extend `nilcore report` (swarm report/matrix/json) | SW-T06 | `cmd/nilcore/report.go` | shipped |
| SW-T17 | 12 | `nilcore swarm` subcommand + `buildSwarm` | SW-T13..T16, SW-T08 | `cmd/nilcore/{swarm.go,main.go}` | shipped |
| SW-T18 | 12 | Promotion into the canonical DAG | SW-T17 | `docs/{TASKS,ARCHITECTURE}.md`, `CLAUDE.md`, `CHANGELOG.md`, `README.md` | **contract** |

> **First wave:** only `P0-T01` and `P0-T02` are eligible at the start, and `P0-T02` is solo (it may touch the whole tree to get the build green). Once `P0-T02` is Done, the tree opens up: `P0-T03`, `P1-T01`, `P2-T01`, `P2-T06`, `P4-T01` become eligible in parallel.

> **Later phases:** Phases 0–6 (56 tasks) shipped at `[0.1.0]`. **Phase 7** (portability — host-native namespace + Landlock sandbox) shipped; its specs are in the section below. **Phase 8** (full multi-agent concurrency) is **shipped** — dynamic-wave async dispatch, tracked in its own design doc (`docs/CONCURRENCY.md`). **Phase 9** (behavioral verification & event-driven autonomy, promoted from `docs/UPGRADE-PATH.md` Tier 1) is **shipped**. **Phase 10** (context depth, trusted steering & distribution, promoted from Tier 2) is **shipped**. **Phase 11** (verifier-backed artifact factory) is **shipped** (`docs/ROADMAP-EVIDENCE-ARTIFACTS.md`). **Phase 12** (verified swarm mode — the bounded in-process `nilcore swarm` surface over the Phase-11 spine) is **shipped**, tracked in its own design doc (`docs/SWARM.md`). **Phase 13** (model-driven work-routing + earned multi-backend Trust-Ledger selection) is **shipped** (`[1.1.0]`). **Phase 14** (agentic browser use — `nilcore browse`) and **Phase CU** (desktop computer use — `nilcore desktop`, Path B + native Path A, plus the native-macOS host-control tier) are **shipped**, tracked in `docs/ROADMAP-BROWSER-USE.md` · `docs/ROADMAP-COMPUTER-USE.md` · `docs/ROADMAP-COMPUTER-USE-DARWIN.md`. **Phase 15** (OpenAI/OpenRouter/openai-compatible provider upgrade) is **shipped**, including the web-search wave (PR #65) (`docs/ROADMAP-PROVIDERS.md`); the one unbuilt task is **P15-T13** (the `eval/provider-compat/` eval coverage — no such directory exists), so a "completes Phase 15" claim is slightly ahead of reality. The external-infrastructure tier (`EXT-01..08`) is registered under "External infrastructure — GATED" below — it is **not** eligible work and stays blocked behind the thesis gate in `docs/ROADMAP-EXTERNAL-INFRA.md` §0. `docs/UPGRADE-PATH.md` holds the deep rationale + file:line sourcing for Phases 9–10 and the gated tier.

> **Deferred items D1–D4 shipped:** the four formerly-deferred items in `docs/IMPLEMENTATION-PLANS.md` are now implemented + merged — **D1** behavioral verification (sandboxed headless browser, the `browser_view` tool + the pure-Go `nilcore-browser` driver, composite verifier opt-in via `NILCORE_BROWSER_VERIFY`), **D2** semantic code search (content-hash-cached pure-Go HNSW, opt-in via `NILCORE_EMBED_KEY`), **D3** multi-language code intelligence (a language-parser seam + pure-Go, CGO-free Python, TS/JS, and Rust backends — not tree-sitter; the seam was broadened to four backends / nine extensions by R2), and **D4** the gated draft PR (`watch --open-pr` / `schedule --open-pr` via `internal/forge`, only after the human gate). All four are additive, opt-in, and pure stdlib — no new module was added (HNSW, the multi-language parser backends, the embedder, the browser driver incl. its pure-Go CDP client, and forge are all stdlib), so the core dependency count in the default binary stays at exactly **two** (pure-Go SQLite + `golang.org/x/sys`); the Charm TUI stack (3 modules) still links **only** under `make tui`. I6 holds; `CGO_ENABLED=0` across the release matrix. Invariants I1–I7 all hold unchanged.

---

## Phase 0 — finalize the core

### P0-T01 — CI pipeline
- **Goal:** every push/PR runs the gate automatically, so no broken code reaches `main`.
- **Depends on:** —  **Owns:** `.github/`, `.golangci.yml`
- **Acceptance criteria:**
  - GitHub Actions workflow runs `make verify` and `golangci-lint run` on push and PR.
  - `.golangci.yml` enables `govet`, `errcheck`, `staticcheck`, `ineffassign`, `gofmt`, `goimports`.
  - Workflow caches the Go build/module cache; fails red on any check.
- **Verify:** workflow file is valid YAML; lint config parses (`golangci-lint config verify` if available).
- **Notes:** invoke `golangci-lint` directly in CI, not via the Makefile (Makefile is a contract file).

### P0-T02 — Compile & `make verify` green  · BLOCKING, runs solo
- **Goal:** the offline-authored scaffold builds, vets, and tests cleanly. Nothing parallel may start until this merges.
- **Depends on:** —  **Owns:** the whole tree (any file needed to compile)
- **Acceptance criteria:**
  - `go build ./...`, `go vet ./...`, `go test ./...` all pass.
  - Any compile/vet fix preserves the public API and all invariants in `docs/ARCHITECTURE.md`.
  - `gofmt`/`goimports` applied repo-wide.
- **Verify:** `make verify` exits 0.
- **Notes:** keep changes minimal and behavior-preserving. If a fix would change an interface, stop and raise it — do not redesign here.

### P0-T03 — Sandbox container image
- **Goal:** a reproducible image the sandbox runs commands in, with build toolchains and (later) the delegated CLIs.
- **Depends on:** P0-T02  **Owns:** `images/sandbox/`
- **Acceptance criteria:**
  - `images/sandbox/Dockerfile` builds a slim image with `git`, `make`, and a Go toolchain.
  - Pinned base image and tool versions; non-root user; documented build/tag command.
  - A doc note on adding the Codex/Claude Code CLIs to the image (Phase 2 in-container delegation).
- **Verify:** `podman build -t nilcore/sandbox:latest images/sandbox` succeeds; `podman run --rm nilcore/sandbox:latest sh -c 'git --version && go version'` works.

### P0-T04 — Test fixtures + smoke test
- **Goal:** an end-to-end check that the native loop actually converges on a real failing-test repo.
- **Depends on:** P0-T02, P0-T03  **Owns:** `test/`
- **Acceptance criteria:**
  - `test/fixtures/failing-go/` — a tiny Go repo with one failing test.
  - `test/smoke/` — an external test (uses the built binary) that runs the native backend and asserts the verifier turns green. Gated behind an `ANTHROPIC_API_KEY` env check; skips cleanly when absent.
- **Verify:** `make verify` green with the smoke test skipped; documented manual run with a key present.
- **Notes:** keep `Owns` to `test/` only — do not add tests under `internal/agent/` (that package is owned by P1 tasks).

---

## Phase 1 — worktrees, gate, channel

### P1-T01 — Worktree manager
- **Goal:** create and tear down an isolated git worktree + branch per task, so every run is disposable by construction.
- **Depends on:** P0-T02  **Owns:** `internal/worktree/`
- **Acceptance criteria:**
  - `Create(ctx, baseRepo, taskID) (Worktree, error)` makes a worktree on a fresh branch; `Worktree.Path()`, `Worktree.Cleanup()`.
  - Cleanup removes the worktree and (optionally) the branch; idempotent.
  - Errors wrapped; no leaked worktrees on failure (cleanup on partial create).
- **Verify:** `make verify`; unit test against a temp git repo (create → assert path exists → cleanup → assert gone).

### P1-T02 — Orchestrator uses worktrees + injection seams
- **Goal:** run each task in a fresh worktree, and introduce the seams Phase 3 needs so later work doesn't re-edit this package.
- **Depends on:** P1-T01  **Owns:** `internal/agent/`
- **Acceptance criteria:**
  - `Execute` creates a worktree for the task, points the sandbox/verifier at it, and cleans up after.
  - Define no-op default `Router` and `Spawner` interfaces consumed by the orchestrator (so P3 implements them in their own packages without editing `agent/`).
  - Existing single-backend behavior preserved; verifier remains the final gate.
- **Verify:** `make verify`; orchestrator test with a fake backend asserts worktree lifecycle + final verify.

### P1-T03 — Approver + gate wiring
- **Goal:** turn the reversibility policy into a real gate at the integration boundary.
- **Depends on:** P1-T02  **Owns:** `internal/policy/`, `internal/agent/`
- **Acceptance criteria:**
  - `policy.Approver` implemented by a `ConsoleApprover` (prompts on stdin).
  - The orchestrator consults `policy.GateStructured` before any irreversible action (merge/deploy hooks); reversible actions proceed unattended.
  - Gate decisions are logged to the event log.
- **Verify:** `make verify`; table test of classify→gate with a stub approver (approve/deny paths).

### P1-T04 — `Channel` interface  · contract
- **Goal:** define the one seam all channels implement, before any implementation exists.
- **Depends on:** P1-T02  **Owns:** `internal/channel/channel.go`
- **Acceptance criteria:**
  - `Channel` interface: receive a task request, send progress updates, ask a gate question and await yes/no.
  - Documented, minimal, transport-agnostic. `docs/ARCHITECTURE.md` updated to register the seam (same serialized PR).
- **Verify:** `make verify` (interface compiles; a compile-time `var _ Channel` assertion stub may live with the first impl).
- **Notes:** contract file — runs alone; no parallel task may touch `internal/channel/channel.go`.

### P1-T05 — Telegram channel
- **Goal:** drive NilCore from a phone; gates become yes/no replies.
- **Depends on:** P1-T03, P1-T04  **Owns:** `internal/channel/telegram/`
- **Acceptance criteria:**
  - Long-poll bot using `TELEGRAM_BOT_TOKEN`; maps a chat to a task; streams status; renders gate questions as inline yes/no and feeds the answer to `policy.Approver`.
  - Stdlib HTTP only (no external dep) unless justified in PR/CHANGELOG.
  - Graceful handling of network errors and restarts.
- **Verify:** `make verify`; unit tests with a mocked HTTP transport for the bot API.

### P1-T06 — Slack channel  · parallel with P1-T05
- **Goal:** same `Channel` over Slack.
- **Depends on:** P1-T04  **Owns:** `internal/channel/slack/`
- **Acceptance criteria:** socket-mode app using `SLACK_APP_TOKEN`/`SLACK_BOT_TOKEN`; task mapping, status, gate buttons; same interface conformance and error handling as Telegram.
- **Verify:** `make verify`; mocked-transport tests.

### P1-T07 — `serve` mode
- **Goal:** a long-running mode that listens on a channel and dispatches tasks to the orchestrator.
- **Depends on:** P1-T05  **Owns:** `cmd/nilcore/`, `internal/server/`
- **Acceptance criteria:**
  - `nilcore serve -channel telegram` runs the chosen channel and routes incoming task requests through `Execute`.
  - Clean shutdown (SIGINT/SIGTERM), one task at a time by default, structured logs.
- **Verify:** `make verify`; server test with a fake channel + fake orchestrator asserts dispatch + shutdown.

---

### P1-T08 — Structured native tools
- **Goal:** give the loop auditable, policy-scoped tools alongside the `run` shell escape hatch.
- **Depends on:** P1-T02  **Owns:** `internal/tools/`
- **Acceptance criteria:**
  - A tool registry plus structured tools: `read`, `write`, `edit` (structured diff), `search` (grep/glob), and core git operations.
  - Tools register into the native loop via the registry — **adding a tool does not edit `backend/native.go`** (the loop loads from the registry).
  - Each tool declares a schema; structured tools are the preferred path, shell is the fallback.
- **Verify:** `make verify`; per-tool unit tests against a temp dir (read/write/edit/search round-trips).
- **Notes:** these are what the Phase-2 tool-call policy engine scopes precisely; design them so paths and commands are inspectable.

### P1-T09 — MCP via code execution  · contract (go.mod)
- **Goal:** connect MCP servers as **typed code APIs on the sandbox filesystem** that the executor calls programmatically — Anthropic's *Code execution with MCP* model — instead of loading every tool definition into context.
- **Depends on:** P1-T08, P2-T04, P2-T05  **Owns:** `internal/mcp/`, `go.mod`
- **Acceptance criteria:**
  - An MCP client connects to configured servers and **generates typed wrappers deterministically from each server's schema** onto the sandbox filesystem under `./mcp/servers/<server>/<tool>`; unused wrappers cost ~zero tokens.
  - The executor **discovers tools on demand** via its `read`/`search` tools and invokes/chains them by writing code that runs in the sandbox; large results are filtered in-sandbox before reaching context.
  - A **direct tool-call fallback** exists for trivial one-shot calls.
  - **Untrusted boundary:** wrappers are codegen (not model-written); the executor's glue code runs in the sandbox under the gate (P2-T04) and injection guard (P2-T05); per-server policy, default-deny egress.
  - ~~The MCP dependency is added to `go.mod` with justification (second sanctioned dependency).~~ **Not done, by design (I6):** MCP shipped as a **stdlib JSON-RPC** client (`internal/mcp`) — **no module was added**, `go.mod` is untouched. The `· contract (go.mod)` label on this task is therefore vestigial (SQLite + `golang.org/x/sys` remain the only sanctioned module additions; the Charm TUI stack is build-tagged).
- **Verify:** `make verify`; tests that a mock server yields wrappers, that on-demand discovery loads only requested tools, and that a denied/irreversible call is gated.
- **Notes:** touches `go.mod` (contract) — serialized. Reuses the existing container sandbox and structured FS tools. Lands after the Phase-2 guard/gate.



### P1-T10 — Providers (Anthropic / OpenAI / OpenRouter)
- **Goal:** the native loop talks to a `Provider` interface, not one vendor, so executor/advisor/planner can be any model.
- **Depends on:** P0-T02  **Owns:** `internal/provider/`, `internal/model/`
- **Acceptance criteria:**
  - A `Provider` interface and three adapters: `anthropic` (Messages API — the existing client becomes this adapter), `openai` (Chat Completions/Responses), `openrouter` (OpenAI-compatible, base URL + `provider/model` namespace).
  - A **canonical internal message + tool format** translated to/from each provider's wire shape (Anthropic `tool_use`/`tool_result` ↔ OpenAI `tool_calls`/tool messages).
  - Model selection is **role → `provider:model`** (executor, advisor, planner); model strings configurable. Fable 5 is a configured-but-disabled advisor option.
- **Verify:** `make verify`; per-adapter tests against mocked HTTP transports asserting request shape and response/tool-call parsing.
- **Notes:** keep the native backend depending on the interface so adding a provider never edits the loop. Cross-provider Advisor-Executor relies on the self-built `ask_advisor` (P3-T08).

### P1-T11 — SecretStore (keychain / encrypted / env)
- **Goal:** store all credentials securely; the model never sees a key. Implements `docs/SECRETS.md`.
- **Depends on:** P0-T02  **Owns:** `internal/secrets/`
- **Acceptance criteria:**
  - A `SecretStore` interface with backends, auto-detected: OS keychain (macOS Keychain, Linux Secret Service), encrypted-file vault (secretbox/age) for headless hosts, env, and an external hook.
  - Get/set/delete by secret name; values held transiently in host memory; never written to disk in plaintext, never logged, never placed in a prompt/context.
  - Headless-VPS master key configurable (key-file default `0600`, plus systemd-creds / passphrase options).
- **Verify:** `make verify`; backend tests (encrypted round-trip; env backend; keychain behind a build tag/mock); a test asserting no secret value appears in any produced log line.

### P1-T12 — Onboarding wizard (`nilcore init`)
- **Goal:** one guided flow gets a fresh machine fully configured and verified.
- **Depends on:** P1-T10, P1-T11, P1-T04, P0-T03  **Owns:** `internal/onboard/`, `cmd/nilcore/`
- **Acceptance criteria:**
  - A line-based interactive wizard (stdlib; works over SSH on a headless VPS) that captures providers + keys (→ SecretStore, not config), executor/advisor model tiers, delegated CLIs (detect + auth Codex and Claude Code), container runtime + sandbox image, and the chat channel; then runs a **smoke test** end-to-end.
  - Writes a JSON config holding *references* to secrets, never the secrets.
  - A **non-interactive mode** (flags/env) for scripted provisioning.
- **Verify:** `make verify`; onboarding test driving scripted input through the flow and asserting config + stored-secret references (with a fake SecretStore).
- **Notes:** `cmd/nilcore` is a thin subcommand dispatcher (`init`, `serve`, run) so this and P1-T07 add subcommands without colliding; subcommand logic lives in `internal/onboard` / `internal/server`.

### P1-T13 — Cross-platform paths + release matrix
- **Goal:** run on macOS and Linux (amd64/arm64) from one binary, and ship it.
- **Depends on:** P0-T02  **Owns:** `internal/paths/`, `.github/workflows/release.yml`, `scripts/`
- **Acceptance criteria:**
  - A `paths` helper resolving per-OS config/data dirs (XDG on Linux, Application Support on macOS).
  - A release workflow cross-compiling `darwin`/`linux` × `amd64`/`arm64`; a curl-pipe-sh installer and a sample systemd unit in `scripts/`; notes for a Homebrew tap.
- **Verify:** `make verify`; `paths` tests per `GOOS`; the release workflow builds all targets in CI.

## Phase 2 — security hardening

### P2-T01 — Hardened container flags
- **Goal:** minimize the sandbox blast radius.
- **Depends on:** P0-T02  **Owns:** `internal/sandbox/`
- **Acceptance criteria:** rootless by default; `--cap-drop=ALL`, `--security-opt no-new-privileges`, read-only rootfs with a writable tmpfs + the `/work` mount; non-root in-container user; configurable, with safe defaults.
- **Verify:** `make verify`; a test asserting the generated `run` arg list contains the hardening flags.

### P2-T02 — Egress allowlist
- **Goal:** replace blanket `--network none` with policy-driven, default-deny egress.
- **Depends on:** P2-T01  **Owns:** `internal/sandbox/`, `internal/policy/`
- **Acceptance criteria:** an allowlist (e.g. package registries, the model API) expressed in policy; everything else denied; documented mechanism (proxy or network policy); default remains deny-all when no allowlist is given.
- **Verify:** `make verify`; tests for allow/deny decisions.

### P2-T03 — Per-run secret injection
- **Goal:** secrets reach the sandbox (and in-container delegated CLIs) only for the single run, never persisted — sourced from the SecretStore (P1-T11).
- **Depends on:** P2-T02, P1-T11  **Owns:** `internal/sandbox/`, `internal/backend/codex.go`, `internal/backend/claudecode.go`
- **Acceptance criteria:** API keys passed via container env per invocation; the Codex/Claude Code adapters run **inside** the sandbox image with their key injected per run; no secret on disk, in logs, or in the prompt.
- **Verify:** `make verify`; test asserting env is set on the run command and absent from any logged event.

### P2-T04 — Tool-call policy engine
- **Goal:** validate the native loop's tool calls before they execute.
- **Depends on:** P1-T03  **Owns:** `internal/policy/`, `internal/backend/native.go`
- **Acceptance criteria:** each `run` tool call is checked against a schema + policy (path scoping, denylisted commands) before `Box.Exec`; denied calls return a structured error to the model instead of executing; decisions logged.
- **Verify:** `make verify`; table tests of allowed/denied commands; native-loop test with a fake model asserts a denied call is not executed.

### P2-T05 — Prompt-injection boundary
- **Goal:** keep fetched/file content as data, never instructions.
- **Depends on:** P2-T04  **Owns:** `internal/guard/`, `internal/backend/native.go`
- **Acceptance criteria:** a `guard` that wraps/quarantines untrusted content surfaced to the model; the system prompt's controlling instructions never derive from tool output; documented boundary with tests for representative injection strings.
- **Verify:** `make verify`; tests that injected "ignore previous instructions" content is neutralized (treated as data).

### P2-T06 — Hash-chained log + redaction
- **Goal:** make the audit trail tamper-evident and secret-free.
- **Depends on:** P0-T02  **Owns:** `internal/eventlog/`
- **Acceptance criteria:** each event carries a hash chaining it to the prior event; a verify function detects breaks; a redactor strips anything matching secret patterns before write; existing `Append` signature preserved.
- **Verify:** `make verify`; tests for chain integrity (tamper → detected) and redaction.

---

### P2-T07 — Authorized control (channel allowlist + gate auth)
- **Goal:** only authorized principals may command the agent — close the "anyone who finds the bot drives it" hole. See `docs/OPERATIONS.md` §1.
- **Depends on:** P1-T04, P1-T07, P2-T04  **Owns:** `internal/channel/`
- **Acceptance criteria:** an **allowlist** of principals (per-channel user/workspace IDs) in config; every inbound command is checked, and unauthorized senders are **rejected and logged** (never executed); **gate approvals** are accepted only from authorized principals; the allowlist is empty-by-default (deny-all until configured).
- **Verify:** `make verify`; tests that an allowlisted sender's command runs, a non-allowlisted sender's is rejected+logged, and a gate approval from an unauthorized principal is ignored.

## Phase 3 — orchestration & routing

### P3-T01 — Planner
- **Goal:** decompose a goal into an explicit task tree. **Adaptive:** invoked only for complex tasks — a cheap complexity assessment at task entry decides plan-vs-interleave (simple tasks skip the planner entirely). Implemented via the **advisor tier** (P3-T08): the strong advisor model produces the plan.
- **Depends on:** P1-T02  **Owns:** `internal/planner/`
- **Acceptance criteria:** model-driven `Plan(ctx, goal) (Tree, error)` producing tasks with dependencies; deterministic, schema-validated output (JSON); the tree is an inspectable/editable artifact. **Contract-first (principle #6):** the plan states the **acceptance criteria — ideally the failing test — that defines "done"** before any code is written.
- **Verify:** `make verify`; tests with a fake model returning a known plan JSON.

### P3-T02 — Subworker spawner
- **Goal:** run subtasks as scoped backends with budgets, in parallel worktrees, and collect results.
- **Depends on:** P3-T01, P1-T01  **Owns:** `internal/spawn/`
- **Acceptance criteria:** implements the `Spawner` seam from P1-T02; each subworker gets its own worktree + token/time/tool budget; results aggregate; failures isolate (one subworker failing doesn't crash the run).
- **Verify:** `make verify`; tests with fake backends running concurrently, asserting isolation + aggregation.

### P3-T03 — Blackboard
- **Goal:** share task-tree state and artifacts across subworkers without bloating each one's context.
- **Depends on:** P3-T02, P4-T01  **Owns:** `internal/blackboard/`
- **Acceptance criteria:** a store-backed shared state (tasks, statuses, artifacts) with concurrent-safe read/write; subworkers read their slice, write results; no cross-worker context stuffing.
- **Verify:** `make verify`; concurrent read/write tests.
> _Shipped note:_ `internal/blackboard` was **never built** — cross-worker state is carried by the agent message bus (`internal/agent/bus`) and the project loop instead (a deliberate no-blackboard design; see the "NOT a blackboard" comment in `agent/bus/message.go`). Treat this task as superseded.

### P3-T04 — Routing (escalation + race + review)
- **Goal:** the adaptive routing policy, with the verifier as judge.
- **Depends on:** P3-T02, P2-T01  **Owns:** `internal/route/`
- **Acceptance criteria:** implements the `Router` seam from P1-T02 — single backend by default; on a hard/failed task, race best-of-N backends in parallel worktrees and let the **verifier** select the winner; run cross-model review before the irreversible gate; per-task budgets cap the multiplier; every race outcome logged (this is the data that later earns strength-routing).
- **Verify:** `make verify`; tests where two fake backends race and the one passing the (fake) verifier is selected; review step invoked before gate.

### P3-T05 — Wire planner/spawn/route into orchestrator
- **Goal:** the single, serialized `agent/` edit that connects Phase 3.
- **Depends on:** P3-T01, P3-T02, P3-T03, P3-T04  **Owns:** `internal/agent/`
- **Acceptance criteria:** `Execute` uses the planner to decompose, the spawner to parallelize, the router to choose backends, and the message bus for shared state (the blackboard of P3-T03 was never built); single-task path still works; verifier remains the final gate.
- **Verify:** `make verify`; end-to-end orchestrator test with fakes for planner/spawn/route.

---

### P3-T06 — Summarizer + `ContextSummary` handoff
- **Goal:** the summarize-and-handover mechanism — bound context at every level without losing intent.
- **Depends on:** P1-T02  **Owns:** `internal/summarize/`
- **Acceptance criteria:**
  - A `ContextSummary` type (goal, constraints, decisions so far, remaining work) and a summarizer step (one model call) that produces it from working state.
  - The spawner (P3-T02) seeds each fresh-context subworker with a `ContextSummary`; results fold back as compact summaries, not full transcripts.
  - The native loop self-handoffs via the same path when its own window approaches the limit (summarize → restart) instead of failing.
- **Verify:** `make verify`; tests that a summary captures required fields and that a seeded subworker starts from it within budget.

### P3-T07 — Proactive trigger
- **Goal:** let the agent self-start **reversible** work without being asked.
- **Depends on:** P3-T05, P1-T03  **Owns:** `internal/trigger/`
- **Acceptance criteria:**
  - Watches signals (e.g. failing CI, flagged issues) and self-initiates a task for reversible work; anything irreversible routes to the human gate.
  - Self-started work is announced tersely and fully audited; configurable on/off and signal set.
  - Cannot bypass the gate or any invariant.
- **Verify:** `make verify`; test that a reversible trigger starts a task and an irreversible one is gated.

### P3-T08 — Advisor-Executor (two-tier model)
- **Goal:** a cheap executor model drives the loop and escalates to a strong advisor on demand — Anthropic's Advisor Strategy.
- **Depends on:** P1-T08, P3-T06, P1-T10  **Owns:** `internal/advisor/`, `internal/model/`
- **Acceptance criteria:**
  - Two tiers resolved as **role → `provider:model`** via the Provider abstraction (P1-T10), so executor and advisor can be different providers: executor (default `anthropic:claude-sonnet-4-6`; Haiku or `openai:gpt-5.4-mini` options), advisor (default `anthropic:claude-opus-4-8`; Fable 5 when re-enabled).
  - An `ask_advisor` tool in the loop's registry — the executor calls it when stuck / above its skill / needing a plan; the harness seeds the advisor with a `ContextSummary` (P3-T06), returns the guidance, the executor resumes. The advisor advises only; it does not execute.
  - Two paths: **self-built `ask_advisor`** (separate, fully-audited advisor call — default) and Anthropic's **native Advisor Tool** (server-side, one request — config toggle).
  - A per-task advisor-call ceiling + separate advisor-token budget; every escalation logged as a distinct event (when / why / what); a harness fallback escalates after K consecutive verifier failures.
- **Verify:** `make verify`; tests that `ask_advisor` triggers an advisor call with a summary, the fallback fires after K failures, and the budget ceiling caps calls.
- **Notes:** no new dependency — both paths use the existing Messages client. The advisor tier is also the Planner (P3-T01) and the cross-model reviewer (P3-T04).



### P3-T09 — Code intel: AST + symbols (tree-sitter)
- **Goal:** the structural foundation — parse any language to an AST and extract symbols (functions, types, methods, modules) and references (the "tag map"). Broad, fast, incremental, no server required. Full design: `docs/CODE-INTELLIGENCE.md` (L2).
- **Depends on:** P1-T08  **Owns:** `internal/codeintel/ast/`
- **Acceptance criteria:** tree-sitter-backed parser; `Symbols(path)` and `References(path)` over a multi-language fixture; incremental re-parse of a single changed file; results carry source spans.
- **Verify:** `make verify`; fixture tests across ≥2 languages asserting symbol/reference extraction and span accuracy.

### P3-T10 — Code intel: graph + SQLite + queries
- **Goal:** the code graph — nodes (symbols/files), edges (`calls`, `implements`, `imports`, `references`, `inherits`, `defines`, `type-of`) in SQLite; structural queries via recursive CTEs (callers/callees, dependency closure, reachability). The backbone pure-RAG lacks.
- **Depends on:** P3-T09, P4-T01  **Owns:** `internal/codeintel/graph/`
- **Acceptance criteria:** build graph from AST output; `Callers(sym)`, `Callees(sym)`, `Implementers(iface)`, `Reachable(from,to)`; recursive-CTE transitive queries; idempotent rebuild.
- **Verify:** `make verify`; fixture-graph tests asserting edge correctness and transitive-closure results.

### P3-T11 — Code intel: PageRank repo-map
- **Goal:** orientation — a compact, **PageRank-ranked**, token-budgeted skeleton of the most central files/symbols with signatures, read *before* any file. Centrality in the reference graph = importance.
- **Depends on:** P3-T10  **Owns:** `internal/codeintel/repomap/`
- **Acceptance criteria:** `RepoMap(budget)` returns a deterministic, budget-bounded map ranked by centrality; stable under unrelated edits.
- **Verify:** `make verify`; tests asserting the map fits the budget and ranks known-central fixtures first.

### P3-T12 — Code intel: LSP client (SCIP-aligned)
- **Goal:** precision upgrade — query a language server (gopls, rust-analyzer, pyright, …) for exact types, definitions, references, diagnostics; graceful fallback to AST when no server. Aligned with **SCIP**.
- **Depends on:** P3-T09  **Owns:** `internal/codeintel/lsp/`
- **Acceptance criteria:** spawn/handshake an LSP server; `Definition`, `References`, `Hover/Type`, `Diagnostics`; clean degradation to tree-sitter when unavailable; provenance tag = precise.
- **Verify:** `make verify`; tests with a stub/real server asserting precise results and fallback behavior.

### P3-T13 — Code intel: semantic index (hybrid)
- **Goal:** concept reach — embeddings over **whole symbols** (via the Provider embeddings endpoint), stored in SQLite (`sqlite-vec`); used as an **entry point** then **graph-expanded**. Hybrid with lexical for recall.
- **Depends on:** P3-T10, P4-T01, P1-T10  **Owns:** `internal/codeintel/semantic/`
- **Acceptance criteria:** index symbols; `Search(query)` returns ranked symbols with provenance=lead; results expandable along the graph; embeddings optional (degrades to lexical+graph).
- **Verify:** `make verify`; tests with a fake embeddings provider asserting ranked retrieval and graceful absence.

### P3-T14 — Code intel: retrieval + Context Bundle
- **Goal:** the fusion layer — a query planner that routes a need through the right lenses and assembles a **Context Bundle** (minimal-sufficient, structurally-coherent: symbols + immediate neighborhood + "why included", budget-bounded). The unit handed to the loop.
- **Depends on:** P3-T10, P3-T11, P3-T12, P3-T13  **Owns:** `internal/codeintel/retrieve/`
- **Acceptance criteria:** `Retrieve(need, budget) Bundle`; hierarchical narrowing (repo→file→symbol); each item carries provenance + rationale; deterministic under fixed inputs.
- **Verify:** `make verify`; tests asserting bundles stay within budget and include the structurally-correct neighborhood for known needs.

### P3-T15 — Code intel: Impact Set + test-impact + SBFL
- **Goal:** understanding drives the loop and the gate — compute the **Impact Set** (transitive call sites/implementers/dependents/tests of a change); map symbols→tests so the verifier runs affected tests first; **SBFL** ranks likely-faulty symbols from test pass/fail. Feeds the autonomy gate (blast radius = caution).
- **Depends on:** P3-T10  **Owns:** `internal/codeintel/impact/`
- **Acceptance criteria:** `ImpactSet(change)` (symbols + affected tests); `AffectedTests(change)`; `Localize(failures)` SBFL ranking; exposes a blast-radius magnitude the gate can read.
- **Verify:** `make verify`; fixture tests asserting impact closure, correct affected-test selection, and SBFL ranking on a seeded bug.
> _Shipped note:_ `ImpactSet` + `AffectedTests` shipped and are wired (the `affected_tests` tool). The `Localize` **SBFL ranker was pruned as unused dead code (#98)** — it was never wired into the loop; re-adding SBFL would need a post-failure coverage-gathering step.

### P3-T16 — Code intel: living updates + memory fusion
- **Goal:** stay current cheaply + compound over time — incremental re-parse on file-change (worktree-aware, reflecting the agent's own in-progress edits); fuse the static graph with Phase-4 memory (conventions, gotchas, the "why").
- **Depends on:** P3-T10, P4-T03  **Owns:** `internal/codeintel/live/`
- **Acceptance criteria:** file-watch → incremental graph/map update (no full re-index); worktree-scoped view includes uncommitted edits; memory hits surfaced alongside graph facts with provenance=lead.
- **Verify:** `make verify`; tests asserting a single-file edit updates only affected nodes and that worktree edits appear in queries.

## Phase 4 — cross-project memory

### P4-T01 — SQLite store  · contract (go.mod)
- **Goal:** the persistent backbone for events and memory.
- **Depends on:** P0-T02  **Owns:** `internal/store/`, `db/`, `go.mod`
- **Acceptance criteria:** SQLite schema + migrations for events, memory, and tasks; typed queries (sqlc) under `db/`; a thin `store` package wrapping them; the SQLite driver added to `go.mod` with justification (first sanctioned dependency).
- **Verify:** `make verify`; store tests against a temp DB (migrate → insert → query).
- **Notes:** touches `go.mod` (contract) — coordinate as a serialized change.

### P4-T02 — Event log → store backing
- **Goal:** graduate the JSONL log to the store while preserving the interface.
- **Depends on:** P4-T01, P2-T06  **Owns:** `internal/eventlog/`, `internal/store/`
- **Acceptance criteria:** `Append` writes to the store (hash chain preserved); JSONL remains available as an export; callers unchanged.
- **Verify:** `make verify`; tests asserting events land in the store and the chain still verifies.

### P4-T03 — Memory model + write API
- **Goal:** represent conventions, decisions, and learned facts, keyed by project and global scope.
- **Depends on:** P4-T01  **Owns:** `internal/memory/`
- **Acceptance criteria:** typed memory records with scope (project/global), a write API, and a query API (keyword to start; embeddings are a later, justified extension); store-backed.
- **Verify:** `make verify`; write/query tests across scopes.

### P4-T04 — Retrieval into context
- **Goal:** make the native loop start each task informed by relevant memory.
- **Depends on:** P4-T03, P2-T05  **Owns:** `internal/memory/`, `internal/backend/native.go`
- **Acceptance criteria:** at task start, retrieve relevant memory and inject it into context assembly (clearly labeled as memory, not instructions — respect the injection boundary); retrieval is bounded (token budget aware).
- **Verify:** `make verify`; test that retrieved memory appears in the assembled prompt within budget.

### P4-T05 — Memory write-back
- **Goal:** persist durable facts/decisions after a task so the agent improves over time.
- **Depends on:** P4-T03, P3-T05  **Owns:** `internal/memory/`, `internal/agent/`
- **Acceptance criteria:** after a successful task, extract durable conventions/decisions and write them to memory (deduped); noisy/ephemeral detail excluded.
- **Verify:** `make verify`; test that a task with a known outcome writes the expected memory record.

---

## Phase 5 — gated self-improvement

### P5-T01 — Skill / plugin system
- **Goal:** add capabilities as plugins, not core changes — in **both** the Agent Skills standard and a native plugin format.
- **Depends on:** P3-T05  **Owns:** `internal/skills/`
- **Acceptance criteria:** a registry + loader supporting **Agent Skills (`SKILL.md`)** *and* native tool plugins; both discovered and exposed to the loop without modifying the frozen core; a clear contract and one example for each format; skills surface through the same tool registry as native/MCP tools (consistent gating).
- **Verify:** `make verify`; test loading an example of each format and exposing it to a fake loop.

### P5-T02 — Gated self-edit flow
- **Goal:** let the agent **proactively** propose changes to its own prompts, skills, and tools when it spots a recurring pattern — safely.
- **Depends on:** P5-T01, P2-T05  **Owns:** `internal/selfimprove/`
- **Acceptance criteria:** a **proactive trigger** (recurring failures / repeated manual steps / a missing tool) raises a proposal; a **scope check** enforces the allow-list (prompts, skills, tools) and deny-list (invariants, contract files, core loop) — a diff touching anything denied is rejected; the edit runs as a normal task in a worktree, passes the verifier, and requires the human gate before merge; full audit; never bypasses any invariant.
- **Verify:** `make verify`; tests that an in-scope edit is gated and merges, and that an out-of-scope edit (touching the core) is rejected by the scope check.

### P5-T03 — Eval harness
- **Goal:** measure-first — score changes and backends on a benchmark, producing the data that earns strength-routing.
- **Depends on:** P3-T04  **Owns:** `eval/`
- **Acceptance criteria:** a suite of coding tasks with objective pass/fail (verifier-based); runs backends/configs and reports pass rate, cost, and latency; output consumable by the router as routing evidence.
- **Verify:** `make verify`; the harness runs against the `test/fixtures` repos and emits a structured report.

## Phase 6 — runtime resilience & operations

The seams that let NilCore run **unattended** without losing work, overspending, or taking orders from strangers. Full design: `docs/OPERATIONS.md`. (Authorized control lives in Phase 2 as P2-T07 because it is a security boundary.)

### P6-T01 — Provider resilience
- **Goal:** survive transient provider faults below the loop — 429s, timeouts, 5xx. See `docs/OPERATIONS.md` §2.
- **Depends on:** P1-T10  **Owns:** `internal/model/`
- **Acceptance criteria:** a wrapper over the `Provider` interface doing retry with **exponential backoff + jitter**, per-call **timeout**, **failover** across configured providers, and a **circuit-breaker** that skips a degraded provider; retries are bounded; the loop sees a clean call or a final, surfaced error.
- **Verify:** `make verify`; tests with a fake provider that fails N times then succeeds (retry), always fails (failover), and trips the breaker.

### P6-T02 — Cost metering + ceiling enforcement
- **Goal:** make the budget ceiling real — meter and enforce spend. See `docs/OPERATIONS.md` §3.
- **Depends on:** P1-T10, P4-T01  **Owns:** `internal/budget/`
- **Acceptance criteria:** a **ledger** meters tokens and dollars **per task and globally**, persisted to the store; a task that would exceed its ceiling **stops and surfaces**; live spend is queryable by the router and operator.
- **Verify:** `make verify`; tests that spend accrues correctly and that exceeding a ceiling halts the task.

### P6-T03 — Task durability + resume + graceful shutdown
- **Goal:** never lose work to a crash or reboot. See `docs/OPERATIONS.md` §4.
- **Depends on:** P3-T05, P4-T02, P1-T07  **Owns:** `internal/agent/`
- **Acceptance criteria:** orchestrator **task state is persisted**; on restart, in-flight tasks **resume from last checkpoint or fail cleanly** with a surfaced reason; **SIGTERM** triggers a checkpoint before exit; no partial state corrupts the store.
- **Verify:** `make verify`; tests that a task interrupted mid-run resumes from its checkpoint and that SIGTERM checkpoints cleanly.

### P6-T04 — Cross-task scheduler + resource arbitration
- **Goal:** handle multiple concurrent top-level tasks safely. See `docs/OPERATIONS.md` §5.
- **Depends on:** P1-T07, P3-T02, P6-T02  **Owns:** `internal/scheduler/`
- **Acceptance criteria:** a **queue + scheduler** runs concurrent tasks under caps (max concurrent sandboxes, global rate/spend budget) with fair ordering and **backpressure** when limits are hit; no unbounded fan-out.
- **Verify:** `make verify`; tests that concurrency respects the cap and that tasks queue rather than overrun limits.

### P6-T05 — Verification auto-detection
- **Goal:** verify arbitrary, unseen repos. See `docs/OPERATIONS.md` §6.
- **Depends on:** P0-T02, P3-T09  **Owns:** `internal/verify/`
- **Acceptance criteria:** inspect a repo (languages via the AST layer, build/test config) to produce a **verify plan** (build / test / lint); a safe fallback when undetectable; per-project **overrides** can be pinned in config.
- **Verify:** `make verify`; fixture tests across ≥2 ecosystems asserting the correct verify plan is detected, plus override precedence.

### P6-T06 — Resource cleanup / GC
- **Goal:** keep disk bounded over long unattended runs. See `docs/OPERATIONS.md` §7.
- **Depends on:** P1-T01, P0-T03  **Owns:** `internal/maint/`
- **Acceptance criteria:** a maintenance pass GCs merged/stale worktrees and dead containers, **rotates** logs, and enforces a disk-usage bound; safe (never deletes an active worktree/task); schedulable.
- **Verify:** `make verify`; tests that stale worktrees/containers are collected and active ones are preserved.

### P6-T07 — Operator observability + health
- **Goal:** let the operator see, debug, and supervise. See `docs/OPERATIONS.md` §8.
- **Depends on:** P2-T06, P6-T02, P6-T03  **Owns:** `internal/inspect/`
- **Acceptance criteria:** `nilcore` subcommands **inspect/replay** the event log and show **task status** and **spend**; `serve` exposes a **health/readiness** check; built on the hash-chained log (verifies integrity on read).
- **Verify:** `make verify`; tests that replay reconstructs a run, status/spend read correctly, and health reports ready/not-ready.

### P6-T08 — Config schema + validation + migration
- **Goal:** turn malformed config into a precise message, not a runtime surprise. See `docs/OPERATIONS.md` §9.
- **Depends on:** P1-T12  **Owns:** `internal/config/`
- **Acceptance criteria:** a **versioned schema** with **validation** (clear errors, sane defaults) and **migration** across versions; `nilcore init` output validates; an unknown/old config is migrated or rejected with guidance.
- **Verify:** `make verify`; tests for valid/invalid configs and a version migration.
- **Status (retired):** the standalone `internal/config` package was built and tested in isolation but never wired into boot, and its schema (`executor`/`runtime`/`model`/`max_steps`) diverged from the live `onboard.Config` (providers, channel, backend, …). The acceptance criteria are now met by `internal/onboard.Config` itself — `Load` decodes strictly (unknown fields rejected), migrates by `version`, and `Validate`s, so a malformed `config.json` fails loudly at boot. The dead, divergent package was removed to keep one source of truth.

---

## Phase 7 — Portability & efficiency

Drop the hard dependencies that pin NilCore to a container host, so the sandboxed
loop (I4) runs wherever a modern Linux kernel does — a cheap VPS, a Pi, a
locked-down CI runner — without giving up confinement. Built entirely around the
frozen `sandbox.Sandbox` interface and the `backend.CodingBackend` contract (I1):
every backend gets a swappable sandbox without any code change.

### P7-T01 — Host-native namespace + Landlock sandbox backend
- **Goal:** a second `sandbox.Sandbox` implementation that confines a model-emitted command with Linux user/mount/pid/net namespaces + Landlock, needing **no container runtime, image, or daemon**, plus a `sandbox.New` factory that auto-detects and prefers it over a container when the kernel supports it.
- **Depends on:** P0-T03 (the `sandbox` package), P2 sandbox hardening  **Owns:** `internal/sandbox/`, `cmd/nilcore/` (sandbox wiring only), `.github/workflows/ci.yml`, `docs/ARCHITECTURE.md`, `go.mod`
- **Acceptance criteria:**
  - `sandbox.Namespace` satisfies `Sandbox` and confines via a re-exec: the command is born in fresh namespaces, and the re-exec'd child sets `no_new_privs` + a Landlock domain (read+exec the host toolchain; read+write **only** the worktree + a `/tmp` scratch + the usual char devices) before `execve`ing `/bin/sh -c <cmd>` — so I4 holds (no arbitrary program on the host; FS confined; `CLONE_NEWNET` = default-deny egress).
  - `sandbox.New(Options)` auto-detects: prefer namespace where Landlock + unprivileged userns are usable, else fall back to a container; an explicit, unsatisfiable preference errors. `-sandbox auto|namespace|container` + `NILCORE_SANDBOX` select; `auto` is the default. The probe is **conservative** (an AppArmor/sysctl-restricted userns reads as unsupported) so `auto` never picks a backend that would `EPERM`.
  - Additive only: the container backend and all callers are unchanged (the factory returns the existing interface); the package builds on darwin via a `//go:build !linux` stub; `golang.org/x/sys` is promoted to a direct dependency (I6 exception, scoped to `internal/sandbox`, justified in the PR + CHANGELOG).
  - A dedicated `sandbox-linux` CI job runs the confinement/escape tests with `NILCORE_SANDBOX_MUST_RUN=1` (fail, not skip) — the security property is only observable on Linux, so CI is its authoritative verifier.
- **Verify:** `make verify` (darwin + linux); `GOOS=linux go build/vet ./...`; the `sandbox-linux` job proves a command runs confined, a write outside the worktree is denied (Landlock), `/dev/null` + `/tmp` scratch work, per-run env reaches the command, and egress is denied.

### P7-T02 — seccomp-bpf syscall filter for the namespace backend (follow-up)
- **Goal:** add a defense-in-depth seccomp-bpf allow/deny syscall filter to `sandbox.Namespace`, applied in the same re-exec child (TSYNC, after `no_new_privs`, before `execve`), shrinking the kernel attack surface beyond namespaces + Landlock.
- **Depends on:** P7-T01  **Owns:** `internal/sandbox/`
- **Acceptance criteria:** a conservative syscall policy that doesn't break common toolchains (compilers, test runners); applied fail-closed; ABI-aware; covered by the `sandbox-linux` job (a denied syscall is blocked, an allowed one runs).
- **Verify:** `make verify`; the `sandbox-linux` job asserts a denied syscall fails and normal builds/tests still pass under the filter.
- **Status (shipped):** `internal/sandbox/seccomp_linux.go` installs a classic-BPF **denylist** (arch-validated; blocks `mount`/`umount2`/`pivot_root`/`chroot`/`setns`/`unshare`/`ptrace`/`kexec_load`/module-load/`reboot`/`swap`/`bpf`/`perf_event_open`/keyring/`acct`/clock-set/`quotactl`/`process_vm_*` with EPERM, allows the rest) via `seccomp(2)` + `SECCOMP_FILTER_FLAG_TSYNC`, applied in the re-exec child after Landlock and before `execve`. Per-arch `AUDIT_ARCH` lives in `seccomp_linux_{amd64,arm64,other}.go`; an arch NilCore doesn't target (or a kernel without seccomp filtering) degrades gracefully to namespaces + Landlock (still I4). Fail-closed on a malformed filter. The `sandbox-linux` job asserts the filter is active (`/proc/self/status` Seccomp mode 2), that a denied syscall fails (`chroot` EPERMs), and that normal work still runs; a hermetic `TestSeccompProgramShape` checks the BPF jump arithmetic. Cross-compiles + `go vet` clean for amd64/arm64; `golangci-lint` 0 issues.

---

## Phase 8 — Full multi-agent concurrency

Run a decomposition's independent subagents **concurrently** instead of serially,
honoring `DependsOn`, while keeping reasoning + verified integration serial so no
invariant weakens. Full design + adversarial review: `docs/CONCURRENCY.md`. The
model is **dynamic-wave async dispatch**; `-concurrency 1` is byte-identical.

### P8-T01 — Pre-existing fixes the concurrency review surfaced (do first)
- **Goal:** close two latent bugs independent of concurrency. (1) Route the project-loop reflect advisor and the greenfield-bootstrap advisor through the **metered** strong provider (they use raw `d.strong` at `build.go:329/795`, escaping the budget wall). (2) Make `Spawner.Spawn`'s semaphore acquire honor `ctx` (`spawn.go:79`) and record a cancelled `Result` for the remainder.
- **Depends on:** —  **Owns:** `cmd/nilcore/build.go`, `internal/spawn/spawn.go`
- **Acceptance:** advisor spend on reflect/bootstrap charges the ledger (a test asserts `ErrCeiling` reaches them); a pre-cancelled ctx makes `Spawner.Spawn` return promptly with cancelled Results; `make verify` + `-race` green.

### P8-T02 — `-concurrency` flag + pre-wave validation seam
- **Goal:** add `-concurrency N` (default 1, clamp ≥1; gates the whole concurrent path so 1 is byte-identical) and lift the ID-uniqueness + role/depth/fanout rails out of the serial `doSpawn` into a single-goroutine **pre-wave validation** pass.
- **Depends on:** P8-T01  **Owns:** `cmd/nilcore/`, `internal/super/`
- **Acceptance:** at `-concurrency 1` the event log / branches / outcome are byte-identical to today (fixture diff); validation rejects a duplicate `spec.ID` before any dispatch; `make verify` green.

### P8-T03 — Process-global ctx-honoring worker advisor limiter
- **Goal:** a tiny stdlib `model.Provider` limiter (`sem chan struct{}`, acquire `select`s on `ctx.Done()`, `Stream` passthrough) wrapping the provider **handed to roster workers ONLY** — never the reader `Answer` path or the supervisor `Model`. Sized `< MaxFanout`, **process-global**. Saturation falls through to the existing graceful "proceed" fallback.
- **Depends on:** P8-T01  **Owns:** `internal/meter/` (or a new `internal/strongcap` leaf), `cmd/nilcore/build.go`, `internal/roster/`
- **Acceptance:** a correlated `EscalateAfter` herd never hangs and never starves `ask_supervisor`; the limiter degrades to fallback under saturation, never blocks; `ask_advisor` is always reachable; `-race` green.

### P8-T04 — Wire `DAGScheduler` into `dispatch()` (in-turn concurrency)
- **Goal:** batch a supervisor turn's `spawn_subagent` blocks into a wave-DAG and run it via `spawn.DAGScheduler` + the capped pool (cap = `-concurrency`); `OnReady` cuts a dependent from its dependency's branch; results fold into `runState` single-owner between waves (never worker→`runState`); one `tool_result` per `tool_use`, order preserved. Integration stays serial + supervisor-orchestrated.
- **Depends on:** P8-T02, P8-T03  **Owns:** `internal/super/`
- **Acceptance (the property gates):** under N concurrent workers the integration tip is **always** verifier-green and a red combination never poisons it (maximal-green prefix kept); a failed node `Skip`s its dependents; a worker blocking on `ask_supervisor` *and* `ask_advisor` inside a wave still resolves (no deadlock); a budget/deadline breach cancels all in-flight workers and `Wait` drains; peak concurrent sandboxes ≤ the process-global cap; `go test -race` green.

### P8-T05 — Phase 2/3 (follow-on): merged-tip multi-dep re-base · pipelined waves
- **Goal:** extend `OnReady` to re-base a multi-dependency node on a merged tip; pipeline wave N+1 planning with wave N execution; specify the supervisor's between-wave re-plan policy on a red dependency.
- **Depends on:** P8-T04  **Owns:** `internal/super/`, `internal/spawn/`, `internal/project/`
- **Acceptance:** multi-dep dependents see all deps' code; throughput improves with no invariant regression; `make verify` + `-race` green.

---

## Phase 9 — Behavioral verification & event-driven autonomy

Close the two sharpest competitive gaps without breaking an invariant: make the verifier able to exercise a *running* app (a browser/behavioral check feeds the verdict — `verify` stays the sole authority on "done", I2), and let work enter where developers live (SCM/CI events, schedules) through the existing `trigger` + reversibility gate. Promoted from `docs/UPGRADE-PATH.md` Tier 1 (`U1-T01..07` → `P9-T01..07`, same order); that file holds the deep rationale and the file:line sourcing for every task. Every task is additive and nil/flag-gated — the default binary is byte-identical when the feature is off. (Phase 8 is the multi-agent concurrency workstream, tracked separately.)

> **Status (shipped):** Phase 9 is complete and merged. The multimodal image block (P9-T01), the sandboxed `browser_view` tool + the pure-Go `nilcore-browser` driver (P9-T02), the composite browser-folding verifier (P9-T03), the HMAC-verified SCM/CI webhook intake (P9-T04), the gated draft-PR forge action (P9-T05), and the cron/interval trigger source (P9-T06) are all wired into the binary via `nilcore serve --webhook` and `nilcore schedule` (P9-T07). New packages: `internal/{scmhook, cron, forge}`, `cmd/tools/nilcore-browser`. Every path is additive + opt-in (`NILCORE_BROWSER`, `NILCORE_BROWSER_VERIFY`, `NILCORE_CHROMIUM`, `NILCORE_FORGE_TOKEN`, `NILCORE_WEBHOOK_SECRET`, `NILCORE_WEBHOOK_LABEL` — all via env/`SecretStore`, never logged, never given to the model, I3); the default binary is byte-identical with these features off. I1–I7 hold unchanged.

### P9-T01 — Multimodal content blocks (model + providers)  · contract, runs solo
- **Goal:** give the canonical message format an **additive** image content block so the agent can reason over a screenshot — the precondition for behavioral verification — without changing `backend.CodingBackend` or `Provider.Complete`.
- **Depends on:** —  **Owns:** `internal/model/`, `internal/provider/`, `docs/ARCHITECTURE.md`
- **Acceptance criteria:**
  - `model.Block` gains an additive image shape (e.g. a nested `Source{Type, MediaType, Data}` under `Type:"image"`); all existing fields/JSON tags unchanged; a text/tool_use/tool_result block marshals byte-identically.
  - The Anthropic adapter round-trips image blocks; the OpenAI adapter's `toOpenAIMessages` switch gains an explicit `image` case (today only `text`/`tool_use`/`tool_result` — an image block is silently dropped).
  - `Provider.Complete`/`Streamer.Stream` signatures unchanged (images travel inside the existing `[]Block`); `model.Chunk` stays text-only.
  - `docs/ARCHITECTURE.md` message-format / I1 text updated in the **same** PR.
- **Verify:** `make verify`; per-adapter round-trip tests (an image block → the Anthropic and OpenAI wire shapes); a test asserting a text-only `[]Block` is byte-identical to before.
- **Notes:** touches the vendor-neutral format every adapter implements ⇒ **serialized contract task, runs solo** (no parallel task may read `internal/model` as a stable interface meanwhile); stdlib only (I6). Deep rationale + sourcing: `docs/UPGRADE-PATH.md` §4 (U1-T01).
- **Status (shipped):** `model.Block` gained an additive `image` shape carrying a nested `Source{Type, MediaType, Data}`; the Anthropic and OpenAI adapters both carry it (images ride inside the existing `[]Block`). `Provider.Complete` and the `backend.CodingBackend` contract are **unchanged** (I1) — a text/tool_use/tool_result block still marshals byte-identically.

### P9-T02 — Sandboxed headless-browser tool
- **Goal:** an agent-facing tool that drives a headless browser **inside the sandbox** to navigate a running app and return a screenshot (a P9-T01 image block) + fenced DOM/console, so the loop can see what it built.
- **Depends on:** P9-T01  **Owns:** `internal/tools/`
- **Acceptance criteria:**
  - A `tools.Tool` (the 4-method interface) holding a `Box sandbox.Sandbox`, running a headless-browser driver via `Box.Exec`/`Box.ExecWithEnv` — **never** a host-side request; refuses when `Box==nil` (mirror `WebFetchTool`).
  - Returns a screenshot image block (P9-T01) + DOM/console text `guard.Wrap`'d as untrusted data (I7); the tool is **non-mutating** (safe in read-only modes).
  - Container-only and egress-gated (usable only on `*sandbox.Container` with the target host allowlisted); fails closed on the namespace backend (empty `CLONE_NEWNET`).
- **Verify:** `make verify`; a unit test driving a local static-file server started in the sandbox (navigate → screenshot + DOM); a test that `Box==nil` and the namespace backend both refuse.
- **Notes:** needs the browser binary in `images/sandbox/` (still fully self-hosted, not external infra). Distinct from the verifier-side check (P9-T03). I4/I7. Rationale: `docs/UPGRADE-PATH.md` §4 (U1-T02).
- **Status (shipped):** the `browser_view` tool drives a pure-Go `nilcore-browser` driver (`cmd/tools/nilcore-browser`, no new module) baked into the sandbox image via `Box.Exec`, navigates a running app, and hands the model a screenshot as a P9-T01 multimodal image block plus `guard.Wrap`'d DOM/console (I7); it refuses host-side and on the namespace backend (fails closed without a browser). Selected via `NILCORE_BROWSER` / `NILCORE_CHROMIUM`. **Honest caveat:** the live browser run is **CI-only** — there is no Chromium in the hermetic unit tests, and the driver fails **closed** without a browser.

### P9-T03 — Behavioral verifier (composite + browser check)
- **Goal:** make a behavioral browser check a first-class input to the verifier's verdict, so a feature that builds+tests green but renders broken ships **red** — keeping `verify` the sole authority (I2).
- **Depends on:** P9-T02  **Owns:** `internal/verify/`
- **Acceptance criteria:**
  - `verify.Composite` ANDs N child `Verifier.Check` reports into one; any red child ⇒ red overall.
  - `verify.BrowserVerifier{Box, URL, Assertions}` runs a browser-driver command inside the worktree sandbox box (like `CommandVerifier`) and reports `Passed` from the assertions; keeps `verify`'s leaf import graph (imports only `sandbox`).
  - Opt-in: default is the unchanged `CommandVerifier`; `verify.Pass` stays used **only** for read-only Discuss/Plan drives and never substitutes on an Execute drive.
- **Verify:** `make verify`; table test — command-pass+browser-fail ⇒ red; both pass ⇒ green; behavioral-off ⇒ byte-identical to `CommandVerifier`.
- **Notes:** I2 inviolable — the browser result is evidence the verifier consumes; there is no screenshot-bypasses-verify path. Rationale: `docs/UPGRADE-PATH.md` §4 (U1-T03).
- **Status (shipped):** the composite verifier folds a browser behavioral check **into** the verdict (opt-in via `NILCORE_BROWSER_VERIFY`), so a feature that builds+tests green but renders broken ships **red** — the verifier stays the **sole** authority on "done" (I2). Off ⇒ byte-identical to `CommandVerifier`. Live browser checks are CI-only (same caveat as P9-T02).

### P9-T04 — SCM/CI webhook intake → `trigger.Signal`
- **Goal:** let a labeled issue or a failing CI run become a `trigger.Signal` routed through the **existing** reversible-auto-start / irreversible-gate machinery.
- **Depends on:** —  **Owns:** `internal/scmhook/`
- **Acceptance criteria:**
  - A stdlib `net/http` inbound listener that verifies the webhook HMAC against a `secrets.SecretStore` secret (I3), maps issue / CI-failure events to `trigger.Signal{Source, Goal}`, and calls `trigger.Handle`.
  - Payloads are untrusted: any text surfaced is `guard.Wrap`'d (I7); an unsigned/invalid request is rejected + logged metadata-only (I5).
  - Stdlib only — no `go-github`/`go-gitlab` (I6); binds loopback by default (operator terminates TLS at a reverse proxy).
- **Verify:** `make verify`; tests — a signed `issues.labeled` / `workflow_run.failure` payload yields the expected Signal; a bad signature is 401 + logged + no Signal.
- **Notes:** adds a **new Signal source**, not a new mechanism — `trigger.Handle` already classifies reversible vs irreversible and logs `trigger_gated`/`trigger_start`. Rationale: `docs/UPGRADE-PATH.md` §4 (U1-T04).
- **Status (shipped):** `internal/scmhook` (pure stdlib `net/http`, no new module) verifies the webhook HMAC against a `SecretStore` secret (`NILCORE_WEBHOOK_SECRET` / `NILCORE_WEBHOOK_LABEL`, I3), maps SCM/CI events to a `trigger.Signal`, and routes through the **existing** reversible-auto-start / human-gate machinery; surfaced text is `guard.Wrap`'d (I7) and unsigned requests are rejected + logged metadata-only (I5). Wired via `nilcore serve --webhook <addr>`; headless ⇒ irreversible work deny-defaults.

### P9-T05 — Gated PR/push action (`GateAction` + forge)
- **Goal:** let a converged, verified change become a **draft PR** — only through the human gate, never autonomously.
- **Depends on:** —  **Owns:** `internal/policy/`, `internal/forge/`
- **Acceptance criteria:**
  - `policy.GateAction` gains a closed-set `OpenPR` (and/or reuse `Push`) `GateActionType`; `Class()` is `Irreversible`; `GateStructured` consults the approver and a **nil approver default-denies**.
  - `internal/forge` performs the push + draft-PR open **only after** gate approval — host-side hardened git (`HardenArgs`/`HardenedEnv`) and/or the SCM REST API over stdlib `net/http`, token from `secrets.SecretStore` (I3, never logged / model-visible); **never auto-merges**.
  - Prefer the structured action over free-text `policy.Classify`; add the SCM API host to egress where the host harness needs it.
- **Verify:** `make verify`; tests — approve ⇒ forge invoked with the expected push/PR shape (mocked transport); deny ⇒ no call; nil approver ⇒ deny; the token never appears in logs.
- **Notes:** push/merge are already `Irreversible` in `policy.Classify`; this is **harness code performing a gated irreversible action** (like the integrator's gated promotion, which itself never lands). The git **tool** stays `status|diff|add|commit|log` only. Rationale: `docs/UPGRADE-PATH.md` §4 (U1-T05).
- **Status (shipped):** this is the deferred **D4** item, merged. `internal/forge` opens a **draft** PR **only after** the human gate (the push runs inside the approved prepare step; token from `SecretStore` via `NILCORE_FORGE_TOKEN`, I3 — never logged, never given to the model, credentials scrubbed from logs); the agent **never** merges. Reached via `nilcore watch --open-pr` / `schedule --open-pr`; an opt-in nil-gated orchestrator `KeepBranch` preserves the verified branch, and the default disposable cleanup is byte-identical.

### P9-T06 — Cron / scheduled trigger source
- **Goal:** time-driven autonomy — a maintenance goal that fires on a schedule — built on the existing trigger + gate, pure stdlib.
- **Depends on:** —  **Owns:** `internal/cron/`
- **Acceptance criteria:**
  - A stdlib `time`-based scheduler that emits a `trigger.Signal` into `trigger.Handle` at configured times/intervals.
  - Reversible scheduled work auto-starts; irreversible scheduled work **deny-defaults and blocks** under an unattended approver (the documented headless posture); a fire logs a metadata-only event (I5); pure stdlib (I6).
- **Verify:** `make verify`; tests with an injected clock — a due spec fires the Goal; reversible auto-starts; irreversible under a nil/deny approver does not start and is logged.
- **Notes:** distinct from `internal/scheduler` (a time-agnostic bounded-concurrency pool) and `internal/loopctl` (a cancel-cause discriminator, not a scheduler). Rationale: `docs/UPGRADE-PATH.md` §4 (U1-T06).
- **Status (shipped):** `internal/cron` (pure stdlib `time`, no new module) emits a `trigger.Signal` on cron/interval schedules and routes through the **existing** trigger + gate; reversible scheduled work auto-starts, irreversible work deny-defaults and blocks under the unattended approver, and each fire logs a metadata-only event (I5). Wired via `nilcore schedule` (cron/interval self-start).

### P9-T07 — Tier-1 CLI wiring
- **Goal:** wire the Phase-9 feature packages into the binary — the single `cmd/nilcore` integration step.
- **Depends on:** P9-T02, P9-T03, P9-T04, P9-T05, P9-T06  **Owns:** `cmd/nilcore/`
- **Acceptance criteria:**
  - Register the browser tool (P9-T02) into `loopTools()`/`readOnlyLoopTools()`, container-and-egress-gated (mirror the web-tool wiring).
  - Construct `verify.Composite` with `BrowserVerifier` (P9-T03) at the per-worktree verifier sites when behavioral verification is enabled (env/flag; default off ⇒ byte-identical).
  - `nilcore serve --webhook <addr>` stands up the SCM/CI intake (P9-T04); `nilcore schedule` runs the cron source (P9-T06); a trigger-originated, verified, reversible change can offer a **gated** PR via the forge (P9-T05) when a channel gate is configured, else deny-default.
  - Every new path nil/flag-gated; the default binary is byte-identical with Phase-9 features off.
- **Verify:** `make verify`; CLI smoke tests (fake channel + fake orchestrator) — webhook intake dispatches; `schedule` self-starts a reversible task; browser-verify off ⇒ identical verdict path.
- **Notes:** `cmd/nilcore/` is a shared wiring surface — this task serializes against any other open `cmd/nilcore`-owning task. Rationale: `docs/UPGRADE-PATH.md` §4 (U1-T07).
- **Status (shipped):** the Phase-9 packages are wired into the binary — the `browser_view` tool registered into the loop tool sets (container-and-egress-gated), the composite `BrowserVerifier` constructed at the per-worktree verifier sites when behavioral verification is enabled, plus the new `nilcore serve --webhook <addr>` and `nilcore schedule` entry points and the `watch/schedule --open-pr` gated forge path. Every path is nil/flag-gated; the default binary is byte-identical with Phase-9 features off.

---

## Phase 10 — Context depth, trusted steering & distribution

Three philosophy-consistent upgrades, none touching the frozen `backend.CodingBackend` contract, all nil/flag-gated: give the operator an **authoritative steering file** (the AGENTS.md/CLAUDE.md convention) as a new *trusted* input class without weakening I7; **activate and scale** the semantic index that is built-but-unwired today, staying CGO-free (I6); and turn the existing skills/MCP primitives into a **versioned, verified-install registry**. Promoted from `docs/UPGRADE-PATH.md` Tier 2 (`U2-T01..07` → `P10-T01..07`, same order); that file holds the deep rationale and file:line sourcing.

> **Status (shipped):** Phase 10 is complete and merged. The authoritative steering-file loader + trusted injection seam (P10-T01) and its principal-only front-door plumbing (P10-T02), the provider-backed Embedder (P10-T03), the pure-Go HNSW semantic index (P10-T04), the multi-language AST + broadened live index (P10-T05), and the versioned skills/MCP registry (P10-T06) are all wired into the binary (P10-T07). New packages: `internal/{steering, embed, registry}`; `internal/codeintel/semantic` gained `hnsw.go` and `internal/codeintel/ast` gained `go.go` + `python.go`. New command `nilcore registry list|install <manifest.json>`. Steering (`NILCORE.md` / `AGENTS.md`) is the deliberate, scoped I7 exception, bounded **below** the safety core — it cannot widen capability or bypass the gate/verifier. The semantic path activates opt-in via `NILCORE_EMBED_KEY` / `NILCORE_EMBED_MODEL`; off ⇒ lexical fallback, byte-identical. **No new module** was added (HNSW, the Python parser, and the embedder are pure stdlib); I6 holds, `CGO_ENABLED=0` across the release matrix. Remote registry fetch stays **gated** as `EXT-07`. I1–I7 hold unchanged.

### P10-T01 — Authoritative steering-file loader + trusted injection seam
- **Goal:** let an operator commit a project steering file whose contents are treated as **authoritative instructions** — the deliberate, scoped exception to I7 — distinct from fenced background memory.
- **Depends on:** —  **Owns:** `internal/steering/`, `internal/backend/`
- **Acceptance criteria:**
  - `internal/steering` parses an operator file into authoritative text. A new **nil-gated** `SteeringContext func(ctx) string` field on `backend.Native` (mirroring `MemoryContext`) injects it **un-`guard.Wrap`'d**, prepended ahead of the goal turn (styled like the trusted `modePreamble`, **not** memory's `"NOT instructions"` label).
  - `nil ⇒ byte-identical` loop; the seam declares no new imports into `backend` (func field only), preserving its leaf import graph.
  - **Tested hard limits:** the steering file cannot widen capability (tools/shell remain a property of `capabilityForMode` wiring, not the prompt), cannot bypass the gate or verifier (I2/I3), and is never parsed for control verbs.
  - This task **does not modify `internal/backend/backend.go`** (the frozen contract) even though it owns the `internal/backend/` directory — it adds only a nil-gated optional field on `Native` (the `MemoryContext`/`LiveSession` precedent).
- **Verify:** `make verify`; tests — steering text is prepended un-wrapped and authoritative; nil seam ⇒ byte-identical; a steering file containing `/execute` or a tool grant does **not** flip mode or add a tool.
- **Notes:** a **new trusted-input class** (operator-authored ⇒ authoritative); there is no steering loader today. It sits *below* the invariants — "behavior never overrides the safety core" (`docs/PERSONA.md`). Rationale: `docs/UPGRADE-PATH.md` §5 (U2-T01).
- **Status (shipped):** `internal/steering` loads an operator project steering file (`NILCORE.md` / `AGENTS.md`) as **trusted** instructions through a nil-gated `SteeringContext` seam on `backend.Native` — the deliberate, scoped I7 exception, bounded **below** the safety core (it cannot widen capability or bypass the gate/verifier). `backend.go` (the frozen contract) is untouched. Nil ⇒ byte-identical.

### P10-T02 — Steering front-door plumbing (principal-only, persisted)
- **Goal:** load the steering file **once at launch from principal/operator origin** and thread it through the drive like `Mode` and read-roots — never from untrusted text.
- **Depends on:** P10-T01  **Owns:** `internal/session/`
- **Acceptance criteria:**
  - Discovery + load at launch (principal context), carried on `WorkState`/`DriveInput` captured-at-launch, like `Mode`/`ReadRoots`; a posture reference round-trips through the persistence snapshot; a missing file ⇒ byte-identical.
  - A guard test mirroring `TestTurnTextDoesNotFlipMode`: steering is set/loaded **only** via the principal front door (post-`channel.Authorized.Permit`), never from `Turn` text, an inbox follow-up, or tool/web output.
- **Verify:** `make verify`; tests — operator file at repo root loads as authoritative; absent ⇒ byte-identical; the principal-only guard test passes.
- **Notes:** the I7-enforcement half of the steering feature; the loader (P10-T01) is inert until wired here and at the cmd layer (P10-T07). Rationale: `docs/UPGRADE-PATH.md` §5 (U2-T02).
- **Status (shipped):** the steering file is loaded **once at launch from principal/operator origin** and threaded through the drive (and the persistence snapshot) like `Mode` and read-roots — never from `Turn` text, an inbox follow-up, or tool/web output (I7). Wired into chat + run/build. A missing file ⇒ byte-identical.

### P10-T03 — Provider-backed Embedder
- **Goal:** supply a real `semantic.Embedder` so the dormant vector path can be turned on — closing dead code (no Embedder implementation exists today; `semantic.Open` has zero non-test callers).
- **Depends on:** —  **Owns:** `internal/embed/`
- **Acceptance criteria:**
  - An `internal/embed` type implementing `semantic.Embedder` (`Embed(ctx, text) ([]float32, error)`) via a model embeddings endpoint through the existing provider/cred seam (`provider.ResolveWith` + injected `getenv`; key via `SecretStore`, I3).
  - Stdlib HTTP only (I6); egress to the model API host (container backend + allowlist); a resolve/credential failure degrades cleanly (caller falls back to the nil-Embedder lexical mode).
- **Verify:** `make verify`; a mocked-transport test asserts the embeddings request/response shape + `[]float32` decode; a no-key path returns a clean error.
- **Notes:** the Embedder is an argument to `semantic.Open(path, e)` — there is no `Retriever.Embedder` field (the Retriever is `{Graph, Semantic, LSP}`). Rationale: `docs/UPGRADE-PATH.md` §5 (U2-T03).
- **Status (shipped):** this is part of the deferred **D2** item, merged. `internal/embed` implements `semantic.Embedder` over an OpenAI-compatible embeddings endpoint through the existing provider/cred seam (key via `SecretStore`: `NILCORE_EMBED_KEY` / `NILCORE_EMBED_MODEL`, I3); stdlib HTTP only (I6). A resolve/credential failure degrades cleanly to the nil-Embedder lexical mode.

### P10-T04 — Pure-Go ANN/HNSW semantic index
- **Goal:** replace the brute-force linear cosine scan with a pure-Go approximate-nearest-neighbour index so retrieval scales — **without** breaking `CGO_ENABLED=0`.
- **Depends on:** —  **Owns:** `internal/codeintel/semantic/`
- **Acceptance criteria:**
  - Replace `searchVector`'s `SELECT … WHERE vec IS NOT NULL` + per-row Go cosine with a pure-Go HNSW (or equivalent). Vectors stay in SQLite (`modernc.org/sqlite`) or a pure-Go on-disk structure — **never** a C-backed lib (FAISS/hnswlib/`sqlite-vec` are cgo and break the release matrix, I6).
  - Preserve the contracts: the `Embedder` seam, the nil-Embedder lexical fallback, and `Add`'s upsert semantics.
  - If a pure-Go ANN module is added it is a `go.mod` change → this task carries **contract (go.mod)**, runs as the dedicated go.mod task, and its CHANGELOG entry includes the I6 dependency justification. Prefer a hand-rolled pure-Go HNSW in-package to keep the dependency count at three.
- **Verify:** `make verify`; a recall/latency test (ANN vs the old linear scan on a fixture corpus); a `CGO_ENABLED=0 GOOS=linux/darwin` cross-compile check.
- **Notes:** today's store is JSON-encoded vectors in one SQLite TEXT column "so the build stays cgo-free"; the replacement inherits that. The semantic lens slots into the fixed `Retrieve` fusion order + closed provenance vocabulary. Rationale: `docs/UPGRADE-PATH.md` §5 (U2-T04).
- **Status (shipped):** part of the deferred **D2** item, merged. `internal/codeintel/semantic/hnsw.go` replaces the brute-force linear cosine scan with a **content-hash-cached, pure-Go HNSW** vector index — **no new module** (hand-rolled in-package, so the dependency count stays at three; FAISS/hnswlib/`sqlite-vec` rejected as cgo). The Embedder seam, the nil-Embedder lexical fallback, and `Add`'s upsert semantics are preserved; `CGO_ENABLED=0` cross-compiles hold (I6).

### P10-T05 — Multi-language AST + broaden live index
- **Goal:** lift code intelligence beyond Go — the live index seeds `.go` files only today — so non-Go repos get structural context.
- **Depends on:** —  **Owns:** `internal/codeintel/ast/`, `internal/codeintel/live/`
- **Acceptance criteria:**
  - Add a multi-language backend behind the **already-named** stable seam (the `ast.go` scope note reserves "a tree-sitter backend … slots in behind it later without changing callers (kept out now to preserve the zero-cgo build)"). It must be **pure-Go or wasm** — common tree-sitter Go bindings are cgo and break I6; a `go.mod` addition carries **contract (go.mod)** + justification.
  - Broaden the two `.go`-suffix gates (`live.IndexDir`, and the standalone tool walk wired in P10-T07) to the supported language set.
  - **Preserve** `graph.BuildFile`'s REPLACE-on-rebuild-per-file atomicity so the incremental live index never leaks stale nodes/edges.
- **Verify:** `make verify`; a second-language fixture repo indexes into the graph; the live re-index of an edited non-Go file replaces only that file's contribution; `CGO_ENABLED=0` still builds.
- **Notes:** the live session is opt-in via `NILCORE_LIVE_INDEX`, task-scoped, in-memory; this broadens *what* it parses, not the lifecycle. Rationale: `docs/UPGRADE-PATH.md` §5 (U2-T05).
- **Status (shipped):** the deferred **D3** item, merged — and **broadened by R2**. A language-parser seam plus **pure-Go Python, TypeScript/JavaScript, and Rust backends** (`internal/codeintel/ast/{python.go,js.go,rust.go}` alongside `go.go`; CGO-free, **not** tree-sitter — Go/Python output identical; the JS/TS and Rust backends are heuristic scanners, with the LSP seam the precise lens) now back the parser; the live + codeintel index walks cover Go, Python, JS/TS, and Rust (nine extensions) via `ast.SupportedExtensions`. `graph.BuildFile`'s REPLACE-on-rebuild-per-file atomicity is preserved; `CGO_ENABLED=0` still builds (I6).

### P10-T06 — Versioned skills/MCP-server registry
- **Goal:** turn the operator-only, local skills + MCP primitives into a **versioned, shareable, verified-install** registry — distribution without an editor surface — preserving every trust property.
- **Depends on:** —  **Owns:** `internal/skills/`, `internal/mcp/`, `internal/registry/`
- **Acceptance criteria:**
  - A version/manifest layer: `skills.Skill` and `mcp.ServerSpec` gain version metadata; `internal/registry` reads a local manifest/lockfile and installs into the existing discovery dirs (`$NILCORE_SKILLS_DIR` / `mcp.json`).
  - **Trust preserved:** MCP servers stay operator-configured-not-model-emitted; wrappers stay deterministically schema-generated; MCP authorization stays at the codegen-descriptor boundary + the untrusted-output fence (I7) are unchanged; an installed skill is still a `skill_<name>` tool that only returns instructions.
  - **Self-edit boundary preserved:** any registry-driven manifest change routes through `selfimprove.Flow` (scope-check → verified task → human gate → merge); **remote fetch is out of scope** (it is `EXT-07`).
  - Stdlib only; no remote/network fetch in this task (I6).
- **Verify:** `make verify`; tests — a local manifest installs a versioned skill that surfaces as a tool; a duplicate/older version is handled; an out-of-scope self-edit is rejected.
- **Notes:** there is no registry/packaging/versioning/install today (`skills.Registry` is an in-memory holder). Self-improvement stays "prompts/skills/tools only, never the core, gated" (`docs/ARCHITECTURE.md`). Rationale: `docs/UPGRADE-PATH.md` §5 (U2-T06).
- **Status (shipped):** `internal/registry` plus version metadata on `skills.Skill` and `mcp.ServerSpec` back `nilcore registry list|install <manifest.json>` over **local** skills (stdlib only, no network). Trust is preserved: MCP servers stay operator-configured, wrappers stay schema-generated, the per-tool gate + the untrusted-output fence (I7) are unchanged, and an installed skill is still a `skill_<name>` tool that only returns instructions. **Remote fetch stays gated as `EXT-07`** (external-infra).

### P10-T07 — Tier-2 CLI wiring
- **Goal:** activate Tier-2 features in the binary — register the default Embedder + Semantic into the Retriever, discover the steering file at launch, enable the multi-language live index, expose the registry install command.
- **Depends on:** P10-T02, P10-T03, P10-T04, P10-T05, P10-T06  **Owns:** `cmd/nilcore/`, `internal/tools/`
- **Acceptance criteria:**
  - Construct `semantic.Open` with the P10-T03 Embedder and set `retrieve.Retriever.Semantic` in `internal/tools/codeintel.go` (today literally `&retrieve.Retriever{Graph: g} // Semantic nil`) — so the vector lens is **on by default** when a key resolves, degrading to lexical otherwise.
  - Discover + thread the steering file at launch (P10-T01/T02); broaden the standalone tool's `.go`-only walk for P10-T05; add `nilcore registry install/list` for P10-T06.
  - Every path nil/flag-gated; default binary byte-identical when features are off/unconfigured.
- **Verify:** `make verify`; tests — with a key, retrieval uses the semantic lens (provenance `semantic`); without, lexical fallback; steering discovered at repo root; registry install round-trip.
- **Notes:** `cmd/nilcore/` and `internal/tools/` are shared surfaces — serialize against P9-T07 (cmd) and P9-T02 (tools). Rationale: `docs/UPGRADE-PATH.md` §5 (U2-T07).
- **Status (shipped):** the Tier-2 features are activated in the binary — the default Embedder + Semantic lens set on the Retriever (on by default when `NILCORE_EMBED_KEY` resolves, lexical otherwise), the steering file discovered + threaded at launch, the multi-language live index enabled, and `nilcore registry install/list` exposed. Every path is nil/flag-gated; the default binary is byte-identical when features are off/unconfigured.

---

## Phase 11 — NilCore as a verifier-backed artifact factory

Make **code one artifact type among many**: reports, comparison matrices, audits, benchmarks, migration plans, release notes, and research dossiers become first-class outputs, each carrying **machine-verifiable acceptance criteria**. Every claim/number/cell rides with provenance `{value, source_url, retrieved_at, extraction_method, verifier, status}`, and an artifact is **GREEN only because every claim passed a runnable check** — a status **produced by a verifier** (I2), never self-claimed. The full per-task specs, the spine data model (the Go type shapes), the wave plan, and the I1–I7 proof live in **`docs/ROADMAP-EVIDENCE-ARTIFACTS.md`** (the staging doc, house pattern); this section is the canonical promotion (`P11-T36`). Every task is **additive, opt-in, flag/env-gated, stdlib-only** (`go.mod` untouched, `CGO_ENABLED=0`) — **the default binary is byte-identical when the features are off.**

> **Status (shipped):** Phase 11 is complete and merged. The upgrade routes **around** the frozen `backend.CodingBackend` contract (I1): a typed `artifact.Artifact{Claims[]}` (each `Claim` carrying an `Evidence{value, source_url, retrieved_at, extraction_method, verifier, status}`) rides **out-of-band** as a worktree JSON file at `.nilcore/artifacts/<id>.json`, never on `backend.Result`. Artifact-GREEN is produced by `evverify.ArtifactVerifier` — a `verify.Verifier` folded into `verify.Composite` **after** the build verifier, so any red claim turns the whole verdict red and a worker's self-written `Status=pass` is overwritten by a real `CheckFunc` run in the sandbox box (I2/I4; unregistered id ⇒ `Unverifiable`, never `Pass`; staleness can only DEMOTE, never PASS on a model timestamp). Reusable **domain verifier packs** (`web`/`software`/`finance`/`ui`) register namespaced verifier-ids into the spine `Registry`, reaching live data only via `box.Exec`/`ExecWithEnv` under the egress allowlist with the body parsed host-side as trusted Go — curl-in-box + `encoding/json`, **no go-github / SEC / finance / markdown SDK** (I6); keyed packs inject `$NAME` from the `SecretStore`, and the persisted `SourceURL` stays key-free (I3). **Typed worker results** land on an additive `spawn.Result.Artifact` field (prose stays `guard.Wrap`-fenced in `super.renderReport`; the trusted surface carries only id/field/status — I7). **Granular requeue** (`internal/requeue`) re-dispatches exactly the failed claims through the existing `spawn.DAGScheduler` + `continue_from`, bounded by a per-unit ledger, flipping green only on a fresh verifier re-run. **Research egress profiles** (`internal/egressprofile`: `finance`/`docs`/`web-research` + a project-local `.nilcore/egress.json`) keep default-deny absolute — a profile widens the *tree* while `roster.EgressFor` still intersects each role narrow-only (R9). A read-only **`nilcore report`** verification UI replays the append-only log into a text/HTML/markdown report (passed/failed checks, the per-claim `{value,source_url,verifier,status}` table, retry history, final clean pass) and refuses GREEN over a broken hash-chain (I5). New leaf packages: `internal/{worktreefs, browserwire, artifact, artifact/evverify, artifact/packs/{web,software,finance,ui}, requeue, egressprofile, report, report/render}`; modified: `internal/{spawn, roster, super, onboard, tools}` + `cmd/nilcore`. Env gates: `NILCORE_EVIDENCE_VERIFY`, `NILCORE_VERIFY_PACKS`, `NILCORE_REQUEUE`, `NILCORE_EGRESS_PROFILE` / `-egress-profile`, `NILCORE_EVIDENCE_MAX_AGE`. The six per-pillar staging-doc stubs (`P11-T06/T13/T18/T24/T29/T34`) were **consolidated** into the single `docs/ROADMAP-EVIDENCE-ARTIFACTS.md`. `make verify` green; `go.mod`/`go.sum` unchanged; I1–I7 hold.

---

## Phase 12 — Verified swarm mode

A first-class high-throughput surface — `nilcore swarm --goal … --agents N --concurrency K
--preset P --verify-pack … --passes until-clean --budget D` — that fans N units of work into a
**bounded in-process pool on one host**, where every unit produces a **typed, checkable artifact**
judged by a **verifier per unit**, requeues **only failed shards** until clean (or a budget / pass
limit), folds verifier-green work through the **serial Integrator** (which never lands to base), and
renders a **verification scoreboard**. Built **on top of** the Phase-11 artifact spine — it
**reuses/extends** `internal/{artifact, artifact/evverify, artifact/packs/*, requeue, report}`, never
rebuilds them. Full design + per-task specs: **`docs/SWARM.md`** (`SW-T01..T18`). It is the
single-host projection of a fleet; multi-host shard dispatch / cross-host task-state / a remote
control plane crosses into `EXT-01` and stays out of scope.

> **Status (shipped):** Phase 12 is complete and merged. New leaf packages: `internal/pool`
> (tiered/capped/failover/metered provider pool over `meter`/`strongcap`/`model.Resilient`),
> `internal/artifact/schema` (structural validation + `SchemaVerifier`), `internal/artifact/packs/
> {audit,benchmark,code}` (the three new verify-packs + `packs.Build` assembler, fail-closed on an
> unknown pack), `internal/swarm` (Shard + durable Queue + Sharder + Runner + multi-pass Controller),
> `internal/swarm/board` (the live verifier-driven scoreboard, `//go:build tui` Charm dashboard linking
> zero Charm by default), `internal/swarm/preset` (the five named bundles). Additive: `internal/report`
> (source/claim-trace + `RenderMatrix` + redacted json), `internal/roster` (`RoleAuditor`/`RoleUI`),
> `internal/onboard` (`Config.Pool`). New command `nilcore swarm`; `nilcore report` gains
> `--format json|matrix` + `--dir`. The default binary is **byte-identical** when `swarm` is unused
> (one new dispatch arm; no existing package imports a swarm/pool leaf). The seven invariants hold by
> **reuse, not new mechanism**: I2 the `evverify.ArtifactVerifier` is the sole ship gate (per-shard +
> Integrator re-verify; `ShipGate` refuses `verify.Pass`/nil; unknown pack ⇒ fail-closed); I1 the
> artifact is a worktree file read by the verifier (`backend.CodingBackend` untouched); I3 keys via the
> SecretStore, never to a decorator/log/render; I4 each shard in its own sandbox box; I6 stdlib only
> (`go.mod` unchanged, `CGO_ENABLED=0`); I7 model-authored fields stay data. `make verify` green;
> `golangci-lint` 0 issues. The Integrator never lands to base (promote-to-base is one gated
> `policy.GateAction{PromoteToBase}`, nil approver default-denies).

---

## Phase 15 — OpenAI / OpenRouter / generic-endpoint provider upgrade

Bring the `openai` / `openrouter` adapters and a **new generic `openai-compatible` vendor** to
state-of-the-art (configurable endpoints, reasoning controls, structured outputs, prompt-cache accounting,
OpenRouter provider-routing, web search) **without touching the frozen `backend.CodingBackend` contract,
without a new `go.mod` dependency (I6), keeping the default binary byte-identical until opted in.** Full
design + feature matrix + rollout in [`docs/ROADMAP-PROVIDERS.md`](ROADMAP-PROVIDERS.md).

> **Status / scope.** OpenAI Responses-API migration is **deferred** (OpenRouter + compat are
> Chat-Completions-only; we keep Chat Completions as the portable baseline). Batch API / `GET /generation`
> are **out of scope** (don't fit the synchronous `Provider.Complete` seam). The web-search tasks
> (P15-T07/T08/T09) extend `model.BuiltinTool` and carry a **cross-phase dependency** on that seam being in
> `main` (it lands with the computer-use work); the rest is independent. Invariants hold by **reuse**: I1 the
> `Provider.Complete` signature and `backend.CodingBackend` are untouched; I3 keys via env-name/SecretStore,
> per-request header only, never logged — and a compat endpoint must use a **dedicated `NILCORE_COMPAT_API_KEY`**
> (a canonical vendor key on a compat BaseURL is rejected with a key-free error); I4 the client-side web search
> stays sandboxed; I6 stdlib only; I7 provider-returned web-search citations are `guard.Wrap`'d untrusted DATA,
> never a text block, never via `emitReasoning`.

| ID | Phase | Title | Depends on | Owns | Note |
|---|---|---|---|---|---|
| P15-T01 | 15 | Configurable BaseURL + Auth + options constructor | — | `internal/provider/openai.go` (+`openai_options_test.go`) | foundation; ∥ P15-T03, P15-T12 |
| P15-T02 | 15 | `openai-compatible` vendor + dedicated-key validation | P15-T01 | `internal/provider/provider.go` (+test) | |
| P15-T03 | 15 | Usage widening + `model.APIError` + resilience | — | `internal/model/{model,apierror,resilience}.go` (+tests), `docs/ARCHITECTURE.md` | **serialised seam**; 2 proof gates |
| P15-T04 | 15 | `max_tokens` single-key marshal (REPLACE) | P15-T01 | `internal/provider/openai_maxtokens.go` (+test) | correctness blocker for gpt-5.x |
| P15-T05 | 15 | SOTA request fields + widened usage/model decode | P15-T01, P15-T03, P15-T04 | `internal/provider/openai.go` (+tests) | sole `openai.go` owner this wave |
| P15-T06 | 15 | OpenRouter typed extras (routing, `models[]`, transforms, headers) | P15-T05 | `internal/provider/openrouter_extras.go` (+test) | |
| P15-T07 | 15 | Web-search `BuiltinTool` variant + adapter render | P15-T05 | `internal/model/builtin.go`, `internal/provider/openai_websearch.go` (+tests) | needs `BuiltinTool` in `main` |
| P15-T08 | 15 | I7 fence: `web_search_result` block + `guard.Wrap` + native.go handler | P15-T05, P15-T07 | `internal/model/model.go`, `internal/provider/openai.go`, `internal/backend/native.go` (+tests) | solo wave |
| P15-T09 | 15 | Exactly-one-web-tool capability switch | P15-T07, P15-T08 | `cmd/nilcore/webcap.go` (+test) | mutual-exclusion dispatch invariant |
| P15-T10 | 15 | Onboarding config + wizard for compat vendor | P15-T02 | `internal/onboard/` (+tests) | |
| P15-T11 | 15 | Metering/pricing for new ids + authoritative `usage.cost` | P15-T03 | `internal/meter/pricer.go` (+test) | |
| P15-T12 | 15 | Egress allowlist extensibility (sandbox only) | — | `internal/policy/egress.go` (+test) | small; ∥ Wave 1 |
| P15-T13 | 15 | Eval coverage (compat, reasoning, structured, native search) | P15-T06, P15-T08, P15-T09 | `eval/provider-compat/` | hermetic httptest fixtures |
| P15-T14 | 15 | Docs: PREREQUISITES + ARCHITECTURE + TASKS specs | P15-T02, P15-T03, P15-T06, P15-T08, P15-T09, P15-T10, P15-T11, P15-T12 | `docs/PREREQUISITES.md`, `docs/ARCHITECTURE.md`, `docs/TASKS.md` | **contract**, serialised, last |

**Waves (Owns disjoint within each; deps in strictly earlier waves):**
W1 `T01·T03·T12` → W2 `T02·T04` → W3 `T05` → W4 `T06·T07·T10·T11` → W5 `T08` → W6 `T09` → W7 `T13` → W8 `T14`.

### P15-T01 — Configurable BaseURL + Auth descriptor + functional-options constructor
`NewOpenAICompatible(model, opts…)` with `WithBaseURL`/`WithAuth`/`WithMaxTokensField`; `NewOpenAI`/`NewOpenRouter`
keep exact defaults. BaseURL is the full prefix (append only `/chat/completions` after `TrimRight`, no auto `/v1`).
Auth header emitted only when `key != ""` (Bearer default · Azure `api-key` · None). Default options ⇒ request body
+ headers byte-identical (httptest baseline). Stdlib only; key in header only (I3). `make verify` green.

### P15-T02 — `openai-compatible` vendor + dedicated key-env validation
`ResolveWith` gains a `openai-compatible`/`compat` case building via `NewOpenAICompatible` from config-injected
BaseURL/Auth/MaxTokensField + key-env name; `split()` unchanged (spec `openai-compatible:<modelID>` preserves `:`/`/`).
Key-env defaults to `NILCORE_COMPAT_API_KEY`; a canonical vendor key name on a compat endpoint returns a key-free error
(I3). Three existing vendor cases byte-identical. `make verify` green.

### P15-T03 — Usage widening + `model.APIError` + resilience terminal/Retry-After (serialised seam)
Additive `model.Usage{ReasoningTokens,CachedTokens,CostUSD}`; new `model.APIError{StatusCode,Retryable,RetryAfter,
Type,Code,Message}` (key-free `Error()`); `resilience.go` via `errors.As` stops terminal `4xx` (no failover), retries
`429/5xx`, honours `Retry-After` as floor. **Proof gate 1:** untyped error retries exactly as today. **Proof gate 2:**
terminal `APIError` stops, `429` honours `Retry-After`. `Provider.Complete` signature untouched (I1). `docs/ARCHITECTURE.md`
updated in this PR. `make verify` green.

### P15-T04 — `max_tokens` single-key marshal (REPLACE not ADD)
One `MaxTokens int` marshalled into exactly one configured key (`max_tokens` default; `max_completion_tokens` for
reasoning models); emitting both is structurally impossible (test decodes the body and asserts exactly one key).
Default ⇒ byte-identical. `make verify` green.

### P15-T05 — SOTA request fields + widened usage/model decode
Additive `omitempty`: `reasoning_effort`, `response_format` (json_schema strict), `parallel_tool_calls` (`*bool`),
`tool_choice`, `service_tier`, optional `prompt_cache_key`. Decode reads `completion_tokens_details.reasoning_tokens`,
`prompt_tokens_details.cached_tokens`, `usage.cost`, served top-level `model`. Every field zero ⇒ body byte-identical
(extend `TestNormalToolByteIdentical`). `make verify` green.

### P15-T06 — OpenRouter typed extras (provider routing · models[] · transforms · attribution headers)
Typed `openRouterExtras` (provider `{order,allow_fallbacks,require_parameters,data_collection,zdr,sort,max_price}`,
`models[]`, `reasoning`, transforms/plugins) merged only on the OpenRouter base; `require_parameters:true` default;
`HTTP-Referer`/`X-Title` from static config (never the key, I3). Decode prefers authoritative `usage.cost` + served
`model`. Zero/ non-OR base ⇒ byte-identical. `make verify` green.

### P15-T07 — Web-search `BuiltinTool` variant + adapter render (kept OUT of generic tools[])
Additive `BuiltinTool` web-search fields + constructors; `Tool.MarshalJSON` unchanged. Adapter filters the web-search
`Builtin` out of the generic `tools[]` slice and renders it directly (OpenRouter entry / OpenAI `web_search_options`,
search-capable models only). `TestNormalToolByteIdentical` covers nil-Builtin AND non-web Builtin. nil ⇒ byte-identical.
**Depends on the `model.BuiltinTool` seam being in `main`.** `make verify` green.

### P15-T08 — I7 fence: `web_search_result` block + `guard.Wrap` at decode + native.go re-entry
Additive `Block{Type:"web_search_result"}` carrying the `guard.Wrap`'d citation payload; decoders never produce a text
block from citations; `native.go` runs `guard.Suspicious` + `injection_flagged` and appends as fenced DATA only (never
via `emitReasoning`; `textBlocks` drops it). No double-fence. Injection-string fixture proves it. `make verify` green.

### P15-T09 — Exactly-one-web-tool capability switch
`resolveWeb` in cmd selects exactly one path: native (server-side `BuiltinTool`, citations) or the sandboxed client-side
`web_search` `model.Tool` — advertised as dispatchable ONLY on the client path, so the model never emits an orphan
`web_search` tool_use. Default (no web) ⇒ byte-identical. `make verify` green.

### P15-T10 — Onboarding config + wizard for compat vendor and provider knobs
`validProviders` gains `openai-compatible`; additive `omitempty` config (BaseURL, AuthScheme, MaxTokensField,
`CompatKeyEnv` default `NILCORE_COMPAT_API_KEY`, OR Referer/Title, default reasoning effort, routing prefs); bump
`CurrentConfigVersion` + `Migrate`. Wizard surfaces the dedicated-key requirement; never echoes/persists the secret
value (I3). Absent fields ⇒ byte-identical. `make verify` green.

### P15-T11 — Metering + pricing for new model ids and authoritative cost
`pricer.go` adds prefix rate/window entries for newly reachable ids; unknown ids keep the conservative floor. Prefers
OpenRouter's authoritative `usage.cost`; bills `cached_tokens` at the reduced input rate; accounts `reasoning_tokens`
(uses T03's `Usage` fields). Existing ids unchanged (regression test). `make verify` green.

### P15-T12 — Egress allowlist extensibility for sandbox-reachable custom endpoints
`policy/egress.go` gains a config-extensible host list (`EgressWith(extra…)`); `DefaultEgress` unchanged with no extras. **Status:** `EgressWith` shipped in #61 but was **pruned as unused in #98** — `policy.DefaultEgress` remains, and operator egress extension is now via egress profiles + `-egress-allow` flags (e.g. a swarm shard applies its derived allowlist through the per-shard allowlist proxy).
Doc note: egress governs the SANDBOX only, not the host-side `model.Provider` call (a host-only base-URL change needs no
egress edit). `make verify` green.

### P15-T13 — Eval coverage for compat endpoints, reasoning, structured output, native search
`eval/provider-compat/` fixtures exercise a generic compat endpoint, `max_completion_tokens` reasoning model,
`json_schema` output, OpenRouter extras, and the native web-search-result fence (assert `injection_flagged` + fenced
data end-to-end). Per-provider `httptest.Server`, golden fixtures, no network. `make verify` green. **Status: NOT built** — no `eval/provider-compat/` directory exists (`eval/` has only `browse`, `desktop`, `self`). Phase-15 web search itself shipped (#65); this eval-coverage task did not, so any "completes Phase 15" claim overstates it until P15-T13 lands.

### P15-T14 — Docs: PREREQUISITES + ARCHITECTURE providers section + Phase-15 specs (contract, serialised)
`PREREQUISITES.md` documents the dedicated compat key-env (hard requirement), auth schemes, BaseURL-as-full-prefix,
tool/vision as per-model capability flags, and egress-governs-sandbox-not-host. `ARCHITECTURE.md` providers section
reflects the new vendor, the web-search `BuiltinTool` variant, the `web_search_result` fenced block, and the
`model.APIError`/resilience semantics (consistent with T03). `docs/TASKS.md` Phase-15 specs finalised. `make verify` green.

---

## Phase 16 — closing the loop on the agent's own evidence

> **Status (shipped — Pillars 1–7, default-off/opt-in/invariant-preserving):** Full plan + per-task specs in [`docs/ROADMAP-CLOSED-LOOP.md`](ROADMAP-CLOSED-LOOP.md); the §0 relaxation decisions are recorded in [`docs/ARCHITECTURE.md`](ARCHITECTURE.md) §"Closed-loop autonomy". NilCore now consumes its own verifier-judged audit trail to route, plan, gate, and improve itself. **Pillar 1 (EXP):** one derived, rebuildable experience projection over the log (`internal/experience` — `Reader`/`OverLog`/`OverStore`/`Projector`, kept warm by an optional `eventlog.Log.OnAppend` hook behind `NILCORE_EXPERIENCE`) + the `internal/capability` descriptor; `nilcore experience`/`capability` read views. **Pillar 2 (RTE):** a nil-safe `agent.TrustOracle` (`NILCORE_TRUST_DEFAULT=1`) that orders/prunes/sizes candidacy by learned per-class evidence — the verifier still judges every race (I2). **Pillar 3 (LRN):** `nilcore lessons` + auto-fold distilled verifier-failure scars into memory (`NILCORE_LESSONS`); a chain-verified content-hash verify cache (`NILCORE_VCACHE`) wired into the build loop. **Pillar 4 (SIF):** the self-improvement flywheel (`nilcore flywheel [--once]` + an optional serve cadence behind `NILCORE_FLYWHEEL`; `docs/ROADMAP-SELF-IMPROVEMENT.md`) — verified + human-gated, never edits the verifier of record; auto-merge is a SEPARATE double opt-in (`NILCORE_SELFIMPROVE_AUTOAPPROVE`). **Pillar 5 (GAA) — the headline:** graduated auto-approval (`internal/graapprove.GradedApprover` wraps the human gate; earned trust + operator envelope) on every surface (run/build/serve/watch/schedule); the preset blast-radius values + the never-main/prod rule are recorded in ARCHITECTURE. **Pillar 6 (BR):** the four-axis `internal/blastbudget` runtime fence (hosts · irreversible · sandbox wall · per-UTC-day $) threaded into the egress proxy, the sandbox, and the gate; `-blast-radius off|tight|standard`; the per-day $ window rebuilds from the log on boot. **Pillar 7 (AUTO):** the autonomy daemon (`internal/autosrc`) + the operator-only standing-objectives backlog (`internal/objective`; `nilcore objective`), folded into `serve` behind `NILCORE_AUTONOMY`. **Cross-cutting (XC-T01..T06):** one $/rate meter, no transitive opt-in, model-blind policy, rebuild-on-boot, the `nilcore auto-approvals` revocation account, and objectives-operator-only — all asserted in code. Every pillar carries a `deps_test.go` leaf guard; the default binary is byte-identical when nothing is opted in; `make verify` + `make tui-verify` green; `go.mod` untouched (I6). **Pillar 8 (UOK — the unified orchestration kernel) — SHIPPED (default-on via `NILCORE_KERNEL`, §0 cutover authorized; `docs/ROADMAP-KERNEL.md`):** `internal/kernel` is one recursive `Run` over `Node`/`Envelope` that the three machines (single-task orchestrator + project loop + swarm) collapse onto — `run`/`build`/`swarm` become presets and the conversational router picks an envelope, not a machine. A pure-stdlib leaf importing no machine (`deps_test`-guarded — I1/I6); the machines inject as `RunFunc`/`Plan`/`Integrate` closures; the kernel never marks a node done (I2), appends nothing (I5), carries only structural data (I3/I7). All four entrypoints (run/build/swarm + the chat/serve drivers) route through `kernel.Run` via the `*ViaKernel` helpers behind `NILCORE_KERNEL` — now **DEFAULT-ON** (escape hatch `NILCORE_KERNEL=0` calls the legacy machine directly, byte-identical) and equivalence-proven (a harness asserts a kernel-routed run is event-for-event identical to legacy). `MaxDepth` defaults to 1 (legacy single-level fan-out); >1 is the new recursive capability. **UOK V2 (`internal/router`; `docs/ROADMAP-KERNEL-V2.md`) — SHIPPED:** the preset router the kernel was designed for — `router.Classify(goal)→run|build|swarm|decompose` + an `Oracle` seam — backing `nilcore do -goal "…"`, one entry that routes a goal to the cheapest fitting preset and dispatches to that proven machine (the agent picks how to work; routing only orders the machine choice — the chosen machine still owns verify/gate/log). The `decompose` preset now **consumes** `kernel.Recursive` (`cmd/nilcore/decompose.go` — split → run each in its own worktree → merge verified branches into one re-verified tip), so the recursive engine is **no longer dormant**: `nilcore decompose` drives it and `router.Preset` includes `Decompose`. The roadmap still records why the *iterative* project-loop is NOT force-fit onto the one-shot recursive engine.

---

## External infrastructure — GATED (not eligible queue tasks)

The remaining gap-closers — managed cloud fleet, full-stack hosting/deploy, in-editor + custom models, remote vector index at scale, enterprise SSO/SCIM/RBAC, central secret distribution, remote skills/MCP registry, Firecracker microVM — are tracked in `docs/ROADMAP-EXTERNAL-INFRA.md` as `EXT-01..08`. They are **deliberately NOT enqueued here.** Each grants the process standing authority (a cloud control plane, a hosting backend, an identity provider, a remote credential store) that the design refuses by default, and each crosses the "one self-hosted Go binary, runs anywhere" identity (`docs/ARCHITECTURE.md`).

**Do not pick these up via the work-selection rule.** Each is blocked behind the explicit thesis gate in `docs/ROADMAP-EXTERNAL-INFRA.md` §0 — a recorded human decision that NilCore's identity may expand toward that capability, which is itself the kind of irreversible, outward-facing action reserved for a human. Only after that gate clears does an `EXT` item become a candidate for promotion into this queue (as its own serialized contract task), and only if it **extends — never bypasses** — I1–I7 (especially I3, no ambient authority: any new standing credential stays scoped, gated, `SecretStore`-held, and never given to the model). The integrator's never-land guarantee and the verifier-as-sole-authority (I2) hold regardless of what runs remotely.

---

## Done-with-everything

When all tasks are merged: tag a release, move `[Unreleased]` in `CHANGELOG.md` into the version section, and NilCore is the agent described in `CLAUDE.md` — a small, verifying, sandboxed, bounded core that plans, parallelizes across three coding backends, remembers across projects, and improves itself under a human gate — and that runs unattended with authorized control, metered budgets, durable resumption, and bounded resources.
