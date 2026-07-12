#!/usr/bin/env bash
# Cross-build the exact release matrix and drive the same fail-closed asset verifier
# used by .github/workflows/release.yml. A missing checksum is the negative control.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
dist="$tmp/dist"
mkdir -p "$dist"

for os in darwin linux; do
  for arch in amd64 arm64; do
    asset="nilcore-${os}-${arch}"
    (
      cd "$repo_root"
      CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w" -o "$dist/$asset" ./cmd/nilcore
    )
    if command -v sha256sum >/dev/null 2>&1; then
      ( cd "$dist" && sha256sum "$asset" > "$asset.sha256" )
    else
      ( cd "$dist" && shasum -a 256 "$asset" > "$asset.sha256" )
    fi
  done
done

"$repo_root/scripts/verify-release-assets.sh" "$dist"
test "$(wc -l < "$dist/SHA256SUMS" | tr -d ' ')" = "4"

rm "$dist/nilcore-linux-arm64.sha256"
if "$repo_root/scripts/verify-release-assets.sh" "$dist" >"$tmp/missing.out" 2>&1; then
  echo "release verifier accepted a missing per-asset checksum" >&2
  exit 1
fi
grep -q "missing checksum nilcore-linux-arm64.sha256" "$tmp/missing.out"

echo "release assets E2E: four targets verified; partial release refused"
