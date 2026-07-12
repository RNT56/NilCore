#!/usr/bin/env bash
# End-to-end release installer proof. It builds the host asset, publishes it into a
# temporary file:// release root, and drives scripts/install.sh exactly as a user does.
# The negative controls are load-bearing: no checksum and a tampered binary must both
# fail before anything is installed.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

case "$(uname -s | tr '[:upper:]' '[:lower:]')" in
  darwin) os=darwin ;;
  linux) os=linux ;;
  *) echo "unsupported test OS" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "unsupported test architecture" >&2; exit 1 ;;
esac

asset="nilcore-${os}-${arch}"
release="$tmp/release"
mkdir -p "$release" "$tmp/bin-good" "$tmp/bin-missing" "$tmp/bin-tampered"

(
  cd "$repo_root"
  CGO_ENABLED=0 go build -trimpath -o "$release/$asset" ./cmd/nilcore
)
cp "$release/$asset" "$tmp/pristine"

write_checksum() {
  if command -v sha256sum >/dev/null 2>&1; then
    ( cd "$release" && sha256sum "$asset" > "$asset.sha256" )
  else
    ( cd "$release" && shasum -a 256 "$asset" > "$asset.sha256" )
  fi
}

run_installer() {
  bindir="$1"
  NILCORE_RELEASE_BASE="file://$release" \
    NILCORE_BINDIR="$bindir" \
    sh "$repo_root/scripts/install.sh"
}

# Positive proof: a matching binary + checksum installs and runs.
write_checksum
run_installer "$tmp/bin-good"
test -x "$tmp/bin-good/nilcore"
"$tmp/bin-good/nilcore" version >/dev/null

# Negative proof 1: the installer must fail closed when the checksum is missing.
rm "$release/$asset.sha256"
if run_installer "$tmp/bin-missing" >"$tmp/missing.out" 2>&1; then
  echo "installer accepted an asset with no checksum" >&2
  exit 1
fi
test ! -e "$tmp/bin-missing/nilcore"
grep -q "refusing to install unverified binary" "$tmp/missing.out"

# Negative proof 2: a checksum for the pristine asset cannot authorize tampered bytes.
cp "$tmp/pristine" "$release/$asset"
write_checksum
printf 'tampered' >> "$release/$asset"
if run_installer "$tmp/bin-tampered" >"$tmp/tampered.out" 2>&1; then
  echo "installer accepted a tampered asset" >&2
  exit 1
fi
test ! -e "$tmp/bin-tampered/nilcore"
grep -q "checksum mismatch" "$tmp/tampered.out"

echo "installer E2E: valid asset installed; missing checksum and tampering refused"
