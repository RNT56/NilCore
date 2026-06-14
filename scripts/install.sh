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
  url="https://github.com/${REPO}/releases/latest/download/${asset}"
else
  url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"
fi

tmp="$(mktemp)"
echo "Downloading ${asset} (${VERSION})..."
curl -fsSL "$url" -o "$tmp"
chmod +x "$tmp"

if [ -w "$BINDIR" ]; then
  mv "$tmp" "${BINDIR}/nilcore"
else
  echo "Installing to ${BINDIR} (requires sudo)..."
  sudo mv "$tmp" "${BINDIR}/nilcore"
fi

echo "Installed nilcore to ${BINDIR}/nilcore"
"${BINDIR}/nilcore" -h 2>/dev/null || true
