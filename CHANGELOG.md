# Changelog

All performed work across all parallel workstreams, in one place. This is how any agent sees what every other agent has shipped.

Format: [Keep a Changelog](https://keepachangelog.com/), adapted for parallel agents. **Every merged task appends exactly one entry** under `## [Unreleased]`, tagged with its task ID, the files it owned, and its phase. The log is **append-only** — never rewrite history; rebase before merge to resolve trivial append conflicts.

Entry shape:

```
- **<TASK-ID>** — <one-line summary of what shipped>. _Owns:_ <paths>. _(Phase N)_
```

On a release, the maintainer moves the accumulated `[Unreleased]` entries into a new dated, versioned section.

---

## [Unreleased]

- **P0-T02** — Compile & `make verify` green. Initialized the git repo; restructured the flat offline scaffold into the documented tree (`cmd/nilcore` + `internal/{model,backend,agent,sandbox,verify,eventlog,policy}`); unified the project identity on `nilcore` (module, all imports, system prompt, Makefile, env var, relocated `CHANGELOG.md` to the repo root per CLAUDE.md §8); and authored the missing stdlib-only `internal/model` Anthropic Messages API client the native loop drives. `go build`/`go vet`/`go test` all pass; zero external dependencies preserved (invariant I6); no public contract changed (I1). _Owns:_ (whole tree). _(Phase 0)_
- **P0-T01** — CI pipeline. GitHub Actions runs the gate (`make verify`) and `golangci-lint run` on every push to `main` and every PR, with Go build/module caching; lint config (`.golangci.yml`) enables `errcheck`, `govet`, `ineffassign`, `staticcheck`, `gofmt`, `goimports`. Lint is invoked directly in CI (not via the frozen Makefile). _Owns:_ `.github/`, `.golangci.yml`. _(Phase 0)_
- **P0-T03** — Sandbox container image. `images/sandbox/Dockerfile` on a pinned `golang:1.23.4-bookworm` base with `git`, `make`, and the Go toolchain; runs as a non-root `nilcore` user. Documented build/tag commands, the `/work` UID-mapping note (completed in P2-T01), and the Phase-2 recipe for layering the Codex/Claude Code CLIs (P2-T03). Verified: image builds and `git/go/make --version` run inside it. _Owns:_ `images/sandbox/`. _(Phase 0)_
- **P0-T04** — Test fixtures + smoke test. `test/fixtures/failing-go/` is a self-contained module (own `go.mod`, so the gate's `go test ./...` skips it) with one deliberately failing test; `test/smoke/TestNativeLoopConverges` builds the binary, runs the native backend against a throwaway copy of the fixture, and asserts the verifier turns green. Gated behind `ANTHROPIC_API_KEY` + a container runtime + the sandbox image; skips cleanly when any is absent, so `make verify` stays green without secrets. _Owns:_ `test/`. _(Phase 0)_
- **P1-T01** — Worktree manager. `internal/worktree` creates an isolated git worktree on a fresh `task/<ID>` branch off HEAD (`Create`), exposes `Path()`/`Branch()`, and tears both down with an idempotent `Cleanup()` that survives a cancelled task context and partial creates. Unit-tested against a temp git repo (create → assert registered → cleanup → assert gone → idempotent). _Owns:_ `internal/worktree/`. _(Phase 1)_
- **P1-T02** — Orchestrator uses worktrees + injection seams. `agent.Execute` now creates a fresh worktree per task, builds the sandbox/verifier/backend for it via a `NewEnv` factory, runs the backend, re-verifies as the final gate, and always cleans up. Adds no-op default `Router` (`SingleRouter`) and `Spawner` (`NoSpawner`) seams so Phase 3 plugs in routing/subworkers without re-editing `agent/`. `cmd/nilcore` rewired to the factory; the smoke test now `git init`s its fixture. Tested with a fake backend (worktree lifecycle + cleanup) and a fake verifier (self-claim does not decide — I2). _Owns:_ `internal/agent/`. _(Phase 1)_
- **P1-T03** — Approver + gate wiring. `policy.ConsoleApprover` prompts a human on a terminal (default-deny: only `y`/`yes` approves); `agent.Orchestrator.Gate` classifies an action and records the decision to the event log — reversible actions auto-proceed, irreversible ones (merge/push/deploy/pay) require the approver and are denied when none is wired. `cmd/nilcore` wires the console approver. Table-tested: `policy.Gate` (reversible/approve/deny/nil paths), `ConsoleApprover` (input parsing), and `Orchestrator.Gate` (auto vs ask vs deny). _Owns:_ `internal/policy/`, `internal/agent/`. _(Phase 1)_
- **P1-T04** — `Channel` interface (contract). `internal/channel` defines the minimal, transport-agnostic seam every chat transport implements: `Receive` (next task request), `Update` (stream progress), `Ask` (render an irreversible-action gate as a yes/no — the chat form of `policy.Approver`). Registered in `docs/ARCHITECTURE.md`; conformance asserted at compile time with a stub. _Owns:_ `internal/channel/channel.go`. _(Phase 1)_
- **P1-T13** — Cross-platform paths + release matrix. `internal/paths` resolves `ConfigDir`/`DataDir`/`CacheDir` per-OS (XDG on Linux, Application Support on macOS) plus `EnsureDir`, stdlib-only; tested per `GOOS`. `Release` workflow cross-compiles `darwin`/`linux` × `amd64`/`arm64` on tags and attaches binaries to the GitHub Release; `scripts/install.sh` (curl-pipe-sh), `scripts/nilcore.service` (hardened systemd unit), and a Homebrew-tap formula sketch. _Owns:_ `internal/paths/`, `.github/workflows/release.yml`, `scripts/`. _(Phase 1)_

---

## [0.1.0-phase0] — 2026-06-14

Phase 0 baseline — the smallest core that proves the loop converges. Established by the founding scaffold; recorded here as the starting point for all parallel work.

- **P0 (baseline)** — Core agent loop: model proposes a shell action → sandbox executes → verifier judges → repeat until green or budget exhausted. _Owns:_ internal/backend/native.go. _(Phase 0)_
- **P0 (baseline)** — `CodingBackend` contract (`Run(ctx, Task) (Result, error)`) with three implementations behind one seam: `native` (own loop), `codex` (`codex exec --json`), `claude-code` (`claude -p --output-format stream-json`). _Owns:_ internal/backend. _(Phase 0)_
- **P0 (baseline)** — Container sandbox executor (docker/podman, `--network none`, worktree bind-mounted at /work). _Owns:_ internal/sandbox. _(Phase 0)_
- **P0 (baseline)** — Verifier: runs the project's own checks and is the sole authority on "done." _Owns:_ internal/verify. _(Phase 0)_
- **P0 (baseline)** — Append-only JSONL event log. _Owns:_ internal/eventlog. _(Phase 0)_
- **P0 (baseline)** — Reversibility classifier + human gate (auto reversible, gate irreversible). _Owns:_ internal/policy. _(Phase 0)_
- **P0 (baseline)** — Orchestrator: runs a backend then re-verifies as the final gate. _Owns:_ internal/agent. _(Phase 0)_
- **P0 (baseline)** — Anthropic Messages API client, stdlib-only (zero external dependencies). _Owns:_ internal/model. _(Phase 0)_
- **P0 (baseline)** — Entrypoint, Makefile (`make verify`), README, and the docs suite (CLAUDE.md, PREREQUISITES, ARCHITECTURE, PERSONA, TASKS). _Owns:_ cmd/nilcore, Makefile, README.md, docs. _(Phase 0)_
- **P0 (baseline)** — Behavioral decisions folded into the plan: tool surface (shell + structured + MCP → tasks P1-T08/P1-T09), summarize-and-handover (P3-T06), proactive trigger (P3-T07), adaptive planning, terse persona, and the bounded self-improvement scope (P5-T01/P5-T02). _Owns:_ docs. _(Phase 0)_
- **P0 (baseline)** — Advisor-Executor (Anthropic's Advisor Strategy): cheap executor drives, strong advisor consulted on demand via `ask_advisor`; self-built + native-Advisor-Tool paths; advisor tier doubles as planner/reviewer (P3-T08). _Owns:_ docs. _(Phase 0)_
- **P0 (baseline)** — Renamed the project **Nullclaw → NilCore** across all code, the Go module/imports, the `nilcore` binary, env vars (`NILCORE_*`), and docs. _Owns:_ (whole tree). _(Phase 0)_
- **P0 (baseline)** — Updated the MCP model to **code execution with MCP** (Anthropic, Nov 2025): servers exposed as typed code APIs on the sandbox filesystem, on-demand discovery, in-sandbox result filtering, direct-call fallback (P1-T09). _Owns:_ docs. _(Phase 0)_
- **P0 (baseline)** — Multi-provider models (Anthropic + OpenAI + OpenRouter) behind one `Provider` interface, role→`provider:model` selection (P1-T10); `SecretStore` with keychain/encrypted/env backends and the never-to-the-model boundary (P1-T11, `docs/SECRETS.md`); `nilcore init` onboarding wizard (P1-T12); cross-platform paths + release matrix for macOS/Linux (P1-T13). _Owns:_ docs. _(Phase 0)_
- **P0 (baseline)** — **Design core** (`docs/PRINCIPLES.md`): ten ranked first principles ("the harness wins; the feedback loop is the product; engineer context ruthlessly; understand before you change; …"), anti-principles, and a definition of *good*. Added code intelligence / repo-map (P3-T09) to close the semantic-codebase-understanding gap, and elevated contract-first into the planner (P3-T01) and persona. _Owns:_ docs. _(Phase 0)_
- **P0 (baseline)** — **Runtime resilience & operations** (`docs/OPERATIONS.md`): the cluster that lets NilCore run **unattended** for months — the gaps an end-to-end audit surfaced. Adds **authorized control** (channel allowlist + gate-approval auth) as **P2-T07** (security), and a new **Phase 6 — runtime resilience & operations** (**P6-T01…P6-T08**): provider resilience (retry/backoff/failover/breaker), cost metering + ceiling enforcement, crash-safe task durability + resume + graceful shutdown, cross-task scheduler + resource arbitration, verification auto-detection for arbitrary repos, resource GC (worktrees/containers/logs), operator inspect/replay + health, and config schema/validation/migration. Context-window assembly is left intentionally distributed (not a task). No new runtime dependency; state is store-backed. _Owns:_ docs. _(Phase 0)_

> Note: the Phase-0 scaffold was authored offline and has **not** been compiled. Task **P0-T02** (compile + make verify green) is the first blocking task and must merge before any parallel Phase-1 work begins.
