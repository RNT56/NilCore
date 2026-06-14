# NilCore sandbox image

The container image every model- or agent-emitted command executes inside
(invariant **I4** — nothing the model emits runs on the host). The host worktree
is bind-mounted at `/work`; networking is `--network none` by default (an egress
allowlist arrives in Phase 2, P2-T02).

## Contents

- Pinned base `golang:1.23.4-bookworm` → Go toolchain + `git`.
- `make` (so `make verify` works as the in-container check).
- A non-root user `nilcore` (uid 1000) — least privilege.

## Build & tag

```sh
# Podman (preferred — rootless)
podman build -t nilcore/sandbox:latest images/sandbox

# Docker
docker build -t nilcore/sandbox:latest images/sandbox
```

Point the agent at it with `-image nilcore/sandbox:latest` (and `-runtime
podman|docker`).

## Verify

```sh
podman run --rm nilcore/sandbox:latest sh -c 'git --version && go version && make --version'
```

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
