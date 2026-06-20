# Prerequisites & setup

Everything you need to build, run, and contribute to NilCore. Source of truth for rules is `CLAUDE.md`; this document is the operational detail.

## 1. Toolchain

| Tool | Version | Why |
|---|---|---|
| Go | 1.23+ (latest stable recommended) | the entire core |
| Container runtime | **Podman ≥ 4 (rootless, preferred)** or Docker | the sandbox |
| git | ≥ 2.30 | worktree-per-task workflow |
| make | any | `make verify` is the gate |
| golangci-lint | latest | lint gate in CI and locally |
| jq | any | inspecting the JSONL event log and CLI streams |
| SQLite | 3.x | Phase 4 memory store |
| sqlc | latest | Phase 4 typed queries (matches the chosen stack) |

Install Go from <https://go.dev/dl/>. Install Podman from <https://podman.io/> (rootless is the default on modern Linux). On macOS use `podman machine` or Docker Desktop. Install golangci-lint per <https://golangci-lint.run/>.

> The core has **zero Go module dependencies** by design. Do not add one without justification in the PR and CHANGELOG (see `CLAUDE.md` §2, invariant 6).

## 2. Delegated coding-agent CLIs (for the `codex` / `claude-code` backends)

The native backend needs only an Anthropic API key. The delegating backends need the respective CLIs installed and runnable headlessly.

- **Codex CLI** — provides `codex exec --json` (non-interactive, JSONL events, `--output-schema`, `--full-auto`, `danger-full-access`). Install per the official OpenAI Codex docs. Auth: `CODEX_API_KEY` set **per invocation** (`CODEX_API_KEY=... codex exec ...`). `codex exec` requires a git repo (a worktree qualifies).
- **Claude Code CLI** — provides `claude -p --output-format stream-json` (headless, with `--allowedTools`, `--permission-mode`, `--resume`). Install per the official Anthropic Claude Code docs. Auth via `claude login` or `ANTHROPIC_API_KEY`.

> Cost note: programmatic Claude Code on subscription plans draws from a separate monthly Agent SDK credit (effective 2026-06-15). Budget accordingly; this is one reason routing and per-task budgets matter (see `docs/ARCHITECTURE.md`).

In production both CLIs run **inside the sandbox container** (defense in depth — they sandbox themselves too) with their API key injected into the container env for the single run only. Phase 0 invokes them directly in the worktree; Phase 2 moves them inside the container.

### Per-CLI model / effort / config (optional, R1)

The delegating backends are configurable, not key-only — every knob is optional and **zero fields ⇒ byte-identical** to the bare command. Set them in `config.json` under `codex` / `claude` (written by `nilcore init`), or override at runtime with env vars:

- **Model** — `NILCORE_CODEX_MODEL` / `NILCORE_CLAUDE_MODEL` (→ `--model`).
- **Effort** — `NILCORE_CODEX_EFFORT` (→ `-c model_reasoning_effort=<e>`) / `NILCORE_CLAUDE_EFFORT` (→ the `CLAUDE_CODE_EFFORT_LEVEL` env, since the flag name drifts across versions).
- **`extra_args`** (config only) — raw extra CLI tokens, e.g. `["-c", "key=value"]`.
- **`env`** (config only) — extra per-run environment merged with the API key. Use it for **`CODEX_HOME` / `CLAUDE_CONFIG_DIR`** to surface a host config dir despite the sandbox's `HOME=/tmp`. The API key is merged **last**, so an operator `env` can never shadow it, and it is never logged (I3).

## 3. Accounts & secrets

| Secret | Used by | Notes |
|---|---|---|
| `ANTHROPIC_API_KEY` | Anthropic provider, Claude Code backend | |
| `OPENAI_API_KEY` | OpenAI provider (GPT-5.5 / 5.5-pro / 5.4-mini) | |
| `OPENROUTER_API_KEY` | OpenRouter provider | OpenAI-compatible aggregator |
| `CODEX_API_KEY` | Codex backend | per-invocation injection |
| `TELEGRAM_BOT_TOKEN` | Telegram channel (Phase 1) | from @BotFather |
| `SLACK_APP_TOKEN` / `SLACK_BOT_TOKEN` | Slack channel (Phase 1, alt) | socket-mode app |
| `NILCORE_EMBED_KEY` | semantic code search (opt-in, Phase 10) | OpenAI-compatible embeddings key; off ⇒ lexical fallback (byte-identical) |
| `NILCORE_FORGE_TOKEN` | gated draft-PR open (`watch`/`schedule --open-pr`) | the agent never merges; push runs in the approved prepare step |
| `NILCORE_WEBHOOK_SECRET` | `serve --webhook` (HMAC verification) | shared secret for SCM/CI webhook signatures |
| Tailscale auth key | tsnet remote access (optional, later) | if exposing over a tailnet |

**Secrets never reach the model.** `nilcore init` (below) stores them in the **SecretStore** — the OS keychain (macOS Keychain / Linux Secret Service) or an encrypted-file vault on a headless host — and they are injected per run into request headers or child-process env, never into a prompt, log, or config file. The full design (backends, headless-VPS master key, redaction) is in **`docs/SECRETS.md`**. A gitignored `.env` is supported only for CI/advanced use.

## 4. First-run setup

The guided path — works on macOS and on a headless Linux VPS over SSH:

```sh
nilcore init        # interactive wizard: providers + keys (→ SecretStore),
                    # executor/advisor models, backend, sandbox runtime + image,
                    # channel + serve allowlist — then a readiness summary
nilcore doctor      # re-check readiness (keys resolve, runtime, serve allowlist)
```

`nilcore init` also has a non-interactive mode (`-non-interactive`, driven by `NILCORE_*`/provider env vars) for scripted VPS provisioning, including a Slack channel and the serve allowlist. Under the hood it does what the manual path below does.

Manual / advanced path:

```sh
git clone <repo> nilcore && cd nilcore

# 1. Make the gate green (this is task P0-T02 if it is not yet done)
make verify

# 2. Build the sandbox image (task P0-T03)
podman build -t nilcore/sandbox:latest images/sandbox    # or: docker build ...

# 3. Provide secrets — prefer `nilcore init`; for CI, a gitignored .env works
cp .env.example .env && $EDITOR .env     # set provider keys, etc.

# 4. Smoke test the native backend against a throwaway repo
export ANTHROPIC_API_KEY=sk-...
go run ./cmd/nilcore \
  -dir ./test/fixtures/failing-go \
  -goal "make the failing test pass" \
  -verify "go build ./... && go test ./..." \
  -runtime podman
```

Inspect what happened:

```sh
jq . nilcore.events.jsonl        # the full, replayable audit trail
```

## 5. Best practices

- **Branch per task, small PRs.** One task = one branch = one PR (see `CLAUDE.md` §5). Never edit outside your task's `Owns` set.
- **Verifier-green before merge.** `make verify` is the Definition of Done gate. CI enforces it on every PR.
- **Rootless containers.** Prefer Podman rootless; drop capabilities; no `danger-full-access` outside an isolated CI runner.
- **Pin tool versions in CI.** Reproducible builds; the same `make verify` locally and in CI.
- **Conventional commits**, scoped to your task. Append your CHANGELOG entry as part of the PR.
- **Measure before optimizing.** No speculative optimization without a probe that shows it matters (this is a project value, not just a slogan).
- **Treat the model as fallible.** Defend with the verifier, not with prompt cleverness. Design as if the model is sometimes wrong.

## 6. Continuous integration

CI (task **P0-T01**, GitHub Actions) runs on every push and PR:

```
make verify          # build + vet + test
golangci-lint run    # lint gate
```

A PR cannot merge unless CI is green. Merge to `main` additionally requires the human/approver sign-off mandated by the autonomy policy (merge is an irreversible action).

## 7. Platform notes

- Podman rootless and Firecracker microVMs (the Phase-2 stronger-isolation option) are Linux/KVM only.
- On macOS, the container sandbox runs inside the Podman/Docker VM; Firecracker is not available — use containers there.
- `tsnet` (optional remote-access path) embeds Tailscale in the binary; no exposed ports, identity over the tailnet.
- **Install:** one cross-compiled binary (`darwin`/`linux` × `amd64`/`arm64`) — a Homebrew tap on macOS, a curl-pipe-sh installer plus a sample systemd unit on a Linux VPS (task P1-T13).
- **Secret backend by host:** macOS → Keychain; Linux desktop → Secret Service; headless VPS → encrypted-file vault with a `0600` key-file (or systemd-creds / passphrase). Auto-detected; see `docs/SECRETS.md`.

## 8. Opt-in capability prerequisites (Phase 9–10)

These capabilities are **off by default** — the default binary is byte-identical when they are unset. Each carries its own prerequisite; nothing here adds a Go module dependency (still exactly two in the default binary).

- **Behavioral verification** (`browser_view` tool + composite verifier, opt-in via `NILCORE_BROWSER_VERIFY`) needs the **container backend** and a **Chromium-bearing sandbox image** — the pure-Go `nilcore-browser` driver is baked into the image; point `NILCORE_CHROMIUM` at the browser binary if it is non-standard. The live browser run is **CI-only** (no Chromium in hermetic unit tests), and the driver **fails closed** without a browser, so the verifier stays the sole authority on "done". `browser_view` hands the model a screenshot as a multimodal image block, and — given an optional `actions` script — first **drives a flow** (navigate/click/type/wait, e.g. log in or submit a form) over a pure-Go CDP client before observing (R3); the same Chromium binary serves both the batch and the `--actions` path.
- **Semantic code search** (pure-Go HNSW vector index) is enabled by an **embeddings API key** in `NILCORE_EMBED_KEY` (any OpenAI-compatible embedder; model overridable via `NILCORE_EMBED_MODEL`). Unset ⇒ the lexical fallback runs and behavior is byte-identical.
- **Event-driven / scheduled autonomy** (`serve --webhook`, `nilcore schedule`) routes through the existing reversible-auto-start / human-gate machinery; headless runs deny-default irreversible work. Webhook signatures are HMAC-verified with `NILCORE_WEBHOOK_SECRET` (optionally scoped by `NILCORE_WEBHOOK_LABEL`); gated draft PRs (`--open-pr`) use `NILCORE_FORGE_TOKEN`.
