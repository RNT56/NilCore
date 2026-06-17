# NilCore sandbox image — the isolated environment every model-emitted command
# runs in (invariant I4). The base is pinned so builds are reproducible, and it
# ships the toolchain the verifier needs (Go, git, make). Delegated CLIs (Codex,
# Claude Code) are layered on here in Phase 2 for in-container delegation — see
# README.md and task P2-T03.
#
# It also carries a pinned headless Chromium and the operator-trusted
# `nilcore-browser` driver on $PATH, which the browser_view tool shells out to
# (D1-T01). The driver is compiled here, never model-emitted, and the browser run
# stays I4-confined (it only reaches hosts the egress allowlist permits).
#
# Build & tag (Podman preferred — rootless; Docker also works):
#   podman build -t nilcore/sandbox:latest images/sandbox
#   docker build -t nilcore/sandbox:latest images/sandbox
#
# The build context is the repo root's images/sandbox dir, but the driver source
# lives in cmd/tools/nilcore-browser. Build from the repo root so the source is in
# context:
#   podman build -f images/sandbox/Dockerfile -t nilcore/sandbox:latest .
#   docker build -f images/sandbox/Dockerfile -t nilcore/sandbox:latest .
#
# Smoke:
#   podman run --rm nilcore/sandbox:latest sh -c 'git --version && go version && make --version && chromium --version && command -v nilcore-browser'

# ── Stage 1: build the nilcore-browser driver ──────────────────────────────────
# The driver is behind the `browserdriver` build tag so it never links into the
# default `go build ./...`; we compile it explicitly here. CGO is disabled so the
# binary is static and matches the project-wide CGO_ENABLED=0 release posture (I6).
FROM golang:1.23.4-bookworm AS driver-build
WORKDIR /src
# Copy the module manifests first for layer caching, then the source the driver
# needs. cmd/tools/nilcore-browser is pure stdlib, so the full tree is not
# required, but copying it keeps the build robust to incidental imports.
COPY . .
RUN CGO_ENABLED=0 go build -tags browserdriver \
        -o /out/nilcore-browser ./cmd/tools/nilcore-browser

# ── Stage 2: the runtime sandbox image ─────────────────────────────────────────
# Pinned base: the golang image already carries the Go toolchain and git.
FROM golang:1.23.4-bookworm

# make is the only build tool the base lacks; Go and git are already present.
# chromium is the headless browser the driver shells out to. Versions are fixed by
# the pinned base image and Debian bookworm's package set, so rebuilds are
# reproducible. (Debian ships Chromium as the `chromium` package; the binary is on
# PATH as `chromium`, which is the driver's default — NILCORE_CHROMIUM can override.)
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        make \
        chromium \
        ca-certificates \
        fonts-liberation \
    && rm -rf /var/lib/apt/lists/*

# Place the operator-trusted driver on PATH.
COPY --from=driver-build /out/nilcore-browser /usr/local/bin/nilcore-browser
RUN chmod 0755 /usr/local/bin/nilcore-browser

# A non-root user runs the sandboxed commands (least privilege). Phase 2 (P2-T01)
# runs the container as this user with host-UID mapping (podman --userns=keep-id,
# or `docker run --user`) so writes to the bind-mounted /work succeed; the image
# itself is ready for that today. Headless Chromium runs fine as this user; the
# driver passes --no-sandbox because Chromium's own setuid sandbox is unavailable
# in an unprivileged container (we are already I4-confined by the host sandbox).
RUN useradd --create-home --uid 1000 nilcore
USER nilcore

# Go's caches live under the user's home (writable); keep them off /work.
ENV GOCACHE=/home/nilcore/.cache/go-build \
    GOPATH=/home/nilcore/go

WORKDIR /work
