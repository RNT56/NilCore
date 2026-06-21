#!/usr/bin/env bash
# desktop-mac-smoke.sh — local smoke for the native-macOS desktop driver (CU-MAC MVP,
# docs/ROADMAP-COMPUTER-USE-DARWIN.md). Unlike test/desktop-e2e.sh (a Linux container),
# this runs on the HOST: it builds nilcore-desktop-darwin, then drives ONE live
# `observe` over the real file-queue and checks a screencapture frame comes back.
#
# It is non-destructive (observe only — no clicks/typing). The live capture needs the
# terminal to hold macOS Screen Recording permission; without it the driver fails
# closed and this script WARNS (does not fail) — exactly like the AT-SPI warn in the
# Linux e2e — so the build/protocol smoke still gates in CI while the permissioned
# live path is the developer's to confirm.
set -uo pipefail

if [[ "$(uname)" != "Darwin" ]]; then
  echo "desktop-mac-smoke: not macOS (uname=$(uname)) — skipping (this smoke is host-only)." >&2
  exit 0
fi

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
BIN="$(mktemp -d)/nilcore-desktop-darwin"
CONTROL="$(mktemp -d)/control"

cleanup() { [[ -n "${SERVE_PID:-}" ]] && kill "$SERVE_PID" 2>/dev/null; rm -rf "$(dirname "$BIN")" "$(dirname "$CONTROL")"; }
trap cleanup EXIT

echo "desktop-mac-smoke: building nilcore-desktop-darwin …"
go build -o "$BIN" ./cmd/tools/nilcore-desktop-darwin || { echo "BUILD FAILED" >&2; exit 1; }

command -v screencapture >/dev/null || { echo "screencapture MISSING (unexpected on macOS)" >&2; exit 1; }
if ! command -v cliclick >/dev/null; then
  echo "desktop-mac-smoke: NOTE cliclick not installed — actuation (click/type) will fail closed until 'brew install cliclick'. (observe-only smoke continues.)" >&2
fi

echo "desktop-mac-smoke: starting driver --serve …"
"$BIN" --serve --control "$CONTROL" &
SERVE_PID=$!

# Wait for the ready marker.
for _ in $(seq 1 50); do [[ -f "$CONTROL/ready" ]] && break; sleep 0.1; done
[[ -f "$CONTROL/ready" ]] || { echo "driver never wrote ready marker" >&2; exit 1; }
echo "desktop-mac-smoke: driver ready ✓"

# Drive one live observe.
printf '{"seq":1,"act":{"op":"observe"}}' > "$CONTROL/req-1.json.tmp"
mv "$CONTROL/req-1.json.tmp" "$CONTROL/req-1.json"
for _ in $(seq 1 50); do [[ -f "$CONTROL/resp-1.json" ]] && break; sleep 0.1; done
[[ -f "$CONTROL/resp-1.json" ]] || { echo "no response to observe (round-trip broken)" >&2; exit 1; }

RESP="$(cat "$CONTROL/resp-1.json")"
echo "desktop-mac-smoke: observe round-trip ✓ (rung $(echo "$RESP" | grep -o '"rung":[0-9]*' | head -1))"

if echo "$RESP" | grep -q '"screenshot_b64":"'; then
  echo "desktop-mac-smoke: LIVE screencapture OK ✓ — a real frame was captured and marked/encoded."
else
  echo "desktop-mac-smoke: WARN — no screenshot in the observation. This almost always means the terminal lacks macOS Screen Recording permission (System Settings ▸ Privacy & Security ▸ Screen Recording). The build + file-queue protocol passed; grant the permission to exercise the live capture." >&2
fi

# Close the session cleanly.
printf '{"seq":2,"act":{"op":"close"}}' > "$CONTROL/req-2.json.tmp"
mv "$CONTROL/req-2.json.tmp" "$CONTROL/req-2.json"
sleep 0.3
echo "desktop-mac-smoke: done."
