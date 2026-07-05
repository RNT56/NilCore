#!/bin/sh
# NilCore installer — curl-pipe-sh. Downloads the release binary matching this
# host's OS/arch and installs it. Usage:
#
#   curl -fsSL https://raw.githubusercontent.com/RNT56/NilCore/main/scripts/install.sh | sh
#
# Override with: NILCORE_VERSION=v0.2.0 NILCORE_BINDIR=$HOME/.local/bin sh install.sh
set -eu

REPO="RNT56/NilCore"
BINDIR="${NILCORE_BINDIR:-/usr/local/bin}"
VERSION="${NILCORE_VERSION:-latest}"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$os" in
  darwin) os="darwin" ;;
  linux)  os="linux" ;;
  *) echo "unsupported OS: $os" >&2; exit 1 ;;
esac

arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) echo "unsupported arch: $arch" >&2; exit 1 ;;
esac

asset="nilcore-${os}-${arch}"
if [ "$VERSION" = "latest" ]; then
  base="https://github.com/${REPO}/releases/latest/download"
else
  base="https://github.com/${REPO}/releases/download/${VERSION}"
fi
url="${base}/${asset}"
sumurl="${url}.sha256"

tmp="$(mktemp)"
sumtmp="$(mktemp)"
cleanup() { rm -f "$tmp" "$sumtmp"; }
trap cleanup EXIT

echo "Downloading ${asset} (${VERSION})..."
curl -fsSL "$url" -o "$tmp"

# Verify the published SHA-256 before running or installing the binary, so a CORRUPTED or
# TRUNCATED download (partial transfer, CDN glitch) is never executed. Note the scope: the
# checksum is fetched from the same GitHub release over HTTPS, so it is an integrity check,
# NOT a signature — it does not defend against a compromised release (an attacker who can
# replace the binary can replace its .sha256 too). Signed releases would be the next step.
# The release publishes "<asset>.sha256" (and a combined SHA256SUMS) alongside every binary.
# Requires a sha256 tool (sha256sum on Linux, shasum on macOS) — if neither is present we
# FAIL CLOSED rather than install unverified.
if curl -fsSL "$sumurl" -o "$sumtmp"; then
  expected="$(awk '{print $1}' "$sumtmp" | head -n1)"
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$tmp" | awk '{print $1}')"
  elif command -v shasum >/dev/null 2>&1; then
    actual="$(shasum -a 256 "$tmp" | awk '{print $1}')"
  else
    echo "error: no sha256 tool (sha256sum/shasum) to verify the download" >&2
    exit 1
  fi
  if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then
    echo "error: checksum mismatch for ${asset}" >&2
    echo "  expected: ${expected:-<empty>}" >&2
    echo "  actual:   ${actual}" >&2
    exit 1
  fi
  echo "Checksum verified (sha256: ${actual})."
else
  echo "error: could not fetch ${asset}.sha256 to verify the download; refusing to install unverified binary" >&2
  echo "  (set NILCORE_VERSION to a release that publishes checksums)" >&2
  exit 1
fi

chmod +x "$tmp"

if [ -w "$BINDIR" ]; then
  mv "$tmp" "${BINDIR}/nilcore"
else
  echo "Installing to ${BINDIR} (requires sudo)..."
  sudo mv "$tmp" "${BINDIR}/nilcore"
fi
trap - EXIT
rm -f "$sumtmp"

echo "Installed nilcore to ${BINDIR}/nilcore"
"${BINDIR}/nilcore" -h 2>/dev/null || true
