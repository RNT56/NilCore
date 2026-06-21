#!/usr/bin/env bash
# desktop-e2e.sh — the live, MODEL-FREE container smoke for desktop computer use
# (Phase CU). It validates the parts the hermetic Go unit tests deliberately STUB:
# the real Xvfb virtual display, scrot capture, xdotool input, and the AT-SPI
# accessibility dump — inside the nilcore/sandbox-desktop container. It needs NO
# model API key and NO real desktop on the host (Xvfb is a headless in-memory X
# server), so it runs identically on Linux OR macOS (Docker/Podman runs the Linux
# container in its lightweight VM).
#
# Usage:   make desktop-e2e            # or: RUNTIME=docker bash test/desktop-e2e.sh
# Exit 0  if the live driver stack is healthy; non-zero on a hard failure.
set -uo pipefail

IMAGE="${DESKTOP_IMAGE:-nilcore/sandbox-desktop:latest}"
RUNTIME="${RUNTIME:-}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"

# ── pick a container runtime (a Mac has one via Docker Desktop / podman machine) ──
if [ -z "$RUNTIME" ]; then
  if command -v podman >/dev/null 2>&1; then RUNTIME=podman
  elif command -v docker >/dev/null 2>&1; then RUNTIME=docker
  else
    echo "SKIP: no container runtime (install Docker Desktop or 'brew install podman' on macOS)"
    exit 0
  fi
fi
echo "desktop-e2e: runtime=$RUNTIME image=$IMAGE"

# ── build the desktop image (Xvfb + WM + AT-SPI + xdotool + scrot + the driver) ──
echo "desktop-e2e: building $IMAGE …"
if ! "$RUNTIME" build -f "$HERE/images/sandbox-desktop/Dockerfile" -t "$IMAGE" "$HERE"; then
  echo "FAIL: image build failed"
  exit 1
fi

# ── in-container smoke: the live X11/AT-SPI stack, no model ──
# The here-doc runs as the container's entrypoint shell. It starts the desktop,
# launches an accessible app, and exercises capture / input / a11y. Hard failures
# (missing tools, Xvfb/scrot/xdotool broken) exit non-zero; the AT-SPI dump is
# reported but NOT hard-failed (the a11y bus is environment-sensitive).
"$RUNTIME" run --rm "$IMAGE" bash -lc '
set -u
fail() { echo "FAIL: $1"; exit 1; }

echo "== tools present =="
for t in Xvfb xdotool scrot dbus-launch nilcore-desktop nilcore-a11y-dump gnome-calculator; do
  command -v "$t" >/dev/null 2>&1 || fail "missing tool: $t"
  echo "  ok: $t"
done

echo "== start the headless desktop =="
export DISPLAY=:99
Xvfb :99 -screen 0 1280x800x24 -nolisten tcp >/tmp/xvfb.log 2>&1 &
for i in $(seq 1 50); do xdotool getdisplaygeometry >/dev/null 2>&1 && break; sleep 0.1; done
xdotool getdisplaygeometry >/dev/null 2>&1 || fail "Xvfb/xdotool did not come up"
echo "  ok: display $(xdotool getdisplaygeometry)"

echo "== capture (scrot) =="
scrot -o /tmp/shot.png || fail "scrot capture failed"
test -s /tmp/shot.png || fail "scrot produced an empty file"
echo "  ok: $(wc -c </tmp/shot.png) byte screenshot"

echo "== input (xdotool) =="
xdotool mousemove 100 100 click 1 || fail "xdotool input failed"
echo "  ok: synthetic click"

echo "== accessibility (AT-SPI dump) =="
# Bring up the session + a11y bus, register accessibility, launch an accessible app.
eval "$(dbus-launch --sh-syntax)" 2>/dev/null || true
/usr/libexec/at-spi-bus-launcher --launch-immediately >/tmp/atspi.log 2>&1 &
sleep 1
gnome-calculator >/tmp/calc.log 2>&1 &
sleep 4
DUMP="$(nilcore-a11y-dump 2>/tmp/a11y.err)"
echo "  dump: ${DUMP:0:200}"
if echo "$DUMP" | grep -q "button"; then
  echo "  ok: AT-SPI exposed interactive elements (Rung 1 available)"
else
  echo "  WARN: AT-SPI dump empty/no buttons — the a11y bus may need tuning in this"
  echo "        environment; capture/input still work, so the ladder runs at Rung 2/3."
fi

echo "PASS: desktop live stack healthy"
'
rc=$?
if [ "$rc" -ne 0 ]; then
  echo "desktop-e2e: FAILED (rc=$rc)"
  exit "$rc"
fi
echo "desktop-e2e: OK"
