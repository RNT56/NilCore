# Roadmap — resolving the external review's findings

A mid-2026 external review of NilCore raised four findings. Three are valid, in-scope, and resolved by the workstreams below (each its own verified PR); the fourth (full desktop computer use) is a **deliberate non-goal**, not a gap. Every workstream is **additive, opt-in, stdlib-only (no new module, `CGO_ENABLED=0`), and changes no invariant** — the default binary stays byte-identical.

| Finding | Verdict | Workstream |
|---|---|---|
| Code intelligence only parses Go + Python | Valid gap | **R2** — pure-Go TS/JS + Rust parser backends |
| Codex / Claude Code delegation is hardcoded, key-only | Valid under-investment | **R1** — model/effort/args/env passthrough |
| Browser observation, not control | Valid (in-scope extension) | **R3** — pure-Go CDP client + `--actions` flow driving |
| No full desktop computer use | **Non-goal** — conflicts with the sandbox / no-ambient-authority thesis (I3/I4) | Not pursued; stays gated (`docs/ROADMAP-EXTERNAL-INFRA.md`) |

---

## R1 — Delegated-CLI configurability

**Problem.** `internal/backend/{codex,claudecode}.go` hardcoded the command (`codex exec --json --full-auto …` / `claude -p … --permission-mode acceptEdits`) and injected **only** the API key — no model, effort, config overrides, or config-dir visibility (the container's `HOME=/tmp` hides host `~/.codex`/`~/.claude`).

**Resolution.** Additive `Model` / `Effort` / `ExtraArgs` / `Env` fields on both structs (I1: the frozen `CodingBackend` interface is untouched). Pure builders (`codexArgs`/`claudeArgs`) thread them: Codex → `--model`, `-c model_reasoning_effort=…`, raw `ExtraArgs`; Claude → `--model`, `ExtraArgs`, with effort as `CLAUDE_CODE_EFFORT_LEVEL` (the flag name drifts across CLI versions; the env is the stable surface). `Env` is merged per-run for `CODEX_HOME`/`CLAUDE_CONFIG_DIR` and the like — solving the `HOME=/tmp` config-visibility issue. Every interpolated value is `shellQuote`'d (no breakout); the API key is merged **last** (an operator `Env` can't shadow it); the event log still records only `{cli, exit}` — never the model/effort/env/key (I3). Wired via `onboard.Config.{Codex,Claude}` (config file) with `NILCORE_CODEX_MODEL`/`_EFFORT` etc. env overrides. **Zero fields ⇒ byte-identical** (asserted).

_Owns:_ `internal/backend`, `internal/onboard`, `cmd/nilcore`.

---

## R2 — Multi-language code intelligence

**Problem.** Structural codeintel only parsed `.go` + `.py`, while the *verifier* already detects JS (`package.json`), Rust (`Cargo.toml`), and Python — so a Node or Rust repo could be verified but got almost no graph/context. The asymmetry is the real finding.

**Resolution.** Two new **pure-Go heuristic** parser backends behind the existing `ast` `languageParser` seam: **TypeScript/JavaScript** (`.ts .tsx .js .jsx .mjs .cjs`) and **Rust** (`.rs`). Brace-depth span tracking with cross-line string/comment carry state; function/class/method/struct/enum/trait extraction + call references (member/path calls record the trailing name; a header never self-references). Registered in the single `parsers` map, so `SupportedExtensions()` auto-grows and the live + codeintel index walks pick up the new languages with **no other wiring**. Go + Python output is byte-identical. Honest framing kept: these are **heuristic line scanners, not full grammars** — the LSP seam (`NILCORE_LSP_COMMAND`) remains the "precise" lens (e.g. `tsserver`, `rust-analyzer`).

_Owns:_ `internal/codeintel/ast`.

---

## R3 — Browser interaction (the in-scope extension of behavioral verification)

**Problem.** `browser_view` could *view* a page (title/text/console/screenshot) but not *act* on it — so behavioral verification couldn't test a **flow** (log in, submit a form). (Full desktop computer use — driving arbitrary GUI apps — stays a non-goal: it grants the kind of ambient control the sandbox/I3/I4 thesis refuses.)

**Resolution.** A new pure-Go `internal/cdp`: a minimal RFC6455 WebSocket client + Chrome DevTools Protocol client (stdlib only — `net`, `crypto/sha1`, `crypto/rand`, `encoding/json`), supporting `Page.navigate`/`captureScreenshot`, `Runtime.evaluate` (title/text + selector→coordinates), and `Input.dispatchMouseEvent`/`dispatchKeyEvent`/`insertText` (click/type). `nilcore-browser` gains an `--actions <json>` mode (navigate / click / type / wait) that drives Chrome over CDP, then captures the **same** observation contract; the batch (no-actions) path is unchanged. The WebSocket codec, CDP request shapes, and the actions parser are unit-tested hermetically (an in-memory peer); the **live interactive run is CI-only** (extends the `browser-e2e` job), like `sandbox-linux`.

_Owns:_ `internal/cdp`, `cmd/tools/nilcore-browser`, `internal/tools` (the verification-layer wiring).

---

*Promoting any of these into the canonical `docs/TASKS.md` DAG is a serialized contract task. Each ships as its own PR, adversarially reviewed and `make verify`-green, the way NilCore ships everything.*
