# NilCore DESKTOP sandbox image (Phase CU, docs/ROADMAP-COMPUTER-USE.md, CU-T01).
#
# This is the OPTIONAL sidecar image for desktop computer use — the contained
# virtual X11 desktop the model drives. Nothing pulls it by default; it is built
# and tagged only when an operator opts into computer use AND has cleared the §0
# gate. The model NEVER touches the host screen (I4): the entire desktop (Xvfb,
# window manager, apps, the input/capture/a11y tools) lives INSIDE this image,
# driven by the operator-trusted `nilcore-desktop` driver baked in here.
#
# It mirrors the headless-browser image pattern (images/sandbox/): the heavy,
# untrusted-adjacent machinery is image-baked binaries, NOT the Go core's go.mod —
# so the default `nilcore` binary stays stdlib-only and CGO_ENABLED=0 (I6). The
# driver shells to these image-baked tools exactly as nilcore-browser shells to
# chromium:
#   - Xvfb            : a virtual X11 display (no physical screen)
#   - a window manager + panel
#   - xdotool         : synthetic mouse/keyboard input (X11)
#   - scrot           : screen capture (PNG)
#   - AT-SPI2 + pyatspi: the accessibility tree (set-of-marks Rung 1)
#   - GTK/Qt apps     : the friendly-accessibility surfaces desktop CU is best at
#
# Build & tag (Podman preferred; Docker also works), from the repo root so the
# driver source is in context:
#   podman build -f images/sandbox-desktop/Dockerfile -t nilcore/sandbox-desktop:latest .
#   docker build -f images/sandbox-desktop/Dockerfile -t nilcore/sandbox-desktop:latest .
#
# Smoke:
#   podman run --rm nilcore/sandbox-desktop:latest sh -c \
#     'command -v Xvfb && command -v xdotool && command -v scrot && command -v nilcore-desktop && command -v nilcore-a11y-dump'

# ── Stage 1: build the nilcore-desktop driver (pure stdlib, CGO_ENABLED=0) ──────
# The driver is a standalone tool in cmd/tools/nilcore-desktop; it is pure stdlib
# (it shells to the image-baked tools above), NOT linked into the default nilcore
# binary, and builds under the normal toolchain with no build tag.
FROM golang:1.23.4-bookworm AS driver-build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /out/nilcore-desktop ./cmd/tools/nilcore-desktop

# ── Stage 2: the runtime desktop image ─────────────────────────────────────────
FROM debian:bookworm-slim

# The contained desktop stack. Versions are fixed by the pinned base + bookworm's
# package set, so rebuilds are reproducible. atspi/pyatspi provide the a11y tree;
# the GTK toolkit env vars below force accessibility ON so the tree is populated.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        xvfb \
        x11-utils \
        openbox \
        tint2 \
        xdotool \
        scrot \
        dbus-x11 \
        at-spi2-core \
        python3 \
        python3-gi \
        gir1.2-atspi-2.0 \
        ca-certificates \
        fonts-liberation \
        gnome-text-editor \
        gnome-calculator \
        pcmanfm \
    && rm -rf /var/lib/apt/lists/*

# The operator-trusted driver on PATH.
COPY --from=driver-build /out/nilcore-desktop /usr/local/bin/nilcore-desktop
RUN chmod 0755 /usr/local/bin/nilcore-desktop

# The AT-SPI dump tool the driver shells to for Rung-1 set-of-marks. It enumerates
# the accessible tree (pyatspi) and prints a compact JSON list of interactive,
# on-screen widgets — the driver parses this in pure Go. Keeping the a11y dependency
# as an image-baked Python script (NOT go.mod) is exactly the chromium pattern: the
# heavy/untrusted-adjacent dependency lives in the image, the Go core stays clean (I6).
COPY images/sandbox-desktop/nilcore-a11y-dump /usr/local/bin/nilcore-a11y-dump
RUN chmod 0755 /usr/local/bin/nilcore-a11y-dump

# Force accessibility ON across toolkits so the a11y tree is actually populated
# (the documented Rung-1 enablement; without this many apps expose an empty tree).
ENV GTK_MODULES=gail:atk-bridge \
    QT_ACCESSIBILITY=1 \
    QT_LINUX_ACCESSIBILITY_ALWAYS_ON=1 \
    NO_AT_BRIDGE=0 \
    DISPLAY=:99

# A non-root user runs the desktop (least privilege).
RUN useradd --create-home --uid 1000 nilcore
USER nilcore
WORKDIR /work
