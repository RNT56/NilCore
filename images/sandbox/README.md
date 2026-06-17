# NilCore sandbox image

The container image every model- or agent-emitted command executes inside
(invariant **I4** — nothing the model emits runs on the host). The host worktree
is bind-mounted at `/work`; networking is `--network none` by default (an egress
allowlist arrives in Phase 2, P2-T02).

## Contents

- Pinned base `golang:1.23.4-bookworm` → Go toolchain + `git`.
- `make` (so `make verify` works as the in-container check).
- A pinned headless **Chromium** (Debian `chromium` package) + the
  operator-trusted **`nilcore-browser`** driver on `$PATH` — see below.
- A non-root user `nilcore` (uid 1000) — least privilege.

## Build & tag

The `nilcore-browser` driver is compiled from `cmd/tools/nilcore-browser` inside
the image, so the **build context must be the repo root** (not `images/sandbox`):

```sh
# Podman (preferred — rootless)
podman build -f images/sandbox/Dockerfile -t nilcore/sandbox:latest .

# Docker
docker build -f images/sandbox/Dockerfile -t nilcore/sandbox:latest .
```

Point the agent at it with `-image nilcore/sandbox:latest` (and `-runtime
podman|docker`).

## Verify

```sh
podman run --rm nilcore/sandbox:latest \
    sh -c 'git --version && go version && make --version && chromium --version && command -v nilcore-browser'
```

## Headless browser (`nilcore-browser`)

The `browser_view` tool (`internal/tools/browser.go`) navigates a URL inside the
sandbox by shelling out to the driver:

```
nilcore-browser --url '<url>' --format json
```

The driver prints a single JSON object — `{title, text, console, screenshot_b64}`
— to stdout and exits **non-zero** on any failure, so the tool **fails closed**
(it never fabricates a passing observation). Implementation notes:

- It drives Chromium through its own built-in batch flags
  (`--headless=new --dump-dom --screenshot=… --virtual-time-budget=…`), **not** a
  hand-rolled CDP/websocket client — so it stays pure-stdlib with zero new module
  dependencies (invariant I6) and builds with `CGO_ENABLED=0`.
- It is a standalone tool (`cmd/tools/nilcore-browser`); it is not linked into the
  default `nilcore` binary, but it builds under the normal toolchain and its
  pure-logic unit tests run in `make verify`. The image compiles it explicitly.
- The browser binary is configurable via `NILCORE_CHROMIUM` (default `chromium`).
  A missing binary is a hard, non-zero failure (fail-closed).
- The rendered DOM and console output are **untrusted** data; `browser_view`
  `guard.Wrap`s them (invariant I7) before they reach the model.

The driver and browser are **operator-trusted** (baked into the image), never
model-emitted. The browser run itself is exercised by a CI `browser-e2e` job
against a fixture static server (mirroring the `sandbox-linux` job); the Go unit
tests in `cmd/tools/nilcore-browser` are hermetic and cover the pure logic.

## Writing to `/work` (UID mapping)

The image runs as the non-root `nilcore` user. For the agent to edit the
bind-mounted worktree, the container user must map to the host file owner:

- **Podman rootless:** `--userns=keep-id` maps your host UID onto `nilcore`.
- **Docker:** `--user "$(id -u):$(id -g)"`.

Wiring this into the sandbox executor is **P2-T01** (hardened flags). Until then,
callers running the sandbox manually should pass the mapping flag themselves.

## Adding the delegated CLIs (Phase 2 — P2-T03)

For in-container delegation, the Codex and Claude Code CLIs are installed into a
derived image so they run **inside** this sandbox (defense in depth) with their
API key injected per run, never persisted:

```dockerfile
FROM nilcore/sandbox:latest
USER root
# Install Node.js (pinned) and the CLIs:
#   npm i -g @openai/codex @anthropic-ai/claude-code   # versions pinned in P2-T03
USER nilcore
```

Keys are passed via per-run container env (`-e ANTHROPIC_API_KEY` /
`-e CODEX_API_KEY`), sourced from the SecretStore (P1-T11) — never baked into the
image, never logged (invariant I3).
