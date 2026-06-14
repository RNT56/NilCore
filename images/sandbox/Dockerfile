# NilCore sandbox image — the isolated environment every model-emitted command
# runs in (invariant I4). The base is pinned so builds are reproducible, and it
# ships the toolchain the verifier needs (Go, git, make). Delegated CLIs (Codex,
# Claude Code) are layered on here in Phase 2 for in-container delegation — see
# README.md and task P2-T03.
#
# Build & tag (Podman preferred — rootless; Docker also works):
#   podman build -t nilcore/sandbox:latest images/sandbox
#   docker build -t nilcore/sandbox:latest images/sandbox
#
# Smoke:
#   podman run --rm nilcore/sandbox:latest sh -c 'git --version && go version && make --version'

# Pinned base: the golang image already carries the Go toolchain and git.
FROM golang:1.23.4-bookworm

# make is the only tool the base lacks; Go and git are already present. Versions
# are fixed by the pinned base image and Debian bookworm's package set.
RUN apt-get update \
    && apt-get install -y --no-install-recommends make \
    && rm -rf /var/lib/apt/lists/*

# A non-root user runs the sandboxed commands (least privilege). Phase 2 (P2-T01)
# runs the container as this user with host-UID mapping (podman --userns=keep-id,
# or `docker run --user`) so writes to the bind-mounted /work succeed; the image
# itself is ready for that today.
RUN useradd --create-home --uid 1000 nilcore
USER nilcore

# Go's caches live under the user's home (writable); keep them off /work.
ENV GOCACHE=/home/nilcore/.cache/go-build \
    GOPATH=/home/nilcore/go

WORKDIR /work
